package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sairaph/interactive-terminal-mcp/internal/bootstrap"
	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

func runConfig(runtime *bootstrap.Runtime, options Options) int {
	settings := runtime.Config
	fmt.Fprintf(options.Stdout, "config file    %s\n", runtime.Paths.Config)
	fmt.Fprintf(options.Stdout, "sessions       %s\n", runtime.Paths.Sessions)
	fmt.Fprintf(options.Stdout, "socket         %s\n\n", runtime.Paths.Socket)
	fmt.Fprintf(options.Stdout, "terminal size          %dx%d\n", settings.DefaultCols, settings.DefaultRows)
	fmt.Fprintf(options.Stdout, "default wait           %ds\n", settings.DefaultWaitSeconds)
	fmt.Fprintf(options.Stdout, "list output            ~%d tokens\n", settings.ListTokenBudget)
	fmt.Fprintf(options.Stdout, "log output             ~%d tokens\n", settings.ReadTokenBudget)
	fmt.Fprintf(options.Stdout, "session log retention  %s\n", config.RetentionLabel(settings.LogRetention))
	fmt.Fprintf(options.Stdout, "scrollback             %d lines\n", settings.ScrollbackLines)
	fmt.Fprintf(options.Stdout, "maximum sessions       %d\n", settings.MaximumSessions)
	fmt.Fprintf(options.Stdout, "daemon idle shutdown   %ds\n", settings.DaemonIdleShutdownSeconds)
	return 0
}

func runStatus(ctx context.Context, runtime *bootstrap.Runtime, options Options) int {
	// Connect rather than Dial: asking whether a daemon runs must not start one.
	client, err := runtime.Connect(ctx)
	if err != nil {
		fmt.Fprintln(options.Stdout, "daemon: not running")
		return 0
	}
	defer client.Close()

	var status ipc.Status
	if err := client.Call(ctx, ipc.OpDaemonStatus, nil, &status); err != nil {
		return fail(options, err)
	}
	fmt.Fprintf(options.Stdout, "daemon:    running (pid %d, %s)\n", status.PID, status.Version)
	fmt.Fprintf(options.Stdout, "started:   %s\n", status.StartedAt.Local().Format(time.RFC3339))
	fmt.Fprintf(options.Stdout, "sessions:  %d (%d running)\n", status.Sessions, status.Live)
	fmt.Fprintf(options.Stdout, "clients:   %d\n", status.Clients)
	fmt.Fprintf(options.Stdout, "socket:    %s\n", status.Socket)
	return 0
}

func runList(ctx context.Context, runtime *bootstrap.Runtime, options Options) int {
	client, err := runtime.Dial(ctx)
	if err != nil {
		return fail(options, err)
	}
	defer client.Close()

	var result ipc.ListResult
	if err := client.Call(ctx, ipc.OpSessionList, nil, &result); err != nil {
		return fail(options, err)
	}
	if len(result.Sessions) == 0 {
		fmt.Fprintln(options.Stdout, "No sessions. Create one with `interactive-terminal-mcp new`.")
		return 0
	}

	fmt.Fprintf(options.Stdout, "%-10s %-16s %-10s %-8s %-14s %s\n", "ID", "NAME", "STATE", "SIZE", "LAST ACTIVITY", "LOG")
	for _, info := range result.Sessions {
		name := info.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(options.Stdout, "%-10s %-16s %-10s %-8s %-14s %d lines\n",
			info.ID, name, stateText(info),
			fmt.Sprintf("%dx%d", info.Cols, info.Rows),
			relativeTime(info.LastActivityAt), info.TranscriptLines)
	}
	return 0
}

func runNew(ctx context.Context, runtime *bootstrap.Runtime, command Command, options Options) int {
	client, err := runtime.Dial(ctx)
	if err != nil {
		return fail(options, err)
	}
	defer client.Close()

	wait := float64(2)
	if command.WaitSet {
		wait = command.Wait
	}
	args := ipc.NewArgs{
		Name: command.Name2, CommandLine: command.Command, Cwd: command.Cwd,
		Cols: command.Cols, Rows: command.Rows,
		WaitMS: int64(wait * 1000),
	}
	var screen ipc.Screen
	if err := client.Call(ctx, ipc.OpSessionNew, args, &screen); err != nil {
		return fail(options, err)
	}
	printScreen(options, screen)
	fmt.Fprintf(options.Stdout, "\nSession %s created and active.\n", screen.Session.ID)
	return 0
}

