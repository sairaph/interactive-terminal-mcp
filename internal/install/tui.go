package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	detectharness "github.com/sairaph/detect-harness"
	"github.com/sairaph/interactive-terminal-mcp/internal/bootstrap"
	"github.com/sairaph/interactive-terminal-mcp/internal/cli"
	"github.com/sairaph/interactive-terminal-mcp/internal/config"
)

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleCursor = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	styleOn     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleOff    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleError  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleHint   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleFooter = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)

// step is one screen of the installer.
type step int

const (
	// stepDetecting is first because detection walks the filesystem looking for
	// thirteen clients, which takes a noticeable moment. It used to run before
	// the program started, so the installer printed its progress bar and then
	// showed nothing at all until it finished.
	stepDetecting step = iota
	stepHarnesses
	stepRetention
	stepSummary
	stepSettings
	stepApplying
	stepDone
)

// installModel is the whole installer flow in one program, so the screen
// evolves in place instead of appending a new block per question.
type installModel struct {
	ctx       context.Context
	runtime   *bootstrap.Runtime
	installer *Installer

	step      step
	frame     int
	harnesses []Harness
	selected  map[detectharness.ID]bool
	cursor    int
	showAll   bool

	settings      config.Config
	original      config.Config
	settingCursor int
	editing       bool
	input         string
	choiceCursor  int

	results []ApplyResult
	failure string
	message string
	cancel  bool
}

func runInteractive(ctx context.Context, runtime *bootstrap.Runtime, installer *Installer, options cli.Options) int {
	state := &installModel{
		ctx: ctx, runtime: runtime, installer: installer,
		step:     stepDetecting,
		selected: map[detectharness.ID]bool{},
		settings: runtime.Config,
		original: runtime.Config,
	}

	program := tea.NewProgram(state, tea.WithContext(ctx),
		tea.WithInput(options.Stdin), tea.WithOutput(options.Stdout))
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(options.Stderr, "interactive-terminal-mcp:", err)
		return 1
	}
	if state.cancel {
		// Indented like every other line the installer prints, including the
		// ones the shell script writes around it.
		fmt.Fprintln(options.Stdout, "  Setup cancelled. Run `interactive-terminal-mcp configure` when you are ready.")
		return 0
	}
	if state.failure != "" {
		fmt.Fprintln(options.Stderr, "interactive-terminal-mcp:", state.failure)
		return 1
	}
	for _, result := range state.results {
		if result.State == detectharness.ApplyFailed || result.State == detectharness.ApplyConflict {
			return 1
		}
	}
	return 0
}

func (m *installModel) Init() tea.Cmd {
	return tea.Batch(m.detect(), spin())
}

// detect looks for AI clients off the main loop, so the first frame is on
// screen before the search starts rather than after it finishes.
func (m *installModel) detect() tea.Cmd {
	return func() tea.Msg { return detectedMsg{harnesses: m.installer.Detect(m.ctx)} }
}

type detectedMsg struct{ harnesses []Harness }

type spinMsg struct{}

// spinFrames are deliberately plain. The installer is the first thing a person
// sees, sometimes in a console whose default font has no box-drawing or braille
// glyphs, and a row of replacement boxes is a worse first impression than a
// character that is guaranteed to draw.
var spinFrames = []string{"-", "\\", "|", "/"}

func spin() tea.Cmd {
	return tea.Tick(110*time.Millisecond, func(time.Time) tea.Msg { return spinMsg{} })
}

// adopt takes the detection result and sets up the selection it implies.
func (m *installModel) adopt(harnesses []Harness) {
	m.harnesses = harnesses
	for _, harness := range harnesses {
		// Already-registered and detected clients start checked, so pressing
		// enter through the flow does the expected thing.
		m.selected[harness.ID] = harness.Configured ||
			(harness.State == detectharness.Detected && harness.Selectable())
	}
	m.cursor = m.firstSelectable()
	m.step = stepHarnesses
}

