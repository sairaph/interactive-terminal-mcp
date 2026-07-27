// Package install registers the MCP server with the AI harnesses on this
// machine, and owns the settings flow shared by the installer and the
// interactive application.
package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	detectharness "github.com/sairaph/detect-harness"
	"github.com/sairaph/interactive-terminal-mcp/internal/bootstrap"
	"github.com/sairaph/interactive-terminal-mcp/internal/cli"
	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"golang.org/x/term"
)

// ServerName is the entry name written into every harness configuration.
const ServerName = "interactive-terminal-mcp"

// ErrCancelled is returned when the user aborts an interactive flow.
var ErrCancelled = errors.New("cancelled")

// Harness is one detected AI client, in the shape the UI needs.
type Harness struct {
	ID   detectharness.ID
	Name string
	// State is present, absent, or unavailable. Unavailable is kept distinct
	// from absent so a permission error is never shown as "not installed".
	State       detectharness.DetectionState
	Configured  bool
	ConfigPath  string
	ConfigError string
	Reason      string
	ReloadHint  string
}

// Selectable reports whether the user may toggle this harness. An environment
// that could not be inspected is shown but cannot be chosen, because writing
// to it would be guesswork.
func (h Harness) Selectable() bool {
	return h.State != detectharness.Unavailable && h.ConfigError == ""
}

// StatusText renders the harness state for a person, in at most two words.
//
// A client that cannot be used is still just "not detected" as far as the list
// is concerned: the user wants to know whether it is there, not why the
// library could not reach it. The reason, when there is one, is available in
// Detail for the highlighted row.
func (h Harness) StatusText() string {
	switch {
	case h.Configured:
		return "configured"
	case h.State == detectharness.Detected:
		return "detected"
	default:
		return "not detected"
	}
}

// Installer wraps detect-harness with this project's server definition.
type Installer struct {
	installer *detectharness.Installer
}

// NewInstaller builds an installer that registers the given executable.
//
// The absolute path is written into every harness config rather than a bare
// command name: a harness launches the server with its own working directory
// and PATH, neither of which can be relied on.
func NewInstaller(executable string) (*Installer, error) {
	if executable == "" {
		resolved, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve executable: %w", err)
		}
		executable = resolved
	}
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}

	inner, err := detectharness.New(detectharness.StdioServer{
		Name:    ServerName,
		Command: absolute,
		Args:    []string{"mcp"},
	})
	if err != nil {
		return nil, err
	}
	return &Installer{installer: inner}, nil
}

// Detect probes every supported harness.
func (i *Installer) Detect(ctx context.Context) []Harness {
	detections := i.installer.Detect(ctx)

	// A harness is "configured" when planning a registration would change
	// nothing, which is the only reliable way to ask the library whether the
	// entry is already exactly right.
	ids := make([]detectharness.ID, 0, len(detections))
	for _, detection := range detections {
		ids = append(ids, detection.ID)
	}
	states := map[detectharness.ID]detectharness.ChangeState{}
	for _, change := range i.installer.Plan(ctx, ids, detectharness.Present, detectharness.PlanOptions{}).Changes() {
		states[change.HarnessID] = change.State
	}

	harnesses := make([]Harness, 0, len(detections))
	for _, detection := range detections {
		harnesses = append(harnesses, Harness{
			ID: detection.ID, Name: detection.Name, State: detection.State,
			Configured:  states[detection.ID] == detectharness.ChangeNoop,
			ConfigPath:  detection.ConfigPath,
			ConfigError: detection.ConfigError,
			Reason:      detection.Reason,
			ReloadHint:  detection.ReloadHint,
		})
	}
	sort.SliceStable(harnesses, func(a, b int) bool {
		left, right := harnesses[a], harnesses[b]
		// Detected first so the rows a user can act on are at the top.
		if (left.State == detectharness.Detected) != (right.State == detectharness.Detected) {
			return left.State == detectharness.Detected
		}
		return left.Name < right.Name
	})
	return harnesses
}