func runRead(ctx context.Context, runtime *bootstrap.Runtime, command Command, options Options) int {
	client, err := runtime.Dial(ctx)
	if err != nil {
		return fail(options, err)
	}
	defer client.Close()

	args := ipc.ReadArgs{Session: command.Session, Cols: command.Cols, Rows: command.Rows}
	if command.WaitSet {
		args.WaitMS = int64(command.Wait * 1000)
	}
	var screen ipc.Screen
	if err := client.Call(ctx, ipc.OpSessionRead, args, &screen); err != nil {
		return fail(options, err)
	}
	printScreen(options, screen)
	return 0
}

func runSend(ctx context.Context, runtime *bootstrap.Runtime, command Command, options Options) int {
	client, err := runtime.Dial(ctx)
	if err != nil {
		return fail(options, err)
	}
	defer client.Close()

	args := ipc.SendArgs{
		Session: command.Session,
		Enter:   !command.NoEnter,
		WaitMS:  int64(runtime.Config.DefaultWait / time.Millisecond),
	}
	if command.Text != "" {
		args.Text, args.HasText = command.Text, true
	}
	if command.Keys != "" {
		args.Keys, args.HasKeys = command.Keys, true
	}
	if command.WaitSet {
		args.WaitMS = int64(command.Wait * 1000)
	}
	var screen ipc.Screen
	if err := client.Call(ctx, ipc.OpSessionSend, args, &screen); err != nil {
		return fail(options, err)
	}
	printScreen(options, screen)
	return 0
}

func runLog(ctx context.Context, runtime *bootstrap.Runtime, command Command, options Options, fromEnd bool) int {
	client, err := runtime.Dial(ctx)
	if err != nil {
		return fail(options, err)
	}
	defer client.Close()

	lines := command.Lines
	if lines <= 0 {
		lines = 100
	}
	args := ipc.LogArgs{Session: command.Session, Lines: lines, FromEnd: fromEnd}
	var result ipc.LogResult
	if err := client.Call(ctx, ipc.OpSessionLog, args, &result); err != nil {
		return fail(options, err)
	}
	for _, line := range result.Lines {
		fmt.Fprintln(options.Stdout, line)
	}
	if len(result.Lines) < result.TotalLines {
		fmt.Fprintf(options.Stderr, "\n(%d of %d lines; full log at %s)\n",
			len(result.Lines), result.TotalLines, result.LogPath)
	}
	return 0
}

func runKill(ctx context.Context, runtime *bootstrap.Runtime, command Command, options Options) int {
	client, err := runtime.Dial(ctx)
	if err != nil {
		return fail(options, err)
	}
	defer client.Close()

	var result ipc.KillResult
	if err := client.Call(ctx, ipc.OpSessionKill, ipc.KillArgs{Session: command.Session, Signal: command.Signal}, &result); err != nil {
		return fail(options, err)
	}
	switch {
	case result.AlreadyGone:
		fmt.Fprintf(options.Stdout, "Session %s had already ended.\n", result.Killed)
	case result.Escalated:
		fmt.Fprintf(options.Stdout, "Session %s did not exit after TERM; ended with KILL.\n", result.Killed)
	default:
		fmt.Fprintf(options.Stdout, "Ended session %s with %s.\n", result.Killed, result.Signal)
	}
	if result.LogsRetained && result.LogPath != "" {
		fmt.Fprintf(options.Stdout, "Log kept at %s\n", result.LogPath)
	}
	return 0
}

func runRename(ctx context.Context, runtime *bootstrap.Runtime, command Command, options Options) int {
	client, err := runtime.Dial(ctx)
	if err != nil {
		return fail(options, err)
	}
	defer client.Close()

	var info ipc.SessionInfo
	if err := client.Call(ctx, ipc.OpSessionRename, ipc.RenameArgs{Session: command.Session, Name: command.Name2}, &info); err != nil {
		return fail(options, err)
	}
	fmt.Fprintf(options.Stdout, "Session %s is now named %q.\n", info.ID, info.Name)
	return 0
}

