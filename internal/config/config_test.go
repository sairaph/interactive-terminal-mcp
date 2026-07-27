package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	return Paths{
		Root:        root,
		Config:      filepath.Join(root, "config.toml"),
		Sessions:    filepath.Join(root, "sessions"),
		Socket:      filepath.Join(root, "daemon.sock"),
		Lock:        filepath.Join(root, "daemon.lock"),
		Diagnostics: filepath.Join(root, "diagnostics.log"),
	}
}

func TestLoadWritesDefaultsOnFirstRun(t *testing.T) {
	paths := testPaths(t)
	settings, err := Load(paths)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if settings.LogRetention != RetentionOnClose {
		t.Errorf("the default retention should be on_close, got %q", settings.LogRetention)
	}
	// A fresh install should leave a readable, editable document behind rather
	// than relying on in-memory defaults nobody can see.
	if _, err := os.Stat(paths.Config); err != nil {
		t.Errorf("Load should have written the config file: %v", err)
	}

	info, err := os.Stat(paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("config should not be readable by other users, got %v", info.Mode().Perm())
	}
}

func TestSaveAndReloadRoundTrip(t *testing.T) {
	paths := testPaths(t)
	settings, err := Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	settings.DefaultCols = 200
	settings.LogRetention = RetentionWeek
	if err := Save(paths, settings); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := Load(paths)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DefaultCols != 200 || reloaded.LogRetention != RetentionWeek {
		t.Errorf("settings did not survive a round trip: %+v", reloaded)
	}
	// Derived durations are not serialized, so they must be recomputed on load.
	if reloaded.RetentionAfterClose == 0 {
		t.Error("the derived retention duration should be recomputed on load")
	}
}

func TestInvalidSettingsAreRejected(t *testing.T) {
	settings := Default()
	cases := map[string]func(*Config){
		"zero columns":       func(c *Config) { c.DefaultCols = 0 },
		"huge rows":          func(c *Config) { c.DefaultRows = 100_000 },
		"tiny token budget":  func(c *Config) { c.ListTokenBudget = 1 },
		"unknown retention":  func(c *Config) { c.LogRetention = "sometimes" },
		"negative sessions":  func(c *Config) { c.MaximumSessions = 0 },
		"wait above maximum": func(c *Config) { c.DefaultWaitSeconds = c.MaximumWaitSeconds + 1 },
		"bad version":        func(c *Config) { c.Version = 99 },
	}
	for name, mutate := range cases {
		candidate := settings
		mutate(&candidate)
		if err := Validate(candidate); err == nil {
			t.Errorf("%s should have been rejected", name)
		}
	}
}

// One bad edit must not leave a partially applied configuration behind.
func TestSetRejectsWithoutMutating(t *testing.T) {
	settings := Default()
	before := settings.DefaultCols

	if err := settings.Set("size", "not-a-size"); err == nil {
		t.Error("an unparseable size should be rejected")
	}
	if settings.DefaultCols != before {
		t.Error("a rejected edit must not change the configuration")
	}

	if err := settings.Set("size", "10x5"); err == nil {
		t.Error("a size below the supported range should be rejected")
	}
	if settings.DefaultCols != before {
		t.Error("a rejected edit must not change the configuration")
	}

	if err := settings.Set("size", "200x50"); err != nil {
		t.Fatalf("a valid size should be accepted: %v", err)
	}
	if settings.DefaultCols != 200 || settings.DefaultRows != 50 {
		t.Errorf("size not applied: %dx%d", settings.DefaultCols, settings.DefaultRows)
	}
}

func TestSetAcceptsEveryDisplayedSetting(t *testing.T) {
	settings := Default()
	for _, key := range settingKeys {
		raw := settings.RawValue(key)
		if raw == "" {
			t.Errorf("setting %q has no raw value", key)
			continue
		}
		// Re-applying a setting's own current value must always be valid, or
		// the configure screen could not open it for editing.
		if err := settings.Set(key, raw); err != nil {
			t.Errorf("setting %q rejected its own value %q: %v", key, raw, err)
		}
		if settings.Value(key) == "" {
			t.Errorf("setting %q has no display value", key)
		}
	}
}

func TestDiffFromDefaults(t *testing.T) {
	settings := Default()
	if changes := settings.DiffFromDefaults(); len(changes) != 0 {
		t.Errorf("defaults should not differ from themselves, got %v", changes)
	}

	if err := settings.Set("scrollback_lines", "50000"); err != nil {
		t.Fatal(err)
	}
	changes := settings.DiffFromDefaults()
	if len(changes) != 1 || changes[0].Key != "scrollback_lines" {
		t.Fatalf("expected exactly one change, got %v", changes)
	}
	// A restore prompt shows both sides, so both must be populated.
	if changes[0].Current == "" || changes[0].Default == "" {
		t.Errorf("a change should report current and default, got %+v", changes[0])
	}
}

func TestRestoreDefaults(t *testing.T) {
	paths := testPaths(t)
	settings, err := Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.Set("maximum_sessions", "7"); err != nil {
		t.Fatal(err)
	}
	if err := Save(paths, settings); err != nil {
		t.Fatal(err)
	}

	restored, err := RestoreDefaults(paths)
	if err != nil {
		t.Fatalf("RestoreDefaults: %v", err)
	}
	if restored.MaximumSessions != Default().MaximumSessions {
		t.Error("defaults were not restored in memory")
	}
	reloaded, err := Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.MaximumSessions != Default().MaximumSessions {
		t.Error("defaults were not persisted")
	}
}

func TestRetentionDurations(t *testing.T) {
	if _, ok := RetentionDuration(RetentionNever); ok {
		t.Error("never should report that logs are not swept")
	}
	after, ok := RetentionDuration(RetentionOnClose)
	if !ok || after != 0 {
		t.Errorf("on_close should be a zero-length retention, got %v %v", after, ok)
	}
	if RetentionLabel(RetentionOnClose) != "When the session is closed" {
		t.Errorf("unexpected label: %q", RetentionLabel(RetentionOnClose))
	}
	// Every option offered by the UI must be a value the config accepts.
	for _, option := range RetentionOptions {
		settings := Default()
		if err := settings.Set("log_retention", option.Value); err != nil {
			t.Errorf("option %q is offered but rejected: %v", option.Value, err)
		}
	}
}

// A deep home directory would otherwise push the socket past the kernel's
// sun_path limit, which fails at connect time with an undiagnosable message.
func TestSocketPathStaysWithinTheKernelLimit(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("named pipes have no length problem")
	}
	deep := "/home/" + strings.Repeat("verylongdirectoryname/", 12) + ".interactive-terminal-mcp"
	socket := unixSocketPath(deep)
	if len(socket) > maxUnixSocketPath {
		t.Errorf("socket path is %d bytes, over the %d limit: %s", len(socket), maxUnixSocketPath, socket)
	}

	// A short root should still keep everything in the application directory.
	short := t.TempDir()
	if got := unixSocketPath(short); got != filepath.Join(short, "daemon.sock") {
		t.Errorf("a short root should use the application directory, got %s", got)
	}

	// The fallback must be stable, so a second process finds the same daemon.
	if unixSocketPath(deep) != socket {
		t.Error("the fallback socket path must be deterministic")
	}
}
