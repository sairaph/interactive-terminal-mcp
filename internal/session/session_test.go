package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestSession(t *testing.T, options Options) *Session {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("these tests drive a POSIX shell")
	}
	if options.ID == "" {
		options.ID = "t-test01"
	}
	if options.Directory == "" {
		options.Directory = t.TempDir()
	}
	if options.Cols == 0 {
		options.Cols = 80
	}
	if options.Rows == 0 {
		options.Rows = 24
	}
	options.ScrollbackLines = 20_000
	options.RawLogMaxBytes = 8 << 20
	options.TranscriptMaxLines = 10_000

	session, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(session.Close)
	return session
}

// waitForScreen polls until want appears on the visible screen, so tests never
// depend on a fixed sleep.
func waitForScreen(t *testing.T, session *Session, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var text string
	for time.Now().Before(deadline) {
		text = session.Snapshot().Text()
		if strings.Contains(text, want) {
			return text
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q on screen; got:\n%s", want, text)
	return text
}

func TestSessionRunsCommandAndCapturesExit(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh", "-c", "printf 'hello world\\r\\n'; exit 7"}})

	if !session.WaitExit(contextWithTimeout(t, 10*time.Second)) {
		t.Fatal("session did not exit in time")
	}
	code, exited := session.ExitCode()
	if !exited || code != 7 {
		t.Fatalf("exit: got (%d, %v), want (7, true)", code, exited)
	}
	if session.Running() {
		t.Error("session should report not running after exit")
	}

	// The visible screen is not part of the scrolled-off transcript, so the
	// reaper appends it. Without that, a short command leaves an empty log.
	session.Flush()
	slice, err := Tail(session.TranscriptPath(), 50)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if !strings.Contains(strings.Join(slice.Lines, "\n"), "hello world") {
		t.Errorf("transcript should contain the final screen, got %q", slice.Lines)
	}
}

func TestSessionRoundTripsInput(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh"}})
	// PS1 is set explicitly so the test does not depend on the developer's
	// prompt, which may contain colours or a hostname.
	if err := session.Write([]byte("PS1='> '\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitForScreen(t, session, "> ", 5*time.Second)

	if err := session.Write([]byte("echo round-trip-ok\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitForScreen(t, session, "round-trip-ok", 5*time.Second)
}

// A settle wait must return as soon as output goes quiet rather than sleeping
// out its whole budget; that is what makes one `wait` value correct for both
// fast and slow commands.
func TestWaitSettledReturnsEarlyOnQuiet(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh", "-c", "printf 'quick\\r\\n'; sleep 30"}})
	waitForScreen(t, session, "quick", 5*time.Second)

	start := time.Now()
	result := session.WaitSettled(context.Background(), 10*time.Second, 200*time.Millisecond)
	elapsed := time.Since(start)

	if !result.Settled {
		t.Error("output was quiet, so the wait should report settled")
	}
	if elapsed > 3*time.Second {
		t.Errorf("settle took %v; it should return shortly after output goes quiet", elapsed)
	}
	_ = session.Kill("KILL", "test")
}

// When output is still arriving as the budget expires, the result must say so.
// An agent that sees settled:false knows the screen may be mid-update.
func TestWaitSettledReportsUnsettledUnderContinuousOutput(t *testing.T) {
	session := newTestSession(t, Options{
		Argv: []string{"sh", "-c", "while :; do printf 'tick\\r\\n'; sleep 0.05; done"},
	})
	waitForScreen(t, session, "tick", 5*time.Second)

	result := session.WaitSettled(context.Background(), 700*time.Millisecond, 250*time.Millisecond)
	if result.Settled {
		t.Error("continuous output should not report settled")
	}
	_ = session.Kill("KILL", "test")
}

func TestWaitSettledReturnsOnExit(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh", "-c", "sleep 0.2; exit 3"}})
	result := session.WaitSettled(context.Background(), 10*time.Second, 5*time.Second)
	if !result.Exited {
		t.Error("wait should report the session exited")
	}
	if result.Waited > 5*time.Second {
		t.Errorf("wait should end at exit, took %v", result.Waited)
	}
}

// Output that scrolls off the top must reach the transcript; that is the only
// way it_tail reaches further back than the visible screen.
func TestTranscriptCapturesScrolledOffOutput(t *testing.T) {
	session := newTestSession(t, Options{
		Rows: 10,
		Argv: []string{"sh", "-c", "i=1; while [ $i -le 200 ]; do printf 'line-%d\\r\\n' $i; i=$((i+1)); done"},
	})
	if !session.WaitExit(contextWithTimeout(t, 15*time.Second)) {
		t.Fatal("session did not exit in time")
	}
	session.Flush()

	head, err := Head(session.TranscriptPath(), 5)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if len(head.Lines) == 0 || !strings.Contains(head.Lines[0], "line-1") {
		t.Errorf("head should start at the oldest output, got %q", head.Lines)
	}

	tail, err := Tail(session.TranscriptPath(), 5)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if !strings.Contains(strings.Join(tail.Lines, "\n"), "line-200") {
		t.Errorf("tail should reach the newest output, got %q", tail.Lines)
	}
	if head.Total < 200 {
		t.Errorf("transcript should hold every scrolled-off line, got %d", head.Total)
	}
}

// Alternate-screen output never scrolls, so it is correctly absent from the
// transcript. The contract documents this and it_tail compensates by appending
// the live screen; this test pins the behaviour so it cannot drift silently.
func TestAlternateScreenOutputIsNotInTranscript(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh"}})
	if err := session.Write([]byte("PS1='> '\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "> ", 5*time.Second)

	// Enter the alternate screen, draw, and stay there.
	if err := session.Write([]byte("printf '\\033[?1049h'; printf 'ALTSCREEN-ONLY\\r\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "ALTSCREEN-ONLY", 5*time.Second)

	if !session.Snapshot().AltScreen {
		t.Fatal("snapshot should report the alternate screen is active")
	}
	session.Flush()

	slice, err := Tail(session.TranscriptPath(), 500)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if strings.Contains(strings.Join(slice.Lines, "\n"), "ALTSCREEN-ONLY") {
		t.Error("alternate-screen output must not enter the transcript; it never scrolls")
	}
}

func TestSnapshotReportsCursorSizeAndTrimming(t *testing.T) {
	session := newTestSession(t, Options{Cols: 40, Rows: 12, Argv: []string{"sh", "-c", "printf 'abc\\r\\n'; sleep 30"}})
	waitForScreen(t, session, "abc", 5*time.Second)

	snapshot := session.Snapshot()
	if snapshot.Cols != 40 || snapshot.Rows != 12 {
		t.Errorf("size: got %dx%d, want 40x12", snapshot.Cols, snapshot.Rows)
	}
	if snapshot.Cursor[0] < 1 || snapshot.Cursor[1] < 1 {
		t.Errorf("cursor must be one-based, got %v", snapshot.Cursor)
	}
	if snapshot.BlankLinesTrimmed == 0 {
		t.Error("trailing blank rows should be trimmed and counted")
	}
	if len(snapshot.Lines) > 12 {
		t.Errorf("snapshot returned %d lines for a 12-row terminal", len(snapshot.Lines))
	}
	_ = session.Kill("KILL", "test")
}

func TestResizeMovesPTYAndEmulatorTogether(t *testing.T) {
	session := newTestSession(t, Options{Cols: 80, Rows: 24, Argv: []string{"sh"}})
	if err := session.Write([]byte("PS1='> '\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "> ", 5*time.Second)

	if err := session.Resize(100, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	cols, rows := session.Size()
	if cols != 100 || rows != 40 {
		t.Errorf("emulator size: got %dx%d, want 100x40", cols, rows)
	}

	// The child must see the new size too, or it will draw for the old one.
	if err := session.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "40 100", 5*time.Second)
}

func TestKillTerminatesAndRecordsCause(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh", "-c", "sleep 60"}})
	waitForScreen(t, session, "", 1*time.Second) // let it start

	if err := session.Kill("TERM", "it_kill"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !session.WaitExit(contextWithTimeout(t, 10*time.Second)) {
		t.Fatal("session did not exit after TERM")
	}
	metadata := session.Metadata()
	if metadata.KilledBy != "it_kill" {
		t.Errorf("KilledBy: got %q, want %q", metadata.KilledBy, "it_kill")
	}
	if metadata.ExitedAt == nil || metadata.ExitCode == nil {
		t.Error("metadata should record the exit time and code")
	}
}

// Writing to a dead session must be a clear typed error rather than a silent
// no-op, so the agent is told to create a new session instead.
func TestWriteToExitedSessionFails(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh", "-c", "exit 0"}})
	if !session.WaitExit(contextWithTimeout(t, 10*time.Second)) {
		t.Fatal("session did not exit")
	}
	if err := session.Write([]byte("echo hi\n")); err != ErrExited {
		t.Errorf("Write to exited session: got %v, want ErrExited", err)
	}
}

func TestMetadataIsReadableFromDisk(t *testing.T) {
	directory := t.TempDir()
	session := newTestSession(t, Options{Name: "named", Directory: directory, Argv: []string{"sh", "-c", "exit 0"}})
	if !session.WaitExit(contextWithTimeout(t, 10*time.Second)) {
		t.Fatal("session did not exit")
	}

	// A restarted daemon reconstructs exited sessions from this file alone.
	metadata, err := ReadMetadata(directory)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if metadata.Name != "named" {
		t.Errorf("name: got %q, want %q", metadata.Name, "named")
	}
	if metadata.Running() {
		t.Error("metadata should report the session is not running")
	}
	if _, err := os.Stat(filepath.Join(directory, "raw.log")); err != nil {
		t.Errorf("raw log should exist: %v", err)
	}
}

func TestEnvironmentIsMergedAndTerminalIsDescribed(t *testing.T) {
	session := newTestSession(t, Options{
		Env:  map[string]string{"IT_TEST_VAR": "custom-value"},
		Argv: []string{"sh", "-c", `printf '%s|%s\r\n' "$IT_TEST_VAR" "$TERM"`},
	})
	if !session.WaitExit(contextWithTimeout(t, 10*time.Second)) {
		t.Fatal("session did not exit")
	}
	session.Flush()
	slice, _ := Tail(session.TranscriptPath(), 50)
	text := strings.Join(slice.Lines, "\n")
	if !strings.Contains(text, "custom-value|xterm-256color") {
		t.Errorf("environment should merge custom vars over a described terminal, got %q", text)
	}
}

func TestUnknownCommandFailsBeforeStarting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX PATH semantics")
	}
	_, err := New(Options{
		ID: "t-nope01", Directory: t.TempDir(), Cols: 80, Rows: 24,
		ScrollbackLines: 20_000, RawLogMaxBytes: 1 << 20, TranscriptMaxLines: 1_000,
		Argv: []string{"definitely-not-a-real-program-xyz"},
	})
	if err == nil {
		t.Fatal("starting a nonexistent program should fail")
	}
	if !strings.Contains(err.Error(), "was not found on PATH") {
		t.Errorf("error should explain the program was not found, got %v", err)
	}
}

func contextWithTimeout(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

// A program that queries the terminal must get an answer.
//
// The emulator generates replies to device-attribute and cursor-position
// queries from inside its write path and buffers them in a pipe that blocks
// once full. If nothing drains that pipe, the emulator deadlocks while holding
// its lock, every snapshot in the daemon hangs, and the querying program waits
// forever for a reply. vim and tmux both query on startup, so this is the
// difference between full-screen programs working and wedging the daemon.
func TestTerminalQueriesAreAnswered(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh"}})
	if err := session.Write([]byte("PS1='> '\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "> ", 5*time.Second)

	// Ask for the primary device attributes and read the reply back. A shell
	// echoes the response onto the screen, so seeing it proves the round trip
	// completed rather than merely not crashing.
	if err := session.Write([]byte("printf '\\033[c'; sleep 0.4; echo QUERY-DONE\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "QUERY-DONE", 5*time.Second)

	// Snapshots must still work: a stalled emulator would hang here forever.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			_ = session.Snapshot()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshots blocked; the emulator's reply pipe is not being drained")
	}
}

// A full-screen editor must start, accept keystrokes, and exit cleanly.
// This is the end-to-end case the whole project exists for.
func TestFullScreenEditorRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("vim"); err != nil {
		t.Skip("vim is not installed")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "edited.txt")

	session := newTestSession(t, Options{
		Cols: 60, Rows: 12,
		Argv: []string{"vim", "-u", "NONE", "-N", target},
	})

	// vim owns the alternate screen once it has drawn.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !session.Snapshot().AltScreen {
		time.Sleep(50 * time.Millisecond)
	}
	if !session.Snapshot().AltScreen {
		t.Fatal("vim did not take the alternate screen; it is probably blocked on a terminal query")
	}

	write := func(text string) {
		t.Helper()
		if err := session.Write([]byte(text)); err != nil {
			t.Fatalf("write %q: %v", text, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	write("i")
	write("hello from vim")
	write("\x1b")
	write(":wq\r")

	if !session.WaitExit(contextWithTimeout(t, 15*time.Second)) {
		t.Fatal("vim did not exit after :wq")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("vim did not write the file: %v", err)
	}
	if !strings.Contains(string(content), "hello from vim") {
		t.Errorf("file contents: got %q, want it to contain %q", content, "hello from vim")
	}
}
