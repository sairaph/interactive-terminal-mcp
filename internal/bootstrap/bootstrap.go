// Package bootstrap resolves the paths, configuration, and daemon connection
// every entrypoint needs, so main.go, the CLI, the MCP server, and the
// interactive application all start from the same state.
package bootstrap

import (
	"context"
	"os"

	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

// Runtime is the shared startup state.
type Runtime struct {
	Paths      config.Paths
	Config     config.Config
	Executable string
}

// Open loads paths and configuration. It does not contact the daemon: an
// installer or a help command must work even when no daemon can run.
func Open() (*Runtime, error) {
	paths, err := config.DefaultPaths()
	if err != nil {
		return nil, err
	}
	settings, err := config.Load(paths)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		// A missing executable path only breaks autostart, and reporting it
		// here would block commands that never need it.
		executable = ""
	}
	return &Runtime{Paths: paths, Config: settings, Executable: executable}, nil
}

// Dial connects to the daemon, starting one if needed.
func (r *Runtime) Dial(ctx context.Context) (*ipc.Client, error) {
	return ipc.Dial(ctx, r.Paths.Socket, r.Executable)
}

// Connect attaches to a running daemon without starting one. Commands that
// only report on the daemon use it, so asking whether a daemon is running does
// not start one.
func (r *Runtime) Connect(ctx context.Context) (*ipc.Client, error) {
	return ipc.Connect(ctx, r.Paths.Socket)
}
