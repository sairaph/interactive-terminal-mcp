package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/interactive-terminal-mcp/internal/budget"
	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
	"github.com/sairaph/interactive-terminal-mcp/internal/render"
	"gopkg.in/yaml.v3"
)

func successResult(front any, body string) *mcp.CallToolResult {
	return documentResult(render.Document{Front: front, Body: body})
}

func errorResult(err error) *mcp.CallToolResult {
	failure := asIPCError(err)
	front := struct {
		Error errorFront `yaml:"error"`
	}{Error: errorFront{
		Code: failure.Code, Message: failure.Message,
		Hint: failure.Hint, Fields: failure.Fields,
	}}
	body := "## Error\n\n" + failure.Message
	if failure.Hint != "" {
		body += "\n\n" + failure.Hint
	}
	result := documentResult(render.Document{Front: front, Body: body, IsError: true})
	result.IsError = true
	return result
}

type errorFront struct {
	Code    string         `yaml:"code"`
	Message string         `yaml:"message"`
	Hint    string         `yaml:"hint,omitempty"`
	Fields  map[string]any `yaml:"fields,omitempty"`
}

func asIPCError(err error) *ipc.Error {
	var typed *ipc.Error
	if errors.As(err, &typed) {
		return typed
	}
	return &ipc.Error{
		Code:    ipc.CodeInternal,
		Message: err.Error(),
		Hint:    "Retry the call; if it keeps failing, run `interactive-terminal-mcp doctor`.",
	}
}

func documentResult(document render.Document) *mcp.CallToolResult {
	text, err := document.String()
	if err != nil {
		text = "---\nerror:\n  code: render_error\n  message: the tool result could not be rendered\n" +
			"  hint: Retry with fewer lines, for example it_tail({\"lines\":50}).\n---\n\n" +
			"## Error\n\nThe tool result could not be rendered.\n"
		document.IsError = true
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: document.IsError,
	}
}

// --- screen output ----------------------------------------------------------

// screenMetadata is the frontmatter shared by it_new, it_read, and it_send.
// One shape across three tools means a model learns it once.
type screenMetadata struct {
	Session   string `yaml:"session"`
	Name      string `yaml:"name,omitempty"`
	Running   bool   `yaml:"running"`
	ExitCode  *int   `yaml:"exit_code,omitempty"`
	PID       int    `yaml:"pid,omitempty"`
	Size      []int  `yaml:"size,flow"`
	Cursor    []int  `yaml:"cursor,flow"`
	AltScreen bool   `yaml:"alt_screen"`
	Title     string `yaml:"title,omitempty"`
	Shell     string `yaml:"shell,omitempty"`
	Command   string `yaml:"command,omitempty"`
	Cwd       string `yaml:"cwd,omitempty"`

	// Settled is absent when the call did not wait long enough to establish
	// anything, because a flat false there reads as "output was still
	// arriving" when in truth nothing was looked at.
	Settled *bool `yaml:"settled,omitempty"`
	// Busy is absent where it cannot be established. Where it is present it
	// answers the question settled only ever approximated: whether a command
	// is still running in this terminal.
	Busy *bool `yaml:"busy,omitempty"`

	// CommandExit is the last command's exit status, present only for a shell
	// that reports its own command boundaries.
	CommandExit *int `yaml:"command_exit,omitempty"`

	// ShellReady appears only when it is false, which is the only time it says
	// anything: a shell that has not prompted yet has run nothing that was
	// asked of it, whatever the screen and the busy flag look like.
	ShellReady *bool `yaml:"shell_ready,omitempty"`
	// StartupMS is how much of this call went on the shell starting rather than
	// on the command. Session creation is the only call that can attribute it.
	StartupMS int64 `yaml:"startup_ms,omitempty"`

	Matched           *bool  `yaml:"matched,omitempty"`
	WaitedFor         string `yaml:"waited_for,omitempty"`
	WaitedMS          int64  `yaml:"waited_ms"`
	LastActivityAt    string `yaml:"last_activity_at,omitempty"`
	BlankLinesTrimmed int    `yaml:"blank_lines_trimmed,omitempty"`
	TranscriptLines   int    `yaml:"transcript_lines"`
	LogsRetained      bool   `yaml:"logs_retained"`
	LogPath           string `yaml:"log_path,omitempty"`
}

