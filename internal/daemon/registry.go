package daemon

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sairaph/interactive-terminal-mcp/internal/config"
	"github.com/sairaph/interactive-terminal-mcp/internal/ipc"
	"github.com/sairaph/interactive-terminal-mcp/internal/session"
)

// idAlphabet is Crockford base32 without the characters that are easy to
// confuse when an agent or a person retypes an id.
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// namePattern is the accepted session name shape. It excludes anything that
// could be mistaken for an id or need quoting in a shell.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// entry is one session the daemon knows about. A retained entry has no live
// session: its process is gone, but its logs are still readable, which is what
// lets it_tail work after a build finished or after a daemon restart.
type entry struct {
	live      *session.Session
	metadata  session.Metadata
	directory string
	// retainedAt is when an exited session became eligible for cleanup.
	retainedAt time.Time
}

func (e *entry) id() string {
	if e.live != nil {
		return e.live.ID()
	}
	return e.metadata.ID
}

func (e *entry) name() string {
	if e.live != nil {
		return e.live.Name()
	}
	return e.metadata.Name
}

func (e *entry) running() bool {
	return e.live != nil && e.live.Running()
}

func (e *entry) lastActivity() time.Time {
	if e.live != nil {
		return e.live.LastActivity()
	}
	return e.metadata.LastActivityAt
}

// registry holds every session and the active-session pointer.
type registry struct {
	mu       sync.RWMutex
	paths    config.Paths
	settings config.Config
	entries  map[string]*entry
	active   string
}

func newRegistry(paths config.Paths, settings config.Config) *registry {
	return &registry{paths: paths, settings: settings, entries: map[string]*entry{}}
}

// settingsSnapshot returns the current configuration.
func (r *registry) settingsSnapshot() config.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.settings
}

// reload replaces the settings a running daemon uses. Sessions already running
// keep the size they were created with; only new sessions see new defaults.
func (r *registry) reload(settings config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settings = settings
}

// recover loads sessions left on disk by a previous daemon.
//
// Their processes are gone, but their transcripts are not: an agent that ran a
// build before the client restarted can still read how it ended. They are
// loaded as retained entries so it_list, it_tail, and it_head keep working.
func (r *registry) recover() {
	directories, err := os.ReadDir(r.paths.Sessions)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range directories {
		if !item.IsDir() {
			continue
		}
		directory := filepath.Join(r.paths.Sessions, item.Name())
		metadata, err := session.ReadMetadata(directory)
		if err != nil || metadata.ID == "" {
			continue
		}
		// A session whose daemon died mid-run has no exit record. It is not
		// running now, so it is recorded as ended with an unknown status
		// rather than being advertised as live.
		retainedAt := metadata.LastActivityAt
		if metadata.ExitedAt != nil {
			retainedAt = *metadata.ExitedAt
		} else {
			metadata.KilledBy = "daemon stopped"
		}
		r.entries[metadata.ID] = &entry{
			metadata: metadata, directory: directory, retainedAt: retainedAt,
		}
	}
}

// resolve finds a session by id or name.
func (r *registry) resolve(reference string) (*entry, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil, &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: "a session id or name is required",
			Hint:    "Call it_list() to see existing sessions.",
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if found, ok := r.entries[reference]; ok {
		return found, nil
	}
	for _, candidate := range r.entries {
		if candidate.name() != "" && candidate.name() == reference {
			return candidate, nil
		}
	}
	return nil, &ipc.Error{
		Code:    ipc.CodeSessionNotFound,
		Message: fmt.Sprintf("no session matches %q", reference),
		Hint:    "Call it_list() to see existing sessions, or it_new() to create one.",
		Fields:  map[string]any{"session": reference},
	}
}

// resolveOrActive falls back to the active session when no reference is given.
// it_kill deliberately never uses this: killing the wrong terminal is
// destructive and should require naming the target.
func (r *registry) resolveOrActive(reference string) (*entry, error) {
	if strings.TrimSpace(reference) != "" {
		return r.resolve(reference)
	}
	r.mu.RLock()
	active := r.active
	count := len(r.entries)
	r.mu.RUnlock()
	if active == "" {
		hint := "Create one with it_new({})."
		if count > 0 {
			hint = `Select one with it_active({"session":"<id>"}), or create one with it_new({}).`
		}
		return nil, &ipc.Error{
			Code:    ipc.CodeNoActiveSession,
			Message: "no session is currently active",
			Hint:    hint,
		}
	}
	return r.resolve(active)
}

// requireLive resolves a session that must still be running.
func (r *registry) requireLive(reference string) (*entry, error) {
	found, err := r.resolveOrActive(reference)
	if err != nil {
		return nil, err
	}
	if !found.running() {
		return nil, exitedError(found)
	}
	return found, nil
}

