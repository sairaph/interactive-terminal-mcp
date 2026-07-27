//go:build windows

package session

// foregroundBusy cannot be answered on Windows.
//
// A console has no notion of a foreground process group that the shell hands
// control to, so there is no equivalent of TIOCGPGRP to ask. Callers fall back
// to watching output, and must present that for the guess it is.
func (s *Session) foregroundBusy() (busy bool, known bool) {
	return false, false
}
