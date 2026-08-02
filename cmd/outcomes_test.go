package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/state"
)

// outcomeTrackerFake is a small hand-written issuetracker.Tracker fake, same
// spirit as ticketflowTrackerFake — records every GetIssueStatus call so
// tests can assert whether the tracker was invoked at all (skip behavior),
// and returns canned statuses (or errors) keyed by issue ID.
type outcomeTrackerFake struct {
	issuetracker.NoopTracker
	statuses map[string]issuetracker.IssueStatus
	errs     map[string]error
	calls    []string
	onCall   func(id string) // optional hook invoked before returning, e.g. to cancel ctx
}

func (f *outcomeTrackerFake) GetIssueStatus(_ context.Context, ref *issuetracker.IssueRef) (*issuetracker.IssueStatus, error) {
	f.calls = append(f.calls, ref.ID)
	if f.onCall != nil {
		f.onCall(ref.ID)
	}
	if f.errs != nil {
		if err, ok := f.errs[ref.ID]; ok {
			return nil, err
		}
	}
	if s, ok := f.statuses[ref.ID]; ok {
		return &s, nil
	}
	return nil, nil
}

// seedTicket inserts a ticket_index row with the given last status and
// status-checked-at timestamp via UpsertTicketIndex, which stores the
// provided values as-is on first insert (the COALESCE-preserving behavior
// only kicks in on conflict).
func seedTicket(t *testing.T, db *state.DB, externalKey, issueID, lastStatus string, statusCheckedAt time.Time) {
	t.Helper()
	now := time.Now()
	err := db.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey:     externalKey,
		IssueID:         issueID,
		Source:          "auto",
		CreatedAt:       now,
		LastSeenAt:      now,
		LastStatus:      lastStatus,
		StatusCheckedAt: statusCheckedAt,
	})
	if err != nil {
		t.Fatalf("seeding ticket index: %v", err)
	}
}

func TestPollOnce_StatusChangePersists(t *testing.T) {
	db := newTestDB(t)
	seedTicket(t, db, "thread:C1:1", "TOAD-1", "Todo", time.Time{})

	fake := &outcomeTrackerFake{
		statuses: map[string]issuetracker.IssueStatus{
			"TOAD-1": {State: "Done"},
		},
	}

	pollOnce(context.Background(), db, fake, time.Hour)

	entry, err := db.GetTicketIndex("thread:C1:1")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected ticket index entry to exist")
	}
	if entry.LastStatus != "Done" {
		t.Errorf("LastStatus = %q, want %q", entry.LastStatus, "Done")
	}
	if entry.StatusCheckedAt.IsZero() {
		t.Error("expected StatusCheckedAt to be set")
	}
}

func TestPollOnce_UnchangedStatusOnlyBumpsCheckedAt(t *testing.T) {
	db := newTestDB(t)
	staleCheck := time.Now().Add(-2 * time.Hour)
	seedTicket(t, db, "thread:C1:2", "TOAD-2", "Done", staleCheck)

	fake := &outcomeTrackerFake{
		statuses: map[string]issuetracker.IssueStatus{
			"TOAD-2": {State: "Done"},
		},
	}

	pollOnce(context.Background(), db, fake, time.Hour)

	if len(fake.calls) != 1 {
		t.Fatalf("expected tracker to be called once, got %d calls", len(fake.calls))
	}

	entry, err := db.GetTicketIndex("thread:C1:2")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry.LastStatus != "Done" {
		t.Errorf("LastStatus changed unexpectedly: %q", entry.LastStatus)
	}
	if !entry.StatusCheckedAt.After(staleCheck) {
		t.Errorf("expected StatusCheckedAt to be bumped forward, got %v (was %v)", entry.StatusCheckedAt, staleCheck)
	}
}

