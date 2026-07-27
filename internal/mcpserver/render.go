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

// screenMetadata is the frontmatter shared by it_active, it_new, it_read, and
// it_send. One shape across four tools means a model learns it once.
type screenMetadata struct {
	Session           string `yaml:"session"`
	Name              string `yaml:"name,omitempty"`
	Active            bool   `yaml:"active"`
	Running           bool   `yaml:"running"`
	ExitCode          *int   `yaml:"exit_code,omitempty"`
	PID               int    `yaml:"pid,omitempty"`
	Size              []int  `yaml:"size,flow"`
	Cursor            []int  `yaml:"cursor,flow"`
	AltScreen         bool   `yaml:"alt_screen"`
	Title             string `yaml:"title,omitempty"`
	Cwd               string `yaml:"cwd,omitempty"`
	Settled           bool   `yaml:"settled"`
	WaitedMS          int64  `yaml:"waited_ms"`
	LastActivityAt    string `yaml:"last_activity_at,omitempty"`
	BlankLinesTrimmed int    `yaml:"blank_lines_trimmed,omitempty"`
	TranscriptLines   int    `yaml:"transcript_lines"`
	LogPath           string `yaml:"log_path,omitempty"`
}

func screenFront(screen ipc.Screen) screenMetadata {
	info := screen.Session
	return screenMetadata{
		Session: info.ID, Name: info.Name, Active: info.Active,
		Running: info.Running, ExitCode: info.ExitCode, PID: info.PID,
		Size:      []int{info.Cols, info.Rows},
		Cursor:    []int{screen.Cursor[0], screen.Cursor[1]},
		AltScreen: info.AltScreen, Title: info.Title, Cwd: info.Cwd,
		Settled: screen.Settled, WaitedMS: screen.WaitedMS,
		LastActivityAt:    formatTime(info.LastActivityAt),
		BlankLinesTrimmed: screen.BlankLinesTrimmed,
		TranscriptLines:   info.TranscriptLines,
		LogPath:           info.LogPath,
	}
}

// screenBody renders the screen followed by guidance.
//
// The screen always comes first: it is what the caller asked for, and burying
// it under prose would make every result harder to read.
func screenBody(screen ipc.Screen, guidance []string) string {
	parts := []string{render.Screen(screen.Lines)}

	// An unsettled screen is the single most important thing to say: the
	// command may still be running and the output may be incomplete.
	if !screen.Settled && screen.Session.Running {
		parts = append(parts, fmt.Sprintf(
			"Output was still arriving when the %s wait ended, so this screen may be incomplete. "+
				"Check again with `%s`.",
			formatDuration(screen.WaitedMS),
			call("it_read", map[string]any{"session": screen.Session.ID, "wait": 10})))
	}
	parts = append(parts, guidance...)
	return strings.Join(parts, "\n\n")
}

func activeGuidance(screen ipc.Screen) []string {
	info := screen.Session
	if !info.Running {
		return []string{fmt.Sprintf(
			"Session %s has ended%s. Its screen and logs are still readable. Start a new one with `%s`.",
			label(info), exitPhrase(info), call("it_new", map[string]any{}))}
	}
	return []string{fmt.Sprintf(
		"Session %s is active. Run a command with `%s`.",
		label(info), call("it_send", map[string]any{"text": "ls -la"}))}
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
	return []string{fmt.Sprintf(
		"Session %s is active. Type into it with `%s`, and read it again later with `%s`.",
		label(info),
		call("it_send", map[string]any{"text": "echo hello"}),
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
			"%d earlier lines have scrolled off this screen. Read them with `%s`.",
			info.TranscriptLines, call("it_tail", map[string]any{"session": info.ID, "lines": 100}))}
	}
	return nil
}

// --- it_active with nothing active ------------------------------------------

type noActiveMetadata struct {
	Active        any `yaml:"active"`
	LiveSessions  int `yaml:"live_sessions"`
	TotalSessions int `yaml:"total_sessions"`
}

func noActiveFront(result ipc.ActiveResult) noActiveMetadata {
	return noActiveMetadata{Active: nil, LiveSessions: result.LiveSessions, TotalSessions: result.TotalSessions}
}

func noActiveBody(result ipc.ActiveResult) string {
	if result.TotalSessions == 0 {
		return fmt.Sprintf(
			"No session is currently active, and none exist. Create one with `%s`.",
			call("it_new", map[string]any{}))
	}
	return fmt.Sprintf(
		"No session is currently active, but %d %s. List them with `%s`, then select one with `%s` or create another with `%s`.",
		result.TotalSessions, plural(result.TotalSessions, "session exists", "sessions exist"),
		call("it_list", map[string]any{}),
		call("it_active", map[string]any{"session": "<id>"}),
		call("it_new", map[string]any{}))
}

// --- it_list ----------------------------------------------------------------

