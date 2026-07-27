package app

import "github.com/charmbracelet/lipgloss"

// Colours are chosen from the 256-colour palette so the application looks the
// same in a plain terminal as it does in a truecolor one.
var (
	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	styleFrame   = lipgloss.NewStyle().Foreground(lipgloss.Color("60"))
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleFooter  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleCursor  = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	styleRunning = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleEnded   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleError   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleActive  = lipgloss.NewStyle().Bold(true)
	styleSelect  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("61"))
	styleOff     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleBadge   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("232")).Background(lipgloss.Color("214")).Padding(0, 1)
)

// border is the rounded frame drawn around a session's terminal.
var border = lipgloss.RoundedBorder()

// minWidth and minHeight are the smallest terminal the application can draw
// a usable frame in. Below that it says so rather than rendering a broken one.
const (
	minWidth  = 60
	minHeight = 20
)
