package cmd

import (
	"log/slog"
	"sync"
	"time"

	"github.com/scaler-tech/toad/internal/state"
)

// digestChannelGateRefreshInterval is how often digestChannelGate re-reads
// the disabled-channel set from the DB. The dashboard (a separate process,
// `toad status --port N`) writes per-channel digest toggles directly to the
// shared SQLite state DB (WAL mode); the daemon has no push notification for
// that, so the gate polls lazily on check instead of watching the DB or
// running its own ticker.
const digestChannelGateRefreshInterval = 60 * time.Second

// digestChannelGate answers "should the digest collect from this channel?",
// backed by settings rows the dashboard writes via
// state.DB.SetDigestChannelEnabled ("digest_channel:<id>" = "off" opts a
// channel out; absence means "on" — unchanged default behavior). The
// disabled set is refreshed from the DB at most once per refreshInterval,
// lazily on the next check rather than via a background goroutine, so a
// daemon that never runs the digest never queries the DB for this at all.
type digestChannelGate struct {
	db              *state.DB
	refreshInterval time.Duration

	mu          sync.Mutex
	disabled    map[string]bool
	lastRefresh time.Time
}

// newDigestChannelGate constructs a gate backed by db. db may be nil (e.g.
// an in-memory/CLI one-shot state.Manager with no persistent DB) — enabled
// then always fails open.
func newDigestChannelGate(db *state.DB) *digestChannelGate {
	return &digestChannelGate{db: db, refreshInterval: digestChannelGateRefreshInterval}
}

// enabled reports whether channelID should be collected by the digest. Fails
// OPEN (returns true) whenever the disabled set can't be determined — no
// backing DB, or a refresh attempt errored — since a broken toggle store
// must never silently kill the digest. A refresh is attempted at most once
// per refreshInterval regardless of outcome (success or failure both bump
// lastRefresh), so a sustained DB outage logs a single Warn per interval
// rather than once per message.
func (g *digestChannelGate) enabled(channelID string) bool {
	if g == nil || g.db == nil {
		return true
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if time.Since(g.lastRefresh) >= g.refreshInterval {
		g.lastRefresh = time.Now()
		disabled, err := g.db.DisabledDigestChannels()
		if err != nil {
			slog.Warn("digest channel gate: refresh failed, failing open", "error", err)
		} else {
			g.disabled = disabled
		}
	}

	return !g.disabled[channelID]
}
