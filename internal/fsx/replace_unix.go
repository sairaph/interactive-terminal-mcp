//go:build !windows

// Package fsx holds small filesystem helpers whose correct implementation
// differs between platforms.
package fsx

import "os"

// Replace atomically moves source over destination.
func Replace(source, destination string) error {
	return os.Rename(source, destination)
}
