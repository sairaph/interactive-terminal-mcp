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

	// Integrate starts the shell with an integration script so it reports its
	// own command boundaries and exit codes.
	Integrate bool

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

	// entry is the command line to type once the shell has drawn a prompt. The
	// caller runs it rather than the constructor so the typing and the wait that
	// follows it are one operation.
	entry string
}

// Entry is the command line this session was asked to run, to be typed into the
// shell once it is ready. It is empty for a session that is only a shell.
func (s *Session) Entry() string { return s.entry }

// New starts a session. The returned session is running unless an error is
// reported; the caller owns calling Close.
func New(options Options) (*Session, error) {
	if options.Cols < 1 {
		options.Cols = 120
	}
	if options.Rows < 1 {
		options.Rows = 30
	}

	startup, entry, display, shell, err := resolveCommand(options)
	if err != nil {
		return nil, err
	}
	startup = applyIntegration(startup, shell, options)
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

	command := terminal.Command(startup[0], startup[1:]...)
	command.Dir = cwd
	command.Env = buildEnv(options.Env, options.Cols, options.Rows)

	if err := command.Start(); err != nil {
		terminal.Close()
		logs.close()
		return nil, fmt.Errorf("start %s: %w", startup[0], err)
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
		ID: options.ID, Name: options.Name, Command: display,
		CommandLine: options.CommandLine, Shell: true,
		ShellID: shell.ID, ShellPath: shell.Path, ShellName: shell.Display,
		Cwd: cwd, Env: options.Env, Cols: options.Cols, Rows: options.Rows,
		PID: commandPID(command), CreatedAt: now, LastActivityAt: now,
	}
	_ = logs.writeMetadata(session.metadata)

	session.entry = entry

	go session.pump()
	go session.answerQueries()
	go session.reap()

	// The command is typed in here rather than by the caller so that every way
	// of making a session behaves the same, and so that a session is never
	// handed back with a command pending that nobody entered.
	if entry != "" {
		session.enterCommand(entry)
	}
	return session, nil
}

// promptGrace bounds the wait for a shell to draw something before a command is
// typed into it.
//
// It is short on purpose. The terminal's input buffer holds anything written
// before the shell reads it, so typing early costs nothing but the echo landing
// above the prompt rather than after it -- the same thing that happens when a
// person pastes into a slow terminal. Waiting long enough to be certain is the
// worse trade: a shell whose startup files read from stdin prints nothing at
// all until something is typed, so any wait for a prompt on such a machine
// burns its whole budget and then types anyway.
const promptGrace = 750 * time.Millisecond

// enterCommand gives the shell a moment to draw, then types the command.
//
// What it waits for is the first byte, not quiet. A prompt is the first thing a
// shell prints, so that is the cheapest signal that it is up and the echo will
// land in a sensible place; waiting for quiet on top of it would add the settle
// interval to the creation of every session that runs something.
func (s *Session) enterCommand(commandLine string) {
	deadline := time.After(promptGrace)
	for s.OutputBytes() == 0 {
		activity := s.activityChannel()
		select {
		case <-activity:
		case <-deadline:
			// Nothing drawn. The shell may simply be slow, or its startup files
			// may be waiting on stdin themselves, in which case what is typed
			// next is what unblocks it.
		case <-s.processDone:
		}
		break
	}
	// A failure here leaves a usable shell with nothing typed into it, which is
	// better than refusing to hand back a terminal that works.
	_ = s.TypeCommand(commandLine)
}

