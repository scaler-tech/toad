package linearagent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/scaler-tech/toad/internal/state"
)

// Work is one unhandled user request found in a session.
type Work struct {
	Session     Session
	Prompt      string
	FollowUp    bool
	TriggeredAt time.Time
}

// DetectWork compares a live session snapshot against toad's handled-state
// record. It returns nil when the session needs nothing. The record's
// LastHandledActivityAt is written only after a response posts, so any
// crash before that leaves the work re-detectable here.
func DetectWork(s Session, rec *state.AgentSessionRecord) *Work {
	switch s.Status {
	case "complete", "error", "stale":
		return nil
	}

	// The latest user event: the newest prompt activity, or session
	// creation itself (mention/delegation) when no prompt exists yet.
	latestPromptAt := time.Time{}
	prompt := ""
	for _, a := range s.Activities {
		if a.IsUser() && a.CreatedAt.After(latestPromptAt) {
			latestPromptAt = a.CreatedAt
			prompt = a.Body
		}
	}
	triggeredAt := s.CreatedAt
	if !latestPromptAt.IsZero() {
		triggeredAt = latestPromptAt
	}
	if prompt == "" {
		prompt = s.SourceComment
	}

	if rec != nil && !rec.LastHandledActivityAt.Before(triggeredAt) {
		return nil // already answered this user event
	}
	return &Work{
		Session:     s,
		Prompt:      prompt,
		FollowUp:    rec != nil,
		TriggeredAt: triggeredAt,
	}
}

// SessionLister is the slice of Client the poller needs (test seam, and the
// spot a webhook-fed intake would replace).
type SessionLister interface {
	ListSessions(ctx context.Context, first int) ([]Session, error)
}

// Poller periodically lists sessions and dispatches detected work. It is
// toad's polling-based Intake: no inbound HTTP, at the cost of up to one
// interval of latency (Linear may briefly show the agent as unresponsive).
//
// Each detected Work item is handled in its own goroutine (one goroutine per
// session), bounded by the shared investigation semaphore the handler
// acquires internally — so a long-running investigation for one session
// (e.g. a 10-minute run) never blocks the ack or dispatch of another
// session's work on the same poll tick.
type Poller struct {
	lister   SessionLister
	db       *state.DB
	interval time.Duration
	handle   func(context.Context, Work)

	// mu guards inFlight, which tracks session IDs currently being
	// processed. handle now runs asynchronously (dispatched via `go` from
	// pollOnce), so both the poll loop and each per-session goroutine's
	// completion (which deletes its own entry) touch this map — hence the
	// mutex. Do not remove it in favor of a synchronous mark/delete without
	// also reverting the async dispatch below.
	mu       sync.Mutex
	inFlight map[string]bool

	// wg tracks in-flight per-session goroutines so Run can wait for them to
	// finish draining before returning on ctx.Done(), instead of stranding
	// them mid-investigation when the caller proceeds to tear down shared
	// state (e.g. closing the state DB).
	wg sync.WaitGroup
}

func NewPoller(lister SessionLister, db *state.DB, interval time.Duration, handle func(context.Context, Work)) *Poller {
	return &Poller{lister: lister, db: db, interval: interval, handle: handle, inFlight: make(map[string]bool)}
}

// Run polls until ctx is done, dispatching each tick's detected work into its
// own goroutine (see the Poller doc comment). It waits for any still-running
// per-session goroutines to finish before returning, so callers that close
// shared resources (like the state DB) right after Run returns don't race a
// straggling investigation.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.pollOnce(ctx)
		case <-ctx.Done():
			p.wg.Wait()
			return
		}
	}
}

// pollOnce lists sessions once and dispatches each detected Work item in its
// own goroutine; the investigation semaphore (acquired inside the handler via
// the daemon's runInvestigation bridge) bounds how many run concurrently.
func (p *Poller) pollOnce(ctx context.Context) {
	sessions, err := p.lister.ListSessions(ctx, 50)
	if err != nil {
		slog.Warn("linear agent poll failed", "error", err)
		return
	}
	for _, s := range sessions {
		p.mu.Lock()
		if p.inFlight[s.ID] {
			p.mu.Unlock()
			continue
		}
		p.mu.Unlock()

		rec, err := p.db.GetAgentSession(s.ID)
		if err != nil {
			slog.Warn("reading agent session record", "session", s.ID, "error", err)
			continue
		}
		w := DetectWork(s, rec)
		if w == nil {
			continue
		}

		p.mu.Lock()
		p.inFlight[s.ID] = true
		p.mu.Unlock()

		p.wg.Add(1)
		go func(w Work) {
			defer p.wg.Done()
			defer func() {
				p.mu.Lock()
				delete(p.inFlight, w.Session.ID)
				p.mu.Unlock()
			}()
			p.handle(ctx, w) // panic in handle crashes the process; no recover, no stuck inFlight entry
		}(*w)
	}
}
