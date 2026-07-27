package app

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	detectharness "github.com/sairaph/detect-harness"
	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/install"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

// configureState holds the settings screen, which is the same one the
// installer shows so these settings have exactly one definition and one UI.
type configureState struct {
	harnesses []install.Harness
	selected  map[detectharness.ID]bool
	settings  config.Config

	// panel 0 is harnesses, panel 1 is settings.
	panel  int
	cursor int

	editing bool
	input   *composer
	choice  int

	confirmRestore bool
	results        []install.ApplyResult
	busy           bool
	message        string
}

type configureMsg struct {
	harnesses []install.Harness
	results   []install.ApplyResult
	settings  *config.Config
	err       error
	message   string
}

func (m *model) openConfigure() tea.Cmd {
	m.screen = screenConfigure
	m.configure = &configureState{
		selected: map[detectharness.ID]bool{},
		settings: m.runtime.Config,
		message:  "Loading AI clients…",
		busy:     true,
	}
	runtime := m.runtime
	ctx := m.ctx
	return func() tea.Msg {
		installer, err := install.NewInstaller(runtime.Executable)
		if err != nil {
			return configureMsg{err: err}
		}
		return configureMsg{harnesses: installer.Detect(ctx)}
	}
}

func (m *model) handleConfigure(message configureMsg) (tea.Model, tea.Cmd) {
	state := m.configure
	if state == nil {
		return m, nil
	}
	state.busy = false

	if message.err != nil {
		state.message = styleError.Render(message.err.Error())
		return m, nil
	}
	if message.harnesses != nil {
		state.harnesses = message.harnesses
		for _, harness := range message.harnesses {
			// Pre-select what is already registered, plus anything detected,
			// so confirming without changes is the obvious safe action.
			state.selected[harness.ID] = harness.Configured ||
				(harness.State == detectharness.Detected && harness.Selectable())
		}
		state.message = ""
	}
	if message.settings != nil {
		state.settings = *message.settings
		m.runtime.Config = *message.settings
	}
	if message.results != nil {
		state.results = message.results
	}
	if message.message != "" {
		state.message = message.message
	}
	return m, nil
}

func (m *model) handleConfigureKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	state := m.configure
	if state == nil {
		m.screen = screenHome
		return m, nil
	}
	if state.busy {
		if message.String() == "ctrl+c" {
			m.screen = screenHome
		}
		return m, nil
	}

	if state.confirmRestore {
		switch message.String() {
		case "y", "enter":
			state.confirmRestore = false
			return m, m.restoreDefaults()
		case "n", "esc", "q":
			state.confirmRestore = false
			state.message = "Restore cancelled."
		}
		return m, nil
	}

	if state.editing {
		return m.handleConfigureEdit(message)
	}

	switch message.String() {
	case "esc", "q", "ctrl+c":
		m.screen = screenHome
		m.configure = nil
		return m, m.loadSessions()
	case "tab", "left", "right", "h", "l":
		state.panel = 1 - state.panel
		state.cursor = 0
	case "up", "k":
		state.cursor--
		if state.cursor < 0 {
			state.cursor = state.itemCount() - 1
		}
	case "down", "j":
		state.cursor++
		if state.cursor >= state.itemCount() {
			state.cursor = 0
		}
	case " ":
		if state.panel == 0 {
			state.toggleHarness()
		}
	case "a":
		if state.panel == 0 {
			state.toggleAllHarnesses()
		}
	case "enter":
		if state.panel == 0 {
			state.toggleHarness()
			return m, nil
		}
		state.beginEdit(m.width)
	case "s":
		state.busy = true
		state.message = "Saving…"
		return m, m.saveConfigure()
	case "d":
		if len(state.settings.DiffFromDefaults()) == 0 {
			state.message = "Settings already match the recommended defaults."
			return m, nil
		}
		state.confirmRestore = true
	}
	return m, nil
}

