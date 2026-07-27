package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
	"github.com/sairaph/interactive-terminal-mcp/internal/keys"
	"github.com/sairaph/interactive-terminal-mcp/internal/session"
	"github.com/sairaph/interactive-terminal-mcp/internal/vterm"
)

// escalateAfter is how long a TERM is given before a KILL follows.
const escalateAfter = 5 * time.Second

func (d *Daemon) handleList() (ipc.ListResult, error) {
	entries := d.registry.list()
	active := d.registry.activeID()
	sessions := make([]ipc.SessionInfo, 0, len(entries))
	for _, item := range entries {
		sessions = append(sessions, d.describeSession(item, active))
	}
	return ipc.ListResult{Active: active, Sessions: sessions}, nil
}

func (d *Daemon) handleNew(ctx context.Context, args ipc.NewArgs) (ipc.Screen, error) {
	settings := d.registry.settingsSnapshot()

	name := strings.TrimSpace(args.Name)
	if err := d.registry.nameAvailable(name, ""); err != nil {
		return ipc.Screen{}, err
	}

	total, _ := d.registry.counts()
	if live := countLive(d.registry.list()); live >= settings.MaximumSessions {
		return ipc.Screen{}, &ipc.Error{
			Code:    ipc.CodeTooManySessions,
			Message: fmt.Sprintf("%d sessions are already running, which is the configured maximum", live),
			Hint:    "End a session with it_kill({\"session\":\"<id>\"}), or raise the limit in `interactive-terminal-mcp configure`.",
			Fields:  map[string]any{"live_sessions": live, "maximum": settings.MaximumSessions},
		}
	}
	_ = total

	cols, rows := args.Cols, args.Rows
	if cols <= 0 {
		cols = settings.DefaultCols
	}
	if rows <= 0 {
		rows = settings.DefaultRows
	}
	if err := validateSize(cols, rows); err != nil {
		return ipc.Screen{}, err
	}

	id, err := d.registry.newID()
	if err != nil {
		return ipc.Screen{}, err
	}
	directory := d.paths.SessionDir(id)

	live, err := session.New(session.Options{
		ID: id, Name: name,
		CommandLine: args.CommandLine, Argv: args.Argv,
		Cwd: args.Cwd, Env: args.Env, Cols: cols, Rows: rows,
		Directory:          directory,
		ScrollbackLines:    settings.ScrollbackLines,
		RawLogMaxBytes:     settings.RawLogMaxBytes,
		TranscriptMaxLines: settings.TranscriptMaxLines,
	})
	if err != nil {
		// The directory was created before the failure; leaving it behind
		// would make an id look used and clutter the sessions root.
		_ = os.RemoveAll(directory)
		return ipc.Screen{}, &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: err.Error(),
			Hint:    "Check the command, working directory, and environment, then retry it_new.",
		}
	}
	d.registry.add(live, directory)
	d.touch()

	settled := live.WaitSettled(ctx, time.Duration(args.WaitMS)*time.Millisecond, settings.SettleQuiet)
	return d.screen(live, settled), nil
}

func (d *Daemon) handleRead(ctx context.Context, args ipc.ReadArgs) (ipc.Screen, error) {
	item, err := d.registry.resolveOrActive(args.Session)
	if err != nil {
		return ipc.Screen{}, err
	}
	if item.live == nil {
		return ipc.Screen{}, retainedScreenError(item)
	}
	settings := d.registry.settingsSnapshot()

	if args.Cols > 0 || args.Rows > 0 {
		cols, rows := item.live.Size()
		if args.Cols > 0 {
			cols = args.Cols
		}
		if args.Rows > 0 {
			rows = args.Rows
		}
		if err := validateSize(cols, rows); err != nil {
			return ipc.Screen{}, err
		}
		if err := item.live.Resize(cols, rows); err != nil {
			return ipc.Screen{}, internalError(err)
		}
		// A program redraws after SIGWINCH. Snapshotting immediately would
		// capture the screen mid-redraw, so a resize always waits for quiet
		// even when the caller asked for no wait.
		if args.WaitMS <= 0 {
			args.WaitMS = int64(settings.SettleQuiet/time.Millisecond) * 4
		}
	}

	settled := item.live.WaitSettled(ctx, time.Duration(args.WaitMS)*time.Millisecond, settings.SettleQuiet)
	return d.screen(item.live, settled), nil
}

