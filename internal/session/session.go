// Package session owns one live terminal: its PTY, its emulator, its logs,
// and the fan-out to attached human viewers.
package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aymanbagabas/go-pty"
	"github.com/sairaph/interactive-terminal-mcp/internal/vterm"
)

// readChunk bounds one PTY read. It is deliberately no larger than
// vterm.MaxWriteChunk so a single emulator write can never evict more
// scrollback lines than the ring can hold before the transcript drains it.
const readChunk = vterm.MaxWriteChunk

// Errors callers distinguish.
var (
	// ErrExited means input was sent to a session whose process has ended.
	ErrExited = errors.New("session has exited")
)

// Options describes a session to create.
type Options struct {
	ID   string
	Name string
	// CommandLine is run through the user's login shell. Argv is executed
	// directly. Exactly one is used; Argv wins when both are set.
	CommandLine string
	Argv        []string
	Cwd         string
	Env         map[string]string
	Cols, Rows  int
	// Shell names the interpreter a command line runs through, by short name
	// ("powershell", "bash") or full path. Empty means the machine default.
	Shell string

	Directory          string
	ScrollbackLines    int
	RawLogMaxBytes     int64
	TranscriptMaxLines int
}

// Session is one live terminal.
type Session struct {
	id   string
	name string

	pty     pty.Pty
	command *pty.Cmd
	term    vterm.Terminal
	logs    *logStore
	subs    *broadcaster

	mu           sync.RWMutex
	metadata     Metadata
	exited       bool
	exitCode     int
	exitedAt     time.Time
	killedBy     string
	lastActivity time.Time

	// activity is closed and replaced on every read, so any number of waiters
	// can block on output without polling.
	activityMu sync.Mutex
	activity   chan struct{}

	// outputBytes counts everything the child has ever written. It is what
	// distinguishes a session that has finished from one that has not started.
	outputBytes atomic.Int64

	// processDone is closed the instant the child exits, before any of the
	// bookkeeping that follows. Reporting liveness from the end of that
	// bookkeeping made a dead session look alive for as long as the drain took,
	// which spuriously escalated a TERM that had already worked and made two
	// consecutive reads disagree about whether the session was running.
	processDone chan struct{}

	// pumpDone is closed when the reader goroutine has drained the PTY and
	// returned, so the reaper can append the final screen knowing no more
	// output is coming.
	pumpDone chan struct{}
	done     chan struct{}
	closeOne sync.Once
}

// New starts a session. The returned session is running unless an error is
// reported; the caller owns calling Close.
func New(options Options) (*Session, error) {
	if options.Cols < 1 {
		options.Cols = 120
	}
	if options.Rows < 1 {
		options.Rows = 30
	}

	argv, shell, usedShell, err := resolveCommand(options)
	if err != nil {
		return nil, err
	}
	cwd := options.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("working directory %q is not a usable directory", cwd)
	}

	logs, err := openLogStore(options.Directory, options.RawLogMaxBytes, options.TranscriptMaxLines)
	if err != nil {
		return nil, err
	}

	terminal, err := pty.New()
	if err != nil {
		logs.close()
		return nil, fmt.Errorf("allocate pty: %w", err)
	}
	if err := terminal.Resize(options.Cols, options.Rows); err != nil {
		terminal.Close()
		logs.close()
		return nil, fmt.Errorf("size pty: %w", err)
	}

	command := terminal.Command(argv[0], argv[1:]...)
	command.Dir = cwd
	command.Env = buildEnv(options.Env, options.Cols, options.Rows)

	if err := command.Start(); err != nil {
		terminal.Close()
		logs.close()
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}

	now := time.Now().UTC()
	session := &Session{
		id:      options.ID,
		name:    options.Name,
		pty:     terminal,
		command: command,
		term:    vterm.NewCharm(options.Cols, options.Rows, options.ScrollbackLines),
		logs:    logs,
		subs:    newBroadcaster(),

		activity:     make(chan struct{}),
		processDone:  make(chan struct{}),
		pumpDone:     make(chan struct{}),
		done:         make(chan struct{}),
		lastActivity: now,
	}
	session.metadata = Metadata{
		ID: options.ID, Name: options.Name, Command: argv,
		CommandLine: options.CommandLine, Shell: usedShell,
		ShellID: shell.ID, ShellPath: shell.Path, ShellName: shell.Display,
		Cwd: cwd, Env: options.Env, Cols: options.Cols, Rows: options.Rows,
		PID: commandPID(command), CreatedAt: now, LastActivityAt: now,
	}
	_ = logs.writeMetadata(session.metadata)

	go session.pump()
	go session.answerQueries()
	go session.reap()
	return session, nil
}

