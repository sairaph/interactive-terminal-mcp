//go:build !windows

package session

import (
	"github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/unix"
)

// foregroundBusy reports whether a command is running in the terminal, and
// whether that could be established at all.
//
// A terminal tracks which process group is in the foreground, and the kernel
// updates it as the shell hands control to each command and takes it back. So
// the question "has the command finished" has an exact answer here, rather
// than the guess that watching output timing gives: output going quiet for a
// moment is indistinguishable from a command that simply pauses between lines.
//
// This is only meaningful for a session running a shell. When the session is
// one program started directly, that program is the foreground group for its
// whole life, and liveness is the right question instead.
func (s *Session) foregroundBusy() (busy bool, known bool) {
	if !s.startedShell() {
		return false, false
	}
	unixPty, ok := s.pty.(pty.UnixPty)
	if !ok {
		return false, false
	}
	master := unixPty.Master()
	if master == nil {
		return false, false
	}
	group, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return false, false
	}
	// go-pty starts the child with Setsid, so the shell leads its own group and
	// its pid is that group's id. The foreground being anything else means the
	// shell has handed the terminal to a command it is waiting on.
	return group != commandPID(s.command), true
}
