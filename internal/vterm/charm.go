package vterm

import (
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// MinScrollbackLines is the floor applied to the emulator's scrollback ring.
//
// The transcript is built from lines evicted off the top of the screen, and
// eviction is detected by watching the ring grow. Once the ring saturates it
// silently drops its oldest line and the growth signal is lost, so the ring
// must be able to absorb everything one Write can push before the caller
// drains it. Callers write at most MaxWriteChunk bytes per Write, whose
// absolute worst case is one line per byte, so a ring at least that large plus
// a margin can never lose a line between drains.
const MinScrollbackLines = 20_000

// MaxWriteChunk is the largest slice a caller may hand to Write. It bounds the
// number of lines a single Write can evict; see MinScrollbackLines.
const MaxWriteChunk = 16 << 10

// Charm adapts github.com/charmbracelet/x/vt to the Terminal interface.
//
// Every emulator access is serialized by one mutex. The emulator's own
// SafeEmulator is deliberately not used: mode tracking, title, and evicted
// lines have to move atomically with the writes that cause them, and that
// needs a lock this type owns.
type Charm struct {
	mu   sync.Mutex
	term *vt.Emulator

	cols, rows int

	// closed stops Read once the emulator is finished with.
	closed atomic.Bool

	// replies carries what the emulator wants sent back to the program. One
	// goroutine owned by this type is the only caller of the emulator's Read,
	// which is what lets Close reliably wake it; see Close.
	replies    chan []byte
	pending    []byte
	readerDone chan struct{}

	// consumed counts scrollback lines already handed to TakeEvictedLines.
	consumed int

	altScreen bool
	title     string
	modes     Modes
}

// NewCharm builds an emulator of the given size.
func NewCharm(cols, rows, scrollbackLines int) *Charm {
	if cols < 1 {
		cols = 80
	}
	if rows < 1 {
		rows = 24
	}
	if scrollbackLines < MinScrollbackLines {
		scrollbackLines = MinScrollbackLines
	}

	adapter := &Charm{
		cols: cols, rows: rows,
		// Replies are tiny and infrequent; the buffer only has to absorb a
		// burst while the consumer is busy elsewhere.
		replies:    make(chan []byte, 64),
		readerDone: make(chan struct{}),
	}
	adapter.term = vt.NewEmulator(cols, rows)
	adapter.term.SetScrollbackSize(scrollbackLines)

	// Modes, title, and alternate-screen state are only reachable through
	// callbacks; the emulator exposes no getters for them. They fire during
	// Write, which already holds the lock, so they must not re-lock.
	adapter.term.SetCallbacks(vt.Callbacks{
		Title:     func(title string) { adapter.title = title },
		AltScreen: func(active bool) { adapter.altScreen = active },
		EnableMode: func(mode ansi.Mode) {
			adapter.setMode(mode, true)
		},
		DisableMode: func(mode ansi.Mode) {
			adapter.setMode(mode, false)
		},
	})
	go adapter.pumpReplies()
	return adapter
}

// pumpReplies is the only caller of the emulator's Read.
//
// Centralising it here has two purposes. It guarantees a consumer exists for
// the emulator's unbuffered reply pipe, so a device query raised from inside
// Write can never block the writer while it holds the lock. And because this
// goroutine lives for as long as the Charm does, Close can always wake it with
// a single query rather than reaching for the emulator's own Close, which
// mutates an unsynchronised flag and races with a reader.
func (c *Charm) pumpReplies() {
	defer close(c.readerDone)
	defer close(c.replies)

	buffer := make([]byte, 4<<10)
	for {
		n, err := c.term.Read(buffer)
		if c.closed.Load() {
			// The bytes just read are the wake-up query's own reply; the
			// program is gone, so there is nobody left to send them to.
			return
		}
		if n > 0 {
			reply := make([]byte, n)
			copy(reply, buffer[:n])
			select {
			case c.replies <- reply:
			default:
				// A consumer that has stopped reading must not be allowed to
				// block the emulator. Dropping a reply degrades one program's
				// handshake; blocking would stall every write and snapshot.
			}
		}
		if err != nil {
			return
		}
	}
}

// setMode records the input-affecting DEC modes. Callers hold the lock.
func (c *Charm) setMode(mode ansi.Mode, enabled bool) {
	decMode, ok := mode.(ansi.DECMode)
	if !ok {
		return
	}
	switch decMode {
	case ansi.CursorKeysMode:
		c.modes.ApplicationCursor = enabled
	case ansi.NumericKeypadMode:
		c.modes.ApplicationKeypad = enabled
	case ansi.BracketedPasteMode:
		c.modes.BracketedPaste = enabled
	}
}

// Write feeds PTY output to the emulator. Slices longer than MaxWriteChunk are
// split so the scrollback invariant holds; callers should already be reading in
// bounded chunks, but a split here makes the guarantee unconditional.
func (c *Charm) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > MaxWriteChunk {
			chunk = chunk[:MaxWriteChunk]
		}
		c.mu.Lock()
		n, err := c.term.Write(chunk)
		c.mu.Unlock()
		written += n
		if err != nil {
			return written, err
		}
		if n < len(chunk) {
			return written, nil
		}
		p = p[len(chunk):]
	}
	return written, nil
}

