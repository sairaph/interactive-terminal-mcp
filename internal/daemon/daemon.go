// Package daemon owns every live terminal session for one user.
//
// Sessions have to outlive the process that created them: an agent starts a
// build in one MCP call and reads it in the next, a different AI client may
// look at the same terminal, and the human application attaches to what the
// agent is doing. A per-user daemon is what makes all three see one terminal
// instead of three unrelated ones.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
)

// Daemon serves the session registry over a local socket.
type Daemon struct {
	paths    config.Paths
	version  string
	registry *registry
	listener net.Listener
	lock     *flock.Flock

	startedAt time.Time
	clients   atomic.Int64

	stopOnce sync.Once
	stopping chan struct{}
	// idleReset is signalled whenever something happens that should postpone
	// an idle shutdown.
	idleReset chan struct{}
}

// Open acquires the singleton lock and binds the socket.
//
// The lock is taken before binding so two daemons racing at autostart resolve
// deterministically: the loser exits and its client connects to the winner,
// rather than both binding and splitting the sessions between them.
func Open(paths config.Paths, settings config.Config, version string) (*Daemon, error) {
	if err := os.MkdirAll(paths.Root, 0o700); err != nil {
		return nil, fmt.Errorf("create application directory: %w", err)
	}
	if err := os.MkdirAll(paths.Sessions, 0o700); err != nil {
		return nil, fmt.Errorf("create sessions directory: %w", err)
	}

	lock := flock.New(paths.Lock)
	held, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire daemon lock: %w", err)
	}
	if !held {
		return nil, ErrAlreadyRunning
	}

	listener, err := ipc.Listen(paths.Socket)
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}

	daemon := &Daemon{
		paths:     paths,
		version:   version,
		registry:  newRegistry(paths, settings),
		listener:  listener,
		lock:      lock,
		startedAt: time.Now().UTC(),
		stopping:  make(chan struct{}),
		idleReset: make(chan struct{}, 1),
	}
	daemon.registry.recover()
	return daemon, nil
}

// ErrAlreadyRunning means another daemon holds the lock.
var ErrAlreadyRunning = errors.New("another session daemon is already running")

// Serve accepts connections until the context is cancelled, the daemon is
// stopped, or it has been idle long enough.
func (d *Daemon) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var waitGroup sync.WaitGroup
	go d.sweepRetention(ctx)
	go d.watchIdle(ctx, cancel)

	go func() {
		select {
		case <-ctx.Done():
		case <-d.stopping:
		}
		_ = d.listener.Close()
	}()

	for {
		conn, err := d.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
			case <-d.stopping:
			default:
				// A transient accept failure should not take the daemon down
				// and lose every running session with it.
				if isTemporary(err) {
					time.Sleep(20 * time.Millisecond)
					continue
				}
			}
			break
		}
		waitGroup.Add(1)
		d.clients.Add(1)
		go func() {
			defer waitGroup.Done()
			defer d.clients.Add(-1)
			d.serveConn(ctx, conn)
			d.touch()
		}()
	}

	waitGroup.Wait()
	return nil
}

// Close releases the socket, the lock, and every live session.
func (d *Daemon) Close(killSessions bool) {
	d.stopOnce.Do(func() { close(d.stopping) })
	_ = d.listener.Close()

	for _, item := range d.registry.list() {
		if item.live == nil {
			continue
		}
		if killSessions && item.live.Running() {
			_ = item.live.Kill("TERM", "daemon shutdown")
			shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			item.live.WaitExit(shutdown)
			cancel()
		}
		// Closing flushes the logs and finalizes meta.json, which is what
		// lets the next daemon list and tail this session.
		item.live.Close()
	}

	ipc.Cleanup(d.paths.Socket)
	_ = d.lock.Unlock()
}

// Stop asks a running daemon to shut down.
func (d *Daemon) Stop() { d.stopOnce.Do(func() { close(d.stopping) }) }

// touch postpones idle shutdown.
func (d *Daemon) touch() {
	select {
	case d.idleReset <- struct{}{}:
	default:
	}
}

// watchIdle stops the daemon once it has had no sessions and no clients for
// the configured period.
//
// Without it, every machine that ever ran one tool call would keep a process
// forever. With it, the daemon is invisible: it appears on demand and leaves
// when there is nothing to own.
func (d *Daemon) watchIdle(ctx context.Context, cancel context.CancelFunc) {
	settings := d.registry.settingsSnapshot()
	if settings.DaemonIdleShutdown <= 0 {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	idleSince := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopping:
			return
		case <-d.idleReset:
			idleSince = time.Now()
		case <-ticker.C:
			total, _ := d.registry.counts()
			busy := total > 0 || d.clients.Load() > 0
			if busy {
				idleSince = time.Now()
				continue
			}
			if time.Since(idleSince) >= settings.DaemonIdleShutdown {
				d.Stop()
				cancel()
				return
			}
		}
	}
}

// serveConn handles one client connection.
func (d *Daemon) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	d.touch()

	session := &connection{daemon: d, conn: conn}
	session.serve(ctx)
}

func isTemporary(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Status describes the running daemon.
func (d *Daemon) Status() ipc.Status {
	total, live := d.registry.counts()
	return ipc.Status{
		PID: os.Getpid(), Version: d.version, StartedAt: d.startedAt,
		Sessions: total, Live: live, Clients: int(d.clients.Load()),
		Socket: d.paths.Socket, SessionsRoot: d.paths.Sessions,
	}
}

// Reload replaces the configuration a running daemon uses, so a settings
// change from the application or the installer takes effect without a restart.
func (d *Daemon) Reload(settings config.Config) { d.registry.reload(settings) }

// internalError wraps an unexpected failure in the shared error contract.
func internalError(err error) *ipc.Error {
	return &ipc.Error{
		Code:    ipc.CodeInternal,
		Message: err.Error(),
		Hint:    "Retry the call; if it keeps failing, run `interactive-terminal-mcp doctor`.",
	}
}

func toIPCError(err error) *ipc.Error {
	if err == nil {
		return nil
	}
	var typed *ipc.Error
	if errors.As(err, &typed) {
		return typed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &ipc.Error{
			Code:    ipc.CodeCancelled,
			Message: err.Error(),
			Hint:    "Retry the call when it can run to completion.",
		}
	}
	return internalError(err)
}

func decodeArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: "could not decode arguments: " + err.Error(),
		}
	}
	return nil
}