func screenFront(screen ipc.Screen) screenMetadata {
	info := screen.Session
	front := screenMetadata{
		Session: info.ID, Name: info.Name,
		Running: info.Running, ExitCode: info.ExitCode, PID: info.PID,
		Size:      []int{info.Cols, info.Rows},
		Cursor:    []int{screen.Cursor[0], screen.Cursor[1]},
		AltScreen: info.AltScreen, Title: info.Title,
		Shell: info.Shell, Command: commandText(info), Cwd: info.Cwd,
		WaitedMS:          screen.WaitedMS,
		WaitedFor:         screen.WaitedFor,
		LastActivityAt:    formatTime(info.LastActivityAt),
		BlankLinesTrimmed: screen.BlankLinesTrimmed,
		TranscriptLines:   info.TranscriptLines,
		LogsRetained:      info.LogsRetained,
		LogPath:           info.LogPath,
	}
	if screen.Observed {
		settled := screen.Settled
		front.Settled = &settled
	}
	if busy, known := busyState(screen); known {
		front.Busy = &busy
	}
	front.CommandExit = screen.CommandExit
	if info.Running && !screen.ShellReady {
		ready := false
		front.ShellReady = &ready
	}
	front.StartupMS = screen.StartupMS
	if screen.WaitedFor != "" {
		matched := screen.Matched
		front.Matched = &matched
	}
	return front
}

// busyState reports whether a command is running in the terminal, and whether
// that is established rather than assumed.
//
// A full-screen program is always the foreground process, so reporting it as a
// running command would be true and useless: it would appear on every read of
// a session sitting in vim. The question only means something at a shell.
func busyState(screen ipc.Screen) (busy, known bool) {
	if !screen.BusyKnown || screen.Session.AltScreen || !screen.Session.Running {
		return false, false
	}
	return screen.Busy, true
}

// screenBody renders the screen followed by guidance.
//
// The screen always comes first: it is what the caller asked for, and burying
// it under prose would make every result harder to read.
func screenBody(screen ipc.Screen, guidance []string) string {
	parts := []string{render.Screen(screen.Lines)}
	if note := waitNote(screen); note != "" {
		parts = append(parts, note)
	}
	parts = append(parts, guidance...)
	return strings.Join(parts, "\n\n")
}

// waitNote says what the wait established, and no more than that.
//
// Four separate facts are kept apart here, because collapsing them is how a
// quiet screen came to be announced as a finished command: whether a wait ran
// at all, what it saw, whether the shell has started, and whether the terminal
// still has a command in the foreground. The last two are the only ones that
// answer "has it finished", so they are stated wherever they are known and
// never guessed at where they are not.
func waitNote(screen ipc.Screen) string {
	if !screen.Session.Running {
		// An ended session is covered by the per-tool guidance, which has the
		// exit code to report alongside it.
		return ""
	}
	id := screen.Session.ID
	busy, busyKnown := busyState(screen)
	waitAgain := call("it_read", map[string]any{"session": id, "wait": 10})

	// A shell still running its startup files has nothing in the foreground,
	// which is the same thing an idle shell has. Every conclusion below rests
	// on being able to tell those apart, so this is answered first: until the
	// shell prompts, whatever was asked for has not run, and no amount of quiet
	// says otherwise.
	if !screen.ShellReady {
		return startingNote(screen)
	}

	if screen.WaitedFor != "" && !screen.Matched {
		elapsed := formatDuration(screen.WaitedMS)
		switch {
		case busyKnown && busy:
			return fmt.Sprintf(
				"%q did not appear in %s, and a command is still running in this terminal. Keep waiting with `%s`.",
				screen.WaitedFor, elapsed,
				call("it_read", map[string]any{
					"session": id, "wait_for": screen.WaitedFor, "wait": 60}))
		case busyKnown && !busy:
			return fmt.Sprintf(
				"%q did not appear in %s, and no command is running in this terminal now, so it has most likely "+
					"finished without printing that text. The screen above is where it left off; if earlier output "+
					"scrolled past, read it with `%s`.",
				screen.WaitedFor, elapsed, call("it_tail", map[string]any{"session": id}))
		default:
			return fmt.Sprintf(
				"%q did not appear in %s. Read the screen above before assuming the command is still going: "+
					"it may have finished without printing that text. To keep waiting, use `%s`.",
				screen.WaitedFor, elapsed,
				call("it_read", map[string]any{
					"session": id, "wait_for": screen.WaitedFor, "wait": 60}))
		}
	}

	switch {
	case busyKnown && busy:
		// Quiet output and a finished command look identical. Where the
		// terminal can tell them apart, say which one this is.
		return fmt.Sprintf(
			"A command is still running in this terminal, whether or not the screen has changed recently. "+
				"Check on it with `%s`.", waitAgain)
	case !screen.Observed && busyKnown:
		// Nothing holds the terminal, which answers the question a wait would
		// have been asked to answer. Saying more would be filler.
		return ""
	case !screen.Observed:
		return fmt.Sprintf(
			"This screen was captured without waiting, so a command that has just started may not have printed "+
				"anything yet. Give it time with `%s`.",
			call("it_read", map[string]any{"session": id, "wait": 5}))
	case !screen.Settled:
		return fmt.Sprintf(
			"Output was still arriving when the %s wait ended, so this screen may be incomplete. Check again with `%s`.",
			formatDuration(screen.BudgetMS), waitAgain)
	}
	return ""
}

