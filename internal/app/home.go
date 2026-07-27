package app

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

// newSessionRow is the index of the "+ New session" entry, which is always
// first so creating a session is one keypress on a freshly opened application.
const newSessionRow = 0

func (m *model) handleHomeKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.renaming {
		return m.handleRenameKey(message)
	}
	if m.naming {
		return m.handleNameKey(message)
	}

	switch message.String() {
	case "q", "ctrl+c":
		// Quitting the application leaves the daemon and every session alone;
		// the agent's terminals keep running, which is the whole point.
		m.quit = true
		return m, tea.Quit
	case "Q":
		return m, m.confirmStopDaemon()
	case "up", "k":
		m.cursor--
		if m.cursor < 0 {
			m.cursor = len(m.sessions)
		}
	case "down", "j":
		m.cursor++
		if m.cursor > len(m.sessions) {
			m.cursor = 0
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(m.sessions)
	case "enter":
		if m.cursor == newSessionRow {
			m.startNaming()
			return m, nil
		}
		return m, m.openSession(m.selected().ID)
	case "n":
		m.startNaming()
		return m, nil
	case "N":
		// Skip the prompt for anyone who wants a session immediately.
		return m, m.createSession("")
	case "backspace", "delete", "d":
		if m.cursor == newSessionRow {
			return m, nil
		}
		m.confirmDelete(m.selected())
	case "r":
		if m.cursor == newSessionRow {
			return m, nil
		}
		m.startRename(m.selected())
	case "a":
		if m.cursor == newSessionRow {
			return m, nil
		}
		return m, m.setActive(m.selected().ID)
	case "c":
		return m, m.openConfigure()
	case "?":
		m.status = "↑↓ move · enter open · n new · backspace delete · r rename · a set active · c configure · q quit · Q stop daemon"
	}
	return m, nil
}

func (m *model) selected() ipc.SessionInfo {
	index := m.cursor - 1
	if index < 0 || index >= len(m.sessions) {
		return ipc.SessionInfo{}
	}
	return m.sessions[index]
}

// confirmDelete states both the running command and the retention consequence,
// because those are the two facts that decide whether the action is reversible.
func (m *model) confirmDelete(info ipc.SessionInfo) {
	if info.ID == "" {
		return
	}
	name := info.ID
	if info.Name != "" {
		name = fmt.Sprintf("%q (%s)", info.Name, info.ID)
	}

	details := []string{}
	if info.Running {
		command := info.CommandLine
		if command == "" {
			command = strings.Join(info.Command, " ")
		}
		if command != "" {
			details = append(details, "It is still running: "+command)
		} else {
			details = append(details, "It is still running.")
		}
	} else if info.ExitCode != nil {
		details = append(details, fmt.Sprintf("It already ended with exit code %d.", *info.ExitCode))
	}

	details = append(details, "It is removed from this list and its logs are deleted.")

	id := info.ID
	m.confirm = &confirmState{
		title:   "Delete session " + name + "?",
		details: details,
		cancel:  "Cancel", confirm: "Delete",
		action: func() tea.Cmd { return m.killSession(id) },
	}
}

func (m *model) confirmStopDaemon() tea.Cmd {
	live := 0
	for _, info := range m.sessions {
		if info.Running {
			live++
		}
	}
	details := []string{"Every session stops, including any an AI agent is using."}
	if live > 0 {
		details = append(details, fmt.Sprintf("%d %s running right now.", live, plural(live, "session is", "sessions are")))
	} else {
		details = []string{"No sessions are running."}
	}
	m.confirm = &confirmState{
		title:   "Stop the session daemon?",
		details: details,
		cancel:  "Cancel", confirm: "Stop daemon",
		action: func() tea.Cmd {
			ctx, runtime := m.ctx, m.runtime
			return tea.Sequence(
				func() tea.Msg {
					client, err := runtime.Connect(ctx)
					if err != nil {
						return errorMsg{err}
					}
					defer client.Close()
					var status ipc.Status
					_ = client.Call(ctx, ipc.OpDaemonStop, ipc.StopArgs{KillSessions: true}, &status)
					return statusMsg("Daemon stopped.")
				},
				tea.Quit,
			)
		},
	}
	return nil
}

// startNaming asks for the optional session name before creating one. The
// prompt is skippable with a bare enter, so naming stays optional while still
// being offered -- a session that is never named is much harder to pick out of
// a list later.
func (m *model) startNaming() {
	m.naming = true
	m.nameInput = newComposer()
	m.nameInput.setSize(m.width-4, 1)
	m.status = "Name for the new session (optional) · enter create · esc cancel"
}

func (m *model) handleNameKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "esc", "ctrl+c":
		m.naming, m.nameInput = false, nil
		m.status = ""
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.nameInput.Value())
		m.naming, m.nameInput = false, nil
		m.status = ""
		return m, m.createSession(name)
	}
	m.nameInput.update(message)
	return m, nil
}