func (m *model) handleConfigureEdit(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	state := m.configure
	row := install.SettingsRows[state.cursor]

	if row.Kind == install.SettingChoice {
		switch message.String() {
		case "up", "k":
			state.choice = (state.choice - 1 + len(row.Choices)) % len(row.Choices)
		case "down", "j":
			state.choice = (state.choice + 1) % len(row.Choices)
		case "enter":
			if err := state.settings.Set(row.Key, row.Choices[state.choice].Value); err != nil {
				state.message = styleError.Render(err.Error())
				return m, nil
			}
			state.editing = false
			state.message = savedHint(row)
		case "esc":
			state.editing = false
		}
		return m, nil
	}

	switch message.String() {
	case "enter":
		if err := state.settings.Set(row.Key, state.input.Value()); err != nil {
			// An invalid value keeps the editor open with the reason attached,
			// so nothing is half-saved and the user can see what to fix.
			state.message = styleError.Render(err.Error())
			return m, nil
		}
		state.editing = false
		state.message = savedHint(row)
	case "esc":
		state.editing = false
	default:
		state.input.update(message)
	}
	return m, nil
}

func savedHint(row install.SettingsRow) string {
	message := "Changed; press s to save."
	if row.Restart != "" {
		message += " " + strings.ToUpper(row.Restart[:1]) + row.Restart[1:] + "."
	}
	return message
}

func (state *configureState) itemCount() int {
	if state.panel == 0 {
		if len(state.harnesses) == 0 {
			return 1
		}
		return len(state.harnesses)
	}
	return len(install.SettingsRows)
}

// toggleAllHarnesses selects every selectable harness, or clears them all if
// they are already selected.
func (state *configureState) toggleAllHarnesses() {
	anyUnselected := false
	for _, harness := range state.harnesses {
		if harness.Selectable() && !state.selected[harness.ID] {
			anyUnselected = true
			break
		}
	}
	for _, harness := range state.harnesses {
		if harness.Selectable() {
			state.selected[harness.ID] = anyUnselected
		}
	}
	if anyUnselected {
		state.message = "Selected every client; press s to save."
	} else {
		state.message = "Cleared every client; press s to save."
	}
}

func (state *configureState) toggleHarness() {
	if state.cursor >= len(state.harnesses) {
		return
	}
	harness := state.harnesses[state.cursor]
	if !harness.Selectable() {
		// An environment that could not be inspected is never written to;
		// guessing at its config would be worse than leaving it alone.
		state.message = harness.Name + " could not be inspected, so it cannot be changed here."
		return
	}
	state.selected[harness.ID] = !state.selected[harness.ID]
}

func (state *configureState) beginEdit(width int) {
	row := install.SettingsRows[state.cursor]
	state.editing = true
	state.message = row.Help

	if row.Kind == install.SettingChoice {
		current := state.settings.RawValue(row.Key)
		state.choice = 0
		for index, choice := range row.Choices {
			if choice.Value == current {
				state.choice = index
			}
		}
		return
	}
	state.input = newComposer()
	state.input.setSize(width-8, 1)
	state.input.Insert(state.settings.RawValue(row.Key))
}

func (m *model) saveConfigure() tea.Cmd {
	state := m.configure
	settings := state.settings
	harnesses := state.harnesses
	selected := map[detectharness.ID]bool{}
	for id, value := range state.selected {
		selected[id] = value
	}
	ctx, runtime := m.ctx, m.runtime

	return func() tea.Msg {
		if err := config.Save(runtime.Paths, settings); err != nil {
			return configureMsg{err: err}
		}
		installer, err := install.NewInstaller(runtime.Executable)
		if err != nil {
			return configureMsg{err: err}
		}
		results := installer.Apply(ctx, harnesses, selected)

		// A running daemon picks up the new settings without a restart, so a
		// change here affects the next session rather than the next reboot.
		if client, err := runtime.Connect(ctx); err == nil {
			defer client.Close()
			var status ipc.Status
			_ = client.Call(ctx, ipc.OpDaemonStatus, nil, &status)
		}

		refreshed := installer.Detect(ctx)
		return configureMsg{
			harnesses: refreshed, results: results, settings: &settings,
			message: "Saved to " + runtime.Paths.Config,
		}
	}
}

func (m *model) restoreDefaults() tea.Cmd {
	runtime := m.runtime
	return func() tea.Msg {
		settings, err := config.RestoreDefaults(runtime.Paths)
		if err != nil {
			return configureMsg{err: err}
		}
		return configureMsg{settings: &settings, message: "Recommended defaults restored and saved."}
	}
}

