//go:build !windows

package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func dialSocket(ctx context.Context, socket string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	return dialer.DialContext(ctx, "unix", socket)
}

// Listen binds the daemon's socket.
//
// A socket left behind by a crashed daemon looks identical to a live one, so
// a stale file is probed by connecting to it: only a socket that refuses a
// connection is removed. Deleting one that answers would strand a healthy
// daemon and its running sessions.
func Listen(socket string) (net.Listener, error) {
	directory := filepath.Dir(socket)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	if _, err := os.Stat(socket); err == nil {
		conn, dialErr := net.DialTimeout("unix", socket, 500*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("another daemon is already listening on %s", socket)
		}
		if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}

	// The umask would otherwise widen the socket's permissions. Terminal
	// sessions run arbitrary commands as this user, so the socket must not be
	// reachable by anyone else on a shared machine.
	previous := syscall.Umask(0o077)
	listener, err := net.Listen("unix", socket)
	syscall.Umask(previous)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", socket, err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("restrict socket permissions: %w", err)
	}
	return listener, nil
}

// configureDetach makes the spawned daemon survive its parent.
//
// Setsid detaches it from the MCP client's process group and controlling
// terminal, so the sessions it owns are not killed when the AI client that
// happened to start it exits.
func configureDetach(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// Cleanup removes the socket after the daemon stops.
func Cleanup(socket string) {
	_ = os.Remove(socket)
}

var _ = context.Background
