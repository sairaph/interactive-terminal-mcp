package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	detectharness "github.com/sairaph/detect-harness"
)

// fakeHome builds a home directory containing config files for a couple of
// harnesses, so detection has something real to find.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()

	claude := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claude, []byte(`{"mcpServers":{"unrelated":{"command":"/bin/true"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(home, ".cursor")
	if err := os.MkdirAll(cursor, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cursor, "mcp.json"), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func newFixtureInstaller(t *testing.T, home string) *Installer {
	t.Helper()
	inner, err := detectharness.New(
		detectharness.StdioServer{
			Name:    ServerName,
			Command: "/usr/local/bin/interactive-terminal-mcp",
			Args:    []string{"mcp"},
		},
		detectharness.WithEnvironment(detectharness.DetectOptions{
			Platform: "linux",
			HomeDir:  home,
			Env:      map[string]string{"HOME": home},
		}),
	)
	if err != nil {
		t.Fatalf("detectharness.New: %v", err)
	}
	return &Installer{installer: inner}
}

func TestDetectDistinguishesHarnessStates(t *testing.T) {
	home := fakeHome(t)
	installer := newFixtureInstaller(t, home)
	harnesses := installer.Detect(context.Background())

	if len(harnesses) == 0 {
		t.Fatal("no harnesses were reported")
	}
	byID := map[detectharness.ID]Harness{}
	for _, harness := range harnesses {
		byID[harness.ID] = harness
	}

	claude, ok := byID[detectharness.ClaudeCode]
	if !ok {
		t.Fatal("Claude Code was not reported")
	}
	if claude.State != detectharness.Detected {
		t.Errorf("Claude Code has a config file, so it should be detected, got %q", claude.State)
	}
	if claude.Configured {
		t.Error("a config holding only an unrelated server is not configured for us")
	}
	if claude.StatusText() != "detected" {
		t.Errorf("status text: got %q", claude.StatusText())
	}

	// Detected harnesses must sort first so the rows a user can act on are at
	// the top of the list.
	if harnesses[0].State != detectharness.Detected {
		t.Errorf("detected harnesses should come first, got %q", harnesses[0].State)
	}
}

func TestApplyRegistersAndPreservesUnrelatedEntries(t *testing.T) {
	home := fakeHome(t)
	installer := newFixtureInstaller(t, home)
	harnesses := installer.Detect(context.Background())

	selected := map[detectharness.ID]bool{detectharness.ClaudeCode: true}
	results := installer.Apply(context.Background(), harnesses, selected)
	if len(results) == 0 {
		t.Fatal("no results were reported")
	}

	var applied bool
	for _, result := range results {
		if result.Harness.ID == detectharness.ClaudeCode {
			if result.State != detectharness.Applied {
				t.Fatalf("Claude Code: got %q (%s)", result.State, result.Reason)
			}
			applied = true
		}
	}
	if !applied {
		t.Fatal("Claude Code was not registered")
	}

	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("the written config is not valid JSON: %v", err)
	}

	entry, ok := document.Servers[ServerName]
	if !ok {
		t.Fatal("our server was not written into the config")
	}
	// An absolute path plus the mcp subcommand, because a client launches the
	// server with its own working directory and PATH.
	if !filepath.IsAbs(entry.Command) {
		t.Errorf("the command must be an absolute path, got %q", entry.Command)
	}
	if len(entry.Args) != 1 || entry.Args[0] != "mcp" {
		t.Errorf("args: got %v, want [mcp]", entry.Args)
	}
	// Somebody else's server must survive untouched.
	if _, ok := document.Servers["unrelated"]; !ok {
		t.Error("an unrelated server entry was lost")
	}
}

// Re-running setup is the update path, so it must converge rather than rewrite.
func TestApplyIsIdempotent(t *testing.T) {
	home := fakeHome(t)
	installer := newFixtureInstaller(t, home)
	selected := map[detectharness.ID]bool{detectharness.ClaudeCode: true}

	installer.Apply(context.Background(), installer.Detect(context.Background()), selected)

	harnesses := installer.Detect(context.Background())
	for _, harness := range harnesses {
		if harness.ID == detectharness.ClaudeCode && !harness.Configured {
			t.Fatal("after registering, the harness should report as configured")
		}
	}

	results := installer.Apply(context.Background(), harnesses, selected)
	for _, result := range results {
		if result.Harness.ID == detectharness.ClaudeCode && result.State != detectharness.ApplyNoop {
			t.Errorf("a second apply should be a no-op, got %q", result.State)
		}
	}
}

// Deselecting removes only our entry, and only where we had actually written
// one; it must never touch a harness we never configured.
func TestDeselectingRemovesOnlyOurEntry(t *testing.T) {
	home := fakeHome(t)
	installer := newFixtureInstaller(t, home)
	selected := map[detectharness.ID]bool{detectharness.ClaudeCode: true}
	installer.Apply(context.Background(), installer.Detect(context.Background()), selected)

	harnesses := installer.Detect(context.Background())
	results := installer.Apply(context.Background(), harnesses, map[detectharness.ID]bool{})

	var removed bool
	for _, result := range results {
		if result.Harness.ID == detectharness.ClaudeCode {
			if result.State != detectharness.Applied || result.Action != "remove" {
				t.Fatalf("expected a removal, got state %q action %q", result.State, result.Action)
			}
			removed = true
		}
		// Cursor was never registered, so it must not appear as a change.
		if result.Harness.ID == detectharness.Cursor && result.Action == "remove" {
			t.Error("a harness we never configured should not be touched")
		}
	}
	if !removed {
		t.Fatal("our entry was not removed")
	}

	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), ServerName) {
		t.Error("our entry should be gone from the config")
	}
	if !strings.Contains(string(raw), "unrelated") {
		t.Error("the unrelated entry should have survived the removal")
	}
}

// A same-name entry pointing at something else belongs to someone else. It is
// reported, never silently overwritten.
func TestForeignEntryIsReportedAsAConflict(t *testing.T) {
	home := fakeHome(t)
	foreign := `{"mcpServers":{"` + ServerName + `":{"command":"/somewhere/else","args":["serve"]}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}

	installer := newFixtureInstaller(t, home)
	harnesses := installer.Detect(context.Background())
	results := installer.Apply(context.Background(),
		harnesses, map[detectharness.ID]bool{detectharness.ClaudeCode: true})

	for _, result := range results {
		if result.Harness.ID != detectharness.ClaudeCode {
			continue
		}
		if result.State != detectharness.ApplyConflict {
			t.Fatalf("expected a conflict, got %q", result.State)
		}
		if !strings.Contains(result.Summary(), "another server") {
			t.Errorf("the summary should explain the conflict, got %q", result.Summary())
		}
	}

	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "/somewhere/else") {
		t.Error("a conflicting entry must be left exactly as it was")
	}
}

