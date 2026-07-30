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

// Trimming is for reading. A prompt ends in a space, so a caller waiting for
// one has to be able to find that space; matching therefore runs against the
// untrimmed screen while the screen shown to a reader stays trimmed.
func TestMatchTextKeepsWhatDisplayTrims(t *testing.T) {
	terminal := NewCharm(40, 5, 100)
	defer terminal.Close()
	feed(t, terminal, "Password: ")

	if line := terminal.Snapshot().Lines[0]; line != "Password:" {
		t.Errorf("the displayed line should be trimmed, got %q", line)
	}
	if !strings.Contains(terminal.MatchText(), "Password: ") {
		t.Error("the searchable screen must keep the trailing space")
	}
}

// Every row is full width, so a reader sees no padding and a searcher sees a
// screen whose columns line up with the terminal's.
func TestMatchTextIsTheFullGrid(t *testing.T) {
	terminal := NewCharm(20, 4, 100)
	defer terminal.Close()
	feed(t, terminal, "ab\r\ncd")

	text := terminal.MatchText()
	if len(text) != 20*4 {
		t.Errorf("expected a %d-cell grid, got %d characters", 20*4, len(text))
	}
	if text[:2] != "ab" || text[20:22] != "cd" {
		t.Errorf("rows should sit at their own offsets, got %q", text[:42])
	}
}

// Text the terminal wrapped across two rows still reads as one string: a
// wrapped row is full by definition, so nothing is inserted between its halves.
func TestMatchTextReadsThroughAWrap(t *testing.T) {
	terminal := NewCharm(10, 4, 100)
	defer terminal.Close()
	feed(t, terminal, "abcdefghijKLMNO")

	if !strings.Contains(terminal.MatchText(), "abcdefghijKLMNO") {
		t.Errorf("a wrapped word should still match, got %q", terminal.MatchText())
	}
}

// The trimming that display does must not lose anything a reader would notice.
// Trailing blanks are invisible; an interior blank row is layout and stays.
func TestDisplayTrimmingLosesOnlyInvisibleBlanks(t *testing.T) {
	terminal := NewCharm(30, 6, 100)
	defer terminal.Close()
	feed(t, terminal, "top\r\n\r\n   indented   \r\nbottom")

	lines := terminal.Snapshot().Lines
	want := []string{"top", "", "   indented", "bottom"}
	if len(lines) != len(want) {
		t.Fatalf("expected %d lines, got %d: %q", len(want), len(lines), lines)
	}
	for index := range want {
		if lines[index] != want[index] {
			t.Errorf("line %d: got %q, want %q", index, lines[index], want[index])
		}
	}
}
