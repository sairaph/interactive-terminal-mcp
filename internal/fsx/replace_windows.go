//go:build windows

package fsx

import (
	"golang.org/x/sys/windows"
)

// Replace atomically moves source over destination. Windows refuses a plain
// rename onto an existing file, so MoveFileEx with REPLACE_EXISTING is used
// and WRITE_THROUGH flushes before returning.
func Replace(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
