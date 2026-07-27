// Package config owns the persisted per-user settings shared by the daemon,
// every MCP process, the CLI, and the interactive application.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/sairaph/interactive-terminal-mcp/internal/fsx"
)

const currentVersion = 1

// Retention values accepted by LogRetention. OnClose is the default and is the
// only value that never leaves a log behind after a session ends.
const (
	RetentionOnClose = "on_close"
	RetentionHour    = "1h"
	RetentionFourH   = "4h"
	RetentionDay     = "1d"
	RetentionWeek    = "1w"
	RetentionMonth   = "1mo"
	RetentionNever   = "never"
)

// RetentionOptions is the ordered choice list shown by the installer and the
// configure screen. The first entry is the default.
var RetentionOptions = []struct {
	Value string
	Label string
}{
	{RetentionOnClose, "When the session is closed"},
	{RetentionHour, "After 1 hour"},
	{RetentionFourH, "After 4 hours"},
	{RetentionDay, "After 1 day"},
	{RetentionWeek, "After 1 week"},
	{RetentionMonth, "After 1 month"},
	{RetentionNever, "Never"},
}

// Config is the complete persisted settings document.
type Config struct {
	Version int `toml:"version"`

	ListTokenBudget int `toml:"list_token_budget"`
	ReadTokenBudget int `toml:"read_token_budget"`

	DefaultCols        int `toml:"default_cols"`
	DefaultRows        int `toml:"default_rows"`
	DefaultWaitSeconds int `toml:"default_wait_seconds"`
	SettleQuietMS      int `toml:"settle_quiet_ms"`
	MaximumWaitSeconds int `toml:"maximum_wait_seconds"`

	LogRetention       string `toml:"log_retention"`
	ScrollbackLines    int    `toml:"scrollback_lines"`
	RawLogMaxBytes     int64  `toml:"raw_log_max_bytes"`
	TranscriptMaxLines int    `toml:"transcript_max_lines"`

	DaemonIdleShutdownSeconds int `toml:"daemon_idle_shutdown_seconds"`
	MaximumSessions           int `toml:"maximum_sessions"`

	// Derived at load time; never serialized.
	SettleQuiet         time.Duration `toml:"-"`
	DefaultWait         time.Duration `toml:"-"`
	MaximumWait         time.Duration `toml:"-"`
	DaemonIdleShutdown  time.Duration `toml:"-"`
	RetentionAfterClose time.Duration `toml:"-"`
}

// Paths resolves every location the application owns.
type Paths struct {
	Root        string
	Config      string
	Sessions    string
	Socket      string
	Lock        string
	Diagnostics string
}

// Default returns the recommended configuration.
func Default() Config {
	return Config{
		Version:                   currentVersion,
		ListTokenBudget:           2_000,
		ReadTokenBudget:           4_000,
		DefaultCols:               120,
		DefaultRows:               30,
		DefaultWaitSeconds:        5,
		SettleQuietMS:             250,
		MaximumWaitSeconds:        300,
		LogRetention:              RetentionOnClose,
		ScrollbackLines:           10_000,
		RawLogMaxBytes:            32 << 20,
		TranscriptMaxLines:        200_000,
		DaemonIdleShutdownSeconds: 60,
		MaximumSessions:           50,
	}
}

// DefaultPaths resolves the per-user application directory.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	root := filepath.Join(home, ".interactive-terminal-mcp")
	paths := Paths{
		Root:        root,
		Config:      filepath.Join(root, "config.toml"),
		Sessions:    filepath.Join(root, "sessions"),
		Socket:      filepath.Join(root, "daemon.sock"),
		Lock:        filepath.Join(root, "daemon.lock"),
		Diagnostics: filepath.Join(root, "diagnostics.log"),
	}
	if runtime.GOOS == "windows" {
		// Named pipes are not filesystem paths; the name is derived from the
		// user so two accounts on one machine never share a daemon.
		paths.Socket = `\\.\pipe\interactive-terminal-mcp-` + pipeUser()
		return paths, nil
	}
	paths.Socket = unixSocketPath(root)
	return paths, nil
}

