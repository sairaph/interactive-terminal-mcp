package daemon

import (
	"context"
	"os"
	"time"

	"github.com/sairaph/interactive-terminal-mcp/internal/config"
)

// sweepInterval is how often retention runs while the daemon is up.
const sweepInterval = 10 * time.Minute

// sweepRetention deletes the logs of ended sessions once their retention
// period has elapsed.
//
// Live sessions are never swept regardless of the setting: retention describes
// how long a finished session's logs are kept, not a lifetime for the terminal
// itself.
func (d *Daemon) sweepRetention(ctx context.Context) {
	d.sweepOnce()
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopping:
			return
		case <-ticker.C:
			d.sweepOnce()
		}
	}
}

func (d *Daemon) sweepOnce() {
	settings := d.registry.settingsSnapshot()
	if settings.LogRetention == config.RetentionNever {
		return
	}
	retention, ok := config.RetentionDuration(settings.LogRetention)
	if !ok {
		return
	}

	now := time.Now().UTC()
	for _, item := range d.registry.list() {
		// An entry whose process is still alive keeps everything.
		if item.running() {
			continue
		}
		// A live-but-exited session is finalized first so its last output and
		// meta.json reach disk before anything is removed.
		if item.live != nil {
			item.live.Close()
			d.registry.mu.Lock()
			if current, ok := d.registry.entries[item.id()]; ok && current.live != nil {
				current.metadata = current.live.Metadata()
				current.live = nil
				current.retainedAt = retirementTime(current.metadata.ExitedAt, now)
			}
			d.registry.mu.Unlock()
			// on_close removes it on this same pass; anything else gives it
			// its full period starting from the exit.
			if settings.LogRetention != config.RetentionOnClose {
				continue
			}
		}

		d.registry.mu.RLock()
		retainedAt := item.retainedAt
		d.registry.mu.RUnlock()
		if retainedAt.IsZero() {
			retainedAt = now
		}
		if now.Sub(retainedAt) < retention {
			continue
		}

		directory := d.registry.remove(item.id())
		if directory == "" {
			continue
		}
		if err := os.RemoveAll(directory); err != nil {
			// A cleanup failure must never break an unrelated tool call; the
			// entry is already out of the registry, so it stops being offered.
			d.note("remove session logs for " + item.id() + ": " + err.Error())
		}
	}
}

func retirementTime(exitedAt *time.Time, fallback time.Time) time.Time {
	if exitedAt != nil {
		return *exitedAt
	}
	return fallback
}

// note appends a diagnostic line. Diagnostics are best-effort by design: a
// failure to record a problem must not become a second problem.
func (d *Daemon) note(message string) {
	file, err := os.OpenFile(d.paths.Diagnostics, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.WriteString(time.Now().UTC().Format(time.RFC3339) + " " + message + "\n")
}
