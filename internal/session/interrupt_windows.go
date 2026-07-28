//go:build windows

package session

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
)

// Windows has no line discipline, so writing 0x03 into a pseudo-console does
// not become an interrupt the way ^C does on a terminal. The byte simply
// arrives as input, and a program that is not reading stdin never sees it.
// Stopping a running command requires raising a real console control event.
//
// That event cannot be raised from this process. GenerateConsoleCtrlEvent
// delivers CTRL_C_EVENT to every process attached to the console, and the only
// way to reach a session's console is to attach to it -- which would put the
// daemon on the receiving end of the event it is sending. Delivery is
// asynchronous, so no amount of guarding around the call is reliable: the
// daemon would take its own interrupt, cancel its root context, and shut down,
// destroying every session it owns rather than the one command that was meant
// to stop.
//
// So the event is raised by a short-lived helper process instead. It attaches,
// signals, and exits. Nothing the daemon owns is ever attached to a session's
// console.
func (s *Session) interrupt() error {
	pid := commandPID(s.command)
	if pid <= 0 {
		return fmt.Errorf("session has no running process to interrupt")
	}

	eventErr := raiseInterruptViaHelper(pid)
	// Some programs read the interrupt character from stdin rather than
	// handling a console event, and a duplicate ^C is harmless where the event
	// already worked.
	writeErr := s.Write([]byte{0x03})

	if eventErr != nil && writeErr != nil {
		return fmt.Errorf("interrupt: console event failed (%v) and writing ^C failed (%w)", eventErr, writeErr)
	}
	return nil
}

// raiseInterruptViaHelper runs this same binary as a helper that borrows the
// target's console and raises Ctrl+C there.
func raiseInterruptViaHelper(pid int) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this executable: %w", err)
	}

	command := exec.Command(executable, interruptHelperCommand, fmt.Sprint(pid))
	// DETACHED_PROCESS keeps the helper off this process's console, so it can
	// attach to the session's own. No window is ever shown.
	command.SysProcAttr = &windows.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start the interrupt helper: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		// The helper is bounded and should finish in milliseconds. If it does
		// not, abandon it rather than holding up the caller.
		_ = command.Process.Kill()
		return fmt.Errorf("the interrupt helper did not finish")
	}
}

// interruptHelperCommand is the hidden subcommand the helper runs as.
const interruptHelperCommand = "__raise-interrupt"

// InterruptHelperCommand is the argument that selects the helper, so the CLI
// can route it without duplicating the string.
const InterruptHelperCommand = interruptHelperCommand

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole         = kernel32.NewProc("AttachConsole")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

// RunInterruptHelper raises Ctrl+C on the console of the given process.
//
// This runs in its own process, so the fact that it receives the event too is
// harmless; it ignores it and exits. Nothing else is affected.
func RunInterruptHelper(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("a process id is required")
	}

	// Ignore the event before raising it, and never restore. This process
	// exists only to send it.
	if err := ignoreCtrlC(true); err != nil {
		return fmt.Errorf("suspend Ctrl+C handling: %w", err)
	}

	freeConsole()
	if err := attachConsole(uint32(pid)); err != nil {
		return fmt.Errorf("attach to the target console: %w", err)
	}
	defer freeConsole()

	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0); err != nil {
		return fmt.Errorf("raise Ctrl+C: %w", err)
	}
	// Delivery is asynchronous; give it a moment to reach the target before
	// this process detaches from the console.
	time.Sleep(150 * time.Millisecond)
	return nil
}

// EnableConsoleInterrupts restores normal Ctrl+C processing for this process
// and, by inheritance, for every session started afterwards.
//
// This is what makes an interrupt reach a command at all on Windows. Two
// separate things disable it, and both applied here:
//
// The daemon is created with CREATE_NEW_PROCESS_GROUP so it survives the AI
// client that spawned it, and that flag disables CTRL+C for every process in
// the new group. Sessions are created by the daemon, so they inherited the
// disabled state and could never be interrupted.
//
// Separately, SetConsoleCtrlHandler(NULL, TRUE) sets an ignore attribute that
// is likewise inherited, so protecting the daemon that way would silently
// protect every command running inside it too.
//
// The daemon does not need either. It is created with DETACHED_PROCESS and so
// owns no console for an event to arrive on, and it never attaches to a
// session's console: the event is raised by a separate helper process.
func EnableConsoleInterrupts() error {
	return ignoreCtrlC(false)
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
