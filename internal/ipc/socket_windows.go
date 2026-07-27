//go:build windows

package ipc

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func dialSocket(ctx context.Context, socket string) (net.Conn, error) {
	timeout := 3 * time.Second
	dialContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return winio.DialPipeContext(dialContext, socket)
}

// Listen binds the daemon's named pipe.
//
// The security descriptor grants access to the creating user and the local
// system only. Terminal sessions run arbitrary commands as this user, so the
// pipe must not be reachable by other accounts on the machine.
func Listen(socket string) (net.Listener, error) {
	// D:P(A;;GA;;;OW)(A;;GA;;;SY) - DACL protected from inheritance, granting
	// generic-all to the owner and to SYSTEM.
	const descriptor = "D:P(A;;GA;;;OW)(A;;GA;;;SY)"
	listener, err := winio.ListenPipe(socket, &winio.PipeConfig{
		SecurityDescriptor: descriptor,
		MessageMode:        false,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", socket, err)
	}
	return listener, nil
}

// configureDetach makes the spawned daemon survive its parent.
//
// A new process group plus DETACHED_PROCESS keeps the daemon out of the AI
// client's console, so the sessions it owns are not killed when that client
// exits or its console window closes.
func configureDetach(command *exec.Cmd) {
	command.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		HideWindow:    true,
	}
}

// Cleanup is a no-op: a named pipe disappears with its last handle.
func Cleanup(socket string) {}
