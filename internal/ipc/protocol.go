// Package ipc defines the wire protocol between the session daemon and its
// clients: every MCP process, every CLI one-shot, and the interactive
// application.
//
// It is deliberately not MCP. The daemon streams live terminal output to
// attached viewers, which is a shape MCP has no room for, and the protocol
// stays a private implementation detail that can change with the binary.
package ipc

import (
	"encoding/json"
	"time"
)

// Version is the protocol version. Client and daemon are shipped in one
// binary, so a mismatch means two different versions are installed; the
// daemon reports that rather than guessing.
const Version = 1

// Operations understood by the daemon.
const (
	OpPing          = "ping"
	OpSessionList   = "session.list"
	OpSessionNew    = "session.new"
	OpSessionRead   = "session.read"
	OpSessionSend   = "session.send"
	OpSessionKill   = "session.kill"
	OpSessionLog    = "session.log"
	OpSessionAtive  = "session.active"
	OpSessionRename = "session.rename"
	OpSessionResize = "session.resize"
	OpSessionScroll = "session.scrollback"
	OpAttachOpen    = "attach.open"
	OpAttachInput   = "attach.input"
	OpAttachResize  = "attach.resize"
	OpDaemonStatus  = "daemon.status"
	OpDaemonStop    = "daemon.stop"
)

