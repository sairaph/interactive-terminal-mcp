//go:build !windows

package session

// EnableConsoleInterrupts is a no-op outside Windows. A Unix terminal delivers
// the interrupt itself through the line discipline, with no process-wide
// attribute to restore.
func EnableConsoleInterrupts() error { return nil }
