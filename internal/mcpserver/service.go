// Package mcpserver exposes the session daemon through MCP.
//
// Handlers are thin: validate, call the daemon, render. All terminal
// behaviour lives in internal/session and internal/daemon so the MCP surface
// and the human CLI cannot drift apart.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
	"github.com/sairaph/interactive-terminal-mcp/internal/keys"
	"github.com/sairaph/interactive-terminal-mcp/internal/session"
)

const (
	serverName         = "interactive-terminal-mcp"
	developmentVersion = "dev"
)

// Instructions tell a model what this server is for before it has called
// anything. They are deliberately short: the tool descriptions carry the
// detail, and a long preamble is paid for on every request.
const Instructions = `Persistent terminal sessions. Each session is a real shell that keeps running between tool calls, so you can start a long command, do something else, and come back to it.

Typical flow: it_list to see whether a session already exists, it_new to create one, it_send to run commands, it_read to check on them, it_tail for output that has scrolled away.

Every tool that touches a session takes its id or name; there is no current session. it_new returns the id to use.

Sessions support full-screen programs (vim, htop, less). Send keystrokes to them with the keys argument.`

// Dialer opens a connection to the daemon. It is a field so tests can supply
// an in-process daemon instead of a real socket.
type Dialer func(ctx context.Context) (*ipc.Client, error)

// Service binds one daemon connection factory to an MCP server.
type Service struct {
	dial     Dialer
	settings config.Config
	server   *mcp.Server
}

// New constructs the seven-tool MCP service.
func New(dial Dialer, settings config.Config, version string) (*Service, error) {
	if dial == nil {
		return nil, errors.New("a daemon dialer is required")
	}
	if version == "" {
		version = developmentVersion
	}
	server := mcp.NewServer(
		&mcp.Implementation{Name: serverName, Version: version},
		&mcp.ServerOptions{
			Instructions: Instructions,
			Capabilities: &mcp.ServerCapabilities{},
		},
	)
	service := &Service{dial: dial, settings: settings, server: server}
	service.registerTools()
	server.AddReceivingMiddleware(renderSDKToolErrors)
	return service, nil
}

// Server returns the underlying SDK server.
func (s *Service) Server() *mcp.Server { return s.server }

// Run serves one MCP connection over transport.
func (s *Service) Run(ctx context.Context, transport mcp.Transport) error {
	if transport == nil {
		return errors.New("an MCP transport is required")
	}
	return s.server.Run(ctx, transport)
}

// RunStdio serves MCP framing on stdin and stdout. The SDK is the only
// component that writes to stdout; everything else uses stderr, because a
// stray byte on stdout corrupts the protocol.
func (s *Service) RunStdio(ctx context.Context) error {
	return normalizeRunError(s.Run(ctx, &mcp.StdioTransport{}))
}

// ServeStdio constructs and runs a service over stdin and stdout.
func ServeStdio(ctx context.Context, dial Dialer, settings config.Config, version string) error {
	service, err := New(dial, settings, version)
	if err != nil {
		return err
	}
	return service.RunStdio(ctx)
}

func normalizeRunError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// call opens a short-lived daemon connection for one tool call.
//
// Connections are per-call rather than pooled: an MCP process may sit idle for
// hours between calls, and a held connection would keep the daemon from ever
// reaching its idle shutdown.
func (s *Service) call(ctx context.Context, op string, args any, result any) error {
	client, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Call(ctx, op, args, result)
}

