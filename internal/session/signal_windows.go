//go:build windows

package session

import (
	"fmt"
	"os/exec"
	"strconv"
)

// signal ends the process tree.
//
// Windows has no signals in the POSIX sense and ConPTY gives no process group
// to address, so TERM, HUP, and KILL all mean the same thing here: terminate
// the tree. taskkill /T reaches descendants, which a bare Process.Kill would
// leave orphaned holding the pseudo-console open. The contract documents that
// INT is handled separately by writing 0x03 to the console.
func (s *Session) signal(name string) error {
	pid := commandPID(s.command)
	if pid <= 0 {
		return fmt.Errorf("session has no running process to signal")
	}
	switch name {
	case "TERM", "HUP", "KILL":
	default:
		return fmt.Errorf("unsupported signal %q", name)
	}
	command := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	if output, err := command.CombinedOutput(); err != nil {
		// taskkill is present on every supported Windows version, but if it
		// cannot run, killing the process alone still ends the session.
		if s.command.Process != nil {
			if killErr := s.command.Process.Kill(); killErr == nil {
				return nil
			}
		}
		return fmt.Errorf("terminate process tree: %w (%s)", err, string(output))
	}
	return nil
}

// signalExitCode reports the raw exit status; Windows has no signal encoding.
func signalExitCode(exit *exec.ExitError) int {
	if code := exit.ExitCode(); code >= 0 {
		return code
	}
	return -1
}