func (d *Daemon) handleSend(ctx context.Context, args ipc.SendArgs) (ipc.Screen, error) {
	item, err := d.registry.requireLive(args.Session)
	if err != nil {
		return ipc.Screen{}, err
	}
	settings := d.registry.settingsSnapshot()
	live := item.live

	// Everything is validated before anything is written. A key sequence that
	// fails halfway would leave the terminal in a state neither the agent nor
	// the user can predict.
	var chords []keys.Chord
	if args.HasKeys {
		parsed, parseErr := keys.Parse(args.Keys)
		if parseErr != nil {
			return ipc.Screen{}, &ipc.Error{
				Code:    ipc.CodeInvalidInput,
				Message: parseErr.Error(),
				Hint:    "Nothing was sent. Correct the keys argument and retry.",
				Fields:  map[string]any{"keys": args.Keys},
			}
		}
		chords = parsed
	}

	if args.HasText {
		if err := writeText(live, args.Text, args.Enter); err != nil {
			return ipc.Screen{}, sendError(err, item)
		}
	}
	if len(chords) > 0 {
		encoded := keys.Encode(chords, live.Modes())
		if err := live.Write(encoded); err != nil {
			return ipc.Screen{}, sendError(err, item)
		}
	}

	settled := live.WaitSettled(ctx, time.Duration(args.WaitMS)*time.Millisecond, settings.SettleQuiet)
	return d.screen(live, settled), nil
}

// writeText types literal text, appending a carriage return when asked.
//
// Multi-line text goes through bracketed paste when the program has enabled
// it, so an editor receives one paste instead of a sequence of commands. A
// trailing newline is not counted: it is the submission, not part of the text.
func writeText(live *session.Session, text string, enter bool) error {
	body := text
	trailing := strings.HasSuffix(body, "\n") || strings.HasSuffix(body, "\r")

	if !trailing && enter {
		if strings.ContainsAny(body, "\n\r") {
			// Interior newlines with a submit at the end: paste the body, then
			// submit separately so the paste markers wrap only the content.
			if err := live.WritePaste(body); err != nil {
				return err
			}
			return live.Write([]byte{'\r'})
		}
		return live.Write([]byte(body + "\r"))
	}

	if strings.ContainsAny(strings.TrimSuffix(strings.TrimSuffix(body, "\n"), "\r"), "\n\r") {
		return live.WritePaste(body)
	}
	return live.Write([]byte(body))
}

func sendError(err error, item *entry) error {
	if err == session.ErrExited {
		return exitedError(item)
	}
	return internalError(err)
}

