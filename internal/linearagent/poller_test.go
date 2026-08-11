package linearagent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/state"
)

func ts(min int) time.Time {
	return time.Date(2026, 8, 10, 10, min, 0, 0, time.UTC)
}

func TestDetectWork_NewSessionNoActivities(t *testing.T) {
	s := Session{ID: "s1", Status: "pending", CreatedAt: ts(0), SourceComment: "@toad look at PLF-9"}
	w := DetectWork(s, nil)
	if w == nil {
		t.Fatal("new session must be work")
	}
	if w.FollowUp || w.Prompt != "@toad look at PLF-9" || !w.TriggeredAt.Equal(ts(0)) {
		t.Errorf("work = %+v", w)
	}
}

func TestDetectWork_TerminalStatusesSkipped(t *testing.T) {
	for _, status := range []string{"complete", "error", "stale"} {
		s := Session{ID: "s1", Status: status, CreatedAt: ts(0), SourceComment: "hi"}
		if DetectWork(s, nil) != nil {
			t.Errorf("status %q must not be work", status)
		}
	}
}

func TestDetectWork_HandledSessionNoNewPrompt(t *testing.T) {
	s := Session{ID: "s1", Status: "active", CreatedAt: ts(0), SourceComment: "hi",
		Activities: []Activity{
			{CreatedAt: ts(1), Type: "thought", Body: "Reading."},
			{CreatedAt: ts(5), Type: "response", Body: "Done."},
		}}
	rec := &state.AgentSessionRecord{SessionID: "s1", LastHandledActivityAt: ts(0)}
	if w := DetectWork(s, rec); w != nil {
		t.Errorf("handled session with no new prompt must not be work, got %+v", w)
	}
}

func TestDetectWork_FollowUpPrompt(t *testing.T) {
	s := Session{ID: "s1", Status: "active", CreatedAt: ts(0), SourceComment: "hi",
		Activities: []Activity{
			{CreatedAt: ts(5), Type: "response", Body: "Done."},
			{CreatedAt: ts(9), Type: "prompt", Body: "what about the retry path?"},
		}}
	rec := &state.AgentSessionRecord{SessionID: "s1", LastHandledActivityAt: ts(0)}
	w := DetectWork(s, rec)
	if w == nil {
		t.Fatal("new prompt after handled point must be work")
	}
	if !w.FollowUp || w.Prompt != "what about the retry path?" || !w.TriggeredAt.Equal(ts(9)) {
		t.Errorf("work = %+v", w)
	}
}

func TestDetectWork_UnhandledNewSessionWithOwnAckOnly(t *testing.T) {
	// Crash case: toad acked (thought) but never responded and never wrote
	// the record — the session must still be detected as work.
	s := Session{ID: "s1", Status: "active", CreatedAt: ts(0), SourceComment: "hi",
		Activities: []Activity{{CreatedAt: ts(1), Type: "thought", Body: "Reading."}}}
	if w := DetectWork(s, nil); w == nil {
		t.Fatal("session without a stored record must be re-detected as work")
	}
}

// TestPoller_DispatchesDetectedWork exercises the poller's async-dispatch
// contract: handle now runs in its own goroutine per Work item (Fix 3), so
// dedup across poll ticks relies on two things working together rather than
// synchronous per-tick handling:
//  1. inFlight (mutex-guarded) marks a session busy *before* the goroutine is
//     spawned, so a second tick firing while the first handler is still
//     running skips the session instead of double-dispatching.
//  2. Once the handler finishes and inFlight is cleared, DetectWork's record
//     check (LastHandledActivityAt >= triggeredAt) prevents a later tick from
//     redetecting the same, now-answered, prompt as new work.
//
// The test asserts on `handled` only after p.Run(ctx) returns, which is only
// once ctx is done AND the poller's WaitGroup has drained every in-flight
// goroutine (see Run's ctx.Done() case) — so there is no race between the
// last dispatched goroutine and this read.
func TestPoller_DispatchesDetectedWork(t *testing.T) {
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer db.Close()

	lister := &fakeLister{sessions: []Session{
		{ID: "s1", Status: "pending", CreatedAt: ts(0), SourceComment: "check this"},
	}}
	var mu sync.Mutex
	var handled []Work
	p := NewPoller(lister, db, 10*time.Millisecond, func(ctx context.Context, w Work) {
		mu.Lock()
		handled = append(handled, w)
		mu.Unlock()
		// Simulate the processor completing: write the handled record.
		db.UpsertAgentSession(&state.AgentSessionRecord{
			SessionID: w.Session.ID, Status: w.Session.Status,
			LastHandledActivityAt: w.TriggeredAt, UpdatedAt: time.Now().UTC(),
		})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p.Run(ctx) // blocks until ctx is done AND all dispatched goroutines finish

	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 1 {
		t.Fatalf("handled %d times, want exactly 1 (dedup across async dispatch)", len(handled))
	}
	if handled[0].Session.ID != "s1" {
		t.Errorf("handled = %+v", handled[0])
	}
}

// TestPoller_WaitsForInFlightGoroutinesBeforeReturning asserts Fix 3's second
// half directly: Run must not return while a dispatched handler goroutine is
// still running, so callers who close shared resources (state DB, etc.)
// right after Run returns don't race a straggling investigation.
func TestPoller_WaitsForInFlightGoroutinesBeforeReturning(t *testing.T) {
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer db.Close()

	lister := &fakeLister{sessions: []Session{
		{ID: "s1", Status: "pending", CreatedAt: ts(0), SourceComment: "check this"},
	}}
	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	p := NewPoller(lister, db, 5*time.Millisecond, func(ctx context.Context, w Work) {
		close(started)
		<-release
		finished.Store(true)
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(runDone)
	}()

	<-started      // handler goroutine is running
	cancel()       // ask Run to stop; it must wait on the in-flight handler
	close(release) // let the handler finish

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation + handler completion")
	}
	if !finished.Load() {
		t.Error("Run returned before the in-flight handler finished")
	}
}

type fakeLister struct{ sessions []Session }

func (f *fakeLister) ListSessions(ctx context.Context, first int) ([]Session, error) {
	return f.sessions, nil
}
