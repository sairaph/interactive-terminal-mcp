package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

// newTestDaemon starts a daemon on a socket short enough for the kernel's
// sun_path limit, which t.TempDir() alone does not guarantee.
func newTestDaemon(t *testing.T) (*Daemon, *ipc.Client, config.Paths) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("these tests drive a POSIX shell")
	}

	root := t.TempDir()
	socket, err := os.MkdirTemp("", "itd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socket) })

	paths := config.Paths{
		Root:        root,
		Config:      filepath.Join(root, "config.toml"),
		Sessions:    filepath.Join(root, "sessions"),
		Socket:      filepath.Join(socket, "d.sock"),
		Lock:        filepath.Join(root, "daemon.lock"),
		Diagnostics: filepath.Join(root, "diagnostics.log"),
	}
	settings := config.Default()
	settings.DaemonIdleShutdownSeconds = 0 // never idle out during a test
	settings, err = normalizeForTest(settings)
	if err != nil {
		t.Fatal(err)
	}

	server, err := Open(paths, settings, "test")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go server.Serve(ctx)
	t.Cleanup(func() {
		cancel()
		server.Close(true)
	})

	client, err := ipc.Connect(context.Background(), paths.Socket)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return server, client, paths
}

func normalizeForTest(settings config.Config) (config.Config, error) {
	// Load applies the derived duration fields; reproduce that for a config
	// built in memory.
	settings.SettleQuiet = time.Duration(settings.SettleQuietMS) * time.Millisecond
	settings.DefaultWait = time.Duration(settings.DefaultWaitSeconds) * time.Second
	settings.MaximumWait = time.Duration(settings.MaximumWaitSeconds) * time.Second
	settings.DaemonIdleShutdown = time.Duration(settings.DaemonIdleShutdownSeconds) * time.Second
	return settings, config.Validate(settings)
}

func call(t *testing.T, client *ipc.Client, op string, args any, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return client.Call(ctx, op, args, result)
}

func mustCall(t *testing.T, client *ipc.Client, op string, args any, result any) {
	t.Helper()
	if err := call(t, client, op, args, result); err != nil {
		t.Fatalf("%s: %v", op, err)
	}
}

func TestSessionLifecycleAcrossCalls(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	// A session created by one call must still be there for the next; that
	// persistence is the reason the daemon exists.
	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Name: "work", Argv: []string{"sh"}, WaitMS: 1500}, &created)
	if created.Session.ID == "" || !created.Session.Running {
		t.Fatalf("session was not created: %+v", created.Session)
	}

	var listed ipc.ListResult
	mustCall(t, client, ipc.OpSessionList, nil, &listed)
	if len(listed.Sessions) != 1 || listed.Active != created.Session.ID {
		t.Fatalf("list: got %d sessions, active %q", len(listed.Sessions), listed.Active)
	}

	// The name resolves as well as the id.
	var read ipc.Screen
	mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{Session: "work"}, &read)
	if read.Session.ID != created.Session.ID {
		t.Errorf("name lookup returned %q, want %q", read.Session.ID, created.Session.ID)
	}
}

func TestSendAndReadRoundTrip(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Argv: []string{"sh"}, WaitMS: 1500}, &created)

	mustCall(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Text: "PS1=''; echo daemon-round-trip", HasText: true, Enter: true, WaitMS: 4000,
	}, &ipc.Screen{})

	var screen ipc.Screen
	mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{WaitMS: 1000}, &screen)
	if !strings.Contains(strings.Join(screen.Lines, "\n"), "daemon-round-trip") {
		t.Errorf("screen did not show the command output:\n%s", strings.Join(screen.Lines, "\n"))
	}
}

// The active session is what every tool falls back to, so switching it must be
// observable immediately by a call that names nothing.
func TestActiveSessionSelection(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	var first, second ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Name: "one", Argv: []string{"sh"}, WaitMS: 800}, &first)
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Name: "two", Argv: []string{"sh"}, WaitMS: 800}, &second)

	var active ipc.ActiveResult
	mustCall(t, client, ipc.OpSessionAtive, ipc.ActiveArgs{}, &active)
	if active.Active == nil || active.Active.Session.ID != second.Session.ID {
		t.Fatalf("the newest session should be active, got %+v", active.Active)
	}

	mustCall(t, client, ipc.OpSessionAtive, ipc.ActiveArgs{Session: "one", Set: true}, &active)
	if active.Active == nil || active.Active.Session.ID != first.Session.ID {
		t.Fatalf("active should now be the first session, got %+v", active.Active)
	}

	var screen ipc.Screen
	mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{}, &screen)
	if screen.Session.ID != first.Session.ID {
		t.Errorf("a call with no session should use the active one, got %q", screen.Session.ID)
	}
}