func (d *Daemon) handleKill(ctx context.Context, args ipc.KillArgs) (ipc.KillResult, error) {
	// Deliberately resolve rather than resolveOrActive: killing the wrong
	// terminal is destructive, so the caller always names its target.
	if strings.TrimSpace(args.Session) == "" {
		return ipc.KillResult{}, &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: "session is required for it_kill",
			Hint:    "Name the session explicitly, for example it_kill({\"session\":\"build\"}). The active session is never assumed here.",
			Fields:  map[string]any{"field": "session"},
		}
	}
	item, err := d.registry.resolve(args.Session)
	if err != nil {
		return ipc.KillResult{}, err
	}

	signal := strings.ToUpper(strings.TrimSpace(args.Signal))
	if signal == "" {
		signal = "TERM"
	}
	switch signal {
	case "TERM", "INT", "HUP", "KILL":
	default:
		return ipc.KillResult{}, &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: fmt.Sprintf("unsupported signal %q", args.Signal),
			Hint:    "Use TERM (default), INT, HUP, or KILL.",
			Fields:  map[string]any{"signal": args.Signal},
		}
	}

	settings := d.registry.settingsSnapshot()
	result := ipc.KillResult{Killed: item.id(), Name: item.name(), Signal: signal}

	if !item.running() {
		// Killing an already-finished session is not an error: the caller's
		// intent was that it should not be running, and it is not.
		result.AlreadyGone = true
		if code, ok := exitCodeOf(item); ok {
			result.ExitCode = &code
		}
		d.retire(item, settings, &result)
		return result, nil
	}

	live := item.live
	if err := live.Kill(signal, "it_kill"); err != nil && err != session.ErrExited {
		// A signal that reports failure while the process is on its way out is
		// routine, especially on Windows where taskkill fails if any child has
		// already gone. Only give up if the session is genuinely still alive
		// afterwards; otherwise fall through and retire it, or the caller is
		// told the kill failed while a dead entry stays in the list.
		wait, cancel := context.WithTimeout(ctx, 2*time.Second)
		exited := live.WaitExit(wait)
		cancel()
		if !exited && live.Running() {
			return ipc.KillResult{}, internalError(err)
		}
	}

	// INT is a request to the foreground program, not a guarantee it ends, so
	// it is never escalated. The others are meant to end the session, so a
	// process that ignores TERM is escalated to KILL rather than leaving the
	// caller believing the terminal is gone when it is not.
	if signal != "INT" {
		wait, cancel := context.WithTimeout(ctx, escalateAfter)
		exited := live.WaitExit(wait)
		cancel()
		if !exited && signal != "KILL" {
			_ = live.Kill("KILL", "it_kill escalation")
			result.Escalated = true
			wait, cancel := context.WithTimeout(ctx, escalateAfter)
			live.WaitExit(wait)
			cancel()
		}
		if code, ok := live.ExitCode(); ok {
			result.ExitCode = &code
		}
		d.retire(item, settings, &result)
		return result, nil
	}

	// After an interrupt the session usually survives; report its state rather
	// than pretending it ended.
	if code, ok := live.ExitCode(); ok {
		result.ExitCode = &code
		d.retire(item, settings, &result)
	} else {
		result.LogsRetained = true
		result.LogPath = live.TranscriptPath()
	}
	return result, nil
}

// retire closes a finished session and applies the retention policy.
func (d *Daemon) retire(item *entry, settings config.Config, result *ipc.KillResult) {
	if item.live != nil {
		item.live.Close()
	}
	if settings.LogRetention == config.RetentionOnClose {
		directory := d.registry.remove(item.id())
		if directory != "" {
			_ = os.RemoveAll(directory)
		}
		result.LogsRetained = false
		result.LogPath = ""
		return
	}
	// Keep the entry so its logs stay reachable, but drop the live session.
	d.registry.mu.Lock()
	if current, ok := d.registry.entries[item.id()]; ok {
		if current.live != nil {
			current.metadata = current.live.Metadata()
			current.live = nil
		}
		current.retainedAt = time.Now().UTC()
		if d.registry.active == item.id() {
			// The killed session stops being active; an exited-but-not-killed
			// one stays active so its final screen remains easy to read.
			d.registry.active = d.registry.mostRecentLiveLocked()
		}
	}
	d.registry.mu.Unlock()

	result.LogsRetained = true
	result.LogPath = filepath.Join(item.directory, "transcript.log")
}

func (d *Daemon) handleLog(args ipc.LogArgs) (ipc.LogResult, error) {
	item, err := d.registry.resolveOrActive(args.Session)
	if err != nil {
		return ipc.LogResult{}, err
	}
	lines := args.Lines
	if lines < 1 {
		lines = 100
	}

	path := filepath.Join(item.directory, "transcript.log")
	if item.live != nil {
		// Buffered writes must reach the file first, or a tail taken right
		// after a command would miss the output the agent is asking about.
		item.live.Flush()
		path = item.live.TranscriptPath()
	}

	var slice session.LogSlice
	if args.FromEnd {
		slice, err = session.Tail(path, lines)
	} else {
		slice, err = session.Head(path, lines)
	}
	if err != nil {
		return ipc.LogResult{}, internalError(fmt.Errorf("read transcript: %w", err))
	}

	result := ipc.LogResult{
		Session:    d.describeSession(item, d.registry.activeID()),
		Lines:      slice.Lines,
		TotalLines: slice.Total,
		AtStart:    slice.AtStart,
		AtEnd:      slice.AtEnd,
		LogPath:    path,
	}
	if args.Screen && item.live != nil {
		snapshot := item.live.Snapshot()
		result.ScreenLines = snapshot.Lines
		result.AltScreen = snapshot.AltScreen
	}
	return result, nil
}

