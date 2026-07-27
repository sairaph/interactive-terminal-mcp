//go:build windows

package session

import (
	"fmt"
	"os/exec"
	"strconv"

	"golang.org/x/sys/windows"
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
	// Without this, taskkill allocates its own console and a window flashes on
	// screen every time a session is ended.
	command.SysProcAttr = &windows.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}

	// taskkill reports failure when any process in the tree has already gone,
	// which is routine: the shell often exits the moment its children do. Fall
	// back to the process itself, and treat an already-dead process as success.
	if s.command.Process != nil {
		if killErr := s.command.Process.Kill(); killErr == nil {
			return nil
		}
	}
	// The caller asked for the session to stop. If it is stopping, or already
	// stopped, that request was satisfied however taskkill chose to report it.
	// Returning an error here would abort the caller before it could retire the
	// session, leaving a dead entry in the list forever.
	if !s.Running() {
		return nil
	}
	return fmt.Errorf("terminate process tree: %w (%s)", err, string(output))
}

// signalExitCode reports the raw exit status; Windows has no signal encoding.
func signalExitCode(exit *exec.ExitError) int {
	if code := exit.ExitCode(); code >= 0 {
		return code
	}
	return -1
}
