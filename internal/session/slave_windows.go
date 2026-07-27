//go:build windows

package session

// releaseSlave is a no-op on Windows.
//
// A ConPTY has no separate slave descriptor for this process to hold: the
// pseudo-console signals end-of-stream once the attached process tree exits,
// so the pump drains and returns without any help.
func (s *Session) releaseSlave() {}