// startingNote covers a shell that has not reached a prompt yet.
//
// This is a real state, not an edge case: a bash that sources conda takes
// eight seconds on the machine this was written on, and oh-my-zsh is no
// quicker. Anything typed in the meantime is held by the terminal and runs when
// the shell gets there, so nothing is lost -- but it has not happened yet, and
// saying so is the whole point. The wording avoids "still running", which
// belongs to a command that has started.
func startingNote(screen ipc.Screen) string {
	id := screen.Session.ID
	again := call("it_read", map[string]any{"session": id, "wait": 15})

	waited := ""
	if screen.Observed && screen.WaitedMS > 0 {
		waited = fmt.Sprintf(" after %s", formatDuration(screen.WaitedMS))
	}

	if screen.WaitedFor != "" && !screen.Matched {
		return fmt.Sprintf(
			"This shell has not reached a prompt yet%s, so %q could not have appeared: its startup files are "+
				"still running and nothing typed into it has been read. What you sent is queued and will run as "+
				"soon as the shell is ready. Wait for it with `%s`.",
			waited, screen.WaitedFor,
			call("it_read", map[string]any{"session": id, "wait_for": screen.WaitedFor, "wait": 30}))
	}
	return fmt.Sprintf(
		"This shell has not reached a prompt yet%s: its startup files are still running, so an empty screen "+
			"here means not started rather than nothing to show. Anything already sent is queued and runs once "+
			"it is ready. Check again with `%s`.",
		waited, again)
}

// exampleCommand picks a command line valid in the interpreter this session is
// actually running. Offering `ls -la` to PowerShell teaches an agent a command
// that fails there.
func exampleCommand(info ipc.SessionInfo) string {
	switch {
	case strings.Contains(info.Shell, "PowerShell"):
		return "Get-ChildItem"
	case strings.Contains(info.Shell, "Command Prompt"):
		return "dir"
	case info.Shell == "":
		// The session was started as a program rather than a shell, so nothing
		// says which syntax it takes. echo is the one command spelled the same
		// in cmd, PowerShell, and every POSIX shell.
		return "echo hi"
	default:
		return "ls -la"
	}
}

