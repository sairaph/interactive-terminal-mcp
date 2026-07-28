package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Shell is one command interpreter a session can be started with.
type Shell struct {
	// ID is the short name a caller passes, such as "powershell" or "bash".
	ID string
	// Path is the resolved executable.
	Path string
	// Display is how the shell is named to a person.
	Display string
	// commandFlag introduces a command line, "-lc" or "/c".
	commandFlag []string
}

// Argv builds the command line that runs one command through this shell.
func (s Shell) Argv(commandLine string) []string {
	return append(append([]string{s.Path}, s.commandFlag...), commandLine)
}

// candidates are the shells looked for, best first. On Windows PowerShell
// leads because it is what people and agents actually write: an agent reaching
// for $LASTEXITCODE against cmd.exe gets silence rather than an error, which is
// the kind of mismatch that wastes a whole exchange to discover.
func candidates() []Shell {
	if runtime.GOOS == "windows" {
		return []Shell{
			{ID: "pwsh", Display: "PowerShell 7", commandFlag: []string{"-NoLogo", "-Command"}},
			{ID: "powershell", Display: "Windows PowerShell", commandFlag: []string{"-NoLogo", "-Command"}},
			{ID: "cmd", Display: "Command Prompt", commandFlag: []string{"/c"}},
		}
	}
	return []Shell{
		{ID: "bash", Display: "bash", commandFlag: []string{"-lc"}},
		{ID: "zsh", Display: "zsh", commandFlag: []string{"-lc"}},
		{ID: "fish", Display: "fish", commandFlag: []string{"-c"}},
		{ID: "sh", Display: "sh", commandFlag: []string{"-lc"}},
	}
}

// executableNames lists the file names a candidate may be installed under.
func executableNames(id string) []string {
	if runtime.GOOS == "windows" {
		switch id {
		case "pwsh":
			return []string{"pwsh.exe"}
		case "powershell":
			return []string{"powershell.exe"}
		case "cmd":
			return []string{"cmd.exe"}
		}
		return nil
	}
	return []string{id}
}

// shellForProgram identifies a shell launched directly as a command.
//
// A session started as ["cmd.exe"] is running the same interpreter as one
// started with shell: "cmd", and reporting a shell for one but not the other
// left an agent unable to tell what syntax the session expected. Only a bare
// invocation counts: `bash -c ...` runs one command and exits, which is not
// the interactive shell the label describes.
func shellForProgram(argv []string) (Shell, bool) {
	if len(argv) != 1 {
		return Shell{}, false
	}
	name := strings.ToLower(filepath.Base(argv[0]))
	for _, candidate := range candidates() {
		for _, executable := range executableNames(candidate.ID) {
			if name == strings.ToLower(executable) {
				candidate.Path = argv[0]
				return candidate, true
			}
		}
	}
	return Shell{}, false
}

// AvailableShells returns the shells present on this machine, best first.
func AvailableShells() []Shell {
	var found []Shell
	for _, candidate := range candidates() {
		for _, name := range executableNames(candidate.ID) {
			path, err := exec.LookPath(name)
			if err != nil {
				continue
			}
			candidate.Path = path
			found = append(found, candidate)
			break
		}
	}

	// A shell the user has explicitly chosen outranks anything discovered,
	// because it is the one their own terminal opens with.
	if preferred, ok := preferredShell(); ok {
		for index, shell := range found {
			if strings.EqualFold(shell.Path, preferred) {
				found = append(found[:index], found[index+1:]...)
				found = append([]Shell{shell}, found...)
				break
			}
		}
	}
	return found
}

// preferredShell reads the shell the environment says the user prefers.
//
// COMSPEC is deliberately not consulted on Windows. It names the legacy
// command processor on essentially every machine, so honouring it would pin
// every session to cmd.exe regardless of what is installed.
func preferredShell() (string, bool) {
	if runtime.GOOS == "windows" {
		return "", false
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		return "", false
	}
	if _, err := os.Stat(shell); err != nil {
		return "", false
	}
	return shell, true
}

// DefaultShell is the shell a session starts with when none is named.
func DefaultShell() Shell {
	if available := AvailableShells(); len(available) > 0 {
		return available[0]
	}
	// Nothing was found, which should not happen on a usable system. Fall back
	// to the one interpreter each platform is guaranteed to have.
	if runtime.GOOS == "windows" {
		return Shell{ID: "cmd", Path: "cmd.exe", Display: "Command Prompt", commandFlag: []string{"/c"}}
	}
	return Shell{ID: "sh", Path: "/bin/sh", Display: "sh", commandFlag: []string{"-lc"}}
}

// ResolveShell finds the shell a caller asked for by its short name.
func ResolveShell(id string) (Shell, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return DefaultShell(), nil
	}
	available := AvailableShells()
	for _, shell := range available {
		if shell.ID == id {
			return shell, nil
		}
	}
	// Accept a full path too, so an unusual shell is still reachable.
	if strings.ContainsAny(id, `/\`) {
		if _, err := os.Stat(id); err == nil {
			return Shell{ID: id, Path: id, Display: id, commandFlag: DefaultShell().commandFlag}, nil
		}
	}
	return Shell{}, fmt.Errorf("shell %q is not available here; installed shells are %s",
		id, strings.Join(ShellIDs(), ", "))
}

// ShellIDs lists the short names accepted on this machine.
func ShellIDs() []string {
	available := AvailableShells()
	ids := make([]string, 0, len(available))
	for _, shell := range available {
		ids = append(ids, shell.ID)
	}
	sort.Strings(ids)
	return ids
}