// answerQueries forwards the emulator's replies back to the program.
//
// Programs ask the terminal questions during startup -- device attributes,
// cursor position, colour support -- and block until they are answered. The
// emulator produces those answers from inside Write, buffering them in a pipe
// that blocks once full, so an undrained emulator deadlocks every writer and
// every snapshot while the program waits for a reply that never comes. Draining
// here both unblocks the emulator and completes the handshake.
func (s *Session) answerQueries() {
	// Replies are tiny and infrequent. The buffer exists so a child that has
	// stopped reading its input cannot stall the emulator; if it ever fills,
	// dropping a reply degrades one program's startup, while blocking would
	// take down every session in the daemon.
	replies := make(chan []byte, 64)

	go func() {
		defer close(replies)
		buffer := make([]byte, 4<<10)
		for {
			n, err := s.term.Read(buffer)
			if n > 0 {
				reply := make([]byte, n)
				copy(reply, buffer[:n])
				select {
				case replies <- reply:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for reply := range replies {
		if !s.Running() {
			return
		}
		if _, err := s.pty.Write(reply); err != nil {
			return
		}
	}
}

// resolveCommand decides what to execute.
//
// A string command line goes through a shell so shell syntax works; an argv
// array is executed directly so no quoting is needed. The chosen shell is
// returned so the caller can report what actually started, which matters most
// on Windows where the answer is not obvious.
func resolveCommand(options Options) (argv []string, shell Shell, usedShell bool, err error) {
	if len(options.Argv) > 0 {
		if strings.TrimSpace(options.Argv[0]) == "" {
			return nil, Shell{}, false, errors.New("command array's first element must be a program name")
		}
		resolved, lookErr := exec.LookPath(options.Argv[0])
		if lookErr != nil {
			return nil, Shell{}, false, fmt.Errorf("command %q was not found on PATH", options.Argv[0])
		}
		return append([]string{resolved}, options.Argv[1:]...), Shell{}, false, nil
	}

	chosen, err := ResolveShell(options.Shell)
	if err != nil {
		return nil, Shell{}, false, err
	}
	if strings.TrimSpace(options.CommandLine) == "" {
		return []string{chosen.Path}, chosen, true, nil
	}
	return chosen.Argv(options.CommandLine), chosen, true, nil
}

// buildEnv merges the caller's variables over the inherited environment and
// sets the terminal-describing variables the child needs to render correctly.
func buildEnv(extra map[string]string, cols, rows int) []string {
	merged := map[string]string{}
	for _, entry := range os.Environ() {
		if index := strings.IndexByte(entry, '='); index > 0 {
			merged[entry[:index]] = entry[index+1:]
		}
	}
	// xterm-256color is what the emulator actually implements. Claiming more
	// would make programs emit sequences the emulator would then mis-render.
	merged["TERM"] = "xterm-256color"
	merged["COLORTERM"] = "truecolor"
	merged["COLUMNS"] = fmt.Sprint(cols)
	merged["LINES"] = fmt.Sprint(rows)
	// Marks the session so a program, or a curious user, can tell where it is.
	merged["INTERACTIVE_TERMINAL_MCP"] = "1"
	for key, value := range extra {
		if key == "" {
			continue
		}
		merged[key] = value
	}
	environment := make([]string, 0, len(merged))
	for key, value := range merged {
		environment = append(environment, key+"="+value)
	}
	return environment
}

// pump is the single reader of the PTY and the single writer of the emulator.
// Everything else observes; keeping one writer is what makes a snapshot taken
// under the emulator lock always land on a byte boundary.
func (s *Session) pump() {
	defer close(s.pumpDone)
	buffer := make([]byte, readChunk)
	for {
		n, err := s.pty.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			s.outputBytes.Add(int64(n))
			s.logs.writeRaw(chunk)
			_, _ = s.term.Write(chunk)
			s.logs.writeTranscript(s.term.TakeEvictedLines())
			s.subs.publish(chunk)
			s.noteActivity()
		}
		if err != nil {
			// A closed PTY is the normal end of a session, not a failure.
			s.logs.writeTranscript(s.term.TakeEvictedLines())
			s.logs.flush()
			return
		}
	}
}

// reap waits for the child, records its exit, and publishes the final state.
func (s *Session) reap() {
	err := s.command.Wait()
	code := exitCode(err)

	// Publish the exit immediately. Everything below is bookkeeping, and a
	// caller asking "did it stop" deserves the answer now rather than after
	// the logs have been flushed.
	s.mu.Lock()
	s.exited, s.exitCode, s.exitedAt = true, code, time.Now().UTC()
	s.mu.Unlock()
	close(s.processDone)

	// The child exiting does not end the PTY stream: this process still holds
	// the slave descriptor open, so reads from the master would block forever
	// instead of draining and reporting EOF. Releasing the slave lets the pump
	// consume everything the child wrote just before exiting and then finish.
	// Without this, the tail of a fast-finishing command is lost.
	s.releaseSlave()
	select {
	case <-s.pumpDone:
	case <-time.After(5 * time.Second):
		// A foreign process holding the slave open can keep the stream alive.
		// Recording the exit matters more than a complete final flush.
	}

	// The visible screen never scrolled off, so it is not in the transcript.
	// Appending it on exit is what lets it_tail show how a command ended.
	snapshot := s.term.Snapshot()
	s.logs.writeTranscript(s.term.TakeEvictedLines())
	if len(snapshot.Lines) > 0 {
		s.logs.writeTranscript(snapshot.Lines)
	}
	s.logs.flush()

	now := time.Now().UTC()
	s.mu.Lock()
	// A copy, not the address of the field: the metadata is marshalled later
	// without this lock, and handing out a pointer into the session would let
	// the encoder read a field another goroutine may be writing.
	exitedAt := s.exitedAt
	s.metadata.ExitedAt = &exitedAt
	s.metadata.ExitCode = &code
	s.metadata.LastActivityAt = now
	s.metadata.TranscriptLine = s.logs.lineCount()
	if s.killedBy != "" {
		s.metadata.KilledBy = s.killedBy
	}
	metadata := s.metadata
	s.mu.Unlock()

	_ = s.logs.writeMetadata(metadata)
	s.subs.closeAll()
	s.closeOne.Do(func() { close(s.done) })
	s.noteActivity()
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 0 {
			return code
		}
		return signalExitCode(exit)
	}
	return -1
}

// commandPID reports the child's process ID, or 0 before it starts.
func commandPID(command *pty.Cmd) int {
	if command == nil || command.Process == nil {
		return 0
	}
	return command.Process.Pid
}

func (s *Session) noteActivity() {
	now := time.Now().UTC()
	s.mu.Lock()
	s.lastActivity = now
	s.metadata.LastActivityAt = now
	s.mu.Unlock()

	s.activityMu.Lock()
	close(s.activity)
	s.activity = make(chan struct{})
	s.activityMu.Unlock()
}

func (s *Session) activityChannel() <-chan struct{} {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	return s.activity
}

// ID reports the session identifier.
func (s *Session) ID() string { return s.id }

// Name reports the session name, which may be empty.
func (s *Session) Name() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name
}

// Rename changes the session name. Uniqueness is the registry's concern.
func (s *Session) Rename(name string) {
	s.mu.Lock()
	s.name = name
	s.metadata.Name = name
	metadata := s.metadata
	s.mu.Unlock()
	_ = s.logs.writeMetadata(metadata)
}

// Running reports whether the child process is still alive.
func (s *Session) Running() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.exited
}