// it_kill must never infer its target: ending the wrong terminal is not
// something an agent can undo.
func TestKillRequiresAnExplicitSession(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Argv: []string{"sh"}, WaitMS: 800}, &ipc.Screen{})

	err := call(t, client, ipc.OpSessionKill, ipc.KillArgs{}, &ipc.KillResult{})
	if err == nil {
		t.Fatal("kill without a session should fail")
	}
	typed, ok := err.(*ipc.Error)
	if !ok || typed.Code != ipc.CodeInvalidInput {
		t.Fatalf("expected invalid_input, got %v", err)
	}
	if !strings.Contains(typed.Hint, "never assumed") {
		t.Errorf("hint should explain why, got %q", typed.Hint)
	}
}

// A process that ignores TERM must still be ended, and the caller must be told
// that force was needed rather than being left believing it exited cleanly.
func TestKillEscalatesWhenTermIsIgnored(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{
		Name:   "stubborn",
		Argv:   []string{"sh", "-c", "trap '' TERM; while :; do sleep 0.2; done"},
		WaitMS: 1000,
	}, &created)

	var result ipc.KillResult
	mustCall(t, client, ipc.OpSessionKill, ipc.KillArgs{Session: "stubborn"}, &result)
	if !result.Escalated {
		t.Error("a process ignoring TERM should be escalated to KILL")
	}

	var listed ipc.ListResult
	mustCall(t, client, ipc.OpSessionList, nil, &listed)
	for _, info := range listed.Sessions {
		if info.ID == created.Session.ID && info.Running {
			t.Error("the session should not still be running after escalation")
		}
	}
}

// INT interrupts the running command without ending the session, which is the
// whole reason it is offered separately from TERM.
func TestInterruptLeavesTheSessionUsable(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Name: "busy", Argv: []string{"sh"}, WaitMS: 1500}, &created)

	mustCall(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Text: "PS1=''; sleep 30", HasText: true, Enter: true, WaitMS: 500,
	}, &ipc.Screen{})

	var result ipc.KillResult
	mustCall(t, client, ipc.OpSessionKill, ipc.KillArgs{Session: "busy", Signal: "INT"}, &result)
	if result.ExitCode != nil {
		t.Errorf("an interrupt should not end the session, got exit code %d", *result.ExitCode)
	}

	var screen ipc.Screen
	mustCall(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Text: "echo still-alive", HasText: true, Enter: true, WaitMS: 4000,
	}, &screen)
	if !strings.Contains(strings.Join(screen.Lines, "\n"), "still-alive") {
		t.Errorf("the session should still accept commands after an interrupt:\n%s",
			strings.Join(screen.Lines, "\n"))
	}
}

func TestWritingToAnEndedSessionIsTyped(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Name: "brief", Argv: []string{"sh", "-c", "exit 4"}, WaitMS: 3000}, &created)

	err := call(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Session: "brief", Text: "echo hi", HasText: true, Enter: true,
	}, &ipc.Screen{})
	if err == nil {
		t.Fatal("sending to an ended session should fail")
	}
	typed, ok := err.(*ipc.Error)
	if !ok || typed.Code != ipc.CodeSessionExited {
		t.Fatalf("expected session_exited, got %v", err)
	}
	if !strings.Contains(typed.Message, "4") {
		t.Errorf("the error should report the exit code, got %q", typed.Message)
	}
}

func TestNameConflictsAreRejected(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Name: "taken", Argv: []string{"sh"}, WaitMS: 800}, &ipc.Screen{})

	err := call(t, client, ipc.OpSessionNew, ipc.NewArgs{Name: "taken", Argv: []string{"sh"}, WaitMS: 800}, &ipc.Screen{})
	if err == nil {
		t.Fatal("a duplicate name should be rejected rather than silently reused")
	}
	typed, ok := err.(*ipc.Error)
	if !ok || typed.Code != ipc.CodeNameConflict {
		t.Fatalf("expected name_conflict, got %v", err)
	}
}

func TestInvalidNamesAreRejected(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	for _, name := range []string{"t-abc123", "Has Spaces", "UPPER", strings.Repeat("x", 65)} {
		err := call(t, client, ipc.OpSessionNew, ipc.NewArgs{Name: name, Argv: []string{"sh"}, WaitMS: 300}, &ipc.Screen{})
		if err == nil {
			t.Errorf("name %q should have been rejected", name)
		}
	}
}