func newGuidance(screen ipc.Screen) []string {
	info := screen.Session
	if !info.Running {
		// A command given to it_new can finish before the call returns; saying
		// so plainly beats leaving the caller to infer it from exit_code.
		return []string{fmt.Sprintf(
			"The command finished%s before this call returned. Read its output with `%s`.",
			exitPhrase(info), call("it_tail", map[string]any{"session": info.ID}))}
	}
	// "Ready" is a claim, and one this reply may already have contradicted two
	// paragraphs above. A shell still running its startup files exists and can
	// be typed into -- the terminal buffers it -- but it is not ready, and
	// saying both things in one message is how a caller learns to believe
	// neither.
	opening := fmt.Sprintf("Session %s is ready", label(info))
	if !screen.ShellReady {
		opening = fmt.Sprintf("Session %s is starting", label(info))
	}
	if info.Shell != "" {
		// Which interpreter is running decides the syntax of every command the
		// caller is about to write, and on Windows it is not guessable.
		opening += fmt.Sprintf(", running %s", info.Shell)
	}
	if info.AltScreen {
		return []string{fmt.Sprintf(
			"%s. A full-screen program is running in it, so send it keystrokes with `%s`, and read it again "+
				"later with `%s`.",
			opening,
			call("it_send", map[string]any{"session": info.ID, "keys": "DOWN*5"}),
			call("it_read", map[string]any{"session": info.ID}))}
	}
	return []string{fmt.Sprintf(
		"%s. Type into it with `%s`, and read it again later with `%s`.",
		opening,
		call("it_send", map[string]any{"session": info.ID, "text": exampleCommand(info)}),
		call("it_read", map[string]any{"session": info.ID}))}
}

func readGuidance(screen ipc.Screen) []string {
	info := screen.Session
	var guidance []string
	if !info.Running {
		guidance = append(guidance, fmt.Sprintf(
			"This session has ended%s. This is its final screen; earlier output is in the log.",
			exitPhrase(info)))
	}
	guidance = append(guidance, scrollbackNote(info)...)
	return guidance
}

func sendGuidance(screen ipc.Screen) []string {
	info := screen.Session
	var guidance []string
	if !info.Running {
		// Pointing at the log is only useful when there is one. A full-screen
		// program's output never scrolls, so a session that spent its life in
		// vim or htop ends with an empty transcript, and sending the caller to
		// it_tail would just cost a round trip to learn nothing.
		if info.TranscriptLines > 0 && info.LogsRetained {
			guidance = append(guidance, fmt.Sprintf(
				"The session ended%s while running this input. Read its full output with `%s`, "+
					"or start a new session with `%s`.",
				exitPhrase(info),
				call("it_tail", map[string]any{"session": info.ID}),
				call("it_new", map[string]any{})))
		} else {
			guidance = append(guidance, fmt.Sprintf(
				"The session ended%s while running this input. The screen above is everything it left behind. "+
					"Start a new session with `%s`.",
				exitPhrase(info), call("it_new", map[string]any{})))
		}
		return guidance
	}
	guidance = append(guidance, scrollbackNote(info)...)
	return guidance
}

// scrollbackNote points at the log when output has scrolled past the screen.
// Without it, a model reading a 30-row screen has no way to know that 1800
// lines of build output exist just above it.
func scrollbackNote(info ipc.SessionInfo) []string {
	if info.AltScreen {
		return []string{
			"A full-screen program is running, so this screen is its complete display. " +
				"Send keystrokes to it with the `keys` argument of it_send.",
		}
	}
	if info.TranscriptLines > 0 && info.LogsRetained {
		return []string{fmt.Sprintf(
			"This session's log holds %d earlier %s from above this screen. Read %s with `%s`.",
			info.TranscriptLines, plural(info.TranscriptLines, "line", "lines"),
			plural(info.TranscriptLines, "it", "them"),
			call("it_tail", map[string]any{"session": info.ID, "lines": 100}))}
	}
	return nil
}

// --- it_list ----------------------------------------------------------------

type listMetadata struct {
	Page       int  `yaml:"page"`
	Total      int  `yaml:"total"`
	TotalPages int  `yaml:"total_pages"`
	Verbose    bool `yaml:"verbose,omitempty"`
	// Retention is the log retention policy, which decides how long an ended
	// session stays listed at all.
	Retention string    `yaml:"retention,omitempty"`
	Sessions  []listRow `yaml:"sessions,omitempty"`
}

// firstRunning returns the first session that can still be typed into.
func firstRunning(rows []listRow) *listRow {
	for index := range rows {
		if rows[index].Running {
			return &rows[index]
		}
	}
	return nil
}

