package vterm

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func write(t *testing.T, terminal Terminal, text string) {
	t.Helper()
	if _, err := terminal.Write([]byte(text)); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestSnapshotExtractsPlainText(t *testing.T) {
	terminal := NewCharm(20, 5, 0)
	defer terminal.Close()

	// Styling must be interpreted and dropped, not passed through as escapes.
	write(t, terminal, "plain \x1b[1;31mbold red\x1b[0m\r\nsecond line\r\n")

	snapshot := terminal.Snapshot()
	if len(snapshot.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(snapshot.Lines), snapshot.Lines)
	}
	if snapshot.Lines[0] != "plain bold red" {
		t.Errorf("line 0: got %q, want %q", snapshot.Lines[0], "plain bold red")
	}
	if strings.Contains(snapshot.Text(), "\x1b") {
		t.Error("snapshot text must not contain escape sequences")
	}
	// Trailing blank rows are noise; the count preserves the information.
	if snapshot.BlankLinesTrimmed != 3 {
		t.Errorf("BlankLinesTrimmed: got %d, want 3", snapshot.BlankLinesTrimmed)
	}
}

// Interior blank lines carry layout in a full-screen program, so only trailing
// ones may be dropped.
func TestSnapshotKeepsInteriorBlankLines(t *testing.T) {
	terminal := NewCharm(20, 6, 0)
	defer terminal.Close()
	write(t, terminal, "top\r\n\r\n\r\nbottom\r\n")

	snapshot := terminal.Snapshot()
	if len(snapshot.Lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %q", len(snapshot.Lines), snapshot.Lines)
	}
	if snapshot.Lines[1] != "" || snapshot.Lines[2] != "" {
		t.Errorf("interior blank lines must be preserved, got %q", snapshot.Lines)
	}
}

// A wide grapheme occupies two cells; emitting its content twice would corrupt
// every line containing CJK text or an emoji.
func TestSnapshotHandlesWideCharacters(t *testing.T) {
	terminal := NewCharm(20, 3, 0)
	defer terminal.Close()
	write(t, terminal, "日本語 ok\r\n")

	snapshot := terminal.Snapshot()
	if snapshot.Lines[0] != "日本語 ok" {
		t.Errorf("got %q, want %q", snapshot.Lines[0], "日本語 ok")
	}
}

func TestCursorIsOneBased(t *testing.T) {
	terminal := NewCharm(20, 5, 0)
	defer terminal.Close()

	snapshot := terminal.Snapshot()
	if snapshot.Cursor != [2]int{1, 1} {
		t.Errorf("a fresh terminal should report cursor [1 1], got %v", snapshot.Cursor)
	}

	write(t, terminal, "abc")
	if snapshot = terminal.Snapshot(); snapshot.Cursor != [2]int{1, 4} {
		t.Errorf("after three characters: got %v, want [1 4]", snapshot.Cursor)
	}
}

func TestAlternateScreenAndTitleAreTracked(t *testing.T) {
	terminal := NewCharm(20, 5, 0)
	defer terminal.Close()

	if terminal.AltScreen() {
		t.Error("a fresh terminal is not on the alternate screen")
	}
	write(t, terminal, "\x1b]0;my title\x07")
	if terminal.Title() != "my title" {
		t.Errorf("title: got %q, want %q", terminal.Title(), "my title")
	}

	write(t, terminal, "\x1b[?1049h")
	if !terminal.AltScreen() {
		t.Error("mode 1049 should switch to the alternate screen")
	}
	write(t, terminal, "\x1b[?1049l")
	if terminal.AltScreen() {
		t.Error("resetting mode 1049 should leave the alternate screen")
	}
}

// Key encoding depends on these modes, so tracking them is what makes arrow
// keys work inside a program rather than only at a prompt.
func TestInputModesAreTracked(t *testing.T) {
	terminal := NewCharm(20, 5, 0)
	defer terminal.Close()

	if modes := terminal.Modes(); modes.ApplicationCursor || modes.BracketedPaste {
		t.Errorf("a fresh terminal should have no input modes set, got %+v", modes)
	}

	write(t, terminal, "\x1b[?1h")    // DECCKM on
	write(t, terminal, "\x1b[?2004h") // bracketed paste on
	modes := terminal.Modes()
	if !modes.ApplicationCursor {
		t.Error("DECCKM should be tracked")
	}
	if !modes.BracketedPaste {
		t.Error("bracketed paste should be tracked")
	}

	write(t, terminal, "\x1b[?1l")
	if terminal.Modes().ApplicationCursor {
		t.Error("resetting DECCKM should be tracked")
	}
}

