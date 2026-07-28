//go:build !windows

package session

// BusyReportsIdle reports whether this platform can establish that *nothing*
// is running, as distinct from establishing that something is.
//
// A Unix terminal tracks its foreground process group, so both answers are
// available. Callers use this to describe the busy field accurately rather
// than promising a value the platform will never produce.
func BusyReportsIdle() bool { return true }