// TypeCommand types a command line and submits it, exactly as a person would.
//
// Multi-line input goes through bracketed paste when the program has enabled
// it, so an editor receives one paste rather than a sequence of commands.
func (s *Session) TypeCommand(text string) error {
	if strings.ContainsAny(text, "\n\r") {
		if err := s.WritePaste(strings.TrimRight(text, "\r\n")); err != nil {
			return err
		}
		return s.Write([]byte{'\r'})
	}
	return s.Write([]byte(text + "\r"))
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

// resolveCommand decides what to execute and what to report.
//
// A session is always an interactive shell. A command is not executed *as* the
// session; it is typed into it, the way a person opens a terminal and runs
// something. That is the difference between a terminal and a subprocess: when
// the command finishes there is still a shell there, still holding the working
// directory and the scrollback, ready for the next thing. Running the command
// as the session process instead made every session die the moment its command
// did, which made an installer that asks a question impossible to answer.
//
// startup is what gets executed. entry is the command line to type once the
// shell is ready, empty for a bare shell. display is what the session reports
// running, which is what the caller asked for rather than the shell wrapping it.
func resolveCommand(options Options) (startup []string, entry string, display []string, shell Shell, err error) {
	chosen, err := ResolveShell(options.Shell)
	if err != nil {
		return nil, "", nil, Shell{}, err
	}

	if len(options.Argv) > 0 {
		if strings.TrimSpace(options.Argv[0]) == "" {
			return nil, "", nil, Shell{}, errors.New("command array's first element must be a program name")
		}
		// The program is resolved here rather than left to the shell so that a
		// name that does not exist is an error on creation, with the name in it,
		// instead of a "command not found" the caller has to go and read.
		resolved, lookErr := exec.LookPath(options.Argv[0])
		if lookErr != nil {
			return nil, "", nil, Shell{}, fmt.Errorf("command %q was not found on PATH", options.Argv[0])
		}
		wanted := append([]string{resolved}, options.Argv[1:]...)
		// A shell named on its own is the session's shell, not something typed
		// into another one. ["bash"] should be a bash session, the same as
		// shell: "bash", rather than a bash running inside the default shell.
		if named, ok := shellForProgram(wanted); ok {
			return wanted, "", wanted, named, nil
		}
		return []string{chosen.Path}, chosen.CommandLineFor(wanted), wanted, chosen, nil
	}

	if line := strings.TrimSpace(options.CommandLine); line != "" {
		return []string{chosen.Path}, options.CommandLine, []string{chosen.Path}, chosen, nil
	}
	return []string{chosen.Path}, "", []string{chosen.Path}, chosen, nil
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

// CommandBusy reports whether a command is running in the terminal, and
// whether that could be established at all.
//
// The two results are not equally strong, and callers are expected to treat
// them differently. True is proof: the terminal has handed the foreground to
// something that is not the shell. False means no separate command holds the
// terminal, which is an idle prompt nearly always and the shell working on its
// own occasionally -- a loop such as `while :; do ...; done` runs inside the
// shell process itself and never takes the foreground. So false is good
// evidence of finished and not proof of it, which is why anything built on it
// says "most likely" rather than asserting.
//
// Where neither can be established, known is false and nothing is claimed.
func (s *Session) CommandBusy() (busy bool, known bool) {
	if !s.Running() {
		return false, true
	}
	// A shell that reports its own command boundaries is the authority, and it
	// is right where the platform checks are weakest: it sees work the shell
	// does inside itself, and it works the same on Windows, which has no
	// foreground process group to read at all.
	// MarksExecution matters: a shell that marks only prompts and exit codes
	// would report every command as finished the moment it started, which is
	// the false idle this whole field exists to avoid.
	if state := s.term.Commands(); state.Integrated && state.MarksExecution {
		return state.Running, true
	}
	return s.foregroundBusy()
}

// Commands reports what the shell has said about its own command boundaries.
// Integrated is false when it has said nothing, which is the usual case until
// shell integration is set up.
func (s *Session) Commands() vterm.CommandState { return s.term.Commands() }

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
	// Observed is true when the wait was long enough to reach a verdict either
	// way. A caller that asked for no wait gets Settled: false and Observed:
	// false, which are different facts: nothing was seen, rather than something
	// was seen still arriving.
	Observed bool
	// Waited is how long the wait actually took.
	Waited time.Duration
	// Budget is the ceiling the wait was given, which is not the same as the
	// time it took. Reporting the elapsed time as though it were the ceiling
	// made a 30ms early return read as a 30ms limit.
	Budget time.Duration
	// Exited is true when the session ended during the wait.
	Exited bool
}

// WaitTarget describes an exact wait for text to appear on the screen.
type WaitTarget struct {
	// Text is what to wait for.
	Text string

	// Echo is input that was just written to the terminal and that the shell
	// will print back. Occurrences of Text inside it are discounted, so waiting
	// for a word the command itself contains does not match the instant the
	// command is typed.
	Echo string

	// Baseline is how many times Text was already on the screen before that
	// input was written. Anything that was already there is not a result.
	Baseline int
}

// Wanted reports whether an exact wait was asked for.
func (t WaitTarget) Wanted() bool { return t.Text != "" }

// flattenLines joins screen rows into one string.
//
// Matching runs against the flattened screen so that text the terminal wrapped
// across two rows still counts. Rows have no trailing whitespace, so joining
// without a separator reconstructs a wrapped word exactly.
func flattenLines(lines []string) string { return strings.Join(lines, "") }

// flattenText removes line breaks from a needle so it can be matched against
// the flattened screen.
func flattenText(text string) string {
	return strings.NewReplacer("\r\n", "", "\r", "", "\n", "").Replace(text)
}

// CountOnScreen reports how many times text appears on the visible screen.
//
// Callers use it to take a baseline before writing input, so that output which
// was already on the screen is never mistaken for a result.
func (s *Session) CountOnScreen(text string) int {
	needle := flattenText(text)
	if needle == "" {
		return 0
	}
	return strings.Count(flattenLines(s.term.Snapshot().Lines), needle)
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
		// Nothing was waited for, so nothing was established. Observed stays
		// false to keep this apart from a wait that ran and saw output still
		// arriving.
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
			return SettleResult{Waited: time.Since(start), Budget: budget, Exited: !s.Running()}
		case <-deadline:
			quietTimer.Stop()
			// The budget ran out with output still coming. That is a real
			// observation: the screen may well be incomplete.
			return SettleResult{Settled: false, Observed: true,
				Waited: time.Since(start), Budget: budget, Exited: !s.Running()}
		case <-s.processDone:
			quietTimer.Stop()
			// Let the reaper append the final screen before returning.
			select {
			case <-time.After(coalesce):
			case <-ctx.Done():
			}
			return SettleResult{Settled: true, Observed: true,
				Waited: time.Since(start), Budget: budget, Exited: true}
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
			return SettleResult{Settled: true, Observed: true,
				Waited: time.Since(start), Budget: budget, Exited: !s.Running()}
		}
	}
}