func (d *Daemon) handleActive(ctx context.Context, args ipc.ActiveArgs) (ipc.ActiveResult, error) {
	total, live := d.registry.counts()
	result := ipc.ActiveResult{LiveSessions: live, TotalSessions: total}

	if args.Set {
		item, err := d.registry.resolve(args.Session)
		if err != nil {
			return ipc.ActiveResult{}, err
		}
		d.registry.setActive(item.id())
		screen, err := d.screenFor(ctx, item)
		if err != nil {
			return ipc.ActiveResult{}, err
		}
		result.Active = &screen
		return result, nil
	}

	activeID := d.registry.activeID()
	if activeID == "" {
		return result, nil
	}
	item, err := d.registry.resolve(activeID)
	if err != nil {
		// The active pointer outlived its session; report no active session
		// rather than an error the caller cannot act on.
		d.registry.setActive("")
		return result, nil
	}
	screen, err := d.screenFor(ctx, item)
	if err != nil {
		return ipc.ActiveResult{}, err
	}
	result.Active = &screen
	return result, nil
}

// screenFor snapshots an entry, tolerating one whose process has ended.
func (d *Daemon) screenFor(ctx context.Context, item *entry) (ipc.Screen, error) {
	if item.live == nil {
		return ipc.Screen{Session: d.describeSession(item, d.registry.activeID())}, nil
	}
	settings := d.registry.settingsSnapshot()
	settled := item.live.WaitSettled(ctx, 0, settings.SettleQuiet)
	return d.screen(item.live, settled), nil
}

func (d *Daemon) handleRename(args ipc.RenameArgs) (ipc.SessionInfo, error) {
	item, err := d.registry.resolve(args.Session)
	if err != nil {
		return ipc.SessionInfo{}, err
	}
	name := strings.TrimSpace(args.Name)
	if err := d.registry.nameAvailable(name, item.id()); err != nil {
		return ipc.SessionInfo{}, err
	}
	if item.live != nil {
		item.live.Rename(name)
	} else {
		d.registry.mu.Lock()
		item.metadata.Name = name
		d.registry.mu.Unlock()
	}
	return d.describeSession(item, d.registry.activeID()), nil
}

func (d *Daemon) handleResize(args ipc.ResizeArgs) (ipc.SessionInfo, error) {
	item, err := d.registry.requireLive(args.Session)
	if err != nil {
		return ipc.SessionInfo{}, err
	}
	if err := validateSize(args.Cols, args.Rows); err != nil {
		return ipc.SessionInfo{}, err
	}
	if err := item.live.Resize(args.Cols, args.Rows); err != nil {
		return ipc.SessionInfo{}, internalError(err)
	}
	return d.describeSession(item, d.registry.activeID()), nil
}

func (d *Daemon) handleScrollback(args ipc.ScrollArgs) (ipc.ScrollResult, error) {
	item, err := d.registry.resolveOrActive(args.Session)
	if err != nil {
		return ipc.ScrollResult{}, err
	}
	if item.live == nil {
		return ipc.ScrollResult{}, retainedScreenError(item)
	}
	lines := args.Lines
	if lines < 1 {
		lines = 100
	}
	return ipc.ScrollResult{
		Lines: item.live.ScrollbackText(args.Offset, lines),
		Total: item.live.ScrollbackLines(),
	}, nil
}

// stopWithSessions shuts the daemon down, optionally ending live sessions.
func (d *Daemon) stopWithSessions(killSessions bool) {
	// Let the reply reach the client before the socket closes.
	time.Sleep(50 * time.Millisecond)
	if killSessions {
		for _, item := range d.registry.list() {
			if item.live != nil && item.live.Running() {
				_ = item.live.Kill("TERM", "daemon stop")
			}
		}
	}
	d.Stop()
}