type appliedMsg struct {
	results []ApplyResult
	err     error
}

func (m *installModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case detectedMsg:
		m.adopt(message.harnesses)
		return m, nil

	case spinMsg:
		if m.step != stepDetecting {
			// The search is over; stop asking for frames.
			return m, nil
		}
		m.frame++
		return m, spin()

	case appliedMsg:
		if message.err != nil {
			m.failure = message.err.Error()
		}
		m.results = message.results
		m.step = stepDone
		return m, nil

	case tea.KeyMsg:
		if m.step == stepDetecting {
			// Nothing has been found yet, so there is nothing to move a cursor
			// over or toggle. Quitting still has to work.
			if key := message.String(); key == "q" || key == "ctrl+c" || key == "esc" {
				m.cancel = true
				return m, tea.Quit
			}
			return m, nil
		}
		if message.String() == "ctrl+c" {
			m.cancel = true
			return m, tea.Quit
		}
		switch m.step {
		case stepHarnesses:
			return m.updateHarnesses(message)
		case stepRetention:
			return m.updateRetention(message)
		case stepSummary:
			return m.updateSummary(message)
		case stepSettings:
			return m.updateSettings(message)
		case stepDone:
			switch message.String() {
			case "enter", "q", "esc":
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m *installModel) updateHarnesses(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "q", "esc":
		m.cancel = true
		return m, tea.Quit
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "v":
		m.showAll = !m.showAll
	case " ":
		m.toggle()
	case "a":
		m.toggleAll()
	case "enter":
		m.step = stepRetention
		m.choiceCursor = retentionIndex(m.settings.LogRetention)
	}
	return m, nil
}

func (m *installModel) updateRetention(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "q", "esc":
		m.step = stepHarnesses
	case "up", "k":
		m.choiceCursor = (m.choiceCursor - 1 + len(config.RetentionOptions)) % len(config.RetentionOptions)
	case "down", "j":
		m.choiceCursor = (m.choiceCursor + 1) % len(config.RetentionOptions)
	case "enter":
		if err := m.settings.Set("log_retention", config.RetentionOptions[m.choiceCursor].Value); err != nil {
			m.message = styleError.Render(err.Error())
			return m, nil
		}
		m.message = ""
		m.step = stepSummary
		m.cursor = 0
	}
	return m, nil
}

func (m *installModel) updateSummary(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "up", "k", "down", "j":
		m.cursor = 1 - m.cursor
	case "esc":
		m.step = stepRetention
	case "r":
		if len(m.settings.DiffFromDefaults()) > 0 {
			m.settings = config.Default()
			m.message = "Recommended defaults restored; they are saved when setup finishes."
		}
	case "enter":
		if m.cursor == 1 {
			m.step = stepSettings
			m.settingCursor = 0
			return m, nil
		}
		m.step = stepApplying
		return m, m.apply()
	}
	return m, nil
}

