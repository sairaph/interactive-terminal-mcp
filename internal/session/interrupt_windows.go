//go:build windows

package session

import (
	"golang.org/x/sys/windows"
)

// interrupt asks the foreground program to stop by writing the interrupt
// character, which is what pressing Ctrl+C in a terminal actually does.
//
// The console host turns ^C in the input stream into a control event for a
// program that has left processed input enabled, which is every ordinary
// console program. A program that has taken the terminal raw -- ssh, wsh, wsl,
// tmux, an editor -- has asked for the byte instead, and gets it. That is the
// whole reason this is a byte and not an event: the byte is interpreted by
// whoever currently owns the terminal, so an interrupt aimed at a command
// running inside an ssh session reaches that command rather than the client.
//
// An earlier version raised CTRL_C_EVENT on the session's console through a
// helper process. That was compensating for a different bug: the daemon is
// created with CREATE_NEW_PROCESS_GROUP, which disables Ctrl+C for every
// process in the group, and sessions inherited it, so the byte could never
// become an interrupt. EnableConsoleInterrupts fixes that at the source. With
// it in place the event is redundant for a local command and destructive for a
// nested one, because GenerateConsoleCtrlEvent reaches every process attached
// to the console -- including the ssh or wsl client itself, which it kills.
func (s *Session) interrupt() error {
	return s.Write([]byte{0x03})
}

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

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
// The daemon needs neither. It is created with DETACHED_PROCESS and so owns no
// console for an event to arrive on.
func EnableConsoleInterrupts() error {
	result, _, err := procSetConsoleCtrlHandler.Call(0, 0)
	if result == 0 {
		return err
	}
	return nil
}
