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
	// Reading the transcript needs the finalisation that follows the exit.
	session.WaitFinalized(contextWithTimeout(t, 10*time.Second))
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
	session.WaitFinalized(contextWithTimeout(t, 15*time.Second))
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
	// Waiting for the marker text is not enough: the shell echoes the command
	// being typed, and that echo contains the marker, so a slow machine matches
	// it before the escape sequence has run. The mode itself is the signal.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !session.Snapshot().AltScreen {
		time.Sleep(20 * time.Millisecond)
	}
	if !session.Snapshot().AltScreen {
		t.Fatalf("snapshot should report the alternate screen is active; screen:\n%s",
			session.Snapshot().Text())
	}
	waitForScreen(t, session, "ALTSCREEN-ONLY", 5*time.Second)
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
	session.WaitFinalized(contextWithTimeout(t, 10*time.Second))
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
	session.WaitFinalized(contextWithTimeout(t, 10*time.Second))

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
	session.WaitFinalized(contextWithTimeout(t, 10*time.Second))
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

// A session that has not produced anything yet must not be reported as
// settled. A shell still starting up looks exactly like one that has finished
// working, and calling that "settled" hands the agent a blank screen with a
// claim that the command is done. This showed up on a loaded CI runner where
// the shell took longer to start than the quiet window.
func TestWaitSettledWaitsForTheFirstOutput(t *testing.T) {
	session := newTestSession(t, Options{
		Argv: []string{"sh", "-c", "sleep 1.2; printf 'late-output\\r\\n'; sleep 30"},
	})

	// The quiet window is far shorter than the delay before the first byte, so
	// a naive quiet check would settle immediately on an empty screen.
	result := session.WaitSettled(context.Background(), 10*time.Second, 150*time.Millisecond)
	if !result.Settled {
		t.Fatalf("the wait should settle once output arrives and stops, got %+v", result)
	}
	if result.Waited < time.Second {
		t.Errorf("the wait returned after %v, before the command produced anything", result.Waited)
	}
	if text := session.Snapshot().Text(); !strings.Contains(text, "late-output") {
		t.Errorf("a settled screen should hold the output that settled it, got %q", text)
	}
	_ = session.Kill("KILL", "test")
}