// echoRule decides whether the target text on screen is a result or just the
// terminal repeating what was typed.
//
// The hard part is that a command line is echoed a character at a time. An
// earlier version tested whether the whole submitted line was on screen and,
// if so, discounted the occurrences it contains. That left a window: while the
// echo was still being drawn, the target could already be visible inside it
// while the complete line was not, so the discount did not apply and the echo
// counted as a result. A 6000-line command reported finished in 551ms, having
// matched its own echo.
//
// The fix is to stop looking for the whole line. What is checked is the
// witness: the input truncated at the end of its last occurrence of the
// target. Since a terminal draws left to right, the witness is on screen
// exactly when the target is on screen because of the echo, so there is no
// moment where one is visible and the other is not. The window does not shrink
// -- it stops existing.
type echoRule struct {
	needle   string
	witness  string
	echoed   int
	baseline int
}

func newEchoRule(needle, echo string, baseline int) echoRule {
	rule := echoRule{needle: needle, baseline: baseline}
	if needle == "" || echo == "" {
		return rule
	}
	last := strings.LastIndex(echo, needle)
	if last < 0 {
		// The input does not contain the target, so its echo can never be
		// mistaken for one. Nothing needs discounting.
		return rule
	}
	rule.echoed = strings.Count(echo, needle)
	rule.witness = echo[:last+len(needle)]
	return rule
}

// matches reports whether the screen holds an occurrence that is neither part
// of the echo nor something that was already there.
func (r echoRule) matches(screen string) bool {
	if r.needle == "" {
		return false
	}
	count := strings.Count(screen, r.needle)
	if count == 0 {
		return false
	}
	threshold := r.baseline
	// Only discount the echo while it is still on the screen. Once output has
	// pushed it off, its occurrences are no longer among those being counted,
	// which is what lets a long-running command match its own final marker.
	if r.echoed > 0 && strings.Contains(screen, r.witness) {
		threshold += r.echoed
	}
	// Fewer occurrences than there were to begin with means the screen has
	// scrolled past the baseline, so what is on it now arrived since.
	return count > threshold || count < r.baseline
}

// WaitUntil blocks until the target text appears on the screen, the budget
// expires, or the session exits.
//
// This exists because quiet is a poor proxy for finished. A command that
// prints nothing looks complete the instant it starts, and one that prints
// periodically looks complete between lines. Waiting for something the caller
// knows will appear -- a prompt, a word the command ends with -- is exact.
//
// Two things on the screen are not results, and both are excluded by counting
// rather than by waiting. Text that was already there before the input was
// written is held in target.Baseline. Text the terminal echoes back as the
// command is typed is accounted for by discounting the occurrences the input
// itself contains, which is what makes waiting for a word inside the command
// work. Nothing is excluded on the basis of when it arrived, so a command that
// finishes in a millisecond is matched just as a slow one is.
func (s *Session) WaitUntil(ctx context.Context, budget, quiet time.Duration, target WaitTarget) SettleResult {
	if !target.Wanted() {
		return s.WaitSettled(ctx, budget, quiet)
	}
	start := time.Now()
	if budget <= 0 {
		budget = 30 * time.Second
	}

	needle := flattenText(target.Text)
	echo := flattenText(strings.TrimRight(target.Echo, "\r\n"))
	rule := newEchoRule(needle, echo, target.Baseline)

	matched := func() bool {
		return rule.matches(flattenLines(s.term.Snapshot().Lines))
	}

	// Give the echo a moment to land before the first verdict. This is no
	// longer what makes the count correct -- the rule below handles a
	// half-drawn echo on its own -- but it keeps the common case from being
	// decided on a screen that has not caught up yet.
	if rule.echoed > 0 {
		s.awaitEcho(ctx, rule.witness, 300*time.Millisecond)
	}

	deadline := time.After(budget)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if matched() {
			return SettleResult{Settled: true, Observed: true, Matched: true,
				Waited: time.Since(start), Budget: budget, Exited: !s.Running()}
		}
		select {
		case <-ctx.Done():
			return SettleResult{Waited: time.Since(start), Budget: budget, Exited: !s.Running()}
		case <-deadline:
			return SettleResult{Observed: true,
				Waited: time.Since(start), Budget: budget, Exited: !s.Running()}
		case <-s.processDone:
			// The session ended. Give the final screen a moment to land, then
			// report whether the text ever arrived.
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
			}
			return SettleResult{Settled: true, Observed: true, Matched: matched(),
				Waited: time.Since(start), Budget: budget, Exited: true}
		case <-ticker.C:
		}
	}
}

// awaitEcho waits for input just written to appear on the screen, giving up
// after limit so a program that does not echo cannot stall the caller.
func (s *Session) awaitEcho(ctx context.Context, echo string, limit time.Duration) {
	deadline := time.After(limit)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if strings.Contains(flattenLines(s.term.Snapshot().Lines), echo) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-s.processDone:
			return
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
