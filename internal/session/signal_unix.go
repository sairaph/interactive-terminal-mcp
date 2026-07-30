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
	var numbers []syscall.Signal
	switch name {
	case "TERM":
		// A session is an interactive shell, and those ignore SIGTERM by
		// design, so TERM alone would never end one: every kill would sit out
		// the escalation window and then report having been forced, as though
		// something had misbehaved. SIGHUP is what closes a terminal, and a
		// shell exits on it. Sending both is what "ask this session to end"
		// actually means now: whatever is in there answers to one of them.
		numbers = []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP}
	case "HUP":
		numbers = []syscall.Signal{syscall.SIGHUP}
	case "KILL":
		numbers = []syscall.Signal{syscall.SIGKILL}
	default:
		return fmt.Errorf("unsupported signal %q", name)
	}

	var failure error
	for _, number := range numbers {
		if err := syscall.Kill(-pid, number); err != nil {
			// The group may already be gone, or the child may never have become
			// a leader; fall back to the process itself rather than reporting a
			// failure the caller cannot act on.
			if err := syscall.Kill(pid, number); err != nil {
				failure = fmt.Errorf("signal %s: %w", name, err)
				continue
			}
		}
		// One signal getting through is enough to have asked.
		failure = nil
	}
	return failure
}

// signalExitCode renders a signalled exit using the shell's 128+signal
// convention, which is more informative to an agent than Go's bare -1.
func signalExitCode(exit *exec.ExitError) int {
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return -1
}