func TestPollOnce_RecentlyCheckedEntriesAreSkipped(t *testing.T) {
	db := newTestDB(t)
	fresh := time.Now().Add(-1 * time.Minute)
	seedTicket(t, db, "thread:C1:3", "TOAD-3", "Todo", fresh)

	fake := &outcomeTrackerFake{
		statuses: map[string]issuetracker.IssueStatus{
			"TOAD-3": {State: "Done"},
		},
	}

	pollOnce(context.Background(), db, fake, time.Hour)

	if len(fake.calls) != 0 {
		t.Fatalf("expected tracker not to be called for a freshly-checked entry, got calls: %v", fake.calls)
	}

	entry, err := db.GetTicketIndex("thread:C1:3")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry.LastStatus != "Todo" {
		t.Errorf("expected status to remain unchanged, got %q", entry.LastStatus)
	}
}

func TestPollOnce_CtxCancellationStopsLoop(t *testing.T) {
	db := newTestDB(t)
	seedTicket(t, db, "thread:C1:4", "TOAD-4", "Todo", time.Time{})
	seedTicket(t, db, "thread:C1:5", "TOAD-5", "Todo", time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	fake := &outcomeTrackerFake{
		statuses: map[string]issuetracker.IssueStatus{
			"TOAD-4": {State: "Done"},
			"TOAD-5": {State: "Done"},
		},
		onCall: func(string) { cancel() },
	}

	pollOnce(ctx, db, fake, time.Hour)

	if len(fake.calls) != 1 {
		t.Fatalf("expected loop to stop after cancellation (1 call), got %d calls: %v", len(fake.calls), fake.calls)
	}
}

// TestPollOnce_ContinuesPastPerEntryError confirms a single entry's
// GetIssueStatus error doesn't abort the whole poll pass: pollOnce logs the
// per-entry failure at debug and continues (no retry storm, no early
// return), so entries seeded after the failing one still get checked and
// their status persisted, while the failing entry's own status is left
// untouched.
func TestPollOnce_ContinuesPastPerEntryError(t *testing.T) {
	db := newTestDB(t)
	seedTicket(t, db, "thread:C4:1", "TOAD-30", "Todo", time.Time{})
	seedTicket(t, db, "thread:C4:2", "TOAD-31", "Todo", time.Time{})
	seedTicket(t, db, "thread:C4:3", "TOAD-32", "Todo", time.Time{})

	fake := &outcomeTrackerFake{
		statuses: map[string]issuetracker.IssueStatus{
			"TOAD-31": {State: "Done"},
			"TOAD-32": {State: "In Progress"},
		},
		errs: map[string]error{
			"TOAD-30": errors.New("linear api unavailable"),
		},
	}

	pollOnce(context.Background(), db, fake, time.Hour)

	if len(fake.calls) != 3 {
		t.Fatalf("expected all 3 entries to be attempted despite the first erroring, got %d calls: %v", len(fake.calls), fake.calls)
	}

	// The entry whose lookup errored must keep its prior status untouched —
	// no partial/zero-value write from the failed call.
	failed, err := db.GetTicketIndex("thread:C4:1")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if failed.LastStatus != "Todo" {
		t.Errorf("expected the errored entry's status to remain unchanged, got %q", failed.LastStatus)
	}

	// Entries after the failing one must still have been processed and
	// persisted — the error on TOAD-30 didn't abort the rest of the pass.
	second, err := db.GetTicketIndex("thread:C4:2")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if second.LastStatus != "Done" {
		t.Errorf("expected entry after the errored one to still be processed, got LastStatus=%q", second.LastStatus)
	}

	third, err := db.GetTicketIndex("thread:C4:3")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if third.LastStatus != "In Progress" {
		t.Errorf("expected the third entry to also be processed, got LastStatus=%q", third.LastStatus)
	}
}

// TestRunOutcomePoller_ExitsOnCtxDone exercises the outer ticker/select
// loop in runOutcomePoller itself (not pollOnce's inner entry loop, which
// TestPollOnce_CtxCancellationStopsLoop already covers). It uses an empty
// DB and a NoopTracker so pollOnce is a fast no-op on every tick and there
// is no shared mutable state for the goroutine to touch after cancellation
// — the test only ever synchronizes through ctx and the done channel, so
// it's race-safe by construction.
func TestRunOutcomePoller_ExitsOnCtxDone(t *testing.T) {
	db := newTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runOutcomePoller(ctx, db, issuetracker.NoopTracker{}, 10*time.Millisecond)
		close(done)
	}()

	// Let the ticker fire at least once before cancelling, so we know we're
	// exercising the running loop and not just a goroutine that never
	// started its select.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// runOutcomePoller returned promptly after ctx cancellation.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runOutcomePoller did not return within 500ms of ctx cancellation")
	}
}

