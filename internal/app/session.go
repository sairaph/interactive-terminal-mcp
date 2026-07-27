package app

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/interactive-terminal-mcp/internal/bootstrap"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
	"github.com/sairaph/interactive-terminal-mcp/internal/keys"
	"github.com/sairaph/interactive-terminal-mcp/internal/vterm"
)

// sessionView holds one attached session: its own emulator fed by the daemon's
// stream, the connection carrying input back, and the scrollback position.
//
// The viewer runs its own emulator rather than asking the daemon to render.
// That keeps the daemon out of the rendering path entirely, so a slow or
// stalled viewer can never delay an agent's tool call.
type sessionView struct {
	ctx    context.Context
	cancel context.CancelFunc

	info   ipc.SessionInfo
	client *ipc.Client
	term   vterm.Terminal
	send   func([]byte) error
	frames <-chan ipc.Frame

	// scrollOffset is how many lines above the live screen the view is parked.
	// Zero means live; any output or keystroke returns to zero.
	scrollOffset int
	exited       bool
	exitCode     *int
}

// readNext waits for one frame. Bubbletea drives the loop: each frame is
// delivered as a message and schedules the next read, so streaming never
// blocks the UI goroutine.
func (v *sessionView) readNext() tea.Cmd {
	frames := v.frames
	return func() tea.Msg {
		frame, ok := <-frames
		if !ok {
			return terminalMsg{closed: true}
		}
		return terminalMsg{frame: frame}
	}
}

// openRequest asks the model to open a session.
type openRequest struct{ id string }

// openedMsg carries the result of attaching.
type openedMsg struct {
	view *sessionView
	err  error
}

// terminalMsg is one streamed frame or a stream ending.
type terminalMsg struct {
	frame  ipc.Frame
	closed bool
}

func (m *model) openSession(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	ctx, runtime := m.ctx, m.runtime
	cols, rows := m.terminalWidth(), m.terminalHeight()
	return func() tea.Msg {
		return attach(ctx, runtime, id, cols, rows)
	}
}

func attach(ctx context.Context, runtime *bootstrap.Runtime, id string, cols, rows int) openedMsg {
	client, err := runtime.Dial(ctx)
	if err != nil {
		return openedMsg{err: err}
	}

	streamCtx, cancel := context.WithCancel(ctx)
	attached, err := client.Attach(streamCtx, ipc.AttachArgs{
		Session: id, Cols: cols, Rows: rows,
	})
	if err != nil {
		cancel()
		client.Close()
		return openedMsg{err: err}
	}

	view := &sessionView{
		ctx: streamCtx, cancel: cancel,
		info:   attached.Session,
		client: client, send: attached.Send, frames: attached.Frames,
		term: vterm.NewCharm(cols, rows, 5_000),
	}
	view.info.Cols, view.info.Rows = cols, rows

	// The attach reply carries the tail of the raw byte log. Replaying it
	// through this viewer's own emulator reconstructs the screen exactly, so an
	// idle shell is visible immediately instead of staying blank until it next
	// writes something.
	if len(attached.Replay) > 0 {
		_, _ = view.term.Write(attached.Replay)
	}
	return openedMsg{view: view}
}

func (m *model) handleOpened(message openedMsg) (tea.Model, tea.Cmd) {
	if message.err != nil {
		m.status = styleError.Render(message.err.Error())
		return m, nil
	}
	m.session = message.view
	m.screen = screenSession
	m.composer.Reset()
	m.rawMode = false
	m.autoRaw = false
	m.status = ""
	m.resizeViews()

	// A session already inside a full-screen program starts in raw mode, so
	// the keyboard reaches it without the user having to notice or choose.
	if m.session.term.AltScreen() {
		m.rawMode, m.autoRaw = true, true
	}
	return m, tea.Batch(
		m.session.readNext(),
		m.session.resize(m.terminalWidth(), m.terminalHeight()),
	)
}

func (m *model) handleSessionKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	view := m.session
	if view == nil {
		m.screen = screenHome
		return m, nil
	}

	switch message.String() {
	case "ctrl+q":
		// Always available, in both modes, so there is never a way to get
		// stuck inside a session.
		return m, m.closeSession()
	case "shift+pgup":
		return m, view.scroll(view.pageSize())
	case "shift+pgdown":
		return m, view.scroll(-view.pageSize())
	case "shift+home":
		view.scrollOffset = view.term.ScrollbackLines()
		return m, nil
	}

	if m.rawMode {
		return m.handleRawKey(message)
	}
	return m.handleComposerKey(message)
}