// Read returns bytes the emulator wants sent back to the program, blocking
// until there are some. It reports io.EOF once the terminal is closed.
//
// It reads from an internal queue rather than the emulator, so it holds no
// lock that Write or Snapshot needs and cannot stall them.
func (c *Charm) Read(p []byte) (int, error) {
	if len(c.pending) == 0 {
		reply, ok := <-c.replies
		if !ok {
			return 0, io.EOF
		}
		c.pending = reply
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// Resize changes the screen dimensions. The emulator reflows its buffers and
// clamps the cursor; delivering SIGWINCH to the child is the caller's job.
func (c *Charm) Resize(cols, rows int) {
	if cols < 1 || rows < 1 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.term.Resize(cols, rows)
	c.cols, c.rows = cols, rows
}

// Size reports the current dimensions.
func (c *Charm) Size() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cols, c.rows
}

// Snapshot converts the visible screen to text under one lock, so the cells,
// the cursor, and the mode flags all describe the same instant.
func (c *Charm) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	lines := make([]string, 0, c.rows)
	for y := range c.rows {
		lines = append(lines, c.lineAt(y))
	}

	// Drop trailing blank rows but keep interior ones: in a full-screen
	// program an interior blank row is layout, while trailing rows are almost
	// always just the unused bottom of the screen.
	trimmed := 0
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
		trimmed++
	}

	position := c.term.CursorPosition()
	return Snapshot{
		Lines:             lines,
		Cursor:            [2]int{position.Y + 1, position.X + 1},
		Cols:              c.cols,
		Rows:              c.rows,
		AltScreen:         c.altScreen,
		Title:             c.title,
		BlankLinesTrimmed: trimmed,
	}
}

// lineAt renders one visible row as text. Callers hold the lock.
func (c *Charm) lineAt(y int) string {
	var out strings.Builder
	for x := 0; x < c.cols; {
		cell := c.term.CellAt(x, y)
		if cell == nil {
			x++
			continue
		}
		width := cell.Width
		if width < 1 {
			// A zero-width cell is the continuation of the wide grapheme to
			// its left; its content was already emitted.
			x++
			continue
		}
		content := cell.Content
		if content == "" {
			content = " "
		}
		out.WriteString(content)
		x += width
	}
	return strings.TrimRight(out.String(), " \t")
}

// TakeEvictedLines returns lines that scrolled off the top since the previous
// call and forgets them. Exactly one caller may use it, because it advances
// shared state.
func (c *Charm) TakeEvictedLines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	scrollback := c.term.Scrollback()
	if scrollback == nil {
		return nil
	}
	length := scrollback.Len()
	if length < c.consumed {
		// The ring was cleared (a reset, or ClearScrollback). Everything still
		// in it is new; resynchronize rather than reporting a negative count.
		c.consumed = 0
	}
	if length == c.consumed {
		return nil
	}
	lines := make([]string, 0, length-c.consumed)
	for index := c.consumed; index < length; index++ {
		lines = append(lines, lineText(scrollback.Line(index)))
	}
	c.consumed = length
	return lines
}

// Modes reports the input-affecting modes currently set.
func (c *Charm) Modes() Modes {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.modes
}

// AltScreen reports whether the alternate buffer is active.
func (c *Charm) AltScreen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.altScreen
}

// Title reports the last title set through OSC 0/2.
func (c *Charm) Title() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.title
}

// ScrollbackLines reports how many lines are retained above the screen.
func (c *Charm) ScrollbackLines() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if scrollback := c.term.Scrollback(); scrollback != nil {
		return scrollback.Len()
	}
	return 0
}

// ScrollbackText returns up to n lines ending offset lines above the live
// screen, oldest first. offset 0 means the lines immediately above the screen.
func (c *Charm) ScrollbackText(offset, n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	scrollback := c.term.Scrollback()
	if scrollback == nil || n <= 0 {
		return nil
	}
	length := scrollback.Len()
	end := length - offset
	if end > length {
		end = length
	}
	start := end - n
	if start < 0 {
		start = 0
	}
	if end <= start {
		return nil
	}
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		lines = append(lines, lineText(scrollback.Line(index)))
	}
	return lines
}

// Render returns the screen with styling, for the human application only.
func (c *Charm) Render() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.term.Render()
}

// Close stops the emulator and releases the goroutine reading its replies.
//
// The emulator's own Close sets an internal flag without synchronisation,
// which the race detector correctly flags against a reader sitting in Read, so
// that path is deliberately not used. Instead the closed flag is raised here
// and a device-attributes query is pushed through the emulator, which answers
// it. That answer wakes pumpReplies, which observes the flag and exits. Because
// pumpReplies exists for the whole life of the Charm, the query always has a
// consumer and this never blocks.
func (c *Charm) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.mu.Lock()
	// The error is ignored deliberately: this write exists only for its side
	// effect of producing a reply that wakes the reader.
	_, _ = c.term.Write([]byte("\x1b[c"))
	c.mu.Unlock()

	// The wait is bounded so a wedged emulator can never hold up a session's
	// shutdown; the goroutine would then be the only thing left leaking.
	select {
	case <-c.readerDone:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// lineText renders a scrollback line as text, applying the same wide-grapheme
// and trailing-whitespace rules as the visible screen.
func lineText(line uv.Line) string {
	var out strings.Builder
	for x := 0; x < len(line); {
		cell := &line[x]
		width := cell.Width
		if width < 1 {
			x++
			continue
		}
		content := cell.Content
		if content == "" {
			content = " "
		}
		out.WriteString(content)
		x += width
	}
	return strings.TrimRight(out.String(), " \t")
}

var _ Terminal = (*Charm)(nil)