// TestClassifyOutcome covers the full Linear state-type matrix plus the
// name-matching fallback used when no state type is available (older
// persisted rows, or a tracker that doesn't supply Linear-style types).
func TestClassifyOutcome(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		stateType string
		want      string
	}{
		{"empty status is always filed, regardless of type", "", "started", "filed"},
		{"empty status and empty type is filed", "", "", "filed"},

		// Type-based classification (takes precedence when present).
		{"completed type", "Done", "completed", "done"},
		{"canceled type (American spelling)", "Canceled", "canceled", "rejected"},
		{"cancelled type (British spelling)", "Cancelled", "cancelled", "rejected"},
		{"triage type", "Needs Triage", "triage", "pending"},
		{"backlog type", "Backlog", "backlog", "accepted"},
		{"unstarted type", "Todo", "unstarted", "accepted"},
		{"started type", "In Progress", "started", "accepted"},
		{"type matching is case-insensitive", "Done", "COMPLETED", "done"},

		// Unrecognized non-empty type falls through to name matching, same
		// as if type were empty.
		{"unrecognized type falls back to name matching", "Done", "some_custom_type", "done"},

		// Name-matching fallback (empty type).
		{"name fallback: done", "Done", "", "done"},
		{"name fallback: cancelled", "Cancelled", "", "rejected"},
		{"name fallback: canceled", "Canceled", "", "rejected"},
		{"name fallback: duplicate", "Duplicate", "", "rejected"},
		{"name fallback: unmatched status is unknown", "In Progress", "", "unknown"},
		{"name fallback is case-insensitive", "DONE", "", "done"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyOutcome(tt.status, tt.stateType)
			if got != tt.want {
				t.Errorf("classifyOutcome(%q, %q) = %q, want %q", tt.status, tt.stateType, got, tt.want)
			}
		})
	}
}

// TestPollOnce_PersistsStateType confirms the poller persists both the raw
// status name and the tracker's state type on ticket_index, not just the
// name.
func TestPollOnce_PersistsStateType(t *testing.T) {
	db := newTestDB(t)
	seedTicket(t, db, "thread:C3:1", "TOAD-20", "Todo", time.Time{})

	fake := &outcomeTrackerFake{
		statuses: map[string]issuetracker.IssueStatus{
			"TOAD-20": {State: "In Progress", StateType: "started"},
		},
	}

	pollOnce(context.Background(), db, fake, time.Hour)

	entry, err := db.GetTicketIndex("thread:C3:1")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected ticket index entry to exist")
	}
	if entry.LastStatus != "In Progress" {
		t.Errorf("LastStatus = %q, want %q", entry.LastStatus, "In Progress")
	}
	if entry.LastStateType != "started" {
		t.Errorf("LastStateType = %q, want %q", entry.LastStateType, "started")
	}
}

func TestOutcomeCounts(t *testing.T) {
	db := newTestDB(t)
	seedTicket(t, db, "thread:C2:1", "TOAD-10", "", time.Time{})           // filed
	seedTicket(t, db, "thread:C2:2", "TOAD-11", "Done", time.Now())        // done (name fallback, no state type)
	seedTicket(t, db, "thread:C2:3", "TOAD-12", "Duplicate", time.Now())   // rejected
	seedTicket(t, db, "thread:C2:4", "TOAD-13", "Canceled", time.Now())    // rejected
	seedTicket(t, db, "thread:C2:5", "TOAD-14", "In Progress", time.Now()) // unknown

	counts, err := outcomeCounts(db)
	if err != nil {
		t.Fatalf("outcomeCounts: %v", err)
	}

	want := map[string]int{"filed": 1, "done": 1, "rejected": 2, "unknown": 1}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("counts[%q] = %d, want %d (full: %+v)", k, counts[k], v, counts)
		}
	}
}