// Evicted lines are the transcript's only source, so every line that scrolls
// off must be reported exactly once.
func TestEvictedLinesAreReportedOnce(t *testing.T) {
	terminal := NewCharm(20, 3, 0)
	defer terminal.Close()

	var written strings.Builder
	for index := 1; index <= 50; index++ {
		line := "line-" + itoa(index)
		written.WriteString(line + "\r\n")
	}
	write(t, terminal, written.String())

	evicted := terminal.TakeEvictedLines()
	if len(evicted) < 45 {
		t.Fatalf("expected roughly 47 evicted lines, got %d", len(evicted))
	}
	if evicted[0] != "line-1" {
		t.Errorf("the first evicted line should be the oldest, got %q", evicted[0])
	}

	// A second call must not repeat them, or the transcript would duplicate
	// every line.
	if again := terminal.TakeEvictedLines(); len(again) != 0 {
		t.Errorf("evicted lines should be reported once, got %d again", len(again))
	}

	write(t, terminal, "line-51\r\nline-52\r\n")
	if next := terminal.TakeEvictedLines(); len(next) == 0 {
		t.Error("new evictions should be reported")
	}
}

func TestResizeChangesSize(t *testing.T) {
	terminal := NewCharm(80, 24, 0)
	defer terminal.Close()

	terminal.Resize(100, 40)
	cols, rows := terminal.Size()
	if cols != 100 || rows != 40 {
		t.Errorf("size: got %dx%d, want 100x40", cols, rows)
	}
	if snapshot := terminal.Snapshot(); snapshot.Cols != 100 || snapshot.Rows != 40 {
		t.Errorf("snapshot size: got %dx%d", snapshot.Cols, snapshot.Rows)
	}
}

func TestScrollbackTextReadsAboveTheScreen(t *testing.T) {
	terminal := NewCharm(20, 3, 0)
	defer terminal.Close()

	for index := 1; index <= 30; index++ {
		write(t, terminal, "row-"+itoa(index)+"\r\n")
	}
	if terminal.ScrollbackLines() == 0 {
		t.Fatal("scrollback should retain lines above the screen")
	}
	lines := terminal.ScrollbackText(0, 5)
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
	// Offset zero means the lines immediately above the visible screen.
	if !strings.HasPrefix(lines[len(lines)-1], "row-") {
		t.Errorf("unexpected scrollback content: %q", lines)
	}
}

// A snapshot must never observe a half-applied write, and a query reply must
// never be able to wedge the emulator.
func TestConcurrentWritesAndSnapshotsAreSafe(t *testing.T) {
	terminal := NewCharm(40, 10, 0)
	defer terminal.Close()

	// Drain replies the way a session does; without this a device query would
	// block the writer forever.
	go func() {
		buffer := make([]byte, 256)
		for {
			if _, err := terminal.Read(buffer); err != nil {
				return
			}
		}
	}()

	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		for index := range 200 {
			write(t, terminal, "output-"+itoa(index)+"\r\n")
		}
	}()
	go func() {
		defer group.Done()
		for range 200 {
			// A device-attributes query produces a reply from inside Write.
			_, _ = terminal.Write([]byte("\x1b[c"))
		}
	}()
	go func() {
		defer group.Done()
		for range 200 {
			_ = terminal.Snapshot()
			_ = terminal.Modes()
			_ = terminal.AltScreen()
		}
	}()
	group.Wait()
}

// Close must release a reader blocked waiting for replies, or every finished
// session would leak a goroutine.
func TestCloseReleasesABlockedReader(t *testing.T) {
	terminal := NewCharm(20, 5, 0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 256)
		for {
			if _, err := terminal.Read(buffer); err != nil {
				return
			}
		}
	}()

	if err := terminal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-timeoutAfterSeconds(5):
		t.Fatal("Close did not release the blocked reader")
	}

	// Closing twice must be safe; shutdown paths can reach it more than once.
	if err := terminal.Close(); err != nil {
		t.Errorf("a second Close should be a no-op, got %v", err)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func timeoutAfterSeconds(seconds int) <-chan time.Time {
	return time.After(time.Duration(seconds) * time.Second)
}