// An environment that could not be inspected is never treated as absent, and
// never written to.
func TestUninspectableHarnessesAreNotSelectable(t *testing.T) {
	harness := Harness{
		ID: detectharness.Zed, Name: "Zed",
		State: detectharness.Unavailable, Reason: "permission denied",
	}
	if harness.Selectable() {
		t.Error("an uninspectable harness must not be selectable")
	}
	if !strings.Contains(harness.StatusText(), "could not inspect") {
		t.Errorf("status should say it could not be inspected, got %q", harness.StatusText())
	}

	withError := Harness{ID: detectharness.Zed, Name: "Zed", ConfigError: "is a symlink"}
	if withError.Selectable() {
		t.Error("a harness whose config could not be resolved must not be selectable")
	}
}

func TestSettingsRowsCoverEverySetting(t *testing.T) {
	if len(SettingsRows) == 0 {
		t.Fatal("no settings are exposed")
	}
	seen := map[string]bool{}
	for _, row := range SettingsRows {
		if row.Label == "" || row.Help == "" {
			t.Errorf("setting %q needs a label and help text", row.Key)
		}
		if seen[row.Key] {
			t.Errorf("setting %q is listed twice", row.Key)
		}
		seen[row.Key] = true
		if row.Kind == SettingChoice && len(row.Choices) == 0 {
			t.Errorf("choice setting %q has no options", row.Key)
		}
	}
	if !seen["log_retention"] {
		t.Error("session log retention must be configurable; the installer asks about it")
	}
}