func TestUnknownSessionIsActionable(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	err := call(t, client, ipc.OpSessionRead, ipc.ReadArgs{Session: "nope"}, &ipc.Screen{})
	if err == nil {
		t.Fatal("an unknown session should fail")
	}
	typed, ok := err.(*ipc.Error)
	if !ok || typed.Code != ipc.CodeSessionNotFound {
		t.Fatalf("expected session_not_found, got %v", err)
	}
	if !strings.Contains(typed.Hint, "it_list") {
		t.Errorf("hint should name a concrete next call, got %q", typed.Hint)
	}
}

func TestNoActiveSessionIsActionable(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	err := call(t, client, ipc.OpSessionRead, ipc.ReadArgs{}, &ipc.Screen{})
	if err == nil {
		t.Fatal("reading with no active session should fail")
	}
	typed, ok := err.(*ipc.Error)
	if !ok || typed.Code != ipc.CodeNoActiveSession {
		t.Fatalf("expected no_active_session, got %v", err)
	}
	if !strings.Contains(typed.Hint, "it_new") {
		t.Errorf("hint should suggest creating a session, got %q", typed.Hint)
	}
}

// Resizing must reach the child, not just the emulator, or a program will keep
// drawing for the old size.
func TestResizeThroughRead(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Argv: []string{"sh"}, Cols: 80, Rows: 24, WaitMS: 1500}, &ipc.Screen{})

	var screen ipc.Screen
	mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{Cols: 100, Rows: 40, WaitMS: 1000}, &screen)
	if screen.Session.Cols != 100 || screen.Session.Rows != 40 {
		t.Fatalf("size: got %dx%d, want 100x40", screen.Session.Cols, screen.Session.Rows)
	}

	mustCall(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Text: "PS1=''; stty size", HasText: true, Enter: true, WaitMS: 4000,
	}, &screen)
	if !strings.Contains(strings.Join(screen.Lines, "\n"), "40 100") {
		t.Errorf("the child should see the new size:\n%s", strings.Join(screen.Lines, "\n"))
	}
}

func TestLogsReachBeyondTheScreen(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{
		Name: "noisy", Rows: 10, Cols: 60,
		Argv:   []string{"sh", "-c", "i=1; while [ $i -le 300 ]; do echo line-$i; i=$((i+1)); done; sleep 30"},
		WaitMS: 5000,
	}, &ipc.Screen{})

	var tail ipc.LogResult
	mustCall(t, client, ipc.OpSessionLog, ipc.LogArgs{Session: "noisy", Lines: 5, FromEnd: true, Screen: true}, &tail)
	if tail.TotalLines < 250 {
		t.Errorf("the transcript should hold the scrolled-off output, got %d lines", tail.TotalLines)
	}
	if len(tail.ScreenLines) == 0 {
		t.Error("tail should include the live screen so nothing on screen is missed")
	}

	var head ipc.LogResult
	mustCall(t, client, ipc.OpSessionLog, ipc.LogArgs{Session: "noisy", Lines: 3, FromEnd: false}, &head)
	if len(head.Lines) == 0 || !strings.Contains(head.Lines[0], "line-1") {
		t.Errorf("head should start at the oldest output, got %q", head.Lines)
	}
}

// A session that ends must remain readable, because an agent frequently checks
// on a build only after it has already finished.
func TestEndedSessionsStayReadable(t *testing.T) {
	_, client, paths := newTestDaemon(t)
	_ = paths

	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{
		Name: "done", Argv: []string{"sh", "-c", "echo finished-output; exit 0"}, WaitMS: 4000,
	}, &created)

	var screen ipc.Screen
	mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{Session: "done"}, &screen)
	if screen.Session.Running {
		t.Error("the session should report as ended")
	}
	if !strings.Contains(strings.Join(screen.Lines, "\n"), "finished-output") {
		t.Errorf("the final screen should still be readable:\n%s", strings.Join(screen.Lines, "\n"))
	}
}

func TestRenameAndScrollback(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{
		Rows: 8, Argv: []string{"sh", "-c", "i=1; while [ $i -le 60 ]; do echo row-$i; i=$((i+1)); done; sleep 30"},
		WaitMS: 3000,
	}, &created)

	var info ipc.SessionInfo
	mustCall(t, client, ipc.OpSessionRename, ipc.RenameArgs{Session: created.Session.ID, Name: "renamed"}, &info)
	if info.Name != "renamed" {
		t.Errorf("rename: got %q", info.Name)
	}

	var scroll ipc.ScrollResult
	mustCall(t, client, ipc.OpSessionScroll, ipc.ScrollArgs{Session: "renamed", Offset: 5, Lines: 5}, &scroll)
	if len(scroll.Lines) == 0 {
		t.Error("scrollback should return retained lines above the screen")
	}
	if scroll.Total == 0 {
		t.Error("scrollback should report how much history exists")
	}
}