// listRow is what a caller needs to pick a session. Everything beyond that is
// available from it_read or the verbose form, because a dozen fields times a
// dozen sessions is a couple of thousand tokens spent to answer "which one".
type listRow struct {
	ID              string `yaml:"id"`
	Name            string `yaml:"name,omitempty"`
	Running         bool   `yaml:"running"`
	ExitCode        *int   `yaml:"exit_code,omitempty"`
	KilledBy        string `yaml:"killed_by,omitempty"`
	PID             int    `yaml:"pid,omitempty"`
	Command         string `yaml:"command,omitempty"`
	Shell           string `yaml:"shell,omitempty"`
	Cwd             string `yaml:"cwd,omitempty"`
	Size            []int  `yaml:"size,flow,omitempty"`
	AltScreen       bool   `yaml:"alt_screen,omitempty"`
	Title           string `yaml:"title,omitempty"`
	CreatedAt       string `yaml:"created_at,omitempty"`
	LastActivityAt  string `yaml:"last_activity_at,omitempty"`
	TranscriptLines int    `yaml:"transcript_lines"`
	LogsRetained    bool   `yaml:"logs_retained"`
	LogPath         string `yaml:"log_path,omitempty"`
}

// compact drops the fields that answer questions a caller has not asked yet.
// What survives is enough to choose a session and to know whether its log is
// worth reading.
func (r listRow) compact() listRow {
	return listRow{
		ID: r.ID, Name: r.Name, Running: r.Running,
		ExitCode: r.ExitCode, KilledBy: r.KilledBy,
		Command: r.Command, LastActivityAt: r.LastActivityAt,
		// Kept because it is not optional detail: dropping it left every row
		// reporting logs_retained: false, including sessions whose logs were
		// perfectly readable.
		TranscriptLines: r.TranscriptLines, LogsRetained: r.LogsRetained,
	}
}

func renderList(result ipc.ListResult, page, tokenBudget int, verbose bool) (any, string, error) {
	rows := make([]listRow, 0, len(result.Sessions))
	for _, info := range result.Sessions {
		rows = append(rows, listRow{
			ID: info.ID, Name: info.Name, Running: info.Running,
			ExitCode: info.ExitCode, KilledBy: info.KilledBy, PID: info.PID,
			Command: commandText(info), Shell: info.Shell, Cwd: info.Cwd,
			Size:      []int{info.Cols, info.Rows},
			AltScreen: info.AltScreen, Title: info.Title,
			CreatedAt: formatTime(info.CreatedAt), LastActivityAt: formatTime(info.LastActivityAt),
			TranscriptLines: info.TranscriptLines,
			LogsRetained:    info.LogsRetained, LogPath: info.LogPath,
		})
	}

	if !verbose {
		for index := range rows {
			rows[index] = rows[index].compact()
		}
	}

	window, totalPages, err := budget.Paginate(rows, page, tokenBudget, func(records []listRow) (string, error) {
		raw, err := yaml.Marshal(struct {
			Sessions []listRow `yaml:"sessions"`
		}{Sessions: records})
		return string(raw), err
	})
	if err != nil {
		return nil, "", &ipc.Error{Code: ipc.CodeInternal, Message: "paginate sessions: " + err.Error()}
	}

	front := listMetadata{
		Page: page, Total: len(rows), TotalPages: totalPages,
		Verbose:   verbose,
		Retention: result.Retention, Sessions: window,
	}
	return front, listBody(front, window, result.Sessions), nil
}

// exampleFor picks a command line valid in the session with this id. The full
// SessionInfo is needed because the compact row drops the shell, and a hint has
// to be right in either mode.
func exampleFor(sessions []ipc.SessionInfo, id string) string {
	for _, info := range sessions {
		if info.ID == id {
			return exampleCommand(info)
		}
	}
	return "echo hi"
}

