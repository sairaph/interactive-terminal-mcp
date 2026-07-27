package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/daemon"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

// newLiveService wires the MCP service to a real daemon over a real socket, so
// these tests exercise the whole stack rather than a mock of it.
func newLiveService(t *testing.T) *mcp.ClientSession {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("these tests drive a POSIX shell")
	}

	root := t.TempDir()
	socketDir, err := os.MkdirTemp("", "itm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })

	paths := config.Paths{
		Root:     root,
		Config:   filepath.Join(root, "config.toml"),
		Sessions: filepath.Join(root, "sessions"),
		Socket:   filepath.Join(socketDir, "s"),
		Lock:     filepath.Join(root, "daemon.lock"),
	}
	settings := config.Default()
	settings.DaemonIdleShutdownSeconds = 0
	if err := config.Save(paths, settings); err != nil {
		t.Fatal(err)
	}
	settings, err = config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}

	server, err := daemon.Open(paths, settings, "test")
	if err != nil {
		t.Fatalf("daemon.Open: %v", err)
	}
	daemonCtx, stopDaemon := context.WithCancel(context.Background())
	go server.Serve(daemonCtx)
	t.Cleanup(func() {
		stopDaemon()
		server.Close(true)
	})

	dial := func(ctx context.Context) (*ipc.Client, error) {
		return ipc.Connect(ctx, paths.Socket)
	}
	service, err := New(dial, settings, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go service.Run(ctx, serverTransport)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("%s returned %d content blocks", name, len(result.Content))
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s did not return text", name)
	}
	return content.Text, result.IsError
}