func (m *installModel) updateSettings(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	row := SettingsRows[m.settingCursor]

	if m.editing {
		if row.Kind == SettingChoice {
			switch message.String() {
			case "up", "k":
				m.choiceCursor = (m.choiceCursor - 1 + len(row.Choices)) % len(row.Choices)
			case "down", "j":
				m.choiceCursor = (m.choiceCursor + 1) % len(row.Choices)
			case "enter":
				if err := m.settings.Set(row.Key, row.Choices[m.choiceCursor].Value); err != nil {
					m.message = styleError.Render(err.Error())
					return m, nil
				}
				m.editing, m.message = false, ""
			case "esc":
				m.editing = false
			}
			return m, nil
		}
		switch message.Type {
		case tea.KeyEnter:
			if err := m.settings.Set(row.Key, m.input); err != nil {
				// The editor stays open with the reason shown, so nothing is
				// half-saved and the user can see exactly what to fix.
				m.message = styleError.Render(err.Error())
				return m, nil
			}
			m.editing, m.message = false, ""
		case tea.KeyEsc:
			m.editing = false
		case tea.KeyBackspace:
			if len(m.input) > 0 {
				m.input = m.input[:len(m.input)-1]
			}
		case tea.KeyRunes:
			m.input += string(message.Runes)
		}
		return m, nil
	}

	switch message.String() {
	case "esc", "q":
		m.step = stepSummary
		m.cursor = 0
	case "up", "k":
		m.settingCursor = (m.settingCursor - 1 + len(SettingsRows)) % len(SettingsRows)
	case "down", "j":
		m.settingCursor = (m.settingCursor + 1) % len(SettingsRows)
	case "r":
		if len(m.settings.DiffFromDefaults()) > 0 {
			m.settings = config.Default()
			m.message = "Recommended defaults restored."
		}
	case "enter":
		m.editing = true
		m.message = row.Help
		if row.Kind == SettingChoice {
			m.choiceCursor = 0
			current := m.settings.RawValue(row.Key)
			for index, choice := range row.Choices {
				if choice.Value == current {
					m.choiceCursor = index
				}
			}
		} else {
			m.input = m.settings.RawValue(row.Key)
		}
	}
	return m, nil
}

func (m *installModel) apply() tea.Cmd {
	ctx := m.ctx
	installer := m.installer
	harnesses := m.harnesses
	settings := m.settings
	paths := m.runtime.Paths
	selected := map[detectharness.ID]bool{}
	for id, value := range m.selected {
		selected[id] = value
	}
	return func() tea.Msg {
		// Settings are saved before registration so a harness that starts the
		// server immediately reads the configuration the user just chose.
		if err := config.Save(paths, settings); err != nil {
			return appliedMsg{err: err}
		}
		return appliedMsg{results: installer.Apply(ctx, harnesses, selected)}
	}
}

// --- selection helpers ------------------------------------------------------

func (m *installModel) visible() []int {
	var indices []int
	for index, harness := range m.harnesses {
		// A client is worth a line if it is here, or if this tool is already
		// registered with it. Everything else belongs behind `v`.
		//
		// Unselectable used to be enough on its own, which put clients that
		// cannot exist on this platform at all -- Claude Desktop has no config
		// path on Linux -- in the visible list as "not detected", directly above
		// a line promising that the hidden ones were the ones not installed. A
		// client that is installed but cannot be configured still shows, because
		// it is detected and its status says why.
		if harness.State == detectharness.Detected || harness.Configured || m.showAll {
			indices = append(indices, index)
		}
	}
	return indices
}

func (m *installModel) firstSelectable() int {
	for index, harness := range m.harnesses {
		if harness.Selectable() && harness.State == detectharness.Detected {
			return index
		}
	}
	return 0
}

func (m *installModel) moveCursor(direction int) {
	indices := m.visible()
	if len(indices) == 0 {
		return
	}
	position := 0
	for index, value := range indices {
		if value == m.cursor {
			position = index
		}
	}
	position += direction
	if position < 0 {
		position = len(indices) - 1
	}
	if position >= len(indices) {
		position = 0
	}
	m.cursor = indices[position]
}

// toggleAll selects every selectable harness, or clears them all if they are
// already selected. One key for both directions keeps it discoverable.
func (m *installModel) toggleAll() {
	anyUnselected := false
	for _, harness := range m.harnesses {
		if harness.Selectable() && !m.selected[harness.ID] {
			anyUnselected = true
			break
		}
	}
	for _, harness := range m.harnesses {
		if harness.Selectable() {
			m.selected[harness.ID] = anyUnselected
		}
	}
	if anyUnselected {
		m.message = "Selected every detected client."
	} else {
		m.message = "Cleared every client."
	}
}