// maxUnixSocketPath is the portable ceiling on a Unix socket path.
//
// sun_path is 108 bytes on Linux and 104 on macOS and the BSDs, including the
// terminating NUL. Exceeding it fails at connect time with a bare "invalid
// argument", which is impossible to diagnose from the message alone, so the
// path is kept short up front instead.
const maxUnixSocketPath = 100

// unixSocketPath picks a socket location that fits the kernel's limit.
//
// The application directory is preferred because it keeps everything in one
// place. A deep home directory can push it past the limit, so a runtime
// directory or the temporary directory is used instead, with a name derived
// from the application root so it stays stable across runs and unique per user.
func unixSocketPath(root string) string {
	preferred := filepath.Join(root, "daemon.sock")
	if len(preferred) <= maxUnixSocketPath {
		return preferred
	}

	name := "itm-" + shortHash(root) + ".sock"
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		// XDG_RUNTIME_DIR is per-user and already mode 0700, which is exactly
		// what a control socket wants.
		if candidate := filepath.Join(runtimeDir, name); len(candidate) <= maxUnixSocketPath {
			return candidate
		}
	}
	return filepath.Join(os.TempDir(), name)
}

// shortHash derives a stable, filename-safe token from a path.
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func pipeUser() string {
	name := os.Getenv("USERNAME")
	if name == "" {
		name = "default"
	}
	safe := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			safe = append(safe, r)
		default:
			safe = append(safe, '_')
		}
	}
	return string(safe)
}

// SessionDir is the directory holding one session's logs and metadata.
func (p Paths) SessionDir(id string) string {
	return filepath.Join(p.Sessions, id)
}

// Load reads the configuration, writing the defaults first when no file
// exists so a fresh install has a readable, editable document immediately.
func Load(paths Paths) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(paths.Config)
	if errors.Is(err, os.ErrNotExist) {
		if err := Save(paths, cfg); err != nil {
			return Config{}, err
		}
		return normalize(cfg)
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return normalize(cfg)
}