// Two daemons must not both bind: the loser exits so its client connects to the
// winner, rather than splitting sessions between two registries.
func TestSecondDaemonRefusesToStart(t *testing.T) {
	_, _, paths := newTestDaemon(t)
	settings, err := normalizeForTest(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(paths, settings, "test"); err == nil {
		t.Fatal("a second daemon should refuse to start")
	}
}

// Reusing the name of a session that has ended is allowed on purpose: naming
// each build "build" is the workflow the tools describe. What must not survive
// is the old session's claim on the name. While both answered to it, lookups
// picked whichever one Go's map iteration reached first, so the same call
// alternated between the live session and the corpse.
func TestAReusedNameBelongsToTheLiveSession(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	var dead ipc.Screen
	mustCall(t, client, ipc.OpSessionNew,
		ipc.NewArgs{Name: "build", Argv: []string{"sh", "-c", "exit 1"}, WaitMS: 2000}, &dead)
	if dead.Session.Running {
		t.Fatal("the first session was supposed to exit immediately")
	}

	var live ipc.Screen
	mustCall(t, client, ipc.OpSessionNew,
		ipc.NewArgs{Name: "build", Argv: []string{"sh"}, WaitMS: 1500}, &live)
	if !live.Session.Running {
		t.Fatal("the replacement session should be running")
	}
	if live.Session.ID == dead.Session.ID {
		t.Fatal("the two sessions should be distinct")
	}

	// Repeated because the failure was a coin flip: one lookup proves nothing.
	for attempt := 0; attempt < 12; attempt++ {
		var got ipc.Screen
		mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{Session: "build"}, &got)
		if got.Session.ID != live.Session.ID {
			t.Fatalf("attempt %d resolved \"build\" to %s; the live session is %s",
				attempt, got.Session.ID, live.Session.ID)
		}
	}

	// The ended session keeps its id, so nothing becomes unreachable.
	var byID ipc.Screen
	mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{Session: dead.Session.ID}, &byID)
	if byID.Session.ID != dead.Session.ID {
		t.Errorf("the ended session should still be reachable by id, got %s", byID.Session.ID)
	}
	if byID.Session.Name != "" {
		t.Errorf("the ended session should have given the name up, still has %q", byID.Session.Name)
	}
}

// The same bug reached it_kill, where it was worse than a failed read: the
// kill landed on the corpse, reported already_ended as a success, and left the
// live session running while the caller believed it was gone.
func TestKillingByAReusedNameEndsTheLiveSession(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	var dead ipc.Screen
	mustCall(t, client, ipc.OpSessionNew,
		ipc.NewArgs{Name: "server", Argv: []string{"sh", "-c", "exit 1"}, WaitMS: 2000}, &dead)
	var live ipc.Screen
	mustCall(t, client, ipc.OpSessionNew,
		ipc.NewArgs{Name: "server", Argv: []string{"sh"}, WaitMS: 1500}, &live)

	var result ipc.KillResult
	mustCall(t, client, ipc.OpSessionKill, ipc.KillArgs{Session: "server", Signal: "KILL"}, &result)
	if result.Killed != live.Session.ID {
		t.Fatalf("kill hit %s, but the live session was %s", result.Killed, live.Session.ID)
	}
	if result.AlreadyGone {
		t.Error("the live session was running; reporting it as already ended is a false success")
	}
}

// A name given by rename must be taken from whoever held it before, or rename
// becomes a way to create the duplicate that creating one cannot.
func TestRenamingTakesTheNameFromAnEndedSession(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	var dead ipc.Screen
	mustCall(t, client, ipc.OpSessionNew,
		ipc.NewArgs{Name: "api", Argv: []string{"sh", "-c", "exit 1"}, WaitMS: 2000}, &dead)
	var live ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Argv: []string{"sh"}, WaitMS: 1500}, &live)

	var renamed ipc.SessionInfo
	mustCall(t, client, ipc.OpSessionRename,
		ipc.RenameArgs{Session: live.Session.ID, Name: "api"}, &renamed)

	for attempt := 0; attempt < 12; attempt++ {
		var got ipc.Screen
		mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{Session: "api"}, &got)
		if got.Session.ID != live.Session.ID {
			t.Fatalf("attempt %d resolved \"api\" to %s; expected %s",
				attempt, got.Session.ID, live.Session.ID)
		}
	}
}