// ExitCode reports the exit status once the session has ended.
func (s *Session) ExitCode() (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exitCode, s.exited
}

// startedShell reports whether this session runs a shell rather than one
// program started directly. The distinction decides which question means
// "finished": a shell is idle between commands, a direct program is not.
func (s *Session) startedShell() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metadata.Shell
}

// CommandBusy reports whether the terminal definitely has a command running.
//
// True is proof. False is not: a shell loop such as `while :; do ...; done`
// executes inside the shell process itself, so the terminal's foreground group
// never leaves the shell even though a command is very much running. Treating
// that as "finished" is exactly how an interrupt a program ignored came to be
// reported as successful.
func (s *Session) CommandBusy() (busy bool, proven bool) {
	if !s.Running() {
		return false, true
	}
	busy, known := s.foregroundBusy()
	if !known || !busy {
		return false, false
	}
	return true, true
}

// OutputBytes reports how many bytes the child has written since it started.
// Zero means it has produced nothing at all, which is different from having
// produced something and then gone quiet.
func (s *Session) OutputBytes() int64 { return s.outputBytes.Load() }

// LastActivity reports when the session last produced output.
func (s *Session) LastActivity() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastActivity
}

// Metadata returns a snapshot of the durable session description.
func (s *Session) Metadata() Metadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	metadata := s.metadata
	metadata.TranscriptLine = s.logs.lineCount()
	return metadata
}

