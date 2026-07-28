package vterm

import (
	"strings"
	"testing"
)

func feed(t *testing.T, terminal *Charm, data string) {
	t.Helper()
	if _, err := terminal.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
}

// A shell that says nothing about its commands must not be reported as if it
// had. Everything built on these marks checks Integrated first, so a false
// positive here would turn silence into a confident wrong answer.
func TestCommandsAreNotClaimedWithoutMarks(t *testing.T) {
	terminal := NewCharm(80, 24, 100)
	defer terminal.Close()
	feed(t, terminal, "$ echo hi\r\nhi\r\n$ ")

	if state := terminal.Commands(); state.Integrated {
		t.Errorf("no marks were emitted, so nothing should be claimed: %+v", state)
	}
}

// The sequence a shell emits around one command: prompt, command line, output,
// then completion with a status.
func TestCommandStateFollowsTheMarks(t *testing.T) {
	terminal := NewCharm(80, 24, 100)
	defer terminal.Close()

	feed(t, terminal, "\x1b]133;A\x07$ ")
	if state := terminal.Commands(); !state.Integrated || state.Running {
		t.Fatalf("a prompt is not a running command: %+v", state)
	}

	feed(t, terminal, "\x1b]133;B\x07make\r\n\x1b]133;C\x07")
	if state := terminal.Commands(); !state.Running {
		t.Fatalf("output has started, so a command is running: %+v", state)
	}

	feed(t, terminal, "building...\r\n\x1b]133;D;0\x07")
	state := terminal.Commands()
	if state.Running {
		t.Error("the command reported completion")
	}
	if !state.HasExit || state.ExitCode != 0 {
		t.Errorf("exit status should be 0, got %+v", state)
	}
	if state.Completed != 1 {
		t.Errorf("one command finished, got %d", state.Completed)
	}
}

// A failing command reports its status, which is the thing no other signal in
// this project can produce.
func TestCommandStateCarriesAFailingExitCode(t *testing.T) {
	terminal := NewCharm(80, 24, 100)
	defer terminal.Close()
	feed(t, terminal, "\x1b]133;A\x07$ \x1b]133;C\x07\x1b]133;D;2\x07")

	state := terminal.Commands()
	if !state.HasExit || state.ExitCode != 2 {
		t.Errorf("expected exit 2, got %+v", state)
	}
}

// Completion without a status is what shells emit for an empty command line or
// an interrupt. It is a completion, but there is no code to report.
func TestCompletionWithoutAStatus(t *testing.T) {
	terminal := NewCharm(80, 24, 100)
	defer terminal.Close()
	feed(t, terminal, "\x1b]133;C\x07\x1b]133;D\x07")

	state := terminal.Commands()
	if state.Running {
		t.Error("the command finished")
	}
	if state.HasExit {
		t.Errorf("no status was reported, so none should be claimed: %+v", state)
	}
}

// Shells append their own fields after the ones this understands, and they do
// not agree on what. Extra fields must not stop a mark being read.
func TestExtraFieldsAreIgnored(t *testing.T) {
	terminal := NewCharm(80, 24, 100)
	defer terminal.Close()
	feed(t, terminal, "\x1b]133;A;special_key=value\x07\x1b]133;C\x07\x1b]133;D;0;extra\x07")

	state := terminal.Commands()
	if !state.HasExit || state.ExitCode != 0 || state.Completed != 1 {
		t.Errorf("the mark should still have been read: %+v", state)
	}
}

// The marks are metadata, not text. If they reached the screen an agent would
// read escape-sequence noise mixed into command output.
func TestMarksDoNotReachTheScreen(t *testing.T) {
	terminal := NewCharm(80, 24, 100)
	defer terminal.Close()
	feed(t, terminal, "\x1b]133;A\x07$ \x1b]133;C\x07hello\r\n\x1b]133;D;0\x07")

	text := terminal.Snapshot().Text()
	if len(text) == 0 || !strings.Contains(text, "hello") {
		t.Fatalf("the real output should be on screen, got %q", text)
	}
	for _, noise := range []string{"133", ";A", ";D"} {
		if strings.Contains(text, noise) {
			t.Errorf("mark text leaked onto the screen (%q): %q", noise, text)
		}
	}
}
