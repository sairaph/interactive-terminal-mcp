//go:build windows

package session

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// foregroundBusy reports whether a command is running in the terminal.
//
// A Windows console has no foreground process group that the shell hands
// control to, so there is no equivalent of TIOCGPGRP to ask. What can be
// established is whether the shell has spawned anything: an external program
// run from a prompt is a child process of the shell and lives exactly as long
// as the command does.
//
// This deliberately answers only one of the two questions. A child means a
// command is running, and that is reported. No child does not mean idle:
// PowerShell runs Start-Sleep, Invoke-WebRequest and most other cmdlets inside
// its own process, so a shell that is busy for a minute can have no child the
// whole time. Reporting that as idle would be a confident wrong answer about
// completion, which is the one thing this tool must not produce, so the absence
// is reported as "cannot establish" rather than as "nothing is running".
func (s *Session) foregroundBusy() (busy bool, known bool) {
	// As on Unix, the question only means something for a session running a
	// shell. A session that is one program is that program for its whole life,
	// and liveness answers it.
	if !s.startedShell() {
		return false, false
	}
	pid := commandPID(s.command)
	if pid <= 0 {
		return false, false
	}
	if hasChildProcess(uint32(pid)) {
		return true, true
	}
	return false, false
}

// hasChildProcess reports whether any live process names pid as its parent.
func hasChildProcess(pid uint32) bool {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return false
	}
	for {
		// A snapshot lists only live processes, so a match is a running child.
		if entry.ParentProcessID == pid && entry.ProcessID != pid {
			return true
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			return false
		}
	}
}