func (s *Service) registerTools() {
	mcp.AddTool(s.server, &mcp.Tool{
		Name: "it_list",
		Description: "List all terminal sessions, running and recently ended, newest activity first. " +
			"Each entry carries its id, name, whether it is running, its exit code, the command it runs, " +
			"and how many lines its log holds. Set verbose for working directory, terminal size, and log path. " +
			"Use it to find a session to reuse with it_send rather than starting another with it_new.",
		InputSchema: listSchema(),
	}, s.list)

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "it_new",
		Description: "Create a terminal session and return its first screen, along with the id every other tool needs. " +
			"The session is " + defaultShellPhrase() + " and stays open until you end it, whatever you run in it. " +
			"Pass command to have it typed in and run straight away, the way you would type it yourself; when it " +
			"finishes the shell is still there, so you can answer a prompt, run something else, or read the output. " +
			"The call returns without waiting for it, so long builds and servers are fine too. " +
			"Type into it later with it_send, and read it with it_read.",
		InputSchema: newSchema(s.settings),
	}, s.new)

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "it_read",
		Description: "Return the current screen of a terminal session. " +
			"Use it to check on a command started earlier, or to see the current state of a full-screen program. " +
			"wait bounds how long the capture waits for output to stop changing, so it helps with a command that " +
			"prints as it works; settled: true only ever means output was quiet, which is what a command that " +
			"prints nothing looks like from the moment it starts. Prefer busy. " + busyPhrase() +
			" wait_for is exact. For output that has already scrolled past, use it_tail.",
		InputSchema: readSchema(s.settings),
	}, s.read)

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "it_send",
		Description: "Type into a terminal session and return the screen afterwards. " +
			"Use text to type a command; it is submitted with Enter unless you set enter to false. " +
			"Use keys for keystrokes a full-screen program needs, such as \"CTRL+C\", \"ESC; :wq; ENTER\", or \"DOWN*5\". " +
			"Both can be sent in one call; text goes first. Read the session again later with it_read.",
		InputSchema: sendSchema(s.settings),
	}, s.send)

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "it_kill",
		Description: "End a terminal session. Check the id you pass: ending the wrong terminal cannot be undone. " +
			"Sends TERM by default and escalates to KILL if the process does not exit. " +
			"INT asks the running command to stop and leaves the session usable, and the reply reports whether it " +
			"actually stopped. Find the session to end with it_list.",
		InputSchema: killSchema(),
	}, s.kill)

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "it_tail",
		Description: "Return the most recent lines of a session's output log, plus its current screen. " +
			"The log holds only what has scrolled off the top, so the newest output is on the screen rather than in it; " +
			"both are returned and labelled. Output from full-screen programs never scrolls and so is never in the log. " +
			"log_path in the reply is a real file you can open with ordinary file tools to reach anything not returned here. " +
			"Use it_head for the start of the log.",
		InputSchema: logSchema(true),
	}, s.tail)

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "it_head",
		Description: "Return the earliest lines of a session's output log. " +
			"Use it to see how a session started, such as the first errors of a build whose ending you have already read with it_tail. " +
			"log_path in the reply is a real file you can open with ordinary file tools to reach the middle, which neither end returns.",
		InputSchema: logSchema(false),
	}, s.head)
}

// --- schema helpers ---------------------------------------------------------

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// sessionProperty is the shared session selector.
//
// It is required on every tool that touches a session. There is deliberately
// no default: agents run in parallel against one daemon, and a shared
// "current session" would route one agent's command into another's terminal
// with no error and no way to tell from the reply. Naming the target is the
// only thing that makes concurrent use safe.
func sessionProperty() map[string]any {
	return stringProperty("Session id or name, as returned by it_new or it_list. Required.")
}

func pageProperty() map[string]any {
	return map[string]any{
		"type": "integer", "minimum": 1, "default": 1,
		"description": "One-based result page; defaults to 1",
	}
}

// waitForProperty is the only completion signal that does not rely on guessing
// from output timing, so it is offered wherever a call can wait.
//
// typed distinguishes a call that puts input into the terminal from one that
// only looks at it, because what counts as a match differs: the shell prints
// back what it is given, and that echo is not a result.
func waitForProperty(typed bool) map[string]any {
	rule := "Text already on the screen counts, so the call returns at once if what you are waiting for is there."
	if typed {
		rule = "Neither text already on the screen nor the echo of the input this call types counts as a match, " +
			"so waiting for a word your own command contains works as expected."
	}
	return map[string]any{
		"type": "string",
		"description": "End the wait as soon as this text appears on the screen. " +
			"Use it when a command prints nothing while it works, or prints in bursts: waiting for something you " +
			"know will appear, such as a word the command finishes with, is exact, " +
			"while quiet output only ever means quiet output. " + rule + " The reply reports matched.",
	}
}

