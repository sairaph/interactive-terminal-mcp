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
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != created.Session.ID {
		t.Fatalf("list: got %d sessions, first %q", len(listed.Sessions), listed.Sessions[0].ID)
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
		Session: created.Session.ID,
		Text:    "PS1=''; echo daemon-round-trip", HasText: true, Enter: true, WaitMS: 4000,
	}, &ipc.Screen{})

	var screen ipc.Screen
	mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{Session: created.Session.ID, WaitMS: 1000}, &screen)
	if !strings.Contains(strings.Join(screen.Lines, "\n"), "daemon-round-trip") {
		t.Errorf("screen did not show the command output:\n%s", strings.Join(screen.Lines, "\n"))
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
		Name: "stubborn",
		// The session's own shell has to refuse, not a child of it. TERM is
		// delivered with HUP now, because an interactive shell ignores TERM by
		// design, so both have to be trapped for this to be a session that
		// genuinely will not leave without force.
		Shell:       "sh",
		CommandLine: "trap '' TERM HUP; while :; do sleep 0.2; done",
		WaitMS:      2000,
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
		Session: "busy",
		Text:    "PS1=''; sleep 30", HasText: true, Enter: true, WaitMS: 500,
	}, &ipc.Screen{})

	var result ipc.KillResult
	mustCall(t, client, ipc.OpSessionKill, ipc.KillArgs{Session: "busy", Signal: "INT"}, &result)
	if result.ExitCode != nil {
		t.Errorf("an interrupt should not end the session, got exit code %d", *result.ExitCode)
	}

	var screen ipc.Screen
	mustCall(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Session: "busy",
		Text:    "echo still-alive", HasText: true, Enter: true, WaitMS: 4000,
	}, &screen)
	if !strings.Contains(strings.Join(screen.Lines, "\n"), "still-alive") {
		t.Errorf("the session should still accept commands after an interrupt:\n%s",
			strings.Join(screen.Lines, "\n"))
	}
}

// endSession leaves a session ended but still listed, which is what happens
// when a shell exits on its own. it_kill would retire the entry under the
// default retention policy and remove it, so it cannot stand in for this.
func endSession(t *testing.T, client *ipc.Client, reference string) {
	t.Helper()
	mustCall(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Session: reference, Text: "exit", HasText: true, Enter: true, WaitMS: 3000,
	}, &ipc.Screen{})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var screen ipc.Screen
		if err := call(t, client, ipc.OpSessionRead, ipc.ReadArgs{Session: reference}, &screen); err != nil {
			return
		}
		if !screen.Session.Running {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session %q did not end", reference)
}

func TestWritingToAnEndedSessionIsTyped(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Name: "brief", Argv: []string{"sh"}, WaitMS: 3000}, &created)
	// A status the shell chose, so the error has a specific code to report.
	mustCall(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Session: "brief", Text: "exit 4", HasText: true, Enter: true, WaitMS: 3000,
	}, &ipc.Screen{})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var screen ipc.Screen
		mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{Session: "brief"}, &screen)
		if !screen.Session.Running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

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

func TestOmittingTheSessionIsRejected(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Argv: []string{"sh"}, WaitMS: 800}, &ipc.Screen{})

	// There is no current session to fall back to. Agents share one daemon, so
	// guessing a target would run one agent's command in another's terminal.
	err := call(t, client, ipc.OpSessionRead, ipc.ReadArgs{}, &ipc.Screen{})
	if err == nil {
		t.Fatal("a call with no session should be rejected, not resolved to something")
	}
	typed, ok := err.(*ipc.Error)
	if !ok || typed.Code != ipc.CodeInvalidInput {
		t.Fatalf("expected invalid_input, got %v", err)
	}
	if !strings.Contains(typed.Hint, "it_list") {
		t.Errorf("hint should name a concrete next call, got %q", typed.Hint)
	}
}

