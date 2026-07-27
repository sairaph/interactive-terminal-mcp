package cli

import (
	"errors"
	"strings"
	"testing"
)

func TestParseBareInvocationOpensTheApp(t *testing.T) {
	command, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if command.Name != "app" {
		t.Errorf("got %q, want app", command.Name)
	}
}

func TestParseSimpleCommands(t *testing.T) {
	cases := map[string]string{
		"help": "help", "-h": "help", "--help": "help",
		"version": "version", "-v": "version", "--version": "version",
		"mcp": "mcp", "doctor": "doctor", "status": "status",
		"ls": "ls", "list": "ls",
	}
	for input, want := range cases {
		command, err := Parse([]string{input})
		if err != nil {
			t.Errorf("Parse(%q): %v", input, err)
			continue
		}
		if command.Name != want {
			t.Errorf("Parse(%q): got %q, want %q", input, command.Name, want)
		}
	}
}

func TestParseNew(t *testing.T) {
	command, err := Parse([]string{"new", "build", "--cmd", "make -j8", "--cols", "100", "--rows", "40", "--wait", "2.5"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Name2 != "build" || command.Command != "make -j8" {
		t.Errorf("name/command: got %q %q", command.Name2, command.Command)
	}
	if command.Cols != 100 || command.Rows != 40 {
		t.Errorf("size: got %dx%d", command.Cols, command.Rows)
	}
	if !command.WaitSet || command.Wait != 2.5 {
		t.Errorf("wait: got %v (set=%v)", command.Wait, command.WaitSet)
	}
}

func TestParseSend(t *testing.T) {
	command, err := Parse([]string{"send", "build", "--text", "make", "--no-enter"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Session != "build" || command.Text != "make" || !command.NoEnter {
		t.Errorf("unexpected parse: %+v", command)
	}

	command, err = Parse([]string{"send", "--keys", "CTRL+C"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Keys != "CTRL+C" || command.Session != "" {
		t.Errorf("send should accept keys with no session: %+v", command)
	}
}

// The CLI mirrors the tool contract: kill never guesses its target.
func TestKillRequiresASession(t *testing.T) {
	_, err := Parse([]string{"kill"})
	if err == nil {
		t.Fatal("kill with no session should fail")
	}
	if !errors.Is(err, ErrUsage) {
		t.Errorf("expected a usage error, got %v", err)
	}
	if !strings.Contains(err.Error(), "never inferred") {
		t.Errorf("the error should explain why, got %q", err)
	}

	if _, err := Parse([]string{"kill", "build"}); err != nil {
		t.Errorf("kill with a session should parse: %v", err)
	}
}

func TestSendRequiresInput(t *testing.T) {
	_, err := Parse([]string{"send", "build"})
	if err == nil {
		t.Fatal("send with neither text nor keys should fail")
	}
	if !strings.Contains(err.Error(), "--text or --keys") {
		t.Errorf("the error should name the missing flags, got %q", err)
	}
}

func TestParseRejectsUnknownInput(t *testing.T) {
	cases := [][]string{
		{"nonsense"},
		{"mcp", "extra"},
		{"new", "--nope"},
		{"new", "one", "two"},
		{"tail", "--lines"},
		{"new", "--cols", "wide"},
		{"read", "--wait", "soon"},
		{"daemon", "--nope"},
		{"rename", "only-one"},
	}
	for _, args := range cases {
		if _, err := Parse(args); err == nil {
			t.Errorf("Parse(%v) should have failed", args)
		} else if !errors.Is(err, ErrUsage) {
			t.Errorf("Parse(%v): expected a usage error, got %v", args, err)
		}
	}
}

func TestParseDaemonAndConfigureFlags(t *testing.T) {
	command, err := Parse([]string{"daemon", "--detach"})
	if err != nil || !command.Detach {
		t.Errorf("daemon --detach: %+v %v", command, err)
	}
	command, err = Parse([]string{"daemon", "--stop", "--kill-sessions"})
	if err != nil || !command.Stop || !command.Kill {
		t.Errorf("daemon --stop --kill-sessions: %+v %v", command, err)
	}

	command, err = Parse([]string{"configure", "--client", "cursor,codex", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(command.Clients) != 2 || command.Clients[0] != "cursor" || !command.Yes {
		t.Errorf("configure flags: %+v", command)
	}
}

func TestParseLogCommands(t *testing.T) {
	for _, name := range []string{"tail", "head"} {
		command, err := Parse([]string{name, "build", "-n", "50"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if command.Session != "build" || command.Lines != 50 {
			t.Errorf("%s: %+v", name, command)
		}
	}
}

func TestParseRename(t *testing.T) {
	command, err := Parse([]string{"rename", "t-abc123", "build"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Session != "t-abc123" || command.Name2 != "build" {
		t.Errorf("rename: %+v", command)
	}
}

// The daemon's agent-facing hints name tool calls; a person at a terminal
// needs the equivalent shell command instead.
func TestHintsAreRewrittenForPeople(t *testing.T) {
	hint := humanHint("Call it_list() to see existing sessions, or it_new({}) to create one.")
	if strings.Contains(hint, "it_list()") || strings.Contains(hint, "it_new({})") {
		t.Errorf("tool calls should be rewritten for a person, got %q", hint)
	}
	if !strings.Contains(hint, "interactive-terminal-mcp ls") {
		t.Errorf("the hint should name the shell command, got %q", hint)
	}
}

func TestUsageCoversEveryCommand(t *testing.T) {
	for _, name := range []string{
		"mcp", "configure", "ls", "new", "attach", "read",
		"send", "tail", "head", "rename", "kill", "daemon", "status", "doctor",
	} {
		if !strings.Contains(Usage, name) {
			t.Errorf("usage text does not mention %q", name)
		}
	}
}
