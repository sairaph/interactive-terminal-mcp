//go:build !windows

package session

import (
	"fmt"
	"os/exec"
	"syscall"
)

// signal delivers a signal to the child's process group.
//
// The group, not the process: a shell running `make -j8` has children of its
// own, and signalling only the shell leaves them running against a PTY nobody
// reads. go-pty starts the child with Setsid, so it leads its own group and a
// negative PID addresses the whole tree.
func (s *Session) signal(name string) error {
	pid := commandPID(s.command)
	if pid <= 0 {
		return fmt.Errorf("session has no running process to signal")
	}
	var number syscall.Signal
	switch name {
	case "TERM":
		number = syscall.SIGTERM
	case "HUP":
		number = syscall.SIGHUP
	case "KILL":
		number = syscall.SIGKILL
	default:
		return fmt.Errorf("unsupported signal %q", name)
	}
	if err := syscall.Kill(-pid, number); err != nil {
		// The group may already be gone, or the child may never have become a
		// leader; fall back to the process itself rather than reporting a
		// failure the caller cannot act on.
		if err := syscall.Kill(pid, number); err != nil {
			return fmt.Errorf("signal %s: %w", name, err)
		}
	}
	return nil
}

// signalExitCode renders a signalled exit using the shell's 128+signal
// convention, which is more informative to an agent than Go's bare -1.
func signalExitCode(exit *exec.ExitError) int {
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return -1
}