func TestAllToolsAreRegistered(t *testing.T) {
	session := newLiveService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, tool := range result.Tools {
		found[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		// A one-line description leaves a model guessing when to reach for the
		// tool at all, which is the expensive kind of mistake.
		if len(tool.Description) < 120 {
			t.Errorf("%s has a thin description (%d chars): %q",
				tool.Name, len(tool.Description), tool.Description)
		}
		// Naming a related tool gives a model a path from one call to the next.
		if !strings.Contains(tool.Description, "it_") {
			t.Errorf("%s should reference a related tool: %q", tool.Name, tool.Description)
		}
	}
	for _, want := range []string{
		"it_active", "it_list", "it_new", "it_read",
		"it_send", "it_kill", "it_tail", "it_head",
	} {
		if !found[want] {
			t.Errorf("%s was not registered", want)
		}
	}
	if len(result.Tools) != 8 {
		t.Errorf("expected exactly 8 tools, got %d", len(result.Tools))
	}
}

// The flow a model is told to follow in the tool descriptions must actually
// work start to finish.
func TestDocumentedWorkflow(t *testing.T) {
	session := newLiveService(t)

	body, isError := callTool(t, session, "it_active", map[string]any{})
	if isError {
		t.Fatalf("it_active on an empty daemon should not be an error:\n%s", body)
	}
	if !strings.Contains(body, "No session is currently active") {
		t.Errorf("it_active should report that nothing is active:\n%s", body)
	}

	body, isError = callTool(t, session, "it_new", map[string]any{"name": "work", "wait": 3})
	if isError {
		t.Fatalf("it_new failed:\n%s", body)
	}
	if !strings.Contains(body, "name: work") || !strings.Contains(body, "running: true") {
		t.Errorf("it_new frontmatter is wrong:\n%s", body)
	}

	body, isError = callTool(t, session, "it_send", map[string]any{
		"text": "PS1=''; echo workflow-ok", "wait": 8,
	})
	if isError {
		t.Fatalf("it_send failed:\n%s", body)
	}
	if !strings.Contains(body, "workflow-ok") {
		t.Errorf("the command output should be on the screen:\n%s", body)
	}

	body, _ = callTool(t, session, "it_read", map[string]any{"session": "work"})
	if !strings.Contains(body, "session: ") {
		t.Errorf("it_read should return a screen document:\n%s", body)
	}

	body, _ = callTool(t, session, "it_list", map[string]any{})
	if !strings.Contains(body, "name: work") {
		t.Errorf("it_list should show the session:\n%s", body)
	}

	body, isError = callTool(t, session, "it_kill", map[string]any{"session": "work"})
	if isError {
		t.Fatalf("it_kill failed:\n%s", body)
	}
	if !strings.Contains(body, "killed: ") {
		t.Errorf("it_kill should report what it ended:\n%s", body)
	}
}

// A string command goes through the shell, so shell syntax has to work; an
// array is executed directly, so no quoting is needed.
func TestCommandAcceptsBothForms(t *testing.T) {
	session := newLiveService(t)

	body, isError := callTool(t, session, "it_new", map[string]any{
		"name": "shellform", "command": "echo one && echo two", "wait": 5,
	})
	if isError {
		t.Fatalf("a string command failed:\n%s", body)
	}
	if !strings.Contains(body, "one") || !strings.Contains(body, "two") {
		t.Errorf("shell syntax should have been interpreted:\n%s", body)
	}

	body, isError = callTool(t, session, "it_new", map[string]any{
		"name": "argvform", "command": []any{"echo", "a b c"}, "wait": 5,
	})
	if isError {
		t.Fatalf("an array command failed:\n%s", body)
	}
	if !strings.Contains(body, "a b c") {
		t.Errorf("an argument containing spaces should survive intact:\n%s", body)
	}
}

func TestKeysDriveAFullScreenProgram(t *testing.T) {
	session := newLiveService(t)

	directory := t.TempDir()
	target := filepath.Join(directory, "numbers.txt")
	var content strings.Builder
	for index := 1; index <= 200; index++ {
		content.WriteString(itoa(index) + "\n")
	}
	if err := os.WriteFile(target, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	body, isError := callTool(t, session, "it_new", map[string]any{
		"name": "pager", "command": []any{"less", "-X", target},
		"cols": 40, "rows": 12, "wait": 4,
	})
	if isError {
		t.Fatalf("starting the pager failed:\n%s", body)
	}
	if !strings.Contains(body, "\n1\n") {
		t.Errorf("the pager should be showing the first lines:\n%s", body)
	}

	// Paging must actually move the pager, which only works if the key bytes
	// are encoded the way it expects.
	body, isError = callTool(t, session, "it_send", map[string]any{
		"session": "pager", "keys": "PAGE_DOWN; PAGE_DOWN", "wait": 4,
	})
	if isError {
		t.Fatalf("sending keys failed:\n%s", body)
	}
	if strings.Contains(body, "\n1\n") {
		t.Errorf("the pager should have scrolled away from the top:\n%s", body)
	}

	callTool(t, session, "it_send", map[string]any{"session": "pager", "keys": "q", "wait": 4})
}

func TestErrorsAreTypedAndActionable(t *testing.T) {
	session := newLiveService(t)

	body, isError := callTool(t, session, "it_read", map[string]any{"session": "nope"})
	if !isError {
		t.Fatal("an unknown session should be an error")
	}
	if !strings.Contains(body, "code: session_not_found") {
		t.Errorf("the error should be typed:\n%s", body)
	}
	if !strings.Contains(body, "it_list()") {
		t.Errorf("the error should name a concrete next call:\n%s", body)
	}

	// it_kill never falls back to the active session.
	body, isError = callTool(t, session, "it_kill", map[string]any{})
	if !isError {
		t.Fatal("it_kill without a session should be an error")
	}

	// Neither text nor keys leaves nothing to send.
	callTool(t, session, "it_new", map[string]any{"wait": 2})
	body, isError = callTool(t, session, "it_send", map[string]any{})
	if !isError {
		t.Fatal("it_send with no input should be an error")
	}
	if !strings.Contains(body, "text, keys, or both") {
		t.Errorf("the error should say what is missing:\n%s", body)
	}

	// An unparseable key sequence must send nothing and say why.
	body, isError = callTool(t, session, "it_send", map[string]any{"keys": "NOT_A_KEY"})
	if !isError {
		t.Fatal("an invalid key sequence should be an error")
	}
	if !strings.Contains(body, "Nothing was sent") {
		t.Errorf("the error should confirm nothing was sent:\n%s", body)
	}
}

func TestWaitCeilingIsEnforced(t *testing.T) {
	session := newLiveService(t)
	callTool(t, session, "it_new", map[string]any{"wait": 2})

	body, isError := callTool(t, session, "it_read", map[string]any{"wait": 100000})
	if !isError {
		t.Fatal("a wait above the configured ceiling should be rejected")
	}
	// The rejection must name the argument and the limit, without the
	// validator's internal framing.
	if !strings.Contains(body, "wait") || !strings.Contains(body, "300") {
		t.Errorf("the error should name the argument and its limit:\n%s", body)
	}
	if strings.Contains(body, "validating root") {
		t.Errorf("the validator's internal framing should be stripped:\n%s", body)
	}
}

// A settle wait is a ceiling, not a sleep: a fast command must return promptly
// even when a generous budget is offered.
func TestGenerousWaitReturnsEarlyForAFastCommand(t *testing.T) {
	session := newLiveService(t)
	callTool(t, session, "it_new", map[string]any{"name": "quick", "wait": 3})

	start := time.Now()
	body, isError := callTool(t, session, "it_send", map[string]any{
		"session": "quick", "text": "PS1=''; echo fast", "wait": 60,
	})
	elapsed := time.Since(start)
	if isError {
		t.Fatalf("it_send failed:\n%s", body)
	}
	if elapsed > 15*time.Second {
		t.Errorf("a generous wait should not be spent on a fast command, took %v", elapsed)
	}
	if !strings.Contains(body, "settled: true") {
		t.Errorf("the result should report that output settled:\n%s", body)
	}
}

func TestTailReturnsLogAndScreen(t *testing.T) {
	session := newLiveService(t)
	callTool(t, session, "it_new", map[string]any{
		"name": "noisy", "rows": 10, "cols": 60,
		"command": "i=1; while [ $i -le 300 ]; do echo line-$i; i=$((i+1)); done; sleep 30",
		"wait":    8,
	})

	body, isError := callTool(t, session, "it_tail", map[string]any{"session": "noisy", "lines": 5})
	if isError {
		t.Fatalf("it_tail failed:\n%s", body)
	}
	if !strings.Contains(body, "Live screen:") {
		t.Errorf("tail should append the live screen by default:\n%s", body)
	}
	if !strings.Contains(body, "total_lines:") {
		t.Errorf("tail should report the log size:\n%s", body)
	}

	body, _ = callTool(t, session, "it_head", map[string]any{"session": "noisy", "lines": 3})
	if !strings.Contains(body, "line-1") {
		t.Errorf("head should start at the oldest output:\n%s", body)
	}
	if strings.Contains(body, "Live screen:") {
		t.Errorf("head should not append the screen:\n%s", body)
	}
}

// Unknown arguments must be rejected rather than silently ignored, so a model
// finds out its call was wrong.
func TestUnknownArgumentsAreRejected(t *testing.T) {
	session := newLiveService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "it_list",
		Arguments: map[string]any{"nonsense": true},
	})
	if err != nil {
		// The SDK may reject it at the protocol layer, which is also correct.
		return
	}
	if !result.IsError {
		t.Error("an unknown argument should be rejected")
	}
	if content, ok := result.Content[0].(*mcp.TextContent); ok {
		if !strings.HasPrefix(content.Text, "---\n") {
			t.Errorf("even a schema failure should use the error document shape:\n%s", content.Text)
		}
	}
}
