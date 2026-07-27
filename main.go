// Command interactive-terminal-mcp gives AI agents persistent terminal
// sessions, and gives their users a way to watch and share them.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sairaph/interactive-terminal-mcp/internal/app"
	"github.com/sairaph/interactive-terminal-mcp/internal/bootstrap"
	"github.com/sairaph/interactive-terminal-mcp/internal/cli"
	"github.com/sairaph/interactive-terminal-mcp/internal/daemon"
	"github.com/sairaph/interactive-terminal-mcp/internal/install"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
	"github.com/sairaph/interactive-terminal-mcp/internal/mcpserver"
	"golang.org/x/term"
)

// version is replaced at build time with -ldflags.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	options := cli.Options{
		Version: version,
		Stdin:   os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	}
	if executable, err := os.Executable(); err == nil {
		options.Executable = executable
	}

	command, err := cli.Parse(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp:", err)
		if errors.Is(err, cli.ErrUsage) {
			fmt.Fprint(os.Stderr, "\n", cli.Usage)
		}
		return 2
	}

	// Help and version must work even when the home directory is unwritable,
	// so they run before any state is loaded.
	if command.Name == "help" || command.Name == "version" {
		return cli.Run(ctx, nil, command, options)
	}

	runtime, err := bootstrap.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp:", err)
		return 1
	}
	if options.Executable != "" {
		runtime.Executable = options.Executable
	}

	switch command.Name {
	case "app":
		// A bare invocation means the interactive application on a terminal and
		// the MCP server without one, so an AI client that runs the binary with
		// no arguments still gets a working server.
		if isInteractive() {
			return app.Run(ctx, runtime, version)
		}
		return serveMCP(ctx, runtime)
	case "mcp":
		return serveMCP(ctx, runtime)
	case "daemon":
		return runDaemon(ctx, runtime, command, options)
	case "configure", "install", "uninstall":
		return install.Run(ctx, runtime, command, options)
	case "attach":
		return app.Attach(ctx, runtime, command.Session)
	default:
		return cli.Run(ctx, runtime, command, options)
	}
}

func serveMCP(ctx context.Context, runtime *bootstrap.Runtime) int {
	dial := func(ctx context.Context) (*ipc.Client, error) { return runtime.Dial(ctx) }
	if err := mcpserver.ServeStdio(ctx, dial, runtime.Config, version); err != nil {
		// stdout carries MCP framing, so diagnostics must go to stderr.
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp:", err)
		return 1
	}
	return 0
}

func runDaemon(ctx context.Context, runtime *bootstrap.Runtime, command cli.Command, options cli.Options) int {
	if command.Stop {
		return stopDaemon(ctx, runtime, command, options)
	}

	server, err := daemon.Open(runtime.Paths, runtime.Config, version)
	if err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			// Losing the autostart race is the expected outcome when two
			// clients start at once, not a failure: the winner serves both.
			if !command.Detach {
				fmt.Fprintln(os.Stderr, "interactive-terminal-mcp: a session daemon is already running")
			}
			return 0
		}
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp:", err)
		return 1
	}
	defer server.Close(false)

	if !command.Detach {
		fmt.Fprintf(os.Stderr, "interactive-terminal-mcp: daemon listening on %s\n", runtime.Paths.Socket)
	}
	if err := server.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp:", err)
		return 1
	}
	return 0
}

func stopDaemon(ctx context.Context, runtime *bootstrap.Runtime, command cli.Command, options cli.Options) int {
	client, err := runtime.Connect(ctx)
	if err != nil {
		fmt.Fprintln(options.Stdout, "No session daemon is running.")
		return 0
	}
	defer client.Close()

	var status ipc.Status
	if err := client.Call(ctx, ipc.OpDaemonStop, ipc.StopArgs{KillSessions: command.Kill}, &status); err != nil {
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp:", err)
		return 1
	}
	if command.Kill {
		fmt.Fprintf(options.Stdout, "Stopped the daemon and ended %d running %s.\n",
			status.Live, pluralWord(status.Live, "session", "sessions"))
	} else {
		fmt.Fprintf(options.Stdout, "Stopped the daemon. %d running %s ended with it.\n",
			status.Live, pluralWord(status.Live, "session was", "sessions were"))
	}
	return 0
}

func pluralWord(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

// isInteractive reports whether both stdin and stdout are terminals, which is
// what the full-screen application requires.
func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}