// Resizing must reach the child, not just the emulator, or a program will keep
// drawing for the old size.
func TestResizeThroughRead(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{Argv: []string{"sh"}, Cols: 80, Rows: 24, WaitMS: 1500}, &created)

	var screen ipc.Screen
	mustCall(t, client, ipc.OpSessionRead, ipc.ReadArgs{Session: created.Session.ID, Cols: 100, Rows: 40, WaitMS: 1000}, &screen)
	if screen.Session.Cols != 100 || screen.Session.Rows != 40 {
		t.Fatalf("size: got %dx%d, want 100x40", screen.Session.Cols, screen.Session.Rows)
	}

	mustCall(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Session: created.Session.ID,
		Text:    "PS1=''; stty size", HasText: true, Enter: true, WaitMS: 4000,
	}, &screen)
	if !strings.Contains(strings.Join(screen.Lines, "\n"), "40 100") {
		t.Errorf("the child should see the new size:\n%s", strings.Join(screen.Lines, "\n"))
	}
}

func TestLogsReachBeyondTheScreen(t *testing.T) {
	_, client, _ := newTestDaemon(t)
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{
		Name: "noisy", Rows: 10, Cols: 60,
		Argv:    []string{"sh", "-c", "i=1; while [ $i -le 300 ]; do echo line-$i; i=$((i+1)); done; sleep 30"},
		WaitMS:  20000,
		WaitFor: "line-300",
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
	// Generous on purpose: the log now opens with the prompt and the echoed
	// command line, and how many lines that occupies depends on the width of
	// whatever prompt the machine has. A tight window made this a measurement
	// of the runner's hostname.
	mustCall(t, client, ipc.OpSessionLog, ipc.LogArgs{Session: "noisy", Lines: 40, FromEnd: false}, &head)
	if !strings.Contains(strings.Join(head.Lines, "\n"), "line-1") {
		t.Errorf("head should reach the oldest output, got %q", head.Lines)
	}
}

// A session that ends must remain readable, because an agent frequently checks
// on a build only after it has already finished.
func TestEndedSessionsStayReadable(t *testing.T) {
	_, client, paths := newTestDaemon(t)
	_ = paths

	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{
		Name: "done", CommandLine: "echo finished-output", WaitMS: 4000,
	}, &created)
	endSession(t, client, "done")

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
		Rows: 8, CommandLine: "i=1; while [ $i -le 60 ]; do echo row-$i; i=$((i+1)); done",
		WaitMS:  20000,
		WaitFor: "row-60",
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
		ipc.NewArgs{Name: "build", Argv: []string{"sh"}, WaitMS: 2000}, &dead)
	endSession(t, client, "build")
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
		ipc.NewArgs{Name: "server", Argv: []string{"sh"}, WaitMS: 2000}, &dead)
	endSession(t, client, "server")
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
		ipc.NewArgs{Name: "api", Argv: []string{"sh"}, WaitMS: 2000}, &dead)
	endSession(t, client, "api")
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

// Two agents share one daemon. Nothing in a request identifies which agent
// made it, so any notion of a current session would be shared between them,
// and one agent creating a session would silently redirect the other's next
// command into it. Requiring the target is what makes concurrent use safe, so
// creating a session must not make later calls resolve anywhere by itself.
func TestCreatingASessionDoesNotRedirectOtherCalls(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	var first ipc.Screen
	mustCall(t, client, ipc.OpSessionNew,
		ipc.NewArgs{Name: "agent-a", Argv: []string{"sh"}, WaitMS: 1500}, &first)
	var second ipc.Screen
	mustCall(t, client, ipc.OpSessionNew,
		ipc.NewArgs{Name: "agent-b", Argv: []string{"sh"}, WaitMS: 1500}, &second)

	// Each named call reaches its own session no matter which was created last.
	for _, want := range []ipc.Screen{first, second} {
		var got ipc.Screen
		mustCall(t, client, ipc.OpSessionRead,
			ipc.ReadArgs{Session: want.Session.Name, WaitMS: 500}, &got)
		if got.Session.ID != want.Session.ID {
			t.Errorf("%q resolved to %s, want %s", want.Session.Name, got.Session.ID, want.Session.ID)
		}
	}

	// And an unnamed call resolves to neither. It fails, which is the only
	// answer that cannot be the wrong terminal.
	if err := call(t, client, ipc.OpSessionSend,
		ipc.SendArgs{Text: "echo x", HasText: true, Enter: true, WaitMS: 500}, &ipc.Screen{}); err == nil {
		t.Error("a call naming no session must fail rather than pick one")
	}
}

