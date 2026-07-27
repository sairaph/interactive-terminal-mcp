// Package app is the full-screen application a bare invocation opens on a
// terminal.
//
// It is a terminal wrapper running inside a terminal: the human sees the same
// sessions the agent sees, in the state the agent left them. That shared view
// is the point, because it makes the agent's terminal access visible rather
// than something that happens off-screen.
package app

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/interactive-terminal-mcp/internal/bootstrap"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
	"golang.org/x/term"
)

// screen identifies which view is drawn.
type screen int

const (
	screenHome screen = iota
	screenSession
	screenConfigure
)

// refreshInterval is how often the home list is reloaded. Sessions change
// because of the agent, not because of the user, so the list has to poll.
const refreshInterval = time.Second

// model is the whole application state.
type model struct {
	ctx     context.Context
	runtime *bootstrap.Runtime
	version string

	screen screen
	width  int
	height int

	// home
	sessions []ipc.SessionInfo
	active   string
	cursor   int
	confirm  *confirmState
	renaming bool
	rename   *composer

	// session view
	session  *sessionView
	composer *composer
	rawMode  bool
	autoRaw  bool

	// configure
	configure *configureState

	// pending is a session to open as soon as the application starts, used by
	// the attach entrypoint so the list never flashes on screen first.
	pending string

	status  string
	failure string
	quit    bool
}

// confirmState is a yes/no prompt with the consequences spelled out.
type confirmState struct {
	title   string
	details []string
	confirm string
	cancel  string
	// yes is true when the destructive option is highlighted; it starts false
	// so the safe choice is the default.
	yes    bool
	action func() tea.Cmd
}

// Run opens the application.
func Run(ctx context.Context, runtime *bootstrap.Runtime, version string) int {
	if !isTerminal() {
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp: not a terminal. Run `interactive-terminal-mcp mcp` to start the MCP server.")
		return 1
	}

	state := &model{
		ctx: ctx, runtime: runtime, version: version,
		screen: screenHome, composer: newComposer(),
		width: 100, height: 30,
		status: "Starting session daemon…",
	}
	program := tea.NewProgram(state,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp:", err)
		return 1
	}
	if state.failure != "" {
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp:", state.failure)
		return 1
	}
	return 0
}

func (m *model) Init() tea.Cmd {
	commands := []tea.Cmd{m.loadSessions(), tick()}
	if m.pending != "" {
		commands = append(commands, m.openSession(m.pending))
		m.pending = ""
	}
	return tea.Batch(commands...)
}

func (m *model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
		m.resizeViews()
		if m.screen == screenSession && m.session != nil {
			return m, m.session.resize(m.terminalWidth(), m.terminalHeight())
		}
		return m, nil

	case tickMsg:
		var commands []tea.Cmd
		commands = append(commands, tick())
		if m.screen == screenHome {
			commands = append(commands, m.loadSessions())
		}
		return m, tea.Batch(commands...)

	case sessionsMsg:
		m.sessions, m.active = message.sessions, message.active
		m.failure = ""
		if message.err != nil {
			m.status = message.err.Error()
		} else if m.status == "Starting session daemon…" {
			m.status = ""
		}
		m.clampCursor()
		return m, nil

	case errorMsg:
		m.status = styleError.Render(message.err.Error())
		return m, nil

	case statusMsg:
		m.status = string(message)
		return m, nil

	case openedMsg:
		return m.handleOpened(message)

	case openRequest:
		return m, m.openSession(message.id)

	case terminalMsg:
		if m.session == nil {
			return m, nil
		}
		command := m.session.handle(message)
		// A full-screen program taking the alternate screen is the signal to
		// hand the keyboard straight to it, and giving it back is the signal to
		// return to the composer. Doing this automatically covers essentially
		// every TUI without the user having to think about modes.
		if alt := m.session.term.AltScreen(); alt != m.autoRaw {
			m.autoRaw = alt
			m.rawMode = alt
			m.resizeViews()
			return m, tea.Batch(command, m.session.resize(m.terminalWidth(), m.terminalHeight()))
		}
		return m, command

	case configureMsg:
		return m.handleConfigure(message)

	case tea.KeyMsg:
		return m.handleKey(message)

	case tea.MouseMsg:
		return m.handleMouse(message)
	}
	return m, nil
}