// handleRawKey forwards keystrokes to the session unchanged.
//
// This is what makes vim, htop, and less behave exactly as they would in a
// real terminal: no interpretation, no buffering, no rewriting.
func (m *model) handleRawKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	view := m.session
	view.scrollOffset = 0

	if message.String() == "ctrl+g" && !m.autoRaw {
		m.rawMode = false
		m.resizeViews()
		return m, view.resize(m.terminalWidth(), m.terminalHeight())
	}

	encoded := encodeKey(message, view.term.Modes())
	if len(encoded) == 0 {
		return m, nil
	}
	return m, view.write(encoded)
}

func (m *model) handleComposerKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	view := m.session

	switch message.String() {
	case "ctrl+g":
		m.rawMode = true
		m.resizeViews()
		m.status = ""
		return m, view.resize(m.terminalWidth(), m.terminalHeight())

	case "enter":
		return m, m.submitComposer()

	// Every terminal reports a different one of these, and some report none;
	// accepting all three means a newline always works somewhere.
	case "shift+enter", "ctrl+enter", "alt+enter", "ctrl+j":
		m.composer.Newline()
		m.resizeViews()
		return m, view.resize(m.terminalWidth(), m.terminalHeight())

	case "esc":
		if !m.composer.Empty() {
			m.composer.Reset()
			m.resizeViews()
			return m, view.resize(m.terminalWidth(), m.terminalHeight())
		}
		return m, nil

	case "ctrl+c":
		// With text pending, this clears the line as a shell does. With an
		// empty composer, it means what it means in a terminal: interrupt.
		if !m.composer.Empty() {
			m.composer.Reset()
			m.resizeViews()
			return m, view.resize(m.terminalWidth(), m.terminalHeight())
		}
		view.scrollOffset = 0
		return m, view.write([]byte{0x03})

	case "ctrl+d":
		if m.composer.Empty() {
			view.scrollOffset = 0
			return m, view.write([]byte{0x04})
		}

	case "ctrl+l":
		view.scrollOffset = 0
		return m, view.write([]byte{0x0c})

	case "tab":
		// Completion is the one thing the composer cannot do itself, so the
		// buffer is handed to the shell unsubmitted and the shell completes it.
		if !m.composer.Empty() {
			value := m.composer.Value()
			m.composer.Reset()
			m.resizeViews()
			return m, tea.Batch(
				view.write([]byte(value+"\t")),
				view.resize(m.terminalWidth(), m.terminalHeight()),
			)
		}
		return m, view.write([]byte{'\t'})

	case "up":
		if !m.composer.moveUp() {
			m.composer.recallPrevious()
			m.resizeViews()
			return m, view.resize(m.terminalWidth(), m.terminalHeight())
		}
		return m, nil

	case "down":
		if !m.composer.moveDown() {
			m.composer.recallNext()
			m.resizeViews()
			return m, view.resize(m.terminalWidth(), m.terminalHeight())
		}
		return m, nil

	case "pgup":
		m.composer.page(-1)
		return m, nil
	case "pgdown":
		m.composer.page(1)
		return m, nil
	case "ctrl+home":
		m.composer.toStart()
		return m, nil
	case "ctrl+end":
		m.composer.toEnd()
		return m, nil
	}

	// A bracketed paste from the outer terminal arrives as one key message
	// with many runes. It is inserted literally, never executed: the person
	// presses enter deliberately.
	if message.Paste {
		m.composer.Insert(string(message.Runes))
		m.resizeViews()
		return m, view.resize(m.terminalWidth(), m.terminalHeight())
	}

	previousHeight := m.composer.visibleHeight()
	if m.composer.update(message) {
		if m.composer.visibleHeight() != previousHeight {
			m.resizeViews()
			return m, view.resize(m.terminalWidth(), m.terminalHeight())
		}
		return m, nil
	}
	return m, nil
}