// Done is closed once the session has exited and its final state is durable.
func (s *Session) Done() <-chan struct{} { return s.done }

// Snapshot returns the current visible screen.
func (s *Session) Snapshot() vterm.Snapshot { return s.term.Snapshot() }

// Modes reports the input-affecting terminal modes the program has set.
func (s *Session) Modes() vterm.Modes { return s.term.Modes() }

// Render returns the styled screen for the interactive application.
func (s *Session) Render() string { return s.term.Render() }

// ScrollbackLines reports how many lines are retained above the screen.
func (s *Session) ScrollbackLines() int { return s.term.ScrollbackLines() }

// ScrollbackText returns retained lines above the screen, oldest first.
func (s *Session) ScrollbackText(offset, n int) []string {
	return s.term.ScrollbackText(offset, n)
}

// TranscriptPath is the absolute path to the durable text log.
func (s *Session) TranscriptPath() string { return s.logs.transcriptPath() }

// RawPath is the absolute path to the raw byte log.
func (s *Session) RawPath() string {
	return strings.TrimSuffix(s.logs.transcriptPath(), "transcript.log") + "raw.log"
}

// TranscriptLines reports how many lines the transcript holds.
func (s *Session) TranscriptLines() int { return s.logs.lineCount() }

// Flush makes buffered log writes visible to readers. it_tail and it_head call
// it first so they never miss output the session has already produced.
func (s *Session) Flush() { s.logs.flush() }

// Size reports the terminal dimensions.
func (s *Session) Size() (int, int) { return s.term.Size() }

// Write sends raw bytes to the terminal as if typed.
func (s *Session) Write(p []byte) error {
	if !s.Running() {
		return ErrExited
	}
	if len(p) == 0 {
		return nil
	}
	_, err := s.pty.Write(p)
	return err
}

// WritePaste wraps content in bracketed-paste markers when the program has
// enabled them, so an editor receives multi-line input as one paste rather
// than as a sequence of commands.
func (s *Session) WritePaste(text string) error {
	if !s.Modes().BracketedPaste {
		return s.Write([]byte(text))
	}
	return s.Write([]byte("\x1b[200~" + text + "\x1b[201~"))
}

