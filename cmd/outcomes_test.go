package cmd

import (
	"context"
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

func TestOutcomeCounts(t *testing.T) {
	db := newTestDB(t)
	seedTicket(t, db, "thread:C2:1", "TOAD-10", "", time.Time{})           // filed
	seedTicket(t, db, "thread:C2:2", "TOAD-11", "Done", time.Now())        // accepted
	seedTicket(t, db, "thread:C2:3", "TOAD-12", "Duplicate", time.Now())   // rejected
	seedTicket(t, db, "thread:C2:4", "TOAD-13", "Canceled", time.Now())    // rejected
	seedTicket(t, db, "thread:C2:5", "TOAD-14", "In Progress", time.Now()) // unknown

	counts, err := outcomeCounts(db)
	if err != nil {
		t.Fatalf("outcomeCounts: %v", err)
	}

	want := map[string]int{"filed": 1, "accepted": 1, "rejected": 2, "unknown": 1}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("counts[%q] = %d, want %d (full: %+v)", k, counts[k], v, counts)
		}
	}
}