func listBody(front listMetadata, window []listRow, sessions []ipc.SessionInfo) string {
	var parts []string
	switch {
	case front.Total == 0:
		return fmt.Sprintf("No terminal sessions exist. Create one with `%s`.", call("it_new", map[string]any{}))
	case len(window) == 0:
		parts = append(parts, fmt.Sprintf(
			"No sessions on page %d; there %s %d %s across %d %s.",
			front.Page, plural(front.Total, "is", "are"), front.Total,
			plural(front.Total, "session", "sessions"),
			front.TotalPages, plural(front.TotalPages, "page", "pages")))
		return strings.Join(parts, "\n\n")
	}

	live := 0
	for _, row := range window {
		if row.Running {
			live++
		}
	}
	summary := fmt.Sprintf("%d %s", len(window), plural(len(window), "session", "sessions"))
	if front.TotalPages > 1 {
		summary = fmt.Sprintf("%d of %d sessions", len(window), front.Total)
	}
	parts = append(parts, fmt.Sprintf("%s, %d running.", summary, live))

	// Every suggestion below names a session that can actually accept it.
	// Pointing it_send at a session this same reply reports as ended costs the
	// caller a round trip to be told what it already knew.
	running := firstRunning(window)
	if running != nil {
		parts = append(parts, fmt.Sprintf(
			"Read a session with `%s`, or type into it with `%s`.",
			call("it_read", map[string]any{"session": running.ID}),
			call("it_send", map[string]any{"session": running.ID, "text": exampleFor(sessions, running.ID)})))
	} else {
		// The final screen outlives the process, but the log does not
		// necessarily outlive the retention policy, so only offer it_tail
		// where there is a log left to read.
		recover := call("it_read", map[string]any{"session": window[0].ID})
		if window[0].LogsRetained && window[0].TranscriptLines > 0 {
			recover = call("it_tail", map[string]any{"session": window[0].ID})
		}
		parts = append(parts, fmt.Sprintf(
			"None of these sessions is still running, so nothing can be typed into them. "+
				"Read what one left behind with `%s`, or start a new session with `%s`.",
			recover, call("it_new", map[string]any{})))
	}
	if front.Retention == config.RetentionOnClose {
		// A session vanishing from this list looks like data loss unless the
		// rule behind it is stated.
		parts = append(parts, "Ended sessions leave this list as soon as they end, because session logs "+
			"are set to be deleted when a session closes.")
	}

	if front.Page < front.TotalPages {
		// verbose has to travel with the page number. A verbose row is larger,
		// so the two modes paginate differently, and "page 2" of one can be
		// past the end of the other -- following this hint verbatim landed on
		// an empty page.
		next := map[string]any{"page": front.Page + 1}
		if front.Verbose {
			next["verbose"] = true
		}
		parts = append(parts, fmt.Sprintf("Continue with `%s`.", call("it_list", next)))
	}
	if !front.Verbose {
		parts = append(parts, fmt.Sprintf(
			"Working directory, size, and log path are omitted; add `%s` for them.",
			call("it_list", map[string]any{"verbose": true})))
	}
	return strings.Join(parts, "\n\n")
}

// --- it_kill ----------------------------------------------------------------

type killMetadata struct {
	Killed       string `yaml:"killed"`
	Name         string `yaml:"name,omitempty"`
	Signal       string `yaml:"signal"`
	Escalated    bool   `yaml:"escalated"`
	ExitCode     *int   `yaml:"exit_code,omitempty"`
	AlreadyEnded bool   `yaml:"already_ended,omitempty"`
	Outcome      string `yaml:"outcome,omitempty"`
	ObservedMS   int64  `yaml:"observed_ms,omitempty"`
	LogsRetained bool   `yaml:"logs_retained"`
	LogPath      string `yaml:"log_path,omitempty"`
}

func killFront(result ipc.KillResult) killMetadata {
	return killMetadata{
		Killed: result.Killed, Name: result.Name, Signal: result.Signal,
		Escalated: result.Escalated, ExitCode: result.ExitCode,
		AlreadyEnded: result.AlreadyGone,
		Outcome:      result.Outcome,
		ObservedMS:   result.ObservedMS,
		LogsRetained: result.LogsRetained, LogPath: result.LogPath,
	}
}

