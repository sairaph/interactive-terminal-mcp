//go:build !windows

package session

// interrupt asks the foreground program to stop.
//
// Writing the interrupt character is deliberately not the same as signalling
// the child. Under a shell the child is the shell, and a SIGINT delivered to
// it does not reach whatever it is currently running; ^C on the line
// discipline does, because the kernel turns it into SIGINT for the foreground
// process group of the terminal.
func (s *Session) interrupt() error {
	return s.Write([]byte{0x03})
}