func (m *installModel) toggle() {
	if m.cursor >= len(m.harnesses) {
		return
	}
	harness := m.harnesses[m.cursor]
	if !harness.Selectable() {
		m.message = harness.Name + " could not be inspected, so it cannot be changed."
		return
	}
	m.selected[harness.ID] = !m.selected[harness.ID]
	m.message = ""
}

func retentionIndex(value string) int {
	for index, option := range config.RetentionOptions {
		if option.Value == value {
			return index
		}
	}
	return 0
}

// --- views ------------------------------------------------------------------

func (m *installModel) View() string {
	switch m.step {
	case stepDetecting:
		return m.viewDetecting()
	case stepHarnesses:
		return m.viewHarnesses()
	case stepRetention:
		return m.viewRetention()
	case stepSummary:
		return m.viewSummary()
	case stepSettings:
		return m.viewSettings()
	case stepApplying:
		return header() + "\n\n  Registering…\n"
	default:
		return m.viewDone()
	}
}

func header() string {
	// The leading blank line and the two-space indent match the installer
	// script's own output, so the download and the setup read as one thing
	// rather than two programs taking turns.
	return "\n" + styleTitle.Render("  interactive-terminal-mcp setup")
}

// viewDetecting is what replaces the download's progress bar, so it opens with
// the same blank line and indentation the installer script uses. The two are
// separate programs and should not look it.
func (m *installModel) viewDetecting() string {
	frame := spinFrames[m.frame%len(spinFrames)]
	return header() + "\n\n  " + frame + " Initialising, looking for AI clients...\n"
}

func (m *installModel) viewHarnesses() string {
	var out strings.Builder
	out.WriteString(header())
	out.WriteString("\n\n  AI clients — which should be able to use terminal sessions?\n\n")

	indices := m.visible()
	if len(indices) == 0 {
		out.WriteString(styleDim.Render("  No AI clients detected. Install one, then run\n  `interactive-terminal-mcp configure`.\n"))
	}
	for _, index := range indices {
		harness := m.harnesses[index]
		cursor := " "
		if index == m.cursor && harness.Selectable() {
			cursor = styleCursor.Render(">")
		}
		mark := styleOff.Render("○")
		if !harness.Selectable() {
			mark = styleDim.Render("·")
		} else if m.selected[harness.ID] {
			mark = styleOn.Render("●")
		}
		line := fmt.Sprintf("%-22s %s", harness.Name, styleDim.Render(harness.StatusText()))
		if !harness.Selectable() {
			line = styleDim.Render(fmt.Sprintf("%-22s ", harness.Name)) + styleHint.Render(harness.StatusText())
		}
		fmt.Fprintf(&out, " %s %s %s\n", cursor, mark, line)
	}

	hidden := len(m.harnesses) - len(indices)
	if hidden > 0 && !m.showAll {
		out.WriteString("\n" + styleDim.Render(fmt.Sprintf("  press v to show %d client(s) that are not installed", hidden)))
	} else if m.showAll {
		out.WriteString("\n" + styleDim.Render("  press v to hide clients that are not installed"))
	}
	if m.message != "" {
		out.WriteString("\n\n  " + m.message)
	}
	out.WriteString("\n\n" + styleFooter.Render("  ↑↓ move · space toggle · a all/none · v show all · enter continue · q cancel"))
	return out.String()
}

func (m *installModel) viewRetention() string {
	var out strings.Builder
	out.WriteString(header())
	out.WriteString("\n\n  Session logs — when should logs from closed sessions be deleted?\n\n")

	for index, option := range config.RetentionOptions {
		cursor, dot := " ", " "
		if index == m.choiceCursor {
			cursor, dot = styleCursor.Render(">"), styleOn.Render("●")
		}
		label := option.Label
		if index == 0 {
			label += styleDim.Render("   (recommended)")
		}
		fmt.Fprintf(&out, " %s %s %s\n", cursor, dot, label)
	}

	out.WriteString("\n" + styleDim.Render("  Logs let it_tail and it_head reach past the visible screen.\n  Running sessions are never affected."))
	if m.message != "" {
		out.WriteString("\n\n  " + m.message)
	}
	out.WriteString("\n\n" + styleFooter.Render("  ↑↓ move · enter select · esc back"))
	return out.String()
}