func killBody(result ipc.KillResult) string {
	name := result.Killed
	if result.Name != "" {
		name = fmt.Sprintf("%q (%s)", result.Name, result.Killed)
	}

	var parts []string
	switch {
	case result.AlreadyGone:
		parts = append(parts, fmt.Sprintf("Session %s had already ended%s; nothing was signalled.", name, exitCodePhrase(result.ExitCode)))
	case result.Signal == "INT" && result.ExitCode == nil:
		// Say what was established, and mark inference as inference. A program
		// is free to ignore an interrupt, and a confident false success here
		// makes every other success message less believable.
		observed := formatDuration(result.ObservedMS)
		switch result.Outcome {
		case ipc.OutcomeStillRunning:
			return fmt.Sprintf(
				"Sent an interrupt to session %s, but it was still working %s later, so the command did not stop. "+
					"End the session with `%s`, which cannot be ignored.",
				name, observed,
				call("it_kill", map[string]any{"session": result.Killed, "signal": "TERM"}))
		case ipc.OutcomeQuiet:
			return fmt.Sprintf(
				"Sent an interrupt to session %s. Nothing further was printed in the following %s, which usually "+
					"means the command stopped, but a slow command looks the same. Confirm with `%s` before relying on it.",
				name, observed, call("it_read", map[string]any{"session": result.Killed}))
		default:
			return fmt.Sprintf(
				"Sent an interrupt to session %s. What it did could not be established; check with `%s`.",
				name, call("it_read", map[string]any{"session": result.Killed}))
		}
	case result.Escalated:
		parts = append(parts, fmt.Sprintf(
			"Session %s did not exit after %s and was ended with KILL%s.",
			name, result.Signal, exitCodePhrase(result.ExitCode)))
	default:
		parts = append(parts, fmt.Sprintf("Ended session %s with %s%s.", name, result.Signal, exitCodePhrase(result.ExitCode)))
	}

	switch {
	case result.LogsRetained && result.LogPath != "":
		parts = append(parts, fmt.Sprintf("Its log is kept at `%s`.", result.LogPath))
	case result.Purged:
		parts = append(parts, "Its logs were deleted with it.")
	case !result.LogsRetained:
		parts = append(parts, "Its logs were deleted, because session logs are set to be removed when a session closes.")
	}
	return strings.Join(parts, "\n\n")
}

// --- it_tail and it_head ----------------------------------------------------

type logMetadata struct {
	Session        string `yaml:"session"`
	Name           string `yaml:"name,omitempty"`
	Running        bool   `yaml:"running"`
	ExitCode       *int   `yaml:"exit_code,omitempty"`
	LinesRequested int    `yaml:"lines_requested"`
	LinesReturned  int    `yaml:"lines_returned"`
	LinesOmitted   int    `yaml:"lines_omitted,omitempty"`
	TotalLines     int    `yaml:"total_lines"`
	Truncated      bool   `yaml:"truncated"`
	TruncatedBy    string `yaml:"truncated_by,omitempty"`
	ScreenIncluded bool   `yaml:"screen_included"`
	AltScreen      bool   `yaml:"alt_screen,omitempty"`
	LogPath        string `yaml:"log_path,omitempty"`
}

func renderLog(result ipc.LogResult, requested int, fromEnd bool, tokenBudget int) (any, string, error) {
	// Truncation drops from the far end: it_tail keeps the newest lines and
	// it_head keeps the oldest, so the part the caller actually asked for is
	// never the part that is thrown away.
	kept, omitted, err := budget.FitLines(result.Lines, tokenBudget, fromEnd)
	if err != nil {
		return nil, "", &ipc.Error{Code: ipc.CodeInternal, Message: "apply the response budget: " + err.Error()}
	}

	// truncated_by describes a truncation, so it is only set when one happened.
	// Emitting it alongside truncated: false said two contradictory things at
	// once about whether output was missing.
	truncatedBy := ""
	if omitted > 0 {
		truncatedBy = "token_budget"
	}

	front := logMetadata{
		Session: result.Session.ID, Name: result.Session.Name,
		Running: result.Session.Running, ExitCode: result.Session.ExitCode,
		LinesRequested: requested, LinesReturned: len(kept), LinesOmitted: omitted,
		TotalLines:     result.TotalLines,
		Truncated:      omitted > 0,
		TruncatedBy:    truncatedBy,
		ScreenIncluded: len(result.ScreenLines) > 0,
		AltScreen:      result.AltScreen,
		LogPath:        result.LogPath,
	}
	return front, logBody(result, front, kept, fromEnd), nil
}