func (m *model) startRename(info ipc.SessionInfo) {
	if info.ID == "" {
		return
	}
	m.renaming = true
	m.rename = newComposer()
	m.rename.setSize(m.width-4, 1)
	m.rename.Insert(info.Name)
	m.status = "New name for " + info.ID + " (empty clears it) · enter confirm · esc cancel"
}

func (m *model) handleRenameKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "esc", "ctrl+c":
		m.renaming, m.rename = false, nil
		m.status = ""
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.rename.Value())
		id := m.selected().ID
		m.renaming, m.rename = false, nil
		return m, m.renameSession(id, name)
	}
	m.rename.update(message)
	return m, nil
}

// --- commands ---------------------------------------------------------------

func (m *model) createSession(name string) tea.Cmd {
	ctx, runtime := m.ctx, m.runtime
	return func() tea.Msg {
		client, err := runtime.Dial(ctx)
		if err != nil {
			return errorMsg{err}
		}
		defer client.Close()
		var screen ipc.Screen
		args := ipc.NewArgs{
			Name: name,
			Cols: m.terminalWidth(), Rows: m.terminalHeight(),
			WaitMS: 1500,
		}
		if err := client.Call(ctx, ipc.OpSessionNew, args, &screen); err != nil {
			return errorMsg{err}
		}
		return openRequest{id: screen.Session.ID}
	}
}

func (m *model) killSession(id string) tea.Cmd {
	ctx, runtime := m.ctx, m.runtime
	return func() tea.Msg {
		client, err := runtime.Dial(ctx)
		if err != nil {
			return errorMsg{err}
		}
		defer client.Close()
		var result ipc.KillResult
		// Purge, not just kill: the button says Delete, so the row must go.
		// Retention governs how long an agent can still read a finished
		// session's log, not whether a session the user deleted stays on screen.
		if err := client.Call(ctx, ipc.OpSessionKill, ipc.KillArgs{Session: id, Purge: true}, &result); err != nil {
			return errorMsg{err}
		}
		if result.Escalated {
			return statusMsg("Session " + id + " did not stop on request and was force-killed.")
		}
		return statusMsg("Deleted session " + id + ".")
	}
}

func (m *model) renameSession(id, name string) tea.Cmd {
	ctx, runtime := m.ctx, m.runtime
	return func() tea.Msg {
		client, err := runtime.Dial(ctx)
		if err != nil {
			return errorMsg{err}
		}
		defer client.Close()
		var info ipc.SessionInfo
		if err := client.Call(ctx, ipc.OpSessionRename, ipc.RenameArgs{Session: id, Name: name}, &info); err != nil {
			return errorMsg{err}
		}
		if info.Name == "" {
			return statusMsg("Cleared the name of " + info.ID + ".")
		}
		return statusMsg("Renamed " + info.ID + " to " + info.Name + ".")
	}
}

func (m *model) setActive(id string) tea.Cmd {
	ctx, runtime := m.ctx, m.runtime
	return func() tea.Msg {
		client, err := runtime.Dial(ctx)
		if err != nil {
			return errorMsg{err}
		}
		defer client.Close()
		var result ipc.ActiveResult
		if err := client.Call(ctx, ipc.OpSessionAtive, ipc.ActiveArgs{Session: id, Set: true}, &result); err != nil {
			return errorMsg{err}
		}
		return statusMsg("Session " + id + " is now active; agents reach it without naming a session.")
	}
}

// --- view -------------------------------------------------------------------

func (m *model) viewHome() string {
	var body strings.Builder

	rows := m.homeRows()
	for index, row := range rows {
		prefix := "  "
		if index == m.cursor {
			prefix = styleCursor.Render("> ")
		}
		line := row
		if index == m.cursor {
			line = styleSelect.Render(padTo(stripStyles(row), m.width-6))
		}
		body.WriteString(prefix + line + "\n")
	}

	framed := frame(m.width, m.height-3, "interactive-terminal-mcp", "", body.String())

	var out strings.Builder
	out.WriteString(framed)
	out.WriteByte('\n')

	if m.confirm != nil {
		out.WriteString(m.viewConfirm())
		return out.String()
	}
	if m.renaming {
		out.WriteString(m.rename.view("  name: ", true) + "\n")
		out.WriteString(styleFooter.Render("  enter confirm · esc cancel"))
		return out.String()
	}
	if m.naming {
		out.WriteString(m.nameInput.view("  name: ", true) + "\n")
		out.WriteString(styleFooter.Render("  enter create (blank for none) · esc cancel"))
		if m.status != "" {
			out.WriteString("\n  " + m.status)
		}
		return out.String()
	}

	out.WriteString(styleFooter.Render("  ↑↓ navigate · enter open · n new · N new unnamed · backspace delete · r rename"))
	out.WriteByte('\n')
	out.WriteString(styleFooter.Render("  a set active · c configure · q quit"))
	if m.status != "" {
		out.WriteString("\n  " + m.status)
	}
	return out.String()
}

