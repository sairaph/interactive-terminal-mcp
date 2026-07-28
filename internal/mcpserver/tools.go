package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

// --- it_active --------------------------------------------------------------

type activeInput struct {
	Session string `json:"session,omitempty"`
}

func (s *Service) active(ctx context.Context, _ *mcp.CallToolRequest, input activeInput) (*mcp.CallToolResult, any, error) {
	args := ipc.ActiveArgs{Session: strings.TrimSpace(input.Session)}
	args.Set = args.Session != ""

	var result ipc.ActiveResult
	if err := s.call(ctx, ipc.OpSessionAtive, args, &result); err != nil {
		return errorResult(err), nil, nil
	}
	if result.Active == nil {
		return successResult(noActiveFront(result), noActiveBody(result)), nil, nil
	}
	front := screenFront(*result.Active)
	return successResult(front, screenBody(*result.Active, activeGuidance(*result.Active))), nil, nil
}

// --- it_list ----------------------------------------------------------------

type listInput struct {
	Page    int  `json:"page,omitempty"`
	Verbose bool `json:"verbose,omitempty"`
}

func (s *Service) list(ctx context.Context, _ *mcp.CallToolRequest, input listInput) (*mcp.CallToolResult, any, error) {
	page := input.Page
	if page == 0 {
		page = 1
	}
	if page < 1 {
		return errorResult(&ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: "page must be 1 or greater",
			Hint:    "Call it_list({}) for the first page.",
			Fields:  map[string]any{"page": input.Page},
		}), nil, nil
	}

	var result ipc.ListResult
	if err := s.call(ctx, ipc.OpSessionList, nil, &result); err != nil {
		return errorResult(err), nil, nil
	}
	front, body, err := renderList(result, page, s.settings.ListTokenBudget, input.Verbose)
	if err != nil {
		return errorResult(err), nil, nil
	}
	return successResult(front, body), nil, nil
}

// --- it_new -----------------------------------------------------------------

