//go:build !windows

package session

import "github.com/aymanbagabas/go-pty"

// releaseSlave closes this process's copy of the PTY slave descriptor.
//
// The child has its own duplicated descriptors, so this does not disturb a
// running program; it only removes the reference that would otherwise keep the
// master readable forever after the child exits. Once the last slave reference
// is gone, a read on the master drains whatever is buffered and then reports
// EOF, which is how the pump learns the session is finished.
func (s *Session) releaseSlave() {
	unixPty, ok := s.pty.(pty.UnixPty)
	if !ok {
		return
	}
	if slave := unixPty.Slave(); slave != nil {
		_ = slave.Close()
	}
}
