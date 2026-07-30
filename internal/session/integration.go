package session

import (
	_ "embed"
	"os"
	"path/filepath"
	"runtime"
)

// Shell integration makes a shell announce where each command starts and ends,
// and how it ended, through OSC 133. It is the only completion signal in this
// project that is reported rather than inferred, and the only source of an
// exit status for a command run inside a session.
//
// The scripts are shipped rather than asked of the user because a tool that
// needs setup before it is accurate is a tool that is usually inaccurate.

//go:embed integration/bash.sh
var bashIntegration string

// integrationSetup describes how to start one shell with integration.
type integrationSetup struct {
	// script is written into the session directory and referenced by args.
	filename string
	contents string
	// args are appended to the shell's own argv, with the script path
	// substituted for the empty final element.
	args func(path string) []string
}

// integrationFor returns how to integrate this shell, if it can be.
//
// Only shells whose integration has been exercised against a real shell are
// listed. A startup script that is wrong breaks the user's shell rather than
// degrading a heuristic, so shipping one on the strength of documentation
// alone is the worse trade.
//
// PowerShell is not here yet. It has no hook between reading a command line
// and running it, so integration has to go through the prompt function, and a
// prompt function that throws costs the user their prompt. A first attempt
// reported exit codes on one run and silently stopped on the next, which is
// not a thing to hand to anyone. Windows keeps the process-tree check for busy
// in the meantime, which is tested and honest about what it cannot see.
func integrationFor(shell Shell) (integrationSetup, bool) {
	switch shell.ID {
	case "bash":
		if runtime.GOOS == "windows" {
			// Git Bash on Windows is a different animal and untested here.
			return integrationSetup{}, false
		}
		return integrationSetup{
			filename: "shell-integration.bash",
			contents: bashIntegration,
			args: func(path string) []string {
				// --init-file replaces bash's startup files, which is why the
				// script sources the user's own before doing anything else.
				return []string{"--init-file", path}
			},
		}, true
	}
	return integrationSetup{}, false
}

// applyIntegration returns argv extended so the shell reports its command
// boundaries, or the original argv unchanged.
//
// Every failure here is silent and harmless: the shell starts exactly as it
// would have, and the tools fall back to watching output and the process
// table. Nothing about a session depends on this succeeding.
func applyIntegration(argv []string, shell Shell, options Options) []string {
	if !options.Integrate {
		return argv
	}
	setup, ok := integrationFor(shell)
	if !ok || options.Directory == "" {
		return argv
	}
	// The session directory is created here rather than waited for: the log
	// store makes it too, but not until after the command has been built.
	if err := os.MkdirAll(options.Directory, 0o700); err != nil {
		return argv
	}
	path := filepath.Join(options.Directory, setup.filename)
	if err := os.WriteFile(path, []byte(setup.contents), 0o600); err != nil {
		return argv
	}
	return append(argv, setup.args(path)...)
}