// submitComposer sends the buffer to the session.
func (m *model) submitComposer() tea.Cmd {
	view := m.session
	value := m.composer.Value()
	multiline := m.composer.Multiline()
	m.composer.Remember(value)
	m.composer.Reset()
	m.resizeViews()
	view.scrollOffset = 0

	commands := []tea.Cmd{view.resize(m.terminalWidth(), m.terminalHeight())}
	if multiline && view.term.Modes().BracketedPaste {
		// An editor should receive a multi-line submission as one paste, not
		// as a sequence of separate commands.
		commands = append(commands, view.write([]byte("\x1b[200~"+value+"\x1b[201~\r")))
	} else {
		commands = append(commands, view.write([]byte(value+"\r")))
	}
	return tea.Batch(commands...)
}

func (m *model) closeSession() tea.Cmd {
	view := m.session
	m.session = nil
	m.screen = screenHome
	m.rawMode, m.autoRaw = false, false
	m.composer.Reset()
	m.status = ""
	if view == nil {
		return m.loadSessions()
	}
	return tea.Batch(view.close(), m.loadSessions())
}

// --- session view operations ------------------------------------------------

func (v *sessionView) write(data []byte) tea.Cmd {
	send := v.send
	return func() tea.Msg {
		if err := send(data); err != nil {
			return errorMsg{err}
		}
		return nil
	}
}

func (v *sessionView) resize(cols, rows int) tea.Cmd {
	if cols < 20 || rows < 5 {
		return nil
	}
	v.term.Resize(cols, rows)
	v.info.Cols, v.info.Rows = cols, rows
	client, id := v.client, v.info.ID
	return func() tea.Msg {
		_ = client.ResizeAttached(id, cols, rows)
		return nil
	}
}

func (v *sessionView) scroll(delta int) tea.Cmd {
	v.scrollOffset += delta
	if v.scrollOffset < 0 {
		v.scrollOffset = 0
	}
	if maximum := v.term.ScrollbackLines(); v.scrollOffset > maximum {
		v.scrollOffset = maximum
	}
	return nil
}

func (v *sessionView) pageSize() int {
	_, rows := v.term.Size()
	if rows < 2 {
		return 1
	}
	return rows - 1
}

func (v *sessionView) close() tea.Cmd {
	cancel, client := v.cancel, v.client
	return func() tea.Msg {
		cancel()
		_ = client.Close()
		return nil
	}
}

// handle applies one streamed frame.
func (v *sessionView) handle(message terminalMsg) tea.Cmd {
	if message.closed {
		v.exited = true
		return nil
	}
	switch message.frame.Kind {
	case ipc.FrameOutput:
		_, _ = v.term.Write(message.frame.Data)
		// New output returns the view to live, matching what a real terminal
		// does and avoiding a screen that appears frozen.
		v.scrollOffset = 0
	case ipc.FrameResync:
		// This viewer fell behind and output was skipped, so the stream has a
		// hole in it. The emulator is left alone and the next write redraws;
		// applying a partial stream would corrupt the screen silently.
		v.scrollOffset = 0
	case ipc.FrameClosed:
		v.exited = true
		v.exitCode = message.frame.ExitCode
		v.info.Running = false
		return nil
	}
	return v.readNext()
}

// encodeKey converts a bubbletea key into terminal bytes for raw mode.
//
// It reuses the same encoder the it_send keys argument uses, so a keystroke a
// person types and a keystroke an agent sends reach the program identically.
func encodeKey(message tea.KeyMsg, modes vterm.Modes) []byte {
	if message.Type == tea.KeyRunes {
		if message.Alt {
			return append([]byte{0x1b}, []byte(string(message.Runes))...)
		}
		return []byte(string(message.Runes))
	}

	name, ok := rawKeyNames[message.Type]
	if !ok {
		return nil
	}
	if message.Alt {
		name = "ALT+" + name
	}
	chords, err := keys.Parse(name)
	if err != nil {
		return nil
	}
	return keys.Encode(chords, modes)
}