func (m *model) homeRows() []string {
	rows := []string{styleTitle.Render("+ New session")}
	if len(m.sessions) == 0 {
		rows = append(rows, styleDim.Render("  no sessions yet"))
		return rows
	}
	for _, info := range m.sessions {
		rows = append(rows, m.homeRow(info))
	}
	return rows
}

func (m *model) homeRow(info ipc.SessionInfo) string {
	marker := styleEnded.Render("○")
	state := styleEnded.Render(stateText(info))
	if info.Running {
		marker = styleRunning.Render("●")
		state = styleRunning.Render("running")
	}

	name := info.Name
	if name == "" {
		name = styleDim.Render("(unnamed)")
	} else if info.Active {
		name = styleActive.Render(name)
	}

	return fmt.Sprintf("%s %-18s %-10s %-9s %-8s %-13s %d lines",
		marker, name, info.ID, state,
		fmt.Sprintf("%dx%d", info.Cols, info.Rows),
		relativeTime(info.LastActivityAt), info.TranscriptLines)
}

func (m *model) viewConfirm() string {
	var out strings.Builder
	out.WriteString("  " + styleWarn.Render(m.confirm.title) + "\n")
	for _, detail := range m.confirm.details {
		out.WriteString("  " + styleDim.Render(detail) + "\n")
	}
	out.WriteByte('\n')

	cancel, confirm := "  "+m.confirm.cancel, "  "+m.confirm.confirm
	if m.confirm.yes {
		confirm = styleCursor.Render("> ") + styleError.Render(m.confirm.confirm)
		cancel = "  " + m.confirm.cancel
	} else {
		cancel = styleCursor.Render("> ") + m.confirm.cancel
		confirm = "  " + styleError.Render(m.confirm.confirm)
	}
	out.WriteString("  " + cancel + "\n  " + confirm + "\n\n")
	out.WriteString(styleFooter.Render("  ↑↓ choose · enter confirm · esc cancel"))
	return out.String()
}

// frame draws the rounded border with the product name inset in the top edge.
func frame(width, height int, title, badge, content string) string {
	interior := width - 2
	if interior < 1 {
		interior = 1
	}
	body := height - 2
	if body < 1 {
		body = 1
	}

	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	for len(lines) < body {
		lines = append(lines, "")
	}
	if len(lines) > body {
		lines = lines[:body]
	}

	var out strings.Builder
	out.WriteString(styleFrame.Render(border.TopLeft + border.Top))
	out.WriteString(" " + styleTitle.Render(title) + " ")
	used := 2 + lipgloss.Width(title) + 2
	if badge != "" {
		rendered := styleBadge.Render(badge)
		filler := interior - used - lipgloss.Width(rendered) - 1
		if filler < 0 {
			filler = 0
		}
		out.WriteString(styleFrame.Render(strings.Repeat(border.Top, filler)))
		out.WriteString(rendered)
		out.WriteString(styleFrame.Render(border.Top + border.TopRight))
	} else {
		filler := interior - used + 1
		if filler < 0 {
			filler = 0
		}
		out.WriteString(styleFrame.Render(strings.Repeat(border.Top, filler) + border.TopRight))
	}
	out.WriteByte('\n')

	for _, line := range lines {
		out.WriteString(styleFrame.Render(border.Left))
		out.WriteString(padTo(truncate(line, interior), interior))
		out.WriteString(styleFrame.Render(border.Right))
		out.WriteByte('\n')
	}

	out.WriteString(styleFrame.Render(border.BottomLeft + strings.Repeat(border.Bottom, interior) + border.BottomRight))
	return out.String()
}

// padTo pads a possibly-styled string to a display width.
func padTo(text string, width int) string {
	current := lipgloss.Width(text)
	if current >= width {
		return text
	}
	return text + strings.Repeat(" ", width-current)
}

// stripStyles removes ANSI sequences so a selected row can be restyled without
// the old colours bleeding through the highlight.
func stripStyles(text string) string {
	var out strings.Builder
	inEscape := false
	for _, r := range text {
		switch {
		case r == 0x1b:
			inEscape = true
		case inEscape && (r == 'm' || r == 'K'):
			inEscape = false
		case !inEscape:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func stateText(info ipc.SessionInfo) string {
	if info.Running {
		return "running"
	}
	if info.ExitCode != nil {
		return fmt.Sprintf("exit %d", *info.ExitCode)
	}
	return "ended"
}

func relativeTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	elapsed := time.Since(value)
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds ago", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	}
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}
