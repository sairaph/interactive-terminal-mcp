package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

// autostartTimeout bounds how long a client waits for a daemon it started.
const autostartTimeout = 10 * time.Second

// Client is a connection to the session daemon.
//
// One connection carries many requests, serialized by a mutex. Terminal work
// is short and the daemon does the waiting, so pipelining would add complexity
// without shortening any real call.
type Client struct {
	mu      sync.Mutex
	conn    net.Conn
	reader  *bufio.Reader
	encoder *json.Encoder
	nextID  int
	closed  bool
}

// Dial connects to the daemon, starting one if it is not running.
//
// Autostart is what makes the daemon invisible: an agent's first tool call
// after a reboot works exactly like every later one, with no setup step and no
// error telling the user to run something first.
func Dial(ctx context.Context, socket string, executable string) (*Client, error) {
	if client, err := connect(ctx, socket); err == nil {
		return client, nil
	}

	if executable == "" {
		resolved, err := os.Executable()
		if err != nil {
			return nil, daemonUnavailable(fmt.Errorf("resolve executable: %w", err))
		}
		executable = resolved
	}
	if err := spawnDaemon(executable); err != nil {
		return nil, daemonUnavailable(err)
	}

	// The daemon has to create its socket before anyone can connect; poll
	// rather than sleeping a fixed amount so a fast start is not penalised.
	deadline := time.Now().Add(autostartTimeout)
	delay := 20 * time.Millisecond
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, daemonUnavailable(ctx.Err())
		case <-time.After(delay):
		}
		client, err := connect(ctx, socket)
		if err == nil {
			return client, nil
		}
		lastErr = err
		if delay < 250*time.Millisecond {
			delay *= 2
		}
	}
	return nil, daemonUnavailable(fmt.Errorf("daemon did not become reachable within %s: %w", autostartTimeout, lastErr))
}

// Connect attaches to an already-running daemon without starting one.
func Connect(ctx context.Context, socket string) (*Client, error) {
	client, err := connect(ctx, socket)
	if err != nil {
		return nil, daemonUnavailable(err)
	}
	return client, nil
}

func connect(ctx context.Context, socket string) (*Client, error) {
	conn, err := dialSocket(ctx, socket)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn:    conn,
		reader:  bufio.NewReaderSize(conn, 64<<10),
		encoder: json.NewEncoder(conn),
	}, nil
}

func daemonUnavailable(err error) error {
	return &Error{
		Code:    CodeDaemonUnavailable,
		Message: "the terminal session daemon is not reachable: " + err.Error(),
		Hint:    "Run `interactive-terminal-mcp doctor` to diagnose the daemon, then retry.",
	}
}

func spawnDaemon(executable string) error {
	command := exec.Command(executable, "daemon", "--detach")
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	configureDetach(command)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// The detached child re-execs and this shim exits immediately; reaping it
	// here prevents a zombie in the MCP process's table.
	go func() { _ = command.Wait() }()
	return nil
}

// Call sends one request and returns its decoded result.
func (c *Client) Call(ctx context.Context, op string, args any, result any) error {
	raw, err := marshalArgs(args)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return daemonUnavailable(errors.New("client is closed"))
	}

	c.nextID++
	request := Request{V: Version, ID: c.nextID, Op: op, Args: raw}

	// A cancelled context must not leave a half-read response on a shared
	// connection, so the deadline is pushed down to the socket instead of
	// abandoning the read.
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
		defer c.conn.SetDeadline(time.Time{})
	}

	if err := c.encoder.Encode(request); err != nil {
		c.closed = true
		return daemonUnavailable(fmt.Errorf("send %s: %w", op, err))
	}
	response, err := c.readResponse()
	if err != nil {
		c.closed = true
		return err
	}
	if response.ID != request.ID {
		c.closed = true
		return daemonUnavailable(fmt.Errorf("out-of-order response for %s", op))
	}
	if !response.OK {
		if response.Error != nil {
			return response.Error
		}
		return &Error{Code: CodeInternal, Message: "the daemon reported an unspecified failure"}
	}
	if result == nil || len(response.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return &Error{Code: CodeInternal, Message: fmt.Sprintf("decode %s result: %v", op, err)}
	}
	return nil
}

func (c *Client) readResponse() (Response, error) {
	var response Response
	line, err := readLine(c.reader)
	if err != nil {
		return response, daemonUnavailable(fmt.Errorf("read response: %w", err))
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return response, daemonUnavailable(fmt.Errorf("decode response: %w", err))
	}
	if response.V != Version {
		return response, &Error{
			Code:    CodeInternal,
			Message: fmt.Sprintf("daemon speaks protocol version %d, this client speaks %d", response.V, Version),
			Hint:    "Stop the daemon with `interactive-terminal-mcp daemon --stop` so the current binary can start a matching one.",
		}
	}
	return response, nil
}