func (m *installModel) viewSummary() string {
	var out strings.Builder
	out.WriteString(header())
	out.WriteString("\n\n  MCP tool configuration\n")
	if len(m.settings.DiffFromDefaults()) == 0 {
		out.WriteString(styleDim.Render("  Recommended defaults") + "\n")
	}
	out.WriteByte('\n')

	for _, row := range SettingsRows {
		fmt.Fprintf(&out, "  %-28s %s\n", row.Label, m.settings.Value(row.Key))
	}
	out.WriteByte('\n')

	options := []string{"Continue", "Change settings"}
	for index, option := range options {
		cursor := " "
		if index == m.cursor {
			cursor = styleCursor.Render(">")
		}
		fmt.Fprintf(&out, " %s %s\n", cursor, option)
	}
	if len(m.settings.DiffFromDefaults()) > 0 {
		out.WriteString("\n" + styleFooter.Render("  r restore defaults"))
	}
	if m.message != "" {
		out.WriteString("\n\n  " + m.message)
	}
	out.WriteString("\n\n" + styleDim.Render("  Limits are approximate and apply only to retrieved information."))
	return out.String()
}

func (m *installModel) viewSettings() string {
	var out strings.Builder
	out.WriteString(header())
	out.WriteString("\n\n  Settings\n\n")

	for index, row := range SettingsRows {
		cursor := " "
		if index == m.settingCursor {
			cursor = styleCursor.Render(">")
		}
		value := m.settings.Value(row.Key)
		if m.editing && index == m.settingCursor {
			if row.Kind == SettingChoice {
				value = "< " + row.Choices[m.choiceCursor].Label + " >"
			} else {
				value = m.input + "_"
			}
		}
		fmt.Fprintf(&out, " %s %-28s %s\n", cursor, row.Label, value)
	}

	if m.message != "" {
		out.WriteString("\n  " + m.message)
	}
	footer := "  ↑↓ move · enter edit · r restore defaults · esc back"
	if m.editing {
		footer = "  enter confirm · esc cancel"
	}
	out.WriteString("\n\n" + styleFooter.Render(footer))
	out.WriteString("\n" + styleDim.Render("  Limits are approximate and apply only to retrieved information."))
	return out.String()
}

func (m *installModel) viewDone() string {
	var out strings.Builder
	out.WriteString(header() + "\n\n")

	if m.failure != "" {
		out.WriteString(styleError.Render("  "+m.failure) + "\n\n")
	}
	if len(m.results) == 0 {
		out.WriteString("  No clients were selected, so nothing was registered.\n")
		out.WriteString("  Settings were saved.\n\n")
	} else {
		for _, result := range m.results {
			fmt.Fprintf(&out, "  %-22s %s\n", result.Harness.Name, result.Summary())
		}
		out.WriteByte('\n')

		var reloads []ApplyResult
		for _, result := range m.results {
			if result.Changed() && result.ReloadHint != "" {
				reloads = append(reloads, result)
			}
		}
		if len(reloads) > 0 {
			out.WriteString("  Restart the affected clients so they pick up the change:\n")
			for _, result := range reloads {
				fmt.Fprintf(&out, "    %-22s %s\n", result.Harness.Name, result.ReloadHint)
			}
			out.WriteByte('\n')
		}
	}

	out.WriteString(styleDim.Render("  Run `interactive-terminal-mcp` to browse and use sessions yourself,\n  or `interactive-terminal-mcp configure` to change any of this later.") + "\n\n")
	out.WriteString(styleFooter.Render("  enter to finish"))
	return out.String()
}
