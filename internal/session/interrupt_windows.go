//go:build windows

package session

import (
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

// Windows has no line discipline, so writing 0x03 into a pseudo-console does
// not become an interrupt the way ^C does on a terminal. The byte simply
// arrives as input, and a program that is not reading stdin never sees it.
// Stopping a running command requires raising a real console control event.
//
// These three are absent from golang.org/x/sys/windows, so they are declared
// here.
var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole         = kernel32.NewProc("AttachConsole")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

// consoleMu serializes console attachment. A process may be attached to only
// one console at a time, so two sessions interrupting at once would fight over
// a process-wide resource and signal the wrong terminal.
var consoleMu sync.Mutex

// interrupt raises Ctrl+C inside the session's pseudo-console.
//
// The daemon runs detached and owns no console of its own, which is what makes
// this possible: it can borrow the child's console, raise the event there, and
// give it back. The event is sent to group 0, meaning every process attached to
// that console, because CTRL_C_EVENT cannot be aimed at a single group.
//
// The interrupt character is still written afterwards. Some programs read it
// from stdin rather than handling a console event, and a duplicate ^C is
// harmless where the event already worked.
func (s *Session) interrupt() error {
	pid := commandPID(s.command)
	if pid <= 0 {
		return fmt.Errorf("session has no running process to interrupt")
	}

	eventErr := s.raiseConsoleInterrupt(uint32(pid))
	writeErr := s.Write([]byte{0x03})

	// Either route reaching the program is enough. Only report failure when
	// both were refused, so a program that reads ^C from stdin still counts.
	if eventErr != nil && writeErr != nil {
		return fmt.Errorf("interrupt: console event failed (%v) and writing ^C failed (%w)", eventErr, writeErr)
	}
	return nil
}

func (s *Session) raiseConsoleInterrupt(pid uint32) error {
	consoleMu.Lock()
	defer consoleMu.Unlock()

	// Detach from any console first; AttachConsole fails if one is held.
	freeConsole()
	if err := attachConsole(pid); err != nil {
		return fmt.Errorf("attach to the session console: %w", err)
	}
	defer freeConsole()

	// The event goes to every process on that console, which now includes this
	// one. Ignoring Ctrl+C for the duration stops the daemon killing itself.
	if err := ignoreCtrlC(true); err != nil {
		return fmt.Errorf("suspend this process's Ctrl+C handling: %w", err)
	}
	defer ignoreCtrlC(false) //nolint:errcheck // restoring is best effort

	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0); err != nil {
		return fmt.Errorf("raise Ctrl+C: %w", err)
	}
	return nil
}

func attachConsole(pid uint32) error {
	result, _, err := procAttachConsole.Call(uintptr(pid))
	if result == 0 {
		return err
	}
	return nil
}

func freeConsole() {
	_, _, _ = procFreeConsole.Call()
}

func ignoreCtrlC(ignore bool) error {
	var add uintptr
	if ignore {
		add = 1
	}
	result, _, err := procSetConsoleCtrlHandler.Call(0, add)
	if result == 0 {
		return err
	}
	return nil
}
