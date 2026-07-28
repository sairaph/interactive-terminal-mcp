//go:build windows

package session

// BusyReportsIdle reports whether this platform can establish that *nothing*
// is running, as distinct from establishing that something is.
//
// Windows can only prove the positive: a child process of the shell means a
// command is running. Its absence proves nothing, because PowerShell runs most
// cmdlets inside its own process. So busy appears only as true here, and the
// tool description says so instead of offering a false that never comes.
func BusyReportsIdle() bool { return false }