// A session with a drawn screen that is simply idle must still settle at once,
// or every read of a waiting prompt would burn its whole budget.
func TestWaitSettledReturnsImmediatelyOnAnIdleScreen(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh"}})
	if err := session.Write([]byte("PS1='> '\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "> ", 5*time.Second)

	start := time.Now()
	result := session.WaitSettled(context.Background(), 10*time.Second, 200*time.Millisecond)
	elapsed := time.Since(start)

	if !result.Settled {
		t.Error("an idle session that has already drawn should report settled")
	}
	if elapsed > 2*time.Second {
		t.Errorf("an idle screen took %v to settle; it should return promptly", elapsed)
	}
}

// A command that genuinely produces nothing cannot be called settled, because
// there is no evidence it did anything. Burning the budget and reporting
// settled:false is the honest answer.
func TestSilentCommandIsNotReportedAsSettled(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh", "-c", "sleep 30"}})

	result := session.WaitSettled(context.Background(), 700*time.Millisecond, 150*time.Millisecond)
	if result.Settled {
		t.Error("a session that has produced nothing must not report settled")
	}
	_ = session.Kill("KILL", "test")
}

// A signalled session must be reported as stopped the moment its process is
// gone, not after the log finalisation that follows. Conflating the two made a
// TERM that had already worked look like it had been ignored, so every kill
// paid the escalation timeout and reported using KILL when it had not.
func TestExitIsVisibleBeforeFinalisation(t *testing.T) {
	session := newTestSession(t, Options{
		// Enough output that finalising the transcript takes real time.
		Argv: []string{"sh", "-c", "i=1; while [ $i -le 4000 ]; do echo line-$i; i=$((i+1)); done; exit 0"},
	})

	if !session.WaitExit(contextWithTimeout(t, 15*time.Second)) {
		t.Fatal("session did not exit")
	}
	// The process is gone, so liveness must already say so.
	if session.Running() {
		t.Error("Running() should be false as soon as the process has exited")
	}
	if _, exited := session.ExitCode(); !exited {
		t.Error("the exit code should be available as soon as the process has exited")
	}

	// And finalisation still completes, so the log is whole afterwards.
	if !session.WaitFinalized(contextWithTimeout(t, 15*time.Second)) {
		t.Fatal("session was never finalised")
	}
	session.Flush()
	slice, err := Tail(session.TranscriptPath(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(slice.Lines, "\n"), "line-4000") {
		t.Errorf("the finalised transcript should hold the last line, got %q", slice.Lines)
	}
}

// A session started without a command runs a shell, and which shell it is
// decides the syntax of every command the caller then writes. On Windows that
// is not guessable, so it is recorded rather than assumed.
func TestSessionRecordsWhichShellItStarted(t *testing.T) {
	session := newTestSession(t, Options{})
	metadata := session.Metadata()

	if !metadata.Shell {
		t.Error("a session with no command should be flagged as running a shell")
	}
	if metadata.ShellID == "" || metadata.ShellPath == "" || metadata.ShellName == "" {
		t.Errorf("the shell should be recorded, got %+v", metadata)
	}
	if len(metadata.Command) == 0 || metadata.Command[0] != metadata.ShellPath {
		t.Errorf("the command should be the shell itself, got %v", metadata.Command)
	}
}

func TestExplicitShellSelection(t *testing.T) {
	available := ShellIDs()
	if len(available) == 0 {
		t.Skip("no shells detected")
	}
	session := newTestSession(t, Options{Shell: available[0]})
	if got := session.Metadata().ShellID; got != available[0] {
		t.Errorf("shell: got %q, want %q", got, available[0])
	}

	// An unavailable shell fails before anything starts, naming what is here.
	_, err := New(Options{
		ID: "t-noshell", Directory: t.TempDir(), Cols: 80, Rows: 24,
		ScrollbackLines: 20_000, RawLogMaxBytes: 1 << 20, TranscriptMaxLines: 1_000,
		Shell: "definitely-not-a-shell",
	})
	if err == nil {
		t.Fatal("an unknown shell should be rejected")
	}
	if !strings.Contains(err.Error(), "installed shells are") {
		t.Errorf("the error should list what is available, got %v", err)
	}
}

// Interrupting must never take down anything but the session it targets.
//
// A console control event on Windows is delivered to every process attached to
// the console, so raising it from the daemon put the daemon on the receiving
// end of its own signal: it took os.Interrupt, cancelled its root context, and
// destroyed every session it owned. The event is now raised from a helper
// process for that reason. This test guards the property on every platform:
// interrupting one session leaves the others alone.
func TestInterruptLeavesOtherSessionsAlone(t *testing.T) {
	first := newTestSession(t, Options{ID: "t-int001", Directory: t.TempDir(), Argv: []string{"sh"}})
	second := newTestSession(t, Options{ID: "t-int002", Directory: t.TempDir(), Argv: []string{"sh"}})

	for _, s := range []*Session{first, second} {
		if err := s.Write([]byte("PS1='> '\n")); err != nil {
			t.Fatal(err)
		}
		waitForScreen(t, s, "> ", 5*time.Second)
	}

	if err := first.Kill("INT", "test"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	if !second.Running() {
		t.Error("interrupting one session must not stop another")
	}
	// The untouched session must still work.
	if err := second.Write([]byte("echo untouched\n")); err != nil {
		t.Fatalf("the other session should still accept input: %v", err)
	}
	waitForScreen(t, second, "untouched", 5*time.Second)

	// And the interrupted one survives too: an interrupt is not a kill.
	if !first.Running() {
		t.Error("an interrupt should leave its own session usable")
	}
}

// Waiting for text is the only completion signal that does not depend on
// guessing from output timing, so it has to be exact about what counts.
func TestWaitUntilWaitsForNewOutputNotTheEcho(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh"}})
	if err := session.Write([]byte("PS1='> '\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "> ", 5*time.Second)

	// The command contains the word being waited for. A terminal echoes what is
	// typed, so a naive search matches that echo and reports a command finished
	// before it has begun.
	if err := session.Write([]byte("sleep 3; echo ARRIVED\n")); err != nil {
		t.Fatal(err)
	}

	result := session.WaitUntil(context.Background(), 20*time.Second, 250*time.Millisecond,
		WaitTarget{Text: "ARRIVED", Echo: "sleep 3; echo ARRIVED"})
	if !result.Matched {
		t.Fatalf("the text should have been matched, got %+v", result)
	}
	if result.Waited < 2*time.Second {
		t.Errorf("matched after %v, which is the echo rather than the output", result.Waited)
	}
}

// A silent command is exactly the case output-watching cannot handle.
func TestWaitUntilHandlesASilentCommand(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh"}})
	if err := session.Write([]byte("PS1='> '\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "> ", 5*time.Second)

	// Nothing is printed for two seconds, so settling on quiet would return
	// almost immediately and call it finished.
	quick := session.WaitSettled(context.Background(), 10*time.Second, 250*time.Millisecond)
	if !quick.Settled {
		t.Skip("the shell was not idle; the comparison would not be meaningful")
	}

	if err := session.Write([]byte("sleep 2; echo DONE-NOW\n")); err != nil {
		t.Fatal(err)
	}
	result := session.WaitUntil(context.Background(), 20*time.Second, 250*time.Millisecond,
		WaitTarget{Text: "DONE-NOW", Echo: "sleep 2; echo DONE-NOW"})
	if !result.Matched || result.Waited < time.Second {
		t.Errorf("a silent command should be waited out, got %+v", result)
	}
}

// A command that finishes immediately is the common case, and the one an
// echo-suppressing wait is most likely to miss: its entire output arrives in
// the same burst as the echo of the command line. Excluding output by when it
// arrived rather than by what it is made every fast command unwaitable.
func TestWaitUntilMatchesOutputInTheFirstBurst(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh"}})
	if err := session.Write([]byte("PS1='> '\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "> ", 5*time.Second)

	// Nothing in this command line contains the target, so the first thing on
	// the screen carrying it is the result.
	if err := session.Write([]byte("printf 'BULK%s\\n' -DONE\n")); err != nil {
		t.Fatal(err)
	}
	result := session.WaitUntil(context.Background(), 10*time.Second, 250*time.Millisecond,
		WaitTarget{Text: "BULK-DONE", Echo: "printf 'BULK%s\\n' -DONE"})
	if !result.Matched {
		t.Fatalf("output printed immediately must still be matched, got %+v", result)
	}
	if result.Waited > 3*time.Second {
		t.Errorf("a command that prints at once should match at once, waited %v", result.Waited)
	}
}

// A wait with no input of its own is asking whether something is on the screen,
// so text that is already there answers it.
func TestWaitUntilMatchesTextAlreadyOnScreen(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh"}})
	if err := session.Write([]byte("PS1='> '\necho CEILING-TEST\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "CEILING-TEST", 5*time.Second)

	result := session.WaitUntil(context.Background(), 5*time.Second, 250*time.Millisecond,
		WaitTarget{Text: "CEILING-TEST"})
	if !result.Matched {
		t.Fatalf("text already on the screen must match, got %+v", result)
	}
	if result.Waited > time.Second {
		t.Errorf("it was already there; waiting %v for it is wrong", result.Waited)
	}
}

// The same text is not a result when the caller has typed something new and is
// waiting for what that produces.
func TestWaitUntilIgnoresTheBaselineAfterInput(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh"}})
	if err := session.Write([]byte("PS1='> '\necho REPEATED\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "REPEATED", 5*time.Second)

	baseline := session.CountOnScreen("REPEATED")
	if baseline == 0 {
		t.Fatal("the baseline should have counted the text already on screen")
	}
	if err := session.Write([]byte("sleep 30\n")); err != nil {
		t.Fatal(err)
	}
	result := session.WaitUntil(context.Background(), time.Second, 250*time.Millisecond,
		WaitTarget{Text: "REPEATED", Echo: "sleep 30", Baseline: baseline})
	if result.Matched {
		t.Error("text that was already on the screen is not the result of new input")
	}
	_ = session.Kill("KILL", "test")
}

// Text that never arrives must be reported as not arriving.
func TestWaitUntilReportsAMiss(t *testing.T) {
	session := newTestSession(t, Options{Argv: []string{"sh", "-c", "sleep 30"}})

	result := session.WaitUntil(context.Background(), 1500*time.Millisecond, 250*time.Millisecond,
		WaitTarget{Text: "NEVER"})
	if result.Matched {
		t.Error("text that never appeared must not be reported as matched")
	}
	if result.Settled {
		t.Error("a wait that timed out has not settled")
	}
	_ = session.Kill("KILL", "test")
}

// An interrupt has to reach whatever owns the terminal right now, not the
// program this session started. When that program is a multiplexer or a
// remote client -- tmux, ssh, wsl -- the command being interrupted is running
// on the far side of it, and anything that signals locally hits the client
// instead and tears the whole nested session down.
func TestInterruptReachesACommandInsideANestedTerminal(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is needed to nest a terminal inside the session")
	}
	socket := "itm-test-" + t.Name()
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	// A bare shell keeps this test about nesting rather than about whatever the
	// developer's profile prints on startup.
	session := newTestSession(t, Options{Argv: []string{"bash", "--noprofile", "--norc"}})
	if err := session.Write([]byte("PS1='> '\n")); err != nil {
		t.Fatal(err)
	}
	waitForScreen(t, session, "> ", 10*time.Second)

	// tmux runs the inner shell on a pty of its own and holds this one in raw
	// mode, so the interrupt character is data to tmux and a signal only on
	// the far side.
	if err := session.Write([]byte("TERM=xterm-256color tmux -L " + socket + " new-session\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !session.Snapshot().AltScreen && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !session.Snapshot().AltScreen {
		t.Skip("tmux did not take the screen; the nesting under test never happened")
	}

	// The quoting matters: the terminal echoes the command as it is typed, so a
	// follow-up marker that survives quoting would be found on screen whether or
	// not it ever ran. bash prints NOT-REACHED only if it reaches the echo.
	if err := session.Write([]byte("sleep 300; echo NOT''-REACHED\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)

	if err := session.Kill("INT", "test"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	// The inner command must stop while everything around it survives: the
	// multiplexer, and this session with it.
	// Quoted for the same reason as above: while the command is still running
	// the line discipline echoes anything typed, so an unquoted marker would be
	// on screen whether the inner shell ever got back to a prompt or not.
	if err := session.Write([]byte("echo SURVIVED''-NESTED\n")); err != nil {
		t.Fatalf("the session should still be usable after an interrupt: %v", err)
	}
	text := waitForScreen(t, session, "SURVIVED-NESTED", 10*time.Second)
	if strings.Contains(text, "NOT-REACHED") {
		t.Error("the interrupted command ran its follow-up, so it was never interrupted")
	}
	if !session.Running() {
		t.Error("interrupting a command inside the nested terminal ended the session")
	}
}
