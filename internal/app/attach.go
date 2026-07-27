package app

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/interactive-terminal-mcp/internal/bootstrap"
)

// Attach opens one session directly, skipping the home list.
//
// It is the same view the application uses, so `interactive-terminal-mcp
// attach build` and pressing enter on that row in the list behave identically.
func Attach(ctx context.Context, runtime *bootstrap.Runtime, session string) int {
	if !isTerminal() {
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp: attach needs a terminal.")
		return 1
	}

	state := &model{
		ctx: ctx, runtime: runtime,
		screen: screenHome, composer: newComposer(),
		width: 100, height: 30,
		// Leaving the session returns to the list rather than exiting, which
		// matches the application and gives a way back to the other sessions.
		status: "",
	}
	program := tea.NewProgram(state,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	// The session is opened as the first command so the list never flashes.
	state.pending = session
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp:", err)
		return 1
	}
	if state.failure != "" {
		fmt.Fprintln(os.Stderr, "interactive-terminal-mcp:", state.failure)
		return 1
	}
	return 0
}