func waitProperty(settings config.Config, defaultSeconds int, purpose string) map[string]any {
	return map[string]any{
		"type": "number", "minimum": 0, "maximum": settings.MaximumWaitSeconds,
		"default": defaultSeconds,
		"description": fmt.Sprintf(
			"Seconds to wait for output to stop changing before capturing the screen; defaults to %d. "+
				"This is a ceiling, not a pause: the call returns as soon as output goes quiet, so a large value costs "+
				"nothing for a fast command, and a silent command returns straight away rather than being waited for. %s",
			defaultSeconds, purpose),
	}
}

func colsProperty(settings config.Config) map[string]any {
	return map[string]any{
		"type": "integer", "minimum": 20, "maximum": 1000, "default": settings.DefaultCols,
		"description": fmt.Sprintf("Terminal width in columns; defaults to %d", settings.DefaultCols),
	}
}

func rowsProperty(settings config.Config) map[string]any {
	return map[string]any{
		"type": "integer", "minimum": 5, "maximum": 1000, "default": settings.DefaultRows,
		"description": fmt.Sprintf("Terminal height in rows; defaults to %d", settings.DefaultRows),
	}
}

func listSchema() map[string]any {
	return objectSchema(map[string]any{
		"page": pageProperty(),
		"verbose": map[string]any{
			"type": "boolean", "default": false,
			"description": "Include working directory, terminal size, timestamps, and log path. " +
				"Off by default because the short form is enough to choose a session and costs a fraction of the tokens.",
		},
	})
}

func newSchema(settings config.Config) map[string]any {
	return objectSchema(map[string]any{
		"name": stringProperty(
			"Optional short name for the session, such as \"build\" or \"server\", usable in place of its id. " +
				"1-64 characters, starting with a letter or digit: lowercase letters, digits, dots, underscores, hyphens."),
		"command": map[string]any{
			"oneOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1},
			},
			"description": "A command to run as soon as the session opens, typed into it as you would type it. " +
				"A string goes in as written, so pipes, redirects, and && work. " +
				"An array is quoted for you, so an argument containing spaces needs nothing added and the program " +
				"name is taken exactly. Either way the session stays open when it finishes. " +
				"Omit it to get a bare prompt.",
		},
		"cwd": stringProperty("Directory to start in. Defaults to the directory the server was started from."),
		"env": map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string"},
			"description":          "Extra environment variables, merged over the inherited environment",
		},
		"shell":    shellProperty(),
		"cols":     colsProperty(settings),
		"rows":     rowsProperty(settings),
		"wait":     waitProperty(settings, 2, "Two seconds is enough for a shell prompt to appear."),
		"wait_for": waitForProperty(false),
	})
}

// busyPhrase describes what the busy field can actually report here.
//
// The two platforms establish different things, and describing the stronger
// one everywhere told callers on Windows to prefer a value they would never
// see. What a field means is part of the contract, so it is written from what
// the platform can prove rather than from what would read best.
func busyPhrase() string {
	if session.BusyReportsIdle() {
		return "busy: true is proof a command is running, busy: false is strong evidence none is, " +
			"and the field is absent where neither can be established."
	}
	return "busy: true is proof a command is running. It is absent otherwise, which is not evidence " +
		"of the opposite: here it can only be established from a separate process holding the terminal, " +
		"and a shell doing its own work has none."
}

// defaultShellPhrase names the interpreter a bare it_new will start, so a model
// writing a command line knows which syntax applies before it guesses wrong.
func defaultShellPhrase() string {
	shell := session.DefaultShell()
	switch {
	case shell.Display == "":
		return "your shell"
	case shell.Display == shell.ID:
		// "bash (bash)" says the same thing twice.
		return shell.Display
	default:
		return shell.Display + " (" + shell.ID + ")"
	}
}

// shellProperty offers the interpreters actually installed here. Listing only
// what exists means a model cannot pick something that will fail.
func shellProperty() map[string]any {
	ids := session.ShellIDs()
	property := map[string]any{
		"type": "string",
		"description": "Which shell to run a string command through. Available here: " +
			strings.Join(ids, ", ") + ". Defaults to " + defaultShellPhrase() +
			". Ignored when command is an array, which is executed directly.",
	}
	if len(ids) > 0 {
		enum := make([]any, 0, len(ids))
		for _, id := range ids {
			enum = append(enum, id)
		}
		property["enum"] = enum
	}
	return property
}