// ApplyResult is the outcome for one harness.
type ApplyResult struct {
	Harness    Harness
	State      detectharness.ApplyState
	Action     string
	Reason     string
	ReloadHint string
}

// Changed reports whether the harness configuration was actually written.
func (r ApplyResult) Changed() bool { return r.State == detectharness.Applied }

// Summary renders the outcome for a person.
func (r ApplyResult) Summary() string {
	switch r.State {
	case detectharness.Applied:
		if r.Action == "remove" {
			return "removed"
		}
		return "registered"
	case detectharness.ApplyNoop:
		return "already correct"
	case detectharness.ApplySkipped:
		return "skipped: " + r.Reason
	case detectharness.ApplyConflict:
		// A same-name entry pointing somewhere else is never overwritten
		// silently; the user is told so they can decide.
		return "conflict: another server is already registered under this name"
	case detectharness.ApplyFailed:
		return "failed: " + r.Reason
	default:
		return string(r.State)
	}
}

// Apply registers the selected harnesses and unregisters the deselected ones.
func (i *Installer) Apply(ctx context.Context, harnesses []Harness, selected map[detectharness.ID]bool) []ApplyResult {
	byID := map[detectharness.ID]Harness{}
	var add, remove []detectharness.ID
	for _, harness := range harnesses {
		byID[harness.ID] = harness
		if !harness.Selectable() {
			continue
		}
		if selected[harness.ID] {
			add = append(add, harness.ID)
			continue
		}
		// Only registered harnesses are unregistered, so deselecting one that
		// was never configured is not a write.
		if harness.Configured {
			remove = append(remove, harness.ID)
		}
	}

	var results []ApplyResult
	collect := func(planned []detectharness.Result) {
		for _, item := range planned {
			harness := byID[item.HarnessID]
			results = append(results, ApplyResult{
				Harness: harness, State: item.State, Action: item.Action,
				Reason: item.Reason, ReloadHint: harness.ReloadHint,
			})
		}
	}
	if len(add) > 0 {
		collect(i.installer.Ensure(ctx, add, detectharness.Present, detectharness.PlanOptions{}))
	}
	if len(remove) > 0 {
		collect(i.installer.Ensure(ctx, remove, detectharness.Absent, detectharness.PlanOptions{}))
	}
	return results
}

// Run handles configure, install, and uninstall.
func Run(ctx context.Context, runtime *bootstrap.Runtime, command cli.Command, options cli.Options) int {
	installer, err := NewInstaller(runtime.Executable)
	if err != nil {
		fmt.Fprintln(options.Stderr, "interactive-terminal-mcp:", err)
		return 1
	}
	harnesses := installer.Detect(ctx)

	// An explicit selection, or no terminal to ask on, means unattended mode.
	// Piping the installer through a shell is the normal install path, so it
	// must do something sensible rather than failing on a missing TTY.
	unattended := len(command.Clients) > 0 || command.All || command.Yes || !interactive(options)

	if command.Name == "uninstall" {
		return runUnattended(ctx, installer, harnesses, command, options, true)
	}
	if unattended {
		return runUnattended(ctx, installer, harnesses, command, options, false)
	}
	return runInteractive(ctx, runtime, installer, harnesses, options)
}

func runUnattended(ctx context.Context, installer *Installer, harnesses []Harness, command cli.Command, options cli.Options, remove bool) int {
	selected := map[detectharness.ID]bool{}
	if !remove {
		switch {
		case len(command.Clients) > 0:
			known := map[string]detectharness.ID{}
			for _, harness := range harnesses {
				known[string(harness.ID)] = harness.ID
			}
			for _, raw := range command.Clients {
				name := strings.TrimSpace(raw)
				id, ok := known[name]
				if !ok {
					fmt.Fprintf(options.Stderr, "interactive-terminal-mcp: unknown client %q\n", name)
					fmt.Fprintf(options.Stderr, "  known clients: %s\n", strings.Join(harnessIDs(harnesses), ", "))
					return 2
				}
				selected[id] = true
			}
		case command.All:
			for _, harness := range harnesses {
				if harness.Selectable() {
					selected[harness.ID] = true
				}
			}
		default:
			// Default to the harnesses actually present. Registering with a
			// client that is not installed writes a config file for software
			// the user does not have.
			for _, harness := range harnesses {
				if harness.State == detectharness.Detected && harness.Selectable() {
					selected[harness.ID] = true
				}
			}
		}
	}

	results := installer.Apply(ctx, harnesses, selected)
	if len(results) == 0 {
		if remove {
			fmt.Fprintln(options.Stdout, "interactive-terminal-mcp was not registered with any client.")
		} else {
			fmt.Fprintln(options.Stdout, "No AI clients were detected. Install one, then run `interactive-terminal-mcp configure`.")
		}
		return 0
	}
	return report(results, options)
}