// Resize changes the terminal size and notifies the child. Both the PTY and
// the emulator must move together or the child will draw for the wrong size.
func (s *Session) Resize(cols, rows int) error {
	if cols < 1 || rows < 1 {
		return fmt.Errorf("terminal size must be positive, got %dx%d", cols, rows)
	}
	if err := s.pty.Resize(cols, rows); err != nil && s.Running() {
		return fmt.Errorf("resize pty: %w", err)
	}
	s.term.Resize(cols, rows)
	s.mu.Lock()
	s.metadata.Cols, s.metadata.Rows = cols, rows
	metadata := s.metadata
	s.mu.Unlock()
	_ = s.logs.writeMetadata(metadata)
	return nil
}

// SettleResult describes how a wait ended.
type SettleResult struct {
	// Matched is true when a wait_for string appeared on the screen, which is
	// the only completion signal that does not depend on guessing from output
	// timing. A silent command is indistinguishable from a finished one
	// otherwise, so this is how a caller waits for one.
	Matched bool

	// Settled is true when output stopped changing before the budget expired.
	// False tells the agent the screen may still be in flux.
	Settled bool
	// Waited is how long the wait actually took.
	Waited time.Duration
	// Exited is true when the session ended during the wait.
	Exited bool
}

// WaitSettled blocks until the session has produced no output for quiet, or
// budget elapses, or the session exits.
//
// This is what makes a single `wait` argument correct for both fast and slow
// commands: it is a ceiling, not a sleep, so a quick echo returns in
// milliseconds while a long build uses its full budget and reports that it did.
func (s *Session) WaitSettled(ctx context.Context, budget, quiet time.Duration) SettleResult {
	start := time.Now()
	if quiet <= 0 {
		quiet = 250 * time.Millisecond
	}

	// A short coalesce always runs first so a redraw already in flight is
	// captured after it finishes rather than halfway through.
	const coalesce = 30 * time.Millisecond
	if budget <= 0 {
		select {
		case <-time.After(coalesce):
		case <-ctx.Done():
		}
		return SettleResult{Settled: false, Waited: time.Since(start), Exited: !s.Running()}
	}

	// A session that has never produced anything cannot be called settled just
	// because it is quiet: a shell that has not finished starting looks
	// identical to one that has finished working. Waiting for the first byte
	// distinguishes them. A session that has already drawn something is judged
	// purely on the quiet window, so reading an idle prompt still returns at
	// once.
	needFirstOutput := s.OutputBytes() == 0

	deadline := time.After(budget)
	for {
		activity := s.activityChannel()
		quietTimer := time.NewTimer(quiet)
		select {
		case <-ctx.Done():
			quietTimer.Stop()
			return SettleResult{Waited: time.Since(start), Exited: !s.Running()}
		case <-deadline:
			quietTimer.Stop()
			return SettleResult{Settled: false, Waited: time.Since(start), Exited: !s.Running()}
		case <-s.processDone:
			quietTimer.Stop()
			// Let the reaper append the final screen before returning.
			select {
			case <-time.After(coalesce):
			case <-ctx.Done():
			}
			return SettleResult{Settled: true, Waited: time.Since(start), Exited: true}
		case <-activity:
			quietTimer.Stop()
			// Output arrived; restart the quiet window.
			needFirstOutput = false
		case <-quietTimer.C:
			if needFirstOutput && s.OutputBytes() == 0 {
				// Still nothing at all. Keep waiting for the budget rather than
				// reporting a blank screen as a finished one.
				continue
			}
			return SettleResult{Settled: true, Waited: time.Since(start), Exited: !s.Running()}
		}
	}
}