func readSchema(settings config.Config) map[string]any {
	return objectSchema(map[string]any{
		"session": sessionProperty(),
		"cols": map[string]any{
			"type": "integer", "minimum": 20, "maximum": 1000,
			"description": "Resize the terminal to this width before capturing. Use it to give a pager or full-screen program more room.",
		},
		"rows": map[string]any{
			"type": "integer", "minimum": 5, "maximum": 1000,
			"description": "Resize the terminal to this height before capturing.",
		},
		"wait":     waitProperty(settings, 0, "Useful for a command that prints as it works."),
		"wait_for": waitForProperty(false),
	}, "session")
}

func sendSchema(settings config.Config) map[string]any {
	return objectSchema(map[string]any{
		"session": sessionProperty(),
		"text": stringProperty(
			"Literal text to type, exactly as a person would type it. Submitted with Enter unless enter is false."),
		"keys": stringProperty(
			"Keystrokes, separated by semicolons. Named keys: " + strings.Join(keys.NamedKeys(), ", ") + ", F1-F20. " +
				"Modifiers CTRL+, ALT+, SHIFT+ combine, as in \"CTRL+C\". " +
				"Repeat anything with *n, including quoted text, as in \"DOWN*5\". " +
				"Bare text is typed verbatim unless it matches a key name, so \":wq\" is typed but \"DOWN\" moves the cursor; " +
				"quote it to force literal text, as in 'i; \"hello\"; ESC'. " +
				"Arrow keys are encoded for whatever the running program expects, so they work inside vim and less."),
		"enter": map[string]any{
			"type": "boolean", "default": true,
			"description": "Append Enter after text, submitting it. Set false to fill in a prompt without submitting.",
		},
		"wait":     waitProperty(settings, settings.DefaultWaitSeconds, "Raise it for a command you expect to take a while."),
		"wait_for": waitForProperty(true),
	}, "session")
}

func killSchema() map[string]any {
	return objectSchema(map[string]any{
		"session": stringProperty("Session id or name to end, as returned by it_new or it_list. Required."),
		"signal": map[string]any{
			"type": "string", "enum": []any{"TERM", "INT", "HUP", "KILL"}, "default": "TERM",
			"description": "TERM asks the session to end and escalates to KILL after 5 seconds. " +
				"INT sends Ctrl-C to the running command and leaves the session usable; a program may ignore it, " +
				"and the reply says whether the command stopped. It is delivered as the interrupt character, so it " +
				"reaches whatever owns the terminal right now, including a command running inside ssh, tmux, or a " +
				"nested shell. it_send({\"keys\":\"CTRL+C\"}) does the same thing. " +
				"HUP reports the terminal as closed and ends the session, escalating to KILL like TERM; " +
				"use it for a program that treats a lost terminal differently from a request to quit. " +
				"KILL ends the session immediately and cannot be refused.",
		},
	}, "session")
}

func logSchema(tail bool) map[string]any {
	which := "earliest"
	if tail {
		which = "most recent"
	}
	properties := map[string]any{
		"session": sessionProperty(),
		"lines": map[string]any{
			"type": "integer", "minimum": 1, "maximum": 5000, "default": 100,
			"description": fmt.Sprintf(
				"How many %s lines to return; defaults to 100. Fewer may come back if the response budget is reached, "+
					"in which case the reply says how many were omitted and where to read the rest.", which),
		},
	}
	if tail {
		properties["screen"] = map[string]any{
			"type": "boolean", "default": true,
			"description": "Also return the live screen after the log lines. Keep it on: the log holds only what has " +
				"scrolled off, so with this off the \"most recent lines\" returned stop where the visible screen begins " +
				"and the newest output is missing. It matters most for a full-screen program, whose output never reaches the log at all.",
		}
	}
	return objectSchema(properties, "session")
}