// Save validates and atomically publishes the configuration.
func Save(paths Paths, cfg Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	directory := filepath.Dir(paths.Config)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create application directory: %w", err)
	}
	raw, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(directory, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := fsx.Replace(name, paths.Config); err != nil {
		return fmt.Errorf("publish config: %w", err)
	}
	if dir, err := os.Open(directory); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// Validate checks every persisted setting without writing anything.
func Validate(cfg Config) error {
	_, err := normalize(cfg)
	return err
}

// RestoreDefaults atomically replaces the active configuration with the
// recommended defaults. Confirming with the user is a presentation concern.
func RestoreDefaults(paths Paths) (Config, error) {
	cfg := Default()
	if err := Save(paths, cfg); err != nil {
		return Config{}, err
	}
	return normalize(cfg)
}

// RetentionLabel renders a retention value for a human.
func RetentionLabel(value string) string {
	for _, option := range RetentionOptions {
		if option.Value == value {
			return option.Label
		}
	}
	return value
}

// RetentionDuration reports how long after a session exits its logs are kept.
// The second return is false when logs are kept forever.
func RetentionDuration(value string) (time.Duration, bool) {
	switch value {
	case RetentionOnClose:
		return 0, true
	case RetentionHour:
		return time.Hour, true
	case RetentionFourH:
		return 4 * time.Hour, true
	case RetentionDay:
		return 24 * time.Hour, true
	case RetentionWeek:
		return 7 * 24 * time.Hour, true
	case RetentionMonth:
		return 30 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

func normalize(cfg Config) (Config, error) {
	if cfg.Version != currentVersion {
		return Config{}, fmt.Errorf("unsupported config version %d; this binary understands version %d", cfg.Version, currentVersion)
	}
	if cfg.ListTokenBudget < 200 || cfg.ReadTokenBudget < 200 {
		return Config{}, errors.New("token budgets must be at least 200")
	}
	if cfg.ListTokenBudget > 200_000 || cfg.ReadTokenBudget > 200_000 {
		return Config{}, errors.New("token budgets must be at most 200000")
	}
	if cfg.DefaultCols < 20 || cfg.DefaultCols > 1000 {
		return Config{}, errors.New("default_cols must be between 20 and 1000")
	}
	if cfg.DefaultRows < 5 || cfg.DefaultRows > 1000 {
		return Config{}, errors.New("default_rows must be between 5 and 1000")
	}
	if cfg.MaximumWaitSeconds < 1 || cfg.MaximumWaitSeconds > 3600 {
		return Config{}, errors.New("maximum_wait_seconds must be between 1 and 3600")
	}
	if cfg.DefaultWaitSeconds < 0 || cfg.DefaultWaitSeconds > cfg.MaximumWaitSeconds {
		return Config{}, errors.New("default_wait_seconds must be between 0 and maximum_wait_seconds")
	}
	if cfg.SettleQuietMS < 10 || cfg.SettleQuietMS > 60_000 {
		return Config{}, errors.New("settle_quiet_ms must be between 10 and 60000")
	}
	if _, ok := RetentionDuration(cfg.LogRetention); !ok && cfg.LogRetention != RetentionNever {
		return Config{}, fmt.Errorf("log_retention must be one of %s", strings.Join(retentionValues(), ", "))
	}
	if cfg.ScrollbackLines < 100 || cfg.ScrollbackLines > 1_000_000 {
		return Config{}, errors.New("scrollback_lines must be between 100 and 1000000")
	}
	if cfg.RawLogMaxBytes < 1<<20 {
		return Config{}, errors.New("raw_log_max_bytes must be at least 1048576")
	}
	if cfg.TranscriptMaxLines < 1_000 {
		return Config{}, errors.New("transcript_max_lines must be at least 1000")
	}
	if cfg.DaemonIdleShutdownSeconds < 0 || cfg.DaemonIdleShutdownSeconds > 86_400 {
		return Config{}, errors.New("daemon_idle_shutdown_seconds must be between 0 and 86400")
	}
	if cfg.MaximumSessions < 1 || cfg.MaximumSessions > 1_000 {
		return Config{}, errors.New("maximum_sessions must be between 1 and 1000")
	}
	cfg.SettleQuiet = time.Duration(cfg.SettleQuietMS) * time.Millisecond
	cfg.DefaultWait = time.Duration(cfg.DefaultWaitSeconds) * time.Second
	cfg.MaximumWait = time.Duration(cfg.MaximumWaitSeconds) * time.Second
	cfg.DaemonIdleShutdown = time.Duration(cfg.DaemonIdleShutdownSeconds) * time.Second
	cfg.RetentionAfterClose, _ = RetentionDuration(cfg.LogRetention)
	return cfg, nil
}

func retentionValues() []string {
	values := make([]string, 0, len(RetentionOptions))
	for _, option := range RetentionOptions {
		values = append(values, option.Value)
	}
	return values
}

// Choice is one option in a multiple-choice setting.
type Choice struct {
	Value string
	Label string
}

// RetentionChoices renders the retention options for a picker.
func RetentionChoices() []Choice {
	choices := make([]Choice, 0, len(RetentionOptions))
	for _, option := range RetentionOptions {
		choices = append(choices, Choice{Value: option.Value, Label: option.Label})
	}
	return choices
}

// Value reads a setting by key as display text.
func (c Config) Value(key string) string {
	switch key {
	case "size":
		return fmt.Sprintf("%dx%d", c.DefaultCols, c.DefaultRows)
	case "default_wait_seconds":
		return fmt.Sprintf("%ds", c.DefaultWaitSeconds)
	case "list_token_budget":
		return fmt.Sprintf("~%d tokens", c.ListTokenBudget)
	case "read_token_budget":
		return fmt.Sprintf("~%d tokens", c.ReadTokenBudget)
	case "log_retention":
		return RetentionLabel(c.LogRetention)
	case "scrollback_lines":
		return fmt.Sprintf("%d lines", c.ScrollbackLines)
	case "maximum_sessions":
		return fmt.Sprint(c.MaximumSessions)
	case "daemon_idle_shutdown_seconds":
		if c.DaemonIdleShutdownSeconds == 0 {
			return "never"
		}
		return fmt.Sprintf("%ds", c.DaemonIdleShutdownSeconds)
	default:
		return ""
	}
}

// RawValue reads a setting by key as its editable text.
func (c Config) RawValue(key string) string {
	switch key {
	case "size":
		return fmt.Sprintf("%dx%d", c.DefaultCols, c.DefaultRows)
	case "default_wait_seconds":
		return strconv.Itoa(c.DefaultWaitSeconds)
	case "list_token_budget":
		return strconv.Itoa(c.ListTokenBudget)
	case "read_token_budget":
		return strconv.Itoa(c.ReadTokenBudget)
	case "log_retention":
		return c.LogRetention
	case "scrollback_lines":
		return strconv.Itoa(c.ScrollbackLines)
	case "maximum_sessions":
		return strconv.Itoa(c.MaximumSessions)
	case "daemon_idle_shutdown_seconds":
		return strconv.Itoa(c.DaemonIdleShutdownSeconds)
	default:
		return ""
	}
}

// Set applies a setting by key, validating the whole configuration before
// accepting it so one bad edit cannot leave a partially valid document.
func (c *Config) Set(key, raw string) error {
	next := *c
	raw = strings.TrimSpace(raw)
	switch key {
	case "size":
		cols, rows, err := parseSize(raw)
		if err != nil {
			return err
		}
		next.DefaultCols, next.DefaultRows = cols, rows
	case "log_retention":
		if _, ok := RetentionDuration(raw); !ok && raw != RetentionNever {
			return fmt.Errorf("retention must be one of %s", strings.Join(retentionValues(), ", "))
		}
		next.LogRetention = raw
	default:
		value, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%s must be a whole number", key)
		}
		switch key {
		case "default_wait_seconds":
			next.DefaultWaitSeconds = value
		case "list_token_budget":
			next.ListTokenBudget = value
		case "read_token_budget":
			next.ReadTokenBudget = value
		case "scrollback_lines":
			next.ScrollbackLines = value
		case "maximum_sessions":
			next.MaximumSessions = value
		case "daemon_idle_shutdown_seconds":
			next.DaemonIdleShutdownSeconds = value
		default:
			return fmt.Errorf("unknown setting %q", key)
		}
	}
	normalized, err := normalize(next)
	if err != nil {
		return err
	}
	*c = normalized
	return nil
}

func parseSize(raw string) (int, int, error) {
	parts := strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool {
		return r == 'x' || r == ' ' || r == ','
	})
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("size must look like 120x30")
	}
	cols, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("size must look like 120x30")
	}
	rows, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("size must look like 120x30")
	}
	return cols, rows, nil
}

// DiffFromDefaults lists settings that differ from the recommended defaults,
// so a restore prompt can show exactly what would change.
func (c Config) DiffFromDefaults() []Change {
	defaults := Default()
	var changes []Change
	for _, key := range settingKeys {
		current, recommended := c.Value(key), defaults.Value(key)
		if current != recommended {
			changes = append(changes, Change{Key: key, Current: current, Default: recommended})
		}
	}
	return changes
}

// Change is one setting that differs from its default.
type Change struct {
	Key     string
	Current string
	Default string
}

var settingKeys = []string{
	"size", "default_wait_seconds", "list_token_budget", "read_token_budget",
	"log_retention", "scrollback_lines", "maximum_sessions", "daemon_idle_shutdown_seconds",
}