type listMetadata struct {
	Page       int       `yaml:"page"`
	Total      int       `yaml:"total"`
	TotalPages int       `yaml:"total_pages"`
	Active     string    `yaml:"active,omitempty"`
	Sessions   []listRow `yaml:"sessions,omitempty"`
}

// listRow is deliberately flat and short. A model scanning a list needs to
// pick a session, not reconstruct its full history.
type listRow struct {
	ID              string `yaml:"id"`
	Name            string `yaml:"name,omitempty"`
	Active          bool   `yaml:"active,omitempty"`
	Running         bool   `yaml:"running"`
	ExitCode        *int   `yaml:"exit_code,omitempty"`
	KilledBy        string `yaml:"killed_by,omitempty"`
	PID             int    `yaml:"pid,omitempty"`
	Command         string `yaml:"command,omitempty"`
	Cwd             string `yaml:"cwd,omitempty"`
	Size            []int  `yaml:"size,flow"`
	AltScreen       bool   `yaml:"alt_screen,omitempty"`
	Title           string `yaml:"title,omitempty"`
	CreatedAt       string `yaml:"created_at,omitempty"`
	LastActivityAt  string `yaml:"last_activity_at,omitempty"`
	TranscriptLines int    `yaml:"transcript_lines"`
	LogPath         string `yaml:"log_path,omitempty"`
}

func renderList(result ipc.ListResult, page, tokenBudget int) (any, string, error) {
	rows := make([]listRow, 0, len(result.Sessions))
	for _, info := range result.Sessions {
		rows = append(rows, listRow{
			ID: info.ID, Name: info.Name, Active: info.Active, Running: info.Running,
			ExitCode: info.ExitCode, KilledBy: info.KilledBy, PID: info.PID,
			Command: commandText(info), Cwd: info.Cwd,
			Size:      []int{info.Cols, info.Rows},
			AltScreen: info.AltScreen, Title: info.Title,
			CreatedAt: formatTime(info.CreatedAt), LastActivityAt: formatTime(info.LastActivityAt),
			TranscriptLines: info.TranscriptLines, LogPath: info.LogPath,
		})
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
		Active: result.Active, Sessions: window,
	}
	return front, listBody(front, window), nil
}

func listBody(front listMetadata, window []listRow) string {
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

	if front.Active == "" && live > 0 {
		parts = append(parts, fmt.Sprintf(
			"No session is active. Select one with `%s` so the other tools can default to it.",
			call("it_active", map[string]any{"session": window[0].ID})))
	}
	parts = append(parts, fmt.Sprintf(
		"Read a session with `%s`, or type into it with `%s`.",
		call("it_read", map[string]any{"session": window[0].ID}),
		call("it_send", map[string]any{"session": window[0].ID, "text": "pwd"})))

	if front.Page < front.TotalPages {
		parts = append(parts, fmt.Sprintf("Continue with `%s`.", call("it_list", map[string]any{"page": front.Page + 1})))
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
	LogsRetained bool   `yaml:"logs_retained"`
	LogPath      string `yaml:"log_path,omitempty"`
}

func killFront(result ipc.KillResult) killMetadata {
	return killMetadata{
		Killed: result.Killed, Name: result.Name, Signal: result.Signal,
		Escalated: result.Escalated, ExitCode: result.ExitCode,
		AlreadyEnded: result.AlreadyGone,
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
		// An interrupt usually leaves the shell alive, which is the point of
		// using it; saying so prevents a needless it_new.
		return fmt.Sprintf(
			"Sent an interrupt to session %s. The running command should have stopped, and the session is still usable. "+
				"Check it with `%s`.",
			name, call("it_read", map[string]any{"session": result.Killed}))
	case result.Escalated:
		parts = append(parts, fmt.Sprintf(
			"Session %s did not exit after TERM and was ended with KILL%s.",
			name, exitCodePhrase(result.ExitCode)))
	default:
		parts = append(parts, fmt.Sprintf("Ended session %s with %s%s.", name, result.Signal, exitCodePhrase(result.ExitCode)))
	}

	if result.LogsRetained && result.LogPath != "" {
		parts = append(parts, fmt.Sprintf("Its log is kept at `%s`.", result.LogPath))
	} else if !result.LogsRetained {
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

	truncatedBy := ""
	switch {
	case omitted > 0:
		truncatedBy = "token_budget"
	case len(result.Lines) < requested && result.TotalLines <= len(result.Lines):
		// Fewer lines than asked for simply because the log is shorter. That
		// is not truncation, and reporting it as such would suggest output is
		// missing when none is.
		if fromEnd {
			truncatedBy = "log_start"
		} else {
			truncatedBy = "log_end"
		}
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

	// A caller that was given less than the whole log always gets a way to
	// reach the rest, either through the other tool or straight from the file.
	if front.LinesReturned < front.TotalLines {
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