// --- view -------------------------------------------------------------------

func (m *model) viewConfigure() string {
	state := m.configure
	if state == nil {
		return ""
	}
	if state.confirmRestore {
		return m.viewRestoreConfirm()
	}

	var body strings.Builder
	body.WriteString(panelHeading("AI harnesses", state.panel == 0) + "\n")
	if len(state.harnesses) == 0 {
		body.WriteString(styleDim.Render("  no clients detected") + "\n")
	}
	for index, harness := range state.harnesses {
		mark := styleOff.Render("○")
		if state.selected[harness.ID] {
			mark = styleRunning.Render("●")
		}
		if !harness.Selectable() {
			mark = styleDim.Render("·")
		}
		line := fmt.Sprintf("  %s %-22s %s", mark, harness.Name, harness.StatusText())
		if state.panel == 0 && index == state.cursor {
			line = styleSelect.Render(padTo(">"+stripStyles(line)[1:], m.width-6))
		} else if !harness.Selectable() {
			line = styleDim.Render(stripStyles(line))
		}
		body.WriteString(line + "\n")
	}

	heading := panelHeading("Settings", state.panel == 1)
	if len(state.settings.DiffFromDefaults()) == 0 {
		heading += "   " + styleDim.Render("Recommended defaults")
	}
	body.WriteString("\n" + heading + "\n")
	for index, row := range install.SettingsRows {
		value := state.settings.Value(row.Key)
		if state.editing && state.panel == 1 && index == state.cursor {
			if row.Kind == install.SettingChoice {
				value = "< " + row.Choices[state.choice].Label + " >"
			} else {
				value = state.input.Value() + "_"
			}
		}
		line := fmt.Sprintf("  %-28s %s", row.Label, value)
		if state.panel == 1 && index == state.cursor {
			line = styleSelect.Render(padTo(">"+line[1:], m.width-6))
		}
		body.WriteString(line + "\n")
	}

	if len(state.results) > 0 {
		body.WriteString("\n" + styleDim.Render("  Last apply:") + "\n")
		for _, result := range state.results {
			body.WriteString(fmt.Sprintf("    %-22s %s\n", result.Harness.Name, result.Summary()))
			if result.Changed() && result.ReloadHint != "" {
				body.WriteString(styleDim.Render("      "+result.ReloadHint) + "\n")
			}
		}
	}

	framed := frame(m.width, m.height-4, "interactive-terminal-mcp ── configure", "", body.String())

	var out strings.Builder
	out.WriteString(framed + "\n")
	if state.editing {
		out.WriteString(styleFooter.Render("  enter confirm · esc cancel"))
	} else {
		out.WriteString(styleFooter.Render("  tab panels · ↑↓ select · space toggle · a all/none · enter edit · s save · d restore defaults · esc back"))
	}
	out.WriteByte('\n')
	if state.message != "" {
		out.WriteString("  " + state.message + "\n")
	}
	out.WriteString(styleDim.Render("  Limits are approximate and apply only to retrieved information."))
	return out.String()
}

func (m *model) viewRestoreConfirm() string {
	state := m.configure
	var body strings.Builder
	body.WriteString(styleWarn.Render("  Restore recommended defaults?") + "\n\n")
	// Only the values that would actually change are listed, so the prompt
	// says exactly what confirming costs.
	for _, change := range state.settings.DiffFromDefaults() {
		body.WriteString(fmt.Sprintf("  %-28s %s -> %s\n", labelFor(change.Key), change.Current, change.Default))
	}
	body.WriteString("\n  " + styleDim.Render("Harness registrations and session logs are not affected.") + "\n")
	body.WriteString("\n  y restore · n cancel\n")

	return frame(m.width, m.height-2, "interactive-terminal-mcp ── configure", "", body.String())
}

func labelFor(key string) string {
	for _, row := range install.SettingsRows {
		if row.Key == key {
			return row.Label
		}
	}
	return key
}

func panelHeading(title string, focused bool) string {
	if focused {
		return "  " + styleTitle.Render(title)
	}
	return "  " + styleDim.Render(title)
}
