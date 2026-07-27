package daemon

import (
	"context"
	"encoding/json"

	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
	"github.com/sairaph/interactive-terminal-mcp/internal/session"
)

// defaultReplayBytes is how much of the raw log primes a fresh attach.
//
// A viewer that starts with nothing sees a blank frame until the program next
// writes, which for an idle shell is never. Replaying the tail of the exact
// byte stream through the viewer's own emulator reconstructs the screen
// faithfully, including colours and cursor position.
const defaultReplayBytes = 256 << 10

func (c *connection) handleAttachOpen(args ipc.AttachArgs) (any, error) {
	item, err := c.daemon.registry.requireLive(args.Session)
	if err != nil {
		return nil, err
	}
	live := item.live

	if args.Cols > 0 && args.Rows > 0 {
		if err := validateSize(args.Cols, args.Rows); err != nil {
			return nil, err
		}
		if err := live.Resize(args.Cols, args.Rows); err != nil {
			return nil, internalError(err)
		}
	}

	replayBytes := args.ReplayBytes
	if replayBytes <= 0 {
		replayBytes = defaultReplayBytes
	}
	replay, _ := session.TailRaw(live.RawPath(), replayBytes)

	c.attached = true
	c.attachID = live.ID()
	return map[string]any{
		"session": c.daemon.describeSession(item, c.daemon.registry.activeID()),
		"replay":  replay,
	}, nil
}

// startStream pushes terminal output to an attached viewer until it detaches
// or the session ends.
func (c *connection) startStream(parent context.Context) {
	item, err := c.daemon.registry.resolve(c.attachID)
	if err != nil || item.live == nil {
		_ = c.writeFrame(ipc.Frame{Kind: ipc.FrameClosed})
		return
	}
	live := item.live

	frames, cancel := live.Subscribe()
	streamCtx, stop := context.WithCancel(parent)
	c.detach = cancel
	c.streamCtx = streamCtx
	c.stopStrm = stop

	go func() {
		defer stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-c.daemon.stopping:
				_ = c.writeFrame(ipc.Frame{Kind: ipc.FrameClosed})
				return
			case chunk, ok := <-frames:
				if !ok {
					// The session ended; tell the viewer why rather than
					// letting its window simply stop updating.
					frame := ipc.Frame{Kind: ipc.FrameClosed}
					if code, exited := live.ExitCode(); exited {
						frame.ExitCode = &code
					}
					_ = c.writeFrame(frame)
					return
				}
				// A nil chunk is the broadcaster's resync marker: this viewer
				// fell behind and was skipped, so it must redraw rather than
				// apply a stream with a hole in it.
				if chunk == nil {
					if err := c.writeFrame(ipc.Frame{Kind: ipc.FrameResync}); err != nil {
						return
					}
					continue
				}
				if err := c.writeFrame(ipc.Frame{Kind: ipc.FrameOutput, Data: chunk}); err != nil {
					return
				}
			}
		}
	}()
}

func (c *connection) handleAttachInput(request ipc.Request) {
	var args ipc.AttachInputArgs
	if err := json.Unmarshal(request.Args, &args); err != nil {
		return
	}
	item, err := c.daemon.registry.resolve(c.attachID)
	if err != nil || item.live == nil {
		return
	}
	// A viewer typing into a session that just exited is ordinary, not an
	// error worth interrupting the stream for.
	_ = item.live.Write(args.Data)
}

func (c *connection) handleAttachResize(request ipc.Request) {
	var args ipc.ResizeArgs
	if err := json.Unmarshal(request.Args, &args); err != nil {
		return
	}
	if err := validateSize(args.Cols, args.Rows); err != nil {
		return
	}
	item, err := c.daemon.registry.resolve(c.attachID)
	if err != nil || item.live == nil {
		return
	}
	_ = item.live.Resize(args.Cols, args.Rows)
}