type newInput struct {
	WaitFor string `json:"wait_for,omitempty"`

	Name    string            `json:"name,omitempty"`
	Command json.RawMessage   `json:"command,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Shell   string            `json:"shell,omitempty"`
	Cols    int               `json:"cols,omitempty"`
	Rows    int               `json:"rows,omitempty"`
	Wait    *float64          `json:"wait,omitempty"`
}

func (s *Service) new(ctx context.Context, _ *mcp.CallToolRequest, input newInput) (*mcp.CallToolResult, any, error) {
	commandLine, argv, err := decodeCommand(input.Command)
	if err != nil {
		return errorResult(err), nil, nil
	}
	wait, err := s.waitMilliseconds(input.Wait, 2)
	if err != nil {
		return errorResult(err), nil, nil
	}

	args := ipc.NewArgs{
		Name: strings.TrimSpace(input.Name), CommandLine: commandLine, Argv: argv,
		Cwd: input.Cwd, Env: input.Env, Shell: input.Shell,
		Cols: input.Cols, Rows: input.Rows, WaitMS: wait, WaitFor: input.WaitFor,
	}
	var screen ipc.Screen
	if err := s.call(ctx, ipc.OpSessionNew, args, &screen); err != nil {
		return errorResult(err), nil, nil
	}
	return successResult(screenFront(screen), screenBody(screen, newGuidance(screen))), nil, nil
}

// decodeCommand resolves the string-or-array command argument.
//
// The two forms are not interchangeable and the distinction matters: a string
// goes through the shell so `cd x && make` works, while an array is executed
// directly so an argument containing spaces needs no quoting.
func decodeCommand(raw json.RawMessage) (commandLine string, argv []string, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var asString string
	if jsonErr := json.Unmarshal(raw, &asString); jsonErr == nil {
		return asString, nil, nil
	}
	var asArray []string
	if jsonErr := json.Unmarshal(raw, &asArray); jsonErr == nil {
		if len(asArray) == 0 {
			return "", nil, &ipc.Error{
				Code:    ipc.CodeInvalidInput,
				Message: "command was an empty array",
				Hint:    "Give the program name as the first element, or omit command to start a shell.",
				Fields:  map[string]any{"field": "command"},
			}
		}
		return "", asArray, nil
	}
	return "", nil, &ipc.Error{
		Code:    ipc.CodeInvalidInput,
		Message: "command must be a string or an array of strings",
		Hint:    `Use a string such as "npm run dev" to go through the shell, or an array such as ["git","commit","-m","a message"] to run a program directly.`,
		Fields:  map[string]any{"field": "command"},
	}
}

// --- it_read ----------------------------------------------------------------

type readInput struct {
	WaitFor string `json:"wait_for,omitempty"`

	Session string   `json:"session,omitempty"`
	Cols    int      `json:"cols,omitempty"`
	Rows    int      `json:"rows,omitempty"`
	Wait    *float64 `json:"wait,omitempty"`
}

func (s *Service) read(ctx context.Context, _ *mcp.CallToolRequest, input readInput) (*mcp.CallToolResult, any, error) {
	wait, err := s.waitMilliseconds(input.Wait, 0)
	if err != nil {
		return errorResult(err), nil, nil
	}
	args := ipc.ReadArgs{
		Session: strings.TrimSpace(input.Session),
		Cols:    input.Cols, Rows: input.Rows, WaitMS: wait, WaitFor: input.WaitFor,
	}
	var screen ipc.Screen
	if err := s.call(ctx, ipc.OpSessionRead, args, &screen); err != nil {
		return errorResult(err), nil, nil
	}
	return successResult(screenFront(screen), screenBody(screen, readGuidance(screen))), nil, nil
}

// --- it_send ----------------------------------------------------------------

type sendInput struct {
	WaitFor string `json:"wait_for,omitempty"`

	Session string   `json:"session,omitempty"`
	Text    *string  `json:"text,omitempty"`
	Keys    *string  `json:"keys,omitempty"`
	Enter   *bool    `json:"enter,omitempty"`
	Wait    *float64 `json:"wait,omitempty"`
}

func (s *Service) send(ctx context.Context, _ *mcp.CallToolRequest, input sendInput) (*mcp.CallToolResult, any, error) {
	if input.Text == nil && input.Keys == nil {
		return errorResult(&ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: "it_send needs text, keys, or both",
			Hint:    `Use text to type a command, as in it_send({"text":"ls -la"}), or keys for keystrokes, as in it_send({"keys":"CTRL+C"}).`,
		}), nil, nil
	}
	wait, err := s.waitMilliseconds(input.Wait, s.settings.DefaultWaitSeconds)
	if err != nil {
		return errorResult(err), nil, nil
	}

	args := ipc.SendArgs{
		Session: strings.TrimSpace(input.Session),
		Enter:   true,
		WaitMS:  wait,
		WaitFor: input.WaitFor,
	}
	if input.Text != nil {
		args.Text, args.HasText = *input.Text, true
	}
	if input.Keys != nil {
		args.Keys, args.HasKeys = *input.Keys, true
	}
	if input.Enter != nil {
		args.Enter = *input.Enter
	}

	var screen ipc.Screen
	if err := s.call(ctx, ipc.OpSessionSend, args, &screen); err != nil {
		return errorResult(err), nil, nil
	}
	return successResult(screenFront(screen), screenBody(screen, sendGuidance(screen))), nil, nil
}

// --- it_kill ----------------------------------------------------------------

type killInput struct {
	Session string `json:"session"`
	Signal  string `json:"signal,omitempty"`
}

func (s *Service) kill(ctx context.Context, _ *mcp.CallToolRequest, input killInput) (*mcp.CallToolResult, any, error) {
	args := ipc.KillArgs{Session: strings.TrimSpace(input.Session), Signal: input.Signal}
	var result ipc.KillResult
	if err := s.call(ctx, ipc.OpSessionKill, args, &result); err != nil {
		return errorResult(err), nil, nil
	}
	return successResult(killFront(result), killBody(result)), nil, nil
}

// --- it_tail and it_head ----------------------------------------------------

type logInput struct {
	Session string `json:"session,omitempty"`
	Lines   int    `json:"lines,omitempty"`
	Screen  *bool  `json:"screen,omitempty"`
}

func (s *Service) tail(ctx context.Context, _ *mcp.CallToolRequest, input logInput) (*mcp.CallToolResult, any, error) {
	includeScreen := true
	if input.Screen != nil {
		includeScreen = *input.Screen
	}
	return s.readLog(ctx, input, true, includeScreen)
}

func (s *Service) head(ctx context.Context, _ *mcp.CallToolRequest, input logInput) (*mcp.CallToolResult, any, error) {
	return s.readLog(ctx, input, false, false)
}

func (s *Service) readLog(ctx context.Context, input logInput, fromEnd, includeScreen bool) (*mcp.CallToolResult, any, error) {
	tool := "it_head"
	if fromEnd {
		tool = "it_tail"
	}
	lines := input.Lines
	if lines == 0 {
		lines = 100
	}
	if lines < 1 || lines > 5000 {
		return errorResult(&ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: fmt.Sprintf("lines must be between 1 and 5000, got %d", lines),
			Hint:    fmt.Sprintf("Call %s({}) for the default of 100 lines.", tool),
			Fields:  map[string]any{"lines": input.Lines},
		}), nil, nil
	}

	args := ipc.LogArgs{
		Session: strings.TrimSpace(input.Session),
		Lines:   lines, FromEnd: fromEnd, Screen: includeScreen,
	}
	var result ipc.LogResult
	if err := s.call(ctx, ipc.OpSessionLog, args, &result); err != nil {
		return errorResult(err), nil, nil
	}
	front, body, err := renderLog(result, lines, fromEnd, s.settings.ReadTokenBudget)
	if err != nil {
		return errorResult(err), nil, nil
	}
	return successResult(front, body), nil, nil
}

// --- shared input handling --------------------------------------------------

// waitMilliseconds validates the wait argument and converts it to the wire
// unit. Fractional seconds are accepted because a caller polling a fast
// command reasonably asks for half a second.
func (s *Service) waitMilliseconds(wait *float64, defaultSeconds int) (int64, error) {
	seconds := float64(defaultSeconds)
	if wait != nil {
		seconds = *wait
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return 0, &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: fmt.Sprintf("wait must be zero or more seconds, got %v", seconds),
			Hint:    "Omit wait to use the default, or pass a number of seconds.",
			Fields:  map[string]any{"field": "wait"},
		}
	}
	if seconds > float64(s.settings.MaximumWaitSeconds) {
		return 0, &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: fmt.Sprintf("wait must be at most %d seconds, got %v", s.settings.MaximumWaitSeconds, seconds),
			Hint: fmt.Sprintf(
				"Use a shorter wait and call it_read again to keep checking. The limit can be raised in `interactive-terminal-mcp configure`."),
			Fields: map[string]any{"field": "wait", "maximum": s.settings.MaximumWaitSeconds},
		}
	}
	return int64(seconds * float64(time.Second/time.Millisecond)), nil
}