func report(results []ApplyResult, options cli.Options) int {
	failures := 0
	changed := false
	for _, result := range results {
		fmt.Fprintf(options.Stdout, "  %-24s %s\n", result.Harness.Name, result.Summary())
		if result.State == detectharness.ApplyFailed || result.State == detectharness.ApplyConflict {
			failures++
		}
		if result.Changed() {
			changed = true
		}
	}
	if changed {
		fmt.Fprintln(options.Stdout, "\nRestart the affected clients so they pick up the change:")
		for _, result := range results {
			if result.Changed() && result.ReloadHint != "" {
				fmt.Fprintf(options.Stdout, "  %-24s %s\n", result.Harness.Name, result.ReloadHint)
			}
		}
	}
	if failures > 0 {
		return 1
	}
	return 0
}

func harnessIDs(harnesses []Harness) []string {
	ids := make([]string, 0, len(harnesses))
	for _, harness := range harnesses {
		ids = append(ids, string(harness.ID))
	}
	sort.Strings(ids)
	return ids
}

func interactive(options cli.Options) bool {
	stdin, ok := options.Stdin.(*os.File)
	if !ok {
		return false
	}
	stdout, ok := options.Stdout.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(stdin.Fd())) && term.IsTerminal(int(stdout.Fd()))
}

// SettingsRow describes one editable setting, shared by the installer and the
// application's configure screen so there is exactly one definition of each.
type SettingsRow struct {
	Key     string
	Label   string
	Help    string
	Kind    SettingKind
	Choices []config.Choice
	Restart string
}

// SettingKind selects the editor a row uses.
type SettingKind int

// Setting kinds.
const (
	SettingInt SettingKind = iota
	SettingChoice
	SettingSize
)

// SettingsRows is the ordered list of settings shown to a user.
var SettingsRows = []SettingsRow{
	{Key: "size", Label: "Terminal size", Kind: SettingSize,
		Help:    "Columns x rows for new sessions.",
		Restart: "applies to sessions created from now on"},
	{Key: "default_wait_seconds", Label: "Default wait", Kind: SettingInt,
		Help:    "Seconds it_send waits for output to settle before returning.",
		Restart: "restart your AI clients so they see the new default"},
	{Key: "list_token_budget", Label: "List output", Kind: SettingInt,
		Help: "Approximate token budget for it_list."},
	{Key: "read_token_budget", Label: "Log output", Kind: SettingInt,
		Help: "Approximate token budget for it_tail and it_head."},
	{Key: "log_retention", Label: "Session log retention", Kind: SettingChoice,
		Choices: config.RetentionChoices(),
		Help:    "How long a closed session's log is kept. Logs let it_tail reach past the visible screen."},
	{Key: "scrollback_lines", Label: "Scrollback", Kind: SettingInt,
		Help: "Lines kept above each session's screen."},
	{Key: "maximum_sessions", Label: "Maximum sessions", Kind: SettingInt,
		Help: "How many sessions may run at once."},
	{Key: "daemon_idle_shutdown_seconds", Label: "Daemon idle shutdown", Kind: SettingInt,
		Help:    "Seconds with no sessions and no clients before the daemon exits. Zero keeps it running.",
		Restart: "takes effect the next time the daemon starts"},
}