func (m *model) View() string {
	if m.width < minWidth || m.height < minHeight {
		return fmt.Sprintf("\n  Terminal is %dx%d.\n  interactive-terminal-mcp needs at least %dx%d.\n",
			m.width, m.height, minWidth, minHeight)
	}
	switch m.screen {
	case screenSession:
		return m.viewSession()
	case screenConfigure:
		return m.viewConfigure()
	default:
		return m.viewHome()
	}
}

func (m *model) handleKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A confirmation owns the keyboard until it is answered, so a stray key
	// cannot fall through to the list underneath and act on the wrong row.
	if m.confirm != nil {
		return m.handleConfirmKey(message)
	}
	switch m.screen {
	case screenSession:
		return m.handleSessionKey(message)
	case screenConfigure:
		return m.handleConfigureKey(message)
	default:
		return m.handleHomeKey(message)
	}
}

func (m *model) handleConfirmKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "up", "down", "k", "j", "tab", "left", "right", "h", "l":
		m.confirm.yes = !m.confirm.yes
	case "esc", "n", "q", "ctrl+c":
		m.confirm = nil
		m.status = ""
	case "y":
		action := m.confirm.action
		m.confirm = nil
		return m, action()
	case "enter":
		if !m.confirm.yes {
			m.confirm = nil
			m.status = ""
			return m, nil
		}
		action := m.confirm.action
		m.confirm = nil
		return m, action()
	}
	return m, nil
}

func (m *model) handleMouse(message tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.screen != screenSession || m.session == nil {
		return m, nil
	}
	// Wheel scrolling walks the session's scrollback, which is what a person
	// expects from a terminal even though the program below sees nothing.
	switch message.Button {
	case tea.MouseButtonWheelUp:
		return m, m.session.scroll(3)
	case tea.MouseButtonWheelDown:
		return m, m.session.scroll(-3)
	}
	return m, nil
}

// resizeViews recomputes the sizes that depend on the window.
func (m *model) resizeViews() {
	// The composer may grow to 80% of the window before it starts scrolling,
	// so a long paste is visible without hiding the terminal entirely.
	maxComposer := (m.height * 8) / 10
	if maxComposer < 1 {
		maxComposer = 1
	}
	m.composer.setSize(m.width-4, maxComposer)
	if m.rename != nil {
		m.rename.setSize(m.width-4, 1)
	}
}

// terminalWidth and terminalHeight are the interior of the frame, which is the
// size the session is set to so the human sees exactly one screen.
func (m *model) terminalWidth() int {
	width := m.width - 2
	if width < minWidth-2 {
		width = minWidth - 2
	}
	return width
}

func (m *model) terminalHeight() int {
	// Frame borders take two rows, the composer takes its own, and the footer
	// takes two.
	height := m.height - 2 - m.composerHeight() - 2
	if height < 5 {
		height = 5
	}
	return height
}

func (m *model) composerHeight() int {
	if m.rawMode {
		return 0
	}
	return m.composer.visibleHeight()
}

func (m *model) clampCursor() {
	limit := len(m.sessions) // +1 for the "new session" row, -1 for zero index
	if m.cursor > limit {
		m.cursor = limit
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func isTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// --- messages ---------------------------------------------------------------

type tickMsg time.Time

type sessionsMsg struct {
	sessions []ipc.SessionInfo
	active   string
	err      error
}

type errorMsg struct{ err error }

type statusMsg string

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) loadSessions() tea.Cmd {
	ctx, runtime := m.ctx, m.runtime
	return func() tea.Msg {
		client, err := runtime.Dial(ctx)
		if err != nil {
			return sessionsMsg{err: err}
		}
		defer client.Close()
		var result ipc.ListResult
		if err := client.Call(ctx, ipc.OpSessionList, nil, &result); err != nil {
			return sessionsMsg{err: err}
		}
		return sessionsMsg{sessions: result.Sessions, active: result.Active}
	}
}