// Attached is a live view of a session.
type Attached struct {
	// Session describes the session at the moment of attaching.
	Session SessionInfo
	// Replay is the tail of the raw byte log. Feeding it through the viewer's
	// own emulator reconstructs the current screen, so an idle session is
	// visible immediately instead of blank until it next writes.
	Replay []byte
	// Frames carries streamed output until the view is closed.
	Frames <-chan Frame
	// Send writes input to the session.
	Send func([]byte) error
}

// AttachOpenResult is the daemon's reply to attach.open.
type AttachOpenResult struct {
	Session SessionInfo `json:"session"`
	Replay  []byte      `json:"replay,omitempty"`
}

// Attach converts this connection into a stream of terminal frames.
//
// The connection is consumed: after Attach, the client may only send input
// through the returned function and must not issue ordinary calls.
func (c *Client) Attach(ctx context.Context, args AttachArgs) (*Attached, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, daemonUnavailable(errors.New("client is closed"))
	}
	c.nextID++
	id := c.nextID
	raw, err := marshalArgs(args)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if err := c.encoder.Encode(Request{V: Version, ID: id, Op: OpAttachOpen, Args: raw}); err != nil {
		c.closed = true
		c.mu.Unlock()
		return nil, daemonUnavailable(fmt.Errorf("open attach: %w", err))
	}
	response, err := c.readResponse()
	if err != nil {
		c.closed = true
		c.mu.Unlock()
		return nil, err
	}
	if !response.OK {
		c.mu.Unlock()
		if response.Error != nil {
			return nil, response.Error
		}
		return nil, &Error{Code: CodeInternal, Message: "attach was refused"}
	}
	var opened AttachOpenResult
	if len(response.Result) > 0 {
		_ = json.Unmarshal(response.Result, &opened)
	}
	c.mu.Unlock()

	frames := make(chan Frame, 64)
	go func() {
		defer close(frames)
		for {
			line, err := readLine(c.reader)
			if err != nil {
				return
			}
			var frame Frame
			if err := json.Unmarshal(line, &frame); err != nil {
				return
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
			if frame.Kind == FrameClosed {
				return
			}
		}
	}()

	send := func(data []byte) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed {
			return daemonUnavailable(errors.New("attach connection is closed"))
		}
		c.nextID++
		payload, err := marshalArgs(AttachInputArgs{Session: args.Session, Data: data})
		if err != nil {
			return err
		}
		// Input is fire-and-forget: the daemon acknowledges by echoing the
		// program's own output, and waiting for a reply would add a round trip
		// to every keystroke.
		if err := c.encoder.Encode(Request{V: Version, ID: c.nextID, Op: OpAttachInput, Args: payload}); err != nil {
			c.closed = true
			return daemonUnavailable(fmt.Errorf("send input: %w", err))
		}
		return nil
	}
	return &Attached{
		Session: opened.Session, Replay: opened.Replay,
		Frames: frames, Send: send,
	}, nil
}

// ResizeAttached changes the session size on an attached connection.
func (c *Client) ResizeAttached(session string, cols, rows int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return daemonUnavailable(errors.New("attach connection is closed"))
	}
	c.nextID++
	payload, err := marshalArgs(ResizeArgs{Session: session, Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	if err := c.encoder.Encode(Request{V: Version, ID: c.nextID, Op: OpAttachResize, Args: payload}); err != nil {
		c.closed = true
		return daemonUnavailable(err)
	}
	return nil
}

// Close releases the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

func marshalArgs(args any) (json.RawMessage, error) {
	if args == nil {
		return nil, nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, &Error{Code: CodeInternal, Message: fmt.Sprintf("encode arguments: %v", err)}
	}
	return raw, nil
}

// maxLine bounds one protocol message. Terminal output is chunked well below
// this; the limit exists so a corrupt stream cannot exhaust memory.
const maxLine = 16 << 20

func readLine(reader *bufio.Reader) ([]byte, error) {
	var collected []byte
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if err != nil {
			return nil, err
		}
		collected = append(collected, chunk...)
		if len(collected) > maxLine {
			return nil, fmt.Errorf("protocol message exceeds %d bytes", maxLine)
		}
		if !isPrefix {
			return collected, nil
		}
	}
}

var _ io.Closer = (*Client)(nil)