// renderSDKToolErrors converts the SDK's own argument-validation failures into
// this project's error document, so an agent never sees two different error
// shapes depending on where the failure happened.
func renderSDKToolErrors(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, request)
		if err != nil || method != "tools/call" {
			return result, err
		}
		callResult, ok := result.(*mcp.CallToolResult)
		if !ok || !callResult.IsError || resultHasDocument(callResult) {
			return result, nil
		}
		name := "the tool"
		if call, ok := request.(*mcp.CallToolRequest); ok {
			name = call.Params.Name
		}
		message := "invalid tool arguments"
		if len(callResult.Content) > 0 {
			if text, ok := callResult.Content[0].(*mcp.TextContent); ok && text.Text != "" {
				message = text.Text
			}
		}
		cleaned := cleanValidationMessage(message)
		hint := "Correct the arguments and call " + name + " again. " +
			"The tool's input schema lists the accepted arguments and their limits."
		if strings.HasPrefix(cleaned, "session is required") {
			// The most likely mistake by far, and the generic hint does not
			// answer it. Say where a session id comes from.
			hint = "Name the session to act on, as in " + name +
				`({"session":"build", ...}). Call it_list() to see what exists, or it_new() to start one.`
		}
		return errorResult(&ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: cleaned,
			Hint:    hint,
		}), nil
	}
}

// cleanValidationMessage strips the schema validator's internal framing while
// keeping the two facts a caller can act on: which argument was wrong and why.
//
// Raw messages read like `validating "arguments": validating root: validating
// /properties/wait: maximum: 100000/1 is greater than 300`, which buries the
// constraint under machinery an agent cannot do anything about. The argument
// name is recovered from the JSON pointer as the framing is peeled away.
func cleanValidationMessage(message string) string {
	trimmed := message
	property := ""
	for strings.HasPrefix(trimmed, "validating ") {
		rest := strings.TrimPrefix(trimmed, "validating ")
		colon := strings.Index(rest, ": ")
		if colon < 0 {
			break
		}
		if pointer := rest[:colon]; strings.HasPrefix(pointer, "/properties/") {
			property = strings.TrimPrefix(pointer, "/properties/")
		}
		trimmed = rest[colon+2:]
	}

	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return message
	}
	if missing := missingProperties(trimmed); missing != "" {
		return missing
	}
	if prose := asProse(property, trimmed); prose != "" {
		return prose
	}
	if property != "" && !strings.HasPrefix(trimmed, property) {
		return property + ": " + trimmed
	}
	return trimmed
}

// missingProperties rewrites the validator's required-property failure, which
// arrives as `required: missing properties: ["session"]`. That is the one
// message an agent is most likely to meet, and machine-generated text reads as
// a bug in a product whose every other message is written by hand.
func missingProperties(message string) string {
	const prefix = "required: missing properties: "
	if !strings.HasPrefix(message, prefix) {
		return ""
	}
	list := strings.TrimPrefix(message, prefix)
	list = strings.Trim(list, "[]")
	names := strings.Split(list, ",")
	for index := range names {
		names[index] = strings.Trim(strings.TrimSpace(names[index]), `"`)
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0] + " is required"
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " are required"
	}
}

// asProse rewrites the validator's bound checks as a sentence.
//
// Raw output reads "minimum: 10/1 is less than 20.000000", which is the only
// machine-generated text an agent would meet in a product whose every other
// message is written by hand.
func asProse(property, message string) string {
	if property == "" {
		return ""
	}
	number := func(text string) string {
		text = strings.TrimSpace(text)
		text = strings.TrimSuffix(text, ".000000")
		if index := strings.IndexByte(text, '/'); index > 0 {
			text = text[:index]
		}
		return text
	}
	switch {
	case strings.HasPrefix(message, "minimum:"):
		parts := strings.SplitN(strings.TrimPrefix(message, "minimum:"), " is less than ", 2)
		if len(parts) == 2 {
			return fmt.Sprintf("%s must be at least %s (got %s)", property, number(parts[1]), number(parts[0]))
		}
	case strings.HasPrefix(message, "maximum:"):
		parts := strings.SplitN(strings.TrimPrefix(message, "maximum:"), " is greater than ", 2)
		if len(parts) == 2 {
			return fmt.Sprintf("%s must be at most %s (got %s)", property, number(parts[1]), number(parts[0]))
		}
	}
	return ""
}

func resultHasDocument(result *mcp.CallToolResult) bool {
	if len(result.Content) != 1 {
		return false
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	return ok && strings.HasPrefix(text.Text, "---\n")
}