// WaitUntil blocks until text appears on the screen, the budget expires, or
// the session exits.
//
// This exists because quiet is a poor proxy for finished. A command that
// prints nothing looks complete the instant it starts, and one that prints
// periodically looks complete between lines. Waiting for something the caller
// knows will appear -- a prompt, a word the command ends with -- is exact.
//
// What is counted is a new occurrence, not any occurrence. The terminal echoes
// the command as it is typed, so waiting for a word that command contains
// would otherwise match the echo instantly and report a command finished
// before it had started.
func (s *Session) WaitUntil(ctx context.Context, budget, quiet time.Duration, text string) SettleResult {
	if text == "" {
		return s.WaitSettled(ctx, budget, quiet)
	}
	start := time.Now()
	if budget <= 0 {
		budget = 30 * time.Second
	}

	// Let the echo of whatever was just typed reach the screen before counting,
	// so it is part of the baseline rather than mistaken for the result.
	const echoGrace = 250 * time.Millisecond
	select {
	case <-ctx.Done():
		return SettleResult{Waited: time.Since(start), Exited: !s.Running()}
	case <-time.After(echoGrace):
	case <-s.processDone:
	}
	baseline := strings.Count(s.term.Snapshot().Text(), text)

	deadline := time.After(budget)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	matched := func() bool {
		current := strings.Count(s.term.Snapshot().Text(), text)
		// A greater count means it appeared again. A smaller one means the
		// screen scrolled past the baseline, so any occurrence now is new.
		return current > baseline || (current > 0 && current < baseline)
	}

	for {
		if matched() {
			return SettleResult{Settled: true, Matched: true,
				Waited: time.Since(start), Exited: !s.Running()}
		}
		select {
		case <-ctx.Done():
			return SettleResult{Waited: time.Since(start), Exited: !s.Running()}
		case <-deadline:
			return SettleResult{Waited: time.Since(start), Exited: !s.Running()}
		case <-s.processDone:
			// The session ended. Give the final screen a moment to land, then
			// report whether the text ever arrived.
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
			}
			return SettleResult{Settled: true, Matched: matched(),
				Waited: time.Since(start), Exited: true}
		case <-ticker.C:
		}
	}
}

// Subscribe returns a channel of raw output for an attached viewer, plus a
// cancel function. A viewer that cannot keep up is resynchronized rather than
// backing up the pump, so a slow human view can never slow an agent tool call.
func (s *Session) Subscribe() (<-chan []byte, func()) { return s.subs.subscribe() }

// Kill ends the session. signal selects how.
//
// INT asks the foreground program to stop rather than signalling the child,
// because under a shell the child is the shell: a SIGINT delivered to it does
// not reach whatever it is currently running. How that is done differs by
// platform; see interrupt_unix.go and interrupt_windows.go.
//
// An interrupt is a request, not a guarantee. A program is free to ignore it,
// and the caller must observe the outcome rather than assume one.
func (s *Session) Kill(signal string, by string) error {
	if !s.Running() {
		return ErrExited
	}
	s.mu.Lock()
	s.killedBy = by
	s.mu.Unlock()

	switch strings.ToUpper(signal) {
	case "", "TERM":
		return s.signal("TERM")
	case "INT":
		return s.interrupt()
	case "HUP":
		return s.signal("HUP")
	case "KILL":
		return s.signal("KILL")
	default:
		return fmt.Errorf("unsupported signal %q; use TERM, INT, HUP, or KILL", signal)
	}
}

// WaitExit blocks until the child process ends or the context is cancelled.
//
// It waits on the process rather than on the finalisation that follows it, so
// a caller that signalled a session learns it worked as soon as it did. Use
// WaitFinalized before reading logs: the last of the output is still being
// written when this returns.
func (s *Session) WaitExit(ctx context.Context) bool {
	select {
	case <-s.processDone:
		return true
	case <-ctx.Done():
		return false
	}
}

// WaitFinalized blocks until the session's output is completely on disk and
// its metadata is durable.
//
// This is the signal a log reader needs. The process ending and its output
// being fully recorded are different moments, and treating them as one either
// delays every kill or hands back a truncated transcript.
func (s *Session) WaitFinalized(ctx context.Context) bool {
	select {
	case <-s.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Close releases the PTY and logs.
//
// It waits briefly for the reaper, which is still writing meta.json and the
// final transcript lines. Closing the log store underneath it would silently
// discard a session's last output and leave meta.json describing a session
// that looks like it is still running.
func (s *Session) Close() {
	_ = s.pty.Close()
	if s.Running() {
		_ = s.signal("KILL")
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
	}
	s.subs.closeAll()
	s.closeOne.Do(func() { close(s.done) })
	_ = s.term.Close()
	s.logs.close()
}
