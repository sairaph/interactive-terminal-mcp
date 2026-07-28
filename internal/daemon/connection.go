package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

// connection is one client's link to the daemon.
//
// Ordinary calls are strictly request/response. An attach.open converts the
// connection into a one-way stream of terminal frames, after which only input
// and resize messages are accepted.
type connection struct {
	daemon *Daemon
	conn   net.Conn

	writeMu sync.Mutex
	encoder *json.Encoder

	attached  bool
	attachID  string
	detach    func()
	streamCtx context.Context
	stopStrm  context.CancelFunc
}

func (c *connection) serve(ctx context.Context) {
	c.encoder = json.NewEncoder(c.conn)
	reader := bufio.NewReaderSize(c.conn, 64<<10)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		if c.detach != nil {
			c.detach()
		}
		if c.stopStrm != nil {
			c.stopStrm()
		}
	}()

	// A client going away must not leave a blocked read holding the daemon's
	// shutdown, so the connection is closed when the daemon stops.
	go func() {
		select {
		case <-ctx.Done():
		case <-c.daemon.stopping:
		}
		_ = c.conn.Close()
	}()

	for {
		line, err := readLine(reader)
		if err != nil {
			return
		}
		var request ipc.Request
		if err := json.Unmarshal(line, &request); err != nil {
			c.reply(ipc.Response{V: ipc.Version, OK: false, Error: &ipc.Error{
				Code:    ipc.CodeInvalidInput,
				Message: "malformed request: " + err.Error(),
			}})
			return
		}
		if request.V != ipc.Version {
			c.reply(ipc.Response{V: ipc.Version, ID: request.ID, OK: false, Error: &ipc.Error{
				Code:    ipc.CodeInternal,
				Message: fmt.Sprintf("client speaks protocol version %d, this daemon speaks %d", request.V, ipc.Version),
				Hint:    "Stop the daemon with `interactive-terminal-mcp daemon --stop` so a matching one can start.",
			}})
			return
		}
		c.daemon.touch()

		// Input and resize on an attached connection are fire-and-forget: the
		// program's own output is the acknowledgement, and replying to every
		// keystroke would add a round trip to typing.
		if c.attached {
			switch request.Op {
			case ipc.OpAttachInput:
				c.handleAttachInput(request)
				continue
			case ipc.OpAttachResize:
				c.handleAttachResize(request)
				continue
			}
		}

		result, err := c.dispatch(ctx, request)
		if err != nil {
			c.reply(ipc.Response{V: ipc.Version, ID: request.ID, OK: false, Error: toIPCError(err)})
			continue
		}
		raw, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			c.reply(ipc.Response{V: ipc.Version, ID: request.ID, OK: false, Error: internalError(marshalErr)})
			continue
		}
		c.reply(ipc.Response{V: ipc.Version, ID: request.ID, OK: true, Result: raw})

		// The attach reply must reach the client before any frame does, so the
		// stream starts only after the acknowledgement has been written.
		if request.Op == ipc.OpAttachOpen && c.attached {
			c.startStream(ctx)
		}
	}
}

func (c *connection) dispatch(ctx context.Context, request ipc.Request) (any, error) {
	switch request.Op {
	case ipc.OpPing:
		return map[string]any{"pong": true}, nil
	case ipc.OpSessionList:
		return c.daemon.handleList()
	case ipc.OpSessionNew:
		var args ipc.NewArgs
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, err
		}
		return c.daemon.handleNew(ctx, args)
	case ipc.OpSessionRead:
		var args ipc.ReadArgs
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, err
		}
		return c.daemon.handleRead(ctx, args)
	case ipc.OpSessionSend:
		var args ipc.SendArgs
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, err
		}
		return c.daemon.handleSend(ctx, args)
	case ipc.OpSessionKill:
		var args ipc.KillArgs
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, err
		}
		return c.daemon.handleKill(ctx, args)
	case ipc.OpSessionLog:
		var args ipc.LogArgs
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, err
		}
		return c.daemon.handleLog(args)
	case ipc.OpSessionRename:
		var args ipc.RenameArgs
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, err
		}
		return c.daemon.handleRename(args)
	case ipc.OpSessionResize:
		var args ipc.ResizeArgs
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, err
		}
		return c.daemon.handleResize(args)
	case ipc.OpSessionScroll:
		var args ipc.ScrollArgs
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, err
		}
		return c.daemon.handleScrollback(args)
	case ipc.OpAttachOpen:
		var args ipc.AttachArgs
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, err
		}
		return c.handleAttachOpen(args)
	case ipc.OpDaemonStatus:
		return c.daemon.Status(), nil
	case ipc.OpDaemonStop:
		var args ipc.StopArgs
		if err := decodeArgs(request.Args, &args); err != nil {
			return nil, err
		}
		status := c.daemon.Status()
		// The reply has to be written before the socket closes, so the actual
		// shutdown is deferred until after this response is flushed.
		go func() {
			c.daemon.stopWithSessions(args.KillSessions)
		}()
		return status, nil
	default:
		return nil, &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: fmt.Sprintf("unknown operation %q", request.Op),
		}
	}
}

func (c *connection) reply(response ipc.Response) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.encoder.Encode(response)
}

func (c *connection) writeFrame(frame ipc.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.encoder.Encode(frame)
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	var collected []byte
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if err != nil {
			return nil, err
		}
		collected = append(collected, chunk...)
		if len(collected) > 16<<20 {
			return nil, fmt.Errorf("protocol message exceeds 16 MiB")
		}
		if !isPrefix {
			return collected, nil
		}
	}
}