// screen converts a live snapshot and settle result into the wire shape.
func (d *Daemon) screen(live *session.Session, settled session.SettleResult) ipc.Screen {
	snapshot := live.Snapshot()
	item := &entry{live: live}
	d.registry.mu.RLock()
	if existing, ok := d.registry.entries[live.ID()]; ok {
		item = existing
	}
	d.registry.mu.RUnlock()

	return ipc.Screen{
		Session:           d.describeSession(item, d.registry.activeID()),
		Lines:             snapshot.Lines,
		Cursor:            snapshot.Cursor,
		BlankLinesTrimmed: snapshot.BlankLinesTrimmed,
		Settled:           settled.Settled,
		WaitedMS:          settled.Waited.Milliseconds(),
	}
}

// describeSession renders one entry for the wire, whether live or retained.
func (d *Daemon) describeSession(item *entry, active string) ipc.SessionInfo {
	settings := d.registry.settingsSnapshot()
	info := ipc.SessionInfo{
		ID:     item.id(),
		Name:   item.name(),
		Active: item.id() == active,
	}

	if item.live != nil {
		metadata := item.live.Metadata()
		snapshot := item.live.Snapshot()
		cols, rows := item.live.Size()
		code, exited := item.live.ExitCode()

		info.Running = !exited
		info.PID = metadata.PID
		info.Command = metadata.Command
		info.CommandLine = metadata.CommandLine
		info.Cwd = metadata.Cwd
		info.Cols, info.Rows = cols, rows
		info.AltScreen = snapshot.AltScreen
		info.Title = snapshot.Title
		info.CreatedAt = metadata.CreatedAt
		info.LastActivityAt = metadata.LastActivityAt
		info.ExitedAt = metadata.ExitedAt
		info.KilledBy = metadata.KilledBy
		info.TranscriptLines = item.live.TranscriptLines()
		info.LogPath = item.live.TranscriptPath()
		if exited {
			info.ExitCode = &code
		}
	} else {
		metadata := item.metadata
		info.Running = false
		info.PID = metadata.PID
		info.Command = metadata.Command
		info.CommandLine = metadata.CommandLine
		info.Cwd = metadata.Cwd
		info.Cols, info.Rows = metadata.Cols, metadata.Rows
		info.CreatedAt = metadata.CreatedAt
		info.LastActivityAt = metadata.LastActivityAt
		info.ExitedAt = metadata.ExitedAt
		info.ExitCode = metadata.ExitCode
		info.KilledBy = metadata.KilledBy
		info.TranscriptLines = metadata.TranscriptLine
		info.LogPath = filepath.Join(item.directory, "transcript.log")
	}

	// A retention policy of on_close means an exited session's logs are gone
	// the moment it ends, so a path would point at nothing.
	info.LogsRetained = info.Running || settings.LogRetention != config.RetentionOnClose
	if !info.LogsRetained {
		info.LogPath = ""
	}
	return info
}

// retainedScreenError explains that a session's screen is no longer in memory.
func retainedScreenError(item *entry) error {
	return &ipc.Error{
		Code:    ipc.CodeSessionExited,
		Message: fmt.Sprintf("session %s ended before this daemon started, so its screen is no longer available", describe(item)),
		Hint:    "Its transcript is still readable with it_tail and it_head.",
		Fields:  map[string]any{"session": item.id()},
	}
}

func validateSize(cols, rows int) error {
	if cols < 20 || cols > 1000 || rows < 5 || rows > 1000 {
		return &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: fmt.Sprintf("terminal size %dx%d is outside the supported range", cols, rows),
			Hint:    "Use 20-1000 columns and 5-1000 rows.",
			Fields:  map[string]any{"cols": cols, "rows": rows},
		}
	}
	return nil
}

func countLive(entries []*entry) int {
	count := 0
	for _, item := range entries {
		if item.running() {
			count++
		}
	}
	return count
}

var _ = vterm.Snapshot{}