func logBody(result ipc.LogResult, front logMetadata, kept []string, fromEnd bool) string {
	tool := "it_head"
	which := "first"
	if fromEnd {
		tool, which = "it_tail", "last"
	}

	var parts []string
	if len(kept) == 0 {
		if result.Session.Running {
			parts = append(parts, "This session's log is empty: nothing has scrolled off its screen yet. "+
				fmt.Sprintf("Read the screen itself with `%s`.", call("it_read", map[string]any{"session": result.Session.ID})))
		} else {
			parts = append(parts, "This session's log is empty.")
		}
	} else {
		parts = append(parts, render.Screen(kept))
		parts = append(parts, logSummary(front, which, tool, result))
	}

	if len(result.ScreenLines) > 0 {
		// A session in a full-screen program has a log that stops where the
		// program started, so labelling the two parts is essential: the log is
		// history, the screen is now.
		heading := "Live screen:"
		if result.AltScreen {
			heading = "A full-screen program is running. Its display never scrolls into the log, so here is the live screen:"
		}
		parts = append(parts, heading, render.Screen(result.ScreenLines))
	}

	if front.Running && fromEnd && len(kept) > 0 {
		parts = append(parts, fmt.Sprintf("The session is still running; call `%s` again for newer output.", tool))
	}
	return strings.Join(parts, "\n\n")
}

func logSummary(front logMetadata, which, tool string, result ipc.LogResult) string {
	var summary strings.Builder
	if front.LinesReturned == front.TotalLines {
		fmt.Fprintf(&summary, "That is the complete log: %d %s.",
			front.TotalLines, plural(front.TotalLines, "line", "lines"))
	} else {
		fmt.Fprintf(&summary, "Showing the %s %d of %d requested lines (%d in the log).",
			which, front.LinesReturned, front.LinesRequested, front.TotalLines)
	}

	if front.LinesOmitted > 0 {
		dropped := "older"
		if !strings.EqualFold(which, "last") {
			dropped = "newer"
		}
		fmt.Fprintf(&summary, " %d %s %s omitted to fit the response budget.",
			front.LinesOmitted, dropped, plural(front.LinesOmitted, "line was", "lines were"))
	}

	// Point at something that can actually reach what was dropped. When the
	// budget cut lines out of the middle of the requested range, the opposite
	// end cannot return them -- only the file can, so say that first.
	if front.LinesOmitted > 0 && front.LogPath != "" {
		fmt.Fprintf(&summary, " Those lines are only reachable by reading `%s` directly; "+
			"the other end of the log is a different part of the output.", front.LogPath)
	} else if front.LinesReturned < front.TotalLines {
		other := "it_head"
		if tool == "it_head" {
			other = "it_tail"
		}
		fmt.Fprintf(&summary, " Read the other end with `%s`.",
			call(other, map[string]any{"session": result.Session.ID}))
		if front.LogPath != "" {
			fmt.Fprintf(&summary, " The complete log is at `%s`.", front.LogPath)
		}
	}
	return summary.String()
}

// --- shared helpers ---------------------------------------------------------

// call renders an exact, copy-pasteable tool call. Every piece of guidance
// uses it, so a model is never asked to assemble arguments itself.
//
// HTML escaping is disabled: encoding/json turns < and > into < and
// > by default, which would render a placeholder like <id> as unreadable
// noise in text a model has to follow.
func call(name string, args map[string]any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(args); err != nil {
		return name + "({})"
	}
	return name + "(" + strings.TrimRight(buffer.String(), "\n") + ")"
}

func label(info ipc.SessionInfo) string {
	if info.Name != "" {
		return fmt.Sprintf("`%s` (`%s`)", info.Name, info.ID)
	}
	return fmt.Sprintf("`%s`", info.ID)
}

func commandText(info ipc.SessionInfo) string {
	if info.CommandLine != "" {
		return info.CommandLine
	}
	return strings.Join(info.Command, " ")
}

func exitPhrase(info ipc.SessionInfo) string {
	return exitCodePhrase(info.ExitCode)
}

func exitCodePhrase(code *int) string {
	if code == nil {
		return ""
	}
	if *code == 0 {
		return " successfully (exit 0)"
	}
	return fmt.Sprintf(" with exit code %d", *code)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func formatDuration(milliseconds int64) string {
	duration := time.Duration(milliseconds) * time.Millisecond
	if duration < time.Second {
		return fmt.Sprintf("%dms", milliseconds)
	}
	return duration.Round(100 * time.Millisecond).String()
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}