// A session is a terminal, not a subprocess: running something in it must not
// end it. This is what makes an interactive installer usable -- one that asks a
// question needs the session to still be there to answer in, and one that
// finishes needs the shell to still be there to read the result from.
func TestASessionSurvivesTheCommandItWasGiven(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{
		Name: "installer", Shell: "sh",
		CommandLine: "echo first-command-ran",
		WaitMS:      15000, WaitFor: "first-command-ran",
	}, &created)
	if !created.Session.Running {
		t.Fatal("the session must outlive the command it was given")
	}

	// And it still takes input, which is the whole point.
	var after ipc.Screen
	mustCall(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Session: "installer", Text: "echo second-command-ran", HasText: true, Enter: true,
		WaitMS: 15000, WaitFor: "second-command-ran",
	}, &after)
	if !after.Matched {
		t.Errorf("the session should still run commands:\n%s", strings.Join(after.Lines, "\n"))
	}
	if !after.Session.Running {
		t.Error("the session should still be running")
	}
}

// A command given as an array is quoted for the shell it is typed into, so an
// argument containing spaces survives without the caller escaping anything.
func TestArrayCommandsAreQuotedForTheShell(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{
		Name: "quoted", Shell: "sh",
		Argv:   []string{"printf", "%s|\n", "two words"},
		WaitMS: 15000, WaitFor: "two words|",
	}, &created)
	if !created.Matched {
		t.Errorf("the argument should have arrived intact:\n%s", strings.Join(created.Lines, "\n"))
	}
}

// Every interactive prompt ends in a space. Waiting for one exactly as it
// appears on screen has to work, or the flagship case -- answering an
// installer's question -- fails while looking like the prompt never came.
func TestWaitForMatchesAPromptEndingInASpace(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{
		Name: "prompted", Shell: "sh",
		CommandLine: "printf 'Continue? [y/N] '; read answer; echo \"answered:$answer\"",
		WaitMS:      20000, WaitFor: "Continue? [y/N] ",
	}, &created)
	if !created.Matched {
		t.Fatalf("the prompt is on screen; waiting for it must match:\n%s",
			strings.Join(created.Lines, "\n"))
	}

	var answered ipc.Screen
	mustCall(t, client, ipc.OpSessionSend, ipc.SendArgs{
		Session: "prompted", Text: "y", HasText: true, Enter: true,
		WaitMS: 20000, WaitFor: "answered:y",
	}, &answered)
	if !answered.Matched {
		t.Errorf("the answer should have been accepted:\n%s", strings.Join(answered.Lines, "\n"))
	}
}

// wait: 0 means look now. Inventing a budget made it_read({wait_for}) block for
// thirty seconds while its own schema said the default was zero.
func TestWaitForWithNoBudgetAnswersImmediately(t *testing.T) {
	_, client, _ := newTestDaemon(t)

	var created ipc.Screen
	mustCall(t, client, ipc.OpSessionNew, ipc.NewArgs{
		Name: "quick", Shell: "sh", CommandLine: "echo ALREADY-HERE",
		WaitMS: 20000, WaitFor: "ALREADY-HERE",
	}, &created)

	start := time.Now()
	var absent ipc.Screen
	mustCall(t, client, ipc.OpSessionRead,
		ipc.ReadArgs{Session: "quick", WaitFor: "NEVER-APPEARS", WaitMS: 0}, &absent)
	elapsed := time.Since(start)

	if absent.Matched {
		t.Error("text that is not there must not match")
	}
	if elapsed > 3*time.Second {
		t.Errorf("wait: 0 took %v; it should answer from the screen as it is", elapsed)
	}

	// And text that is there is found in the same single look.
	var present ipc.Screen
	mustCall(t, client, ipc.OpSessionRead,
		ipc.ReadArgs{Session: "quick", WaitFor: "ALREADY-HERE", WaitMS: 0}, &present)
	if !present.Matched {
		t.Error("text already on screen should match without waiting")
	}
}