func runDoctor(ctx context.Context, runtime *bootstrap.Runtime, options Options) int {
	problems := 0
	report := func(ok bool, label, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			problems++
		}
		fmt.Fprintf(options.Stdout, "[%s] %-28s %s\n", mark, label, detail)
	}

	info, err := os.Stat(runtime.Paths.Root)
	report(err == nil && info.IsDir(), "application directory", runtime.Paths.Root)
	if err == nil && info.Mode().Perm()&0o077 != 0 {
		report(false, "directory permissions",
			fmt.Sprintf("%s is readable by other users; expected 0700", runtime.Paths.Root))
	} else if err == nil {
		report(true, "directory permissions", "0700")
	}

	_, err = os.Stat(runtime.Paths.Config)
	report(err == nil, "config file", runtime.Paths.Config)
	report(runtime.Executable != "", "executable path", executableOrUnknown(runtime))

	// Probing a real PTY catches the container and sandbox cases where
	// everything else looks fine but no terminal can ever be allocated.
	if err := probePTY(); err != nil {
		report(false, "pseudo-terminal", err.Error())
	} else {
		report(true, "pseudo-terminal", "allocated and released")
	}

	client, err := runtime.Connect(ctx)
	if err != nil {
		report(true, "daemon", "not running (it starts automatically on first use)")
	} else {
		defer client.Close()
		var status ipc.Status
		if err := client.Call(ctx, ipc.OpDaemonStatus, nil, &status); err != nil {
			report(false, "daemon", err.Error())
		} else {
			report(true, "daemon", fmt.Sprintf("running, pid %d, %d sessions (%d live)", status.PID, status.Sessions, status.Live))
			if status.Version != options.Version && options.Version != "" {
				report(false, "daemon version",
					fmt.Sprintf("daemon is %s but this binary is %s; run `interactive-terminal-mcp daemon --stop`", status.Version, options.Version))
			}
		}
	}

	if problems > 0 {
		fmt.Fprintf(options.Stdout, "\n%d %s found.\n", problems, plural(problems, "problem", "problems"))
		return 1
	}
	fmt.Fprintln(options.Stdout, "\nNo problems found.")
	return 0
}

func executableOrUnknown(runtime *bootstrap.Runtime) string {
	if runtime.Executable == "" {
		return "could not be resolved; the daemon cannot start automatically"
	}
	return runtime.Executable
}

// --- shared output ----------------------------------------------------------

func printScreen(options Options, screen ipc.Screen) {
	for _, line := range screen.Lines {
		fmt.Fprintln(options.Stdout, line)
	}
	if !screen.Settled && screen.Session.Running {
		fmt.Fprintf(options.Stderr, "\n(output was still arriving after %dms; run read again for more)\n", screen.WaitedMS)
	}
	if !screen.Session.Running && screen.Session.ExitCode != nil {
		fmt.Fprintf(options.Stderr, "\n(session ended with exit code %d)\n", *screen.Session.ExitCode)
	}
}

func fail(options Options, err error) int {
	var typed *ipc.Error
	if ok := asError(err, &typed); ok {
		fmt.Fprintf(options.Stderr, "interactive-terminal-mcp: %s\n", typed.Message)
		if typed.Hint != "" {
			fmt.Fprintf(options.Stderr, "  %s\n", humanHint(typed.Hint))
		}
		return 1
	}
	fmt.Fprintf(options.Stderr, "interactive-terminal-mcp: %v\n", err)
	return 1
}

// humanHint rewrites tool-call guidance into CLI guidance. The same daemon
// error reaches an agent and a person, and each should be told what to run in
// their own terms.
func humanHint(hint string) string {
	replacements := []struct{ from, to string }{
		// The longer form is replaced first; otherwise "it_list()" would match
		// inside "it_list({})" and leave a stray "{})" behind.
		{`it_list({})`, "`interactive-terminal-mcp ls`"},
		{"it_list()", "`interactive-terminal-mcp ls`"},
		{"it_new({})", "`interactive-terminal-mcp new`"},
	}
	for _, replacement := range replacements {
		hint = strings.ReplaceAll(hint, replacement.from, replacement.to)
	}
	return hint
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