// Request is one client call.
type Request struct {
	V    int             `json:"v"`
	ID   int             `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Response is the daemon's reply. Exactly one is sent per request, except for
// attach.open, which replies once and then streams frames.
type Response struct {
	V      int             `json:"v"`
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is a typed failure. Code drives the agent-facing error contract, so it
// is part of the protocol rather than a formatted string.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Hint    string         `json:"hint,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Error codes. These are the same codes the MCP layer reports, so a failure
// keeps its meaning from the daemon all the way to the agent.
const (
	CodeInvalidInput      = "invalid_input"
	CodeNoActiveSession   = "no_active_session"
	CodeSessionNotFound   = "session_not_found"
	CodeSessionExited     = "session_exited"
	CodeNameConflict      = "name_conflict"
	CodeTooManySessions   = "too_many_sessions"
	CodeDaemonUnavailable = "daemon_unavailable"
	CodeCancelled         = "cancelled"
	CodeInternal          = "internal_error"
)

// SessionInfo is one session as reported to clients.
type SessionInfo struct {
	ID              string     `json:"id"`
	Name            string     `json:"name,omitempty"`
	Active          bool       `json:"active"`
	Running         bool       `json:"running"`
	PID             int        `json:"pid,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	KilledBy        string     `json:"killed_by,omitempty"`
	Command         []string   `json:"command,omitempty"`
	CommandLine     string     `json:"command_line,omitempty"`
	Shell           string     `json:"shell,omitempty"`
	ShellPath       string     `json:"shell_path,omitempty"`
	Cwd             string     `json:"cwd,omitempty"`
	Cols            int        `json:"cols"`
	Rows            int        `json:"rows"`
	AltScreen       bool       `json:"alt_screen"`
	Title           string     `json:"title,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	LastActivityAt  time.Time  `json:"last_activity_at"`
	ExitedAt        *time.Time `json:"exited_at,omitempty"`
	TranscriptLines int        `json:"transcript_lines"`
	LogPath         string     `json:"log_path,omitempty"`
	LogsRetained    bool       `json:"logs_retained"`
}

// Screen is a captured terminal snapshot plus the wait outcome that produced
// it. Every tool that returns a screen returns this shape.
type Screen struct {
	Session           SessionInfo `json:"session"`
	Lines             []string    `json:"lines"`
	Cursor            [2]int      `json:"cursor"`
	BlankLinesTrimmed int         `json:"blank_lines_trimmed"`
	Settled           bool        `json:"settled"`
	Matched           bool        `json:"matched,omitempty"`
	WaitedFor         string      `json:"waited_for,omitempty"`
	WaitedMS          int64       `json:"waited_ms"`
}

// NewArgs creates a session.
type NewArgs struct {
	// WaitFor ends the wait as soon as this text appears on the screen.
	WaitFor string `json:"wait_for,omitempty"`

	Name        string            `json:"name,omitempty"`
	CommandLine string            `json:"command_line,omitempty"`
	Argv        []string          `json:"argv,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Cols        int               `json:"cols,omitempty"`
	Rows        int               `json:"rows,omitempty"`
	Shell       string            `json:"shell,omitempty"`
	WaitMS      int64             `json:"wait_ms"`
}

// ReadArgs snapshots a session, optionally resizing first.
type ReadArgs struct {
	// WaitFor ends the wait as soon as this text appears on the screen.
	WaitFor string `json:"wait_for,omitempty"`

	Session string `json:"session,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	WaitMS  int64  `json:"wait_ms"`
}

// SendArgs writes to a session.
type SendArgs struct {
	// WaitFor ends the wait as soon as this text appears on the screen.
	WaitFor string `json:"wait_for,omitempty"`

	Session string `json:"session,omitempty"`
	Text    string `json:"text,omitempty"`
	HasText bool   `json:"has_text"`
	Keys    string `json:"keys,omitempty"`
	HasKeys bool   `json:"has_keys"`
	Enter   bool   `json:"enter"`
	WaitMS  int64  `json:"wait_ms"`
}

// KillArgs terminates a session. Session is always required; the daemon never
// infers a kill target from the active session.
type KillArgs struct {
	Session string `json:"session"`
	Signal  string `json:"signal,omitempty"`
	// Purge forgets the session entirely rather than retaining it.
	//
	// Killing and deleting are different intents. An agent that ends a build
	// still wants to read what it printed, so it_kill retains the session under
	// the configured log policy. A person who chose "Delete" in the application
	// wants it gone, and leaving the row on screen reads as a broken button.
	Purge bool `json:"purge,omitempty"`
}

// KillResult reports what ended the session and what happened to its logs.
type KillResult struct {
	Killed       string `json:"killed"`
	Name         string `json:"name,omitempty"`
	Signal       string `json:"signal"`
	Escalated    bool   `json:"escalated"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	LogsRetained bool   `json:"logs_retained"`
	LogPath      string `json:"log_path,omitempty"`
	AlreadyGone  bool   `json:"already_gone"`
	Purged       bool   `json:"purged,omitempty"`
	// Outcome is what was actually observed after the signal, rather than what
	// was intended. An interrupt is a request a program may ignore, so
	// reporting success without looking would be a lie the caller cannot check.
	Outcome string `json:"outcome,omitempty"`
	// ObservedMS is how long the session was watched to reach that outcome.
	ObservedMS int64 `json:"observed_ms,omitempty"`
}

// Outcomes reported after a signal.
const (
	// OutcomeEnded means the session's process exited.
	OutcomeEnded = "ended"
	// OutcomeQuiet means the session survived and stopped producing output,
	// which is what a successful interrupt looks like from outside.
	OutcomeQuiet = "quiet"
	// OutcomeStillRunning means the session demonstrably kept working, so the
	// interrupt was not obeyed.
	OutcomeStillRunning = "still_running"
	// OutcomeUnknown means the observation could not be completed.
	OutcomeUnknown = "unknown"
)

// LogArgs reads a session transcript.
type LogArgs struct {
	Session string `json:"session,omitempty"`
	Lines   int    `json:"lines"`
	FromEnd bool   `json:"from_end"`
	Screen  bool   `json:"screen"`
}

// LogResult carries transcript lines and, for tail, the live screen.
//
// The live screen is included because a session sitting in a full-screen
// program has output that never scrolled and so is not in the transcript at
// all. Returning only the log would show an agent the history from before the
// program started and nothing about its current state.
type LogResult struct {
	Session     SessionInfo `json:"session"`
	Lines       []string    `json:"lines"`
	TotalLines  int         `json:"total_lines"`
	AtStart     bool        `json:"at_start"`
	AtEnd       bool        `json:"at_end"`
	LogPath     string      `json:"log_path"`
	ScreenLines []string    `json:"screen_lines,omitempty"`
	AltScreen   bool        `json:"alt_screen"`
}

// ActiveArgs reports or changes the active session.
type ActiveArgs struct {
	Session string `json:"session,omitempty"`
	Set     bool   `json:"set"`
}

// ActiveResult describes the active session, which may be absent.
type ActiveResult struct {
	Active        *Screen `json:"active,omitempty"`
	LiveSessions  int     `json:"live_sessions"`
	TotalSessions int     `json:"total_sessions"`
}

// ListResult is every session the daemon knows about, live and retained.
type ListResult struct {
	Active   string        `json:"active,omitempty"`
	Sessions []SessionInfo `json:"sessions"`
}

// RenameArgs changes a session name.
type RenameArgs struct {
	Session string `json:"session"`
	Name    string `json:"name"`
}

// ResizeArgs changes a session's terminal size.
type ResizeArgs struct {
	Session string `json:"session"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// ScrollArgs reads retained lines above the live screen.
type ScrollArgs struct {
	Session string `json:"session"`
	Offset  int    `json:"offset"`
	Lines   int    `json:"lines"`
}

// ScrollResult is a window into the scrollback.
type ScrollResult struct {
	Lines []string `json:"lines"`
	Total int      `json:"total"`
}

// AttachArgs opens a streaming view of a session.
type AttachArgs struct {
	Session string `json:"session"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	// ReplayBytes asks the daemon to prime the stream with the tail of the raw
	// log, so a viewer sees a correct screen immediately instead of a blank
	// one until the next output arrives.
	ReplayBytes int64 `json:"replay_bytes,omitempty"`
}

// AttachInputArgs sends bytes from an attached viewer.
type AttachInputArgs struct {
	Session string `json:"session"`
	Data    []byte `json:"data"`
}

// Frame is one streamed message on an attached connection.
type Frame struct {
	// Kind is "output", "resync", or "closed".
	Kind string `json:"kind"`
	Data []byte `json:"data,omitempty"`
	// ExitCode is set on a "closed" frame when the session ended.
	ExitCode *int `json:"exit_code,omitempty"`
}

// Frame kinds.
const (
	FrameOutput = "output"
	FrameResync = "resync"
	FrameClosed = "closed"
)

// Status describes the running daemon.
type Status struct {
	PID          int       `json:"pid"`
	Version      string    `json:"version"`
	StartedAt    time.Time `json:"started_at"`
	Sessions     int       `json:"sessions"`
	Live         int       `json:"live"`
	Clients      int       `json:"clients"`
	Socket       string    `json:"socket"`
	SessionsRoot string    `json:"sessions_root"`
}

// StopArgs stops the daemon.
type StopArgs struct {
	// KillSessions ends running sessions instead of leaving them; the CLI and
	// the application confirm with the user before setting it.
	KillSessions bool `json:"kill_sessions"`
}