func exitedError(found *entry) *ipc.Error {
	fields := map[string]any{"session": found.id()}
	message := fmt.Sprintf("session %s has exited", describe(found))
	if code, ok := exitCodeOf(found); ok {
		fields["exit_code"] = code
		message = fmt.Sprintf("session %s has exited with code %d", describe(found), code)
	}
	return &ipc.Error{
		Code:    ipc.CodeSessionExited,
		Message: message,
		Hint:    "Its screen and logs are still readable with it_read, it_tail, and it_head. Start a new session with it_new({}) to run more commands.",
		Fields:  fields,
	}
}

func exitCodeOf(found *entry) (int, bool) {
	if found.live != nil {
		return found.live.ExitCode()
	}
	if found.metadata.ExitCode != nil {
		return *found.metadata.ExitCode, true
	}
	return 0, false
}

func describe(found *entry) string {
	if name := found.name(); name != "" {
		return fmt.Sprintf("%q (%s)", name, found.id())
	}
	return found.id()
}

// add registers a new live session and makes it active.
func (r *registry) add(live *session.Session, directory string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[live.ID()] = &entry{live: live, directory: directory}
	r.active = live.ID()
}

// remove deletes a session from the registry, returning its directory.
func (r *registry) remove(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.entries[id]
	if !ok {
		return ""
	}
	delete(r.entries, id)
	if r.active == id {
		r.active = r.mostRecentLiveLocked()
	}
	return found.directory
}

// mostRecentLiveLocked picks the successor active session. Callers hold the
// write lock.
func (r *registry) mostRecentLiveLocked() string {
	var best *entry
	for _, candidate := range r.entries {
		if !candidate.running() {
			continue
		}
		if best == nil || candidate.lastActivity().After(best.lastActivity()) {
			best = candidate
		}
	}
	if best == nil {
		return ""
	}
	return best.id()
}

// setActive changes the active session.
func (r *registry) setActive(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = id
}

// activeID reports the active session.
func (r *registry) activeID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// counts reports total and live session counts.
func (r *registry) counts() (total, live int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, candidate := range r.entries {
		total++
		if candidate.running() {
			live++
		}
	}
	return total, live
}

// list returns every session, live first and then by most recent activity.
//
// That order puts what an agent is most likely to want at the top of a
// token-budgeted page, so the useful sessions survive truncation.
func (r *registry) list() []*entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries := make([]*entry, 0, len(r.entries))
	for _, candidate := range r.entries {
		entries = append(entries, candidate)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		if left.running() != right.running() {
			return left.running()
		}
		leftTime, rightTime := left.lastActivity(), right.lastActivity()
		if !leftTime.Equal(rightTime) {
			return leftTime.After(rightTime)
		}
		return left.id() < right.id()
	})
	return entries
}

// nameAvailable reports whether a name may be used, excluding one session.
//
// Only live sessions hold a name: a retained session's name is released when
// its process ends, so an agent can reuse "build" for the next build without
// first cleaning up the previous one.
func (r *registry) nameAvailable(name, exceptID string) error {
	if name == "" {
		return nil
	}
	if !namePattern.MatchString(name) {
		return &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: fmt.Sprintf("session name %q is not valid", name),
			Hint:    "Use 1-64 characters: lowercase letters, digits, dots, underscores, or hyphens, starting with a letter or digit.",
			Fields:  map[string]any{"name": name},
		}
	}
	if strings.HasPrefix(name, "t-") {
		return &ipc.Error{
			Code:    ipc.CodeInvalidInput,
			Message: fmt.Sprintf("session name %q is reserved", name),
			Hint:    `Names cannot start with "t-" because that prefix identifies generated session ids.`,
			Fields:  map[string]any{"name": name},
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, candidate := range r.entries {
		if candidate.id() == exceptID || !candidate.running() {
			continue
		}
		if candidate.name() == name {
			return &ipc.Error{
				Code:    ipc.CodeNameConflict,
				Message: fmt.Sprintf("a running session is already named %q", name),
				Hint:    fmt.Sprintf("Use it_send({\"session\":%q,...}) to reach it, choose a different name, or end it with it_kill({\"session\":%q}).", name, name),
				Fields:  map[string]any{"name": name, "session": candidate.id()},
			}
		}
	}
	return nil
}

// newID generates an unused session identifier.
func (r *registry) newID() (string, error) {
	for range 100 {
		id, err := generateID()
		if err != nil {
			return "", err
		}
		r.mu.RLock()
		_, taken := r.entries[id]
		r.mu.RUnlock()
		if taken {
			continue
		}
		// A directory can outlive its registry entry after a failed cleanup;
		// reusing that id would mix two sessions' logs.
		if _, err := os.Stat(r.paths.SessionDir(id)); err == nil {
			continue
		}
		return id, nil
	}
	return "", &ipc.Error{Code: ipc.CodeInternal, Message: "could not generate an unused session id"}
}

func generateID() (string, error) {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return "", &ipc.Error{Code: ipc.CodeInternal, Message: "generate session id: " + err.Error()}
	}
	var out strings.Builder
	out.WriteString("t-")
	for _, b := range buffer {
		out.WriteByte(idAlphabet[int(b)%len(idAlphabet)])
	}
	return out.String(), nil
}
