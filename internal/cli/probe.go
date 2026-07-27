package cli

import (
	"errors"
	"fmt"

	"github.com/aymanbagabas/go-pty"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

// probePTY allocates and releases a pseudo-terminal.
//
// It is the one check that catches the environments where every path and
// permission looks correct but no terminal can ever be created: a container
// without /dev/pts, a stripped sandbox, or a Windows build too old for ConPTY.
// Those fail at the first it_new otherwise, with a much less obvious message.
func probePTY() error {
	terminal, err := pty.New()
	if err != nil {
		return fmt.Errorf("could not allocate a pseudo-terminal: %w", err)
	}
	defer terminal.Close()
	if err := terminal.Resize(80, 24); err != nil {
		return fmt.Errorf("allocated a pseudo-terminal but could not size it: %w", err)
	}
	return nil
}

func asError(err error, target **ipc.Error) bool {
	return errors.As(err, target)
}
