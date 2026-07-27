// Package vterm wraps a VT emulator behind a narrow interface and owns the
// conversion from terminal cells to the plain text an agent reads.
//
// The interface exists so the concrete emulator stays swappable: everything
// above this package works in terms of Snapshot and evicted lines, never in
// terms of cells, styles, or escape sequences.
package vterm

import "strings"

// Snapshot is the visible screen at one instant, already converted to text.
type Snapshot struct {
	// Lines are the visible rows with trailing whitespace removed and trailing
	// blank rows dropped. Interior blank rows are preserved because in a
	// full-screen program they carry layout.
	Lines []string
	// Cursor is the one-based [row, column] position on the visible screen.
	Cursor [2]int
	// Cols and Rows are the terminal dimensions the snapshot was taken at.
	Cols, Rows int
	// AltScreen reports whether a full-screen program owns the alternate buffer.
	AltScreen bool
	// Title is the last title the program set through OSC 0/2, if any.
	Title string
	// BlankLinesTrimmed counts blank rows removed from the bottom, so an agent
	// can tell an empty screen from a cropped one.
	BlankLinesTrimmed int
}

// Text joins the snapshot lines for rendering.
func (s Snapshot) Text() string { return strings.Join(s.Lines, "\n") }

// Modes carries the input-affecting terminal modes that key encoding depends
// on. Reading them at send time is what makes arrow keys work correctly inside
// programs like vim and less rather than only at a shell prompt.
type Modes struct {
	// ApplicationCursor is DECCKM. When set, arrows and Home/End use SS3
	// rather than CSI encoding.
	ApplicationCursor bool
	// ApplicationKeypad is DECKPAM.
	ApplicationKeypad bool
	// BracketedPaste is DEC mode 2004. When set, multi-line input is wrapped
	// in paste markers so an editor receives it as a paste.
	BracketedPaste bool
}

// Terminal is the emulator surface the rest of the application depends on.
// Implementations must be safe for concurrent use: one goroutine writes PTY
// output while others take snapshots.
type Terminal interface {
	// Write feeds raw PTY output to the emulator.
	Write(p []byte) (int, error)
	// Read returns bytes the emulator wants sent back to the program: replies
	// to device-status and device-attribute queries, and similar handshakes.
	//
	// A caller MUST drain this continuously and forward it to the PTY.
	// Programs like vim and tmux query the terminal during startup and wait
	// for the answer, so an undrained emulator both hangs the program and
	// blocks the emulator itself once its internal pipe fills. Read must not
	// hold any lock that Write or Snapshot needs.
	Read(p []byte) (int, error)
	// Resize changes the screen dimensions.
	Resize(cols, rows int)
	// Size reports the current dimensions.
	Size() (cols, rows int)
	// Snapshot converts the visible screen to text atomically.
	Snapshot() Snapshot
	// TakeEvictedLines returns the lines that have scrolled off the top since
	// the previous call, oldest first, and forgets them. It is the transcript
	// source, so exactly one caller may use it.
	TakeEvictedLines() []string
	// Modes reports the input-affecting modes currently set by the program.
	Modes() Modes
	// AltScreen reports whether the alternate buffer is active.
	AltScreen() bool
	// Title reports the last title set through OSC 0/2.
	Title() string
	// ScrollbackLines reports how many lines are retained above the screen.
	ScrollbackLines() int
	// ScrollbackText returns up to n lines ending at offset lines above the
	// live screen, oldest first. It backs the application's scrollback view.
	ScrollbackText(offset, n int) []string
	// Render returns the screen with styling as ANSI escape sequences, for the
	// human application. Agent-facing output never uses this.
	Render() string
	// Close releases emulator resources.
	Close() error
}