// rawKeyNames maps bubbletea key types onto the key language's names, so raw
// mode and the agent-facing keys argument share one encoder.
var rawKeyNames = map[tea.KeyType]string{
	tea.KeyEnter: "ENTER", tea.KeyTab: "TAB", tea.KeyEscape: "ESC",
	tea.KeySpace: "SPACE", tea.KeyBackspace: "BACKSPACE", tea.KeyDelete: "DELETE",
	tea.KeyInsert: "INSERT", tea.KeyHome: "HOME", tea.KeyEnd: "END",
	tea.KeyPgUp: "PAGE_UP", tea.KeyPgDown: "PAGE_DOWN",
	tea.KeyUp: "UP", tea.KeyDown: "DOWN", tea.KeyLeft: "LEFT", tea.KeyRight: "RIGHT",
	tea.KeyShiftTab: "SHIFT+TAB",
	tea.KeyCtrlA:    "CTRL+A", tea.KeyCtrlB: "CTRL+B", tea.KeyCtrlC: "CTRL+C",
	tea.KeyCtrlD: "CTRL+D", tea.KeyCtrlE: "CTRL+E", tea.KeyCtrlF: "CTRL+F",
	tea.KeyCtrlG: "CTRL+G", tea.KeyCtrlH: "CTRL+H", tea.KeyCtrlJ: "CTRL+J",
	tea.KeyCtrlK: "CTRL+K", tea.KeyCtrlL: "CTRL+L", tea.KeyCtrlN: "CTRL+N",
	tea.KeyCtrlO: "CTRL+O", tea.KeyCtrlP: "CTRL+P", tea.KeyCtrlR: "CTRL+R",
	tea.KeyCtrlS: "CTRL+S", tea.KeyCtrlT: "CTRL+T", tea.KeyCtrlU: "CTRL+U",
	tea.KeyCtrlV: "CTRL+V", tea.KeyCtrlW: "CTRL+W", tea.KeyCtrlX: "CTRL+X",
	tea.KeyCtrlY: "CTRL+Y", tea.KeyCtrlZ: "CTRL+Z",
	tea.KeyF1: "F1", tea.KeyF2: "F2", tea.KeyF3: "F3", tea.KeyF4: "F4",
	tea.KeyF5: "F5", tea.KeyF6: "F6", tea.KeyF7: "F7", tea.KeyF8: "F8",
	tea.KeyF9: "F9", tea.KeyF10: "F10", tea.KeyF11: "F11", tea.KeyF12: "F12",
}

// --- view -------------------------------------------------------------------

func (m *model) viewSession() string {
	view := m.session
	if view == nil {
		return ""
	}

	title := "interactive-terminal-mcp"
	badge := ""
	if m.rawMode {
		badge = "RAW"
	}

	subtitle := view.info.ID
	if view.info.Name != "" {
		subtitle = view.info.Name + " · " + view.info.ID
	}
	if view.exited {
		subtitle += " · " + endedText(view.exitCode)
	} else {
		subtitle += " · running"
	}
	if view.scrollOffset > 0 {
		subtitle = fmt.Sprintf("scrollback %d/%d", view.scrollOffset, view.term.ScrollbackLines())
	}

	content := view.render(m.terminalHeight())
	framed := frame(m.width, m.terminalHeight()+2, title+" ── "+subtitle, badge, content)

	var out strings.Builder
	out.WriteString(framed)
	out.WriteByte('\n')

	if view.exited {
		out.WriteString("  " + styleWarn.Render("Session ended "+endedText(view.exitCode)) + "\n")
		out.WriteString(styleFooter.Render("  ctrl+q back"))
		return out.String()
	}

	if m.rawMode {
		hint := "  ctrl+q back · ctrl+g leave raw mode · every other key goes to the program"
		if m.autoRaw {
			hint = "  ctrl+q back · a full-screen program is running, so every key goes to it"
		}
		out.WriteString(styleFooter.Render(hint))
		return out.String()
	}

	out.WriteString(m.composer.view("> ", true) + "\n")
	out.WriteString(styleFooter.Render("  ctrl+q back · ctrl+g raw · shift+enter newline · tab complete · ↑ history · shift+pgup scroll"))
	if m.status != "" {
		out.WriteString("\n  " + m.status)
	}
	return out.String()
}

// render produces the visible terminal content, honouring the scrollback
// position.
func (v *sessionView) render(rows int) string {
	if v.scrollOffset > 0 {
		lines := v.term.ScrollbackText(v.scrollOffset, rows)
		return strings.Join(lines, "\n")
	}
	snapshot := v.term.Snapshot()
	return strings.Join(snapshot.Lines, "\n")
}

func endedText(code *int) string {
	if code == nil {
		return "ended"
	}
	return fmt.Sprintf("exit %d", *code)
}
