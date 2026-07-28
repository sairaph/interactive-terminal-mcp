//go:build !windows

package session

// IgnoreConsoleInterrupts is a no-op outside Windows. A Unix daemon is
// detached from any controlling terminal, so no terminal-generated signal
// reaches it in the first place.
func IgnoreConsoleInterrupts() error { return nil }

// InterruptHelperCommand exists so the CLI can reference one name on every
// platform. Nothing dispatches to it outside Windows.
const InterruptHelperCommand = "__raise-interrupt"

// RunInterruptHelper has no work to do outside Windows: a Unix terminal turns
// the interrupt character into a signal itself, so nothing needs to be raised
// out of band.
func RunInterruptHelper(pid int) error { return nil }
