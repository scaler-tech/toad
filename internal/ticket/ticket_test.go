package ticket

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/state"
)

// fakeStore is a minimal in-memory Store for tests: no COALESCE semantics,
// just last-write-wins, since those guard rules are state package's
// responsibility (Task 3), not the Engine's.
type fakeStore struct {
	entries map[string]*state.TicketIndexEntry
}

func newFakeStore() *fakeStore {
	return &fakeStore{entries: map[string]*state.TicketIndexEntry{}}
}

func (s *fakeStore) UpsertTicketIndex(e *state.TicketIndexEntry) error {
	cp := *e
	s.entries[e.ExternalKey] = &cp
	return nil
}

func (s *fakeStore) GetTicketIndex(externalKey string) (*state.TicketIndexEntry, error) {
	e, ok := s.entries[externalKey]
	if !ok {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

// fakeTracker is a small hand-written issuetracker.Tracker fake that counts
// CreateIssue and PostComment calls so tests can assert which path fired.
type fakeTracker struct {
	issuetracker.NoopTracker
	createCalls  []issuetracker.CreateIssueOpts
	commentCalls []struct {
		ref  *issuetracker.IssueRef
		body string
	}
	nextID    string
	createErr error
	postErr   error
}

func (t *fakeTracker) CreateIssue(_ context.Context, opts issuetracker.CreateIssueOpts) (*issuetracker.IssueRef, error) {
	if t.createErr != nil {
		return nil, t.createErr
	}
	t.createCalls = append(t.createCalls, opts)
	id := t.nextID
	if id == "" {
		id = "TOAD-1"
	}
	return &issuetracker.IssueRef{Provider: "linear", ID: id, URL: "https://linear.app/toad/issue/" + id, Title: opts.Title}, nil
}

func (t *fakeTracker) PostComment(_ context.Context, ref *issuetracker.IssueRef, body string) error {
	if t.postErr != nil {
		return t.postErr
	}
	t.commentCalls = append(t.commentCalls, struct {
		ref  *issuetracker.IssueRef
		body string
	}{ref, body})
	return nil
}

func fixedPermalink(link string) func(string, string) (string, error) {
	return func(string, string) (string, error) {
		return link, nil
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name       string
		autoFile   bool
		sentryIDs  []string
		confidence float64
		floor      float64
		feasible   bool
		want       Decision
	}{
		{"all conditions met", true, []string{"S1"}, 0.9, 0.85, true, DecisionAutoFile},
		{"auto-file disabled", false, []string{"S1"}, 0.9, 0.85, true, DecisionPropose},
		{"no sentry corroboration", true, nil, 0.9, 0.85, true, DecisionPropose},
		{"confidence below floor", true, []string{"S1"}, 0.5, 0.85, true, DecisionPropose},
		{"not feasible", true, []string{"S1"}, 0.9, 0.85, false, DecisionPropose},
		{"confidence exactly at floor", true, []string{"S1"}, 0.85, 0.85, true, DecisionAutoFile},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(&fakeTracker{}, newFakeStore(), config.TicketConfig{
				AutoFile:           tt.autoFile,
				AutoFileConfidence: tt.floor,
			}, nil)

			f := investigation.Findings{
				Feasible:       tt.feasible,
				Confidence:     tt.confidence,
				SentryIssueIDs: tt.sentryIDs,
			}

			got := e.Decide(f)
			if got != tt.want {
				t.Errorf("Decide() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExternalKey(t *testing.T) {
	t.Run("prefers sentry id", func(t *testing.T) {
		f := investigation.Findings{SentryIssueIDs: []string{"BILLING-2291", "BILLING-9999"}}
		got := ExternalKey(f, "C123", "1722.000100")
		want := "sentry:BILLING-2291"
		if got != want {
			t.Errorf("ExternalKey() = %q, want %q", got, want)
		}
	})

	t.Run("falls back to thread", func(t *testing.T) {
		f := investigation.Findings{}
		got := ExternalKey(f, "C123", "1722.000100")
		want := "thread:C123:1722.000100"
		if got != want {
			t.Errorf("ExternalKey() = %q, want %q", got, want)
		}
	})
}

func TestFileOrUpdate_CreatesNewTicket(t *testing.T) {
	tracker := &fakeTracker{nextID: "TOAD-42"}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{TriageStateID: "state-uuid"}, fixedPermalink("https://slack.example.com/x"))

	f := investigation.Findings{
		Title:          "",
		Problem:        "Export fails for empty accounts. It happens often.",
		SentryIssueIDs: []string{"BILLING-2291"},
	}

	result, err := e.FileOrUpdate(context.Background(), f, "C123", "1722.0001", "inv-1", SourceAuto)
	if err != nil {
		t.Fatalf("FileOrUpdate() error = %v", err)
	}
	if result.AlreadyExisted {
		t.Errorf("AlreadyExisted = true, want false for a fresh key")
	}
	if result.Ref == nil || result.Ref.ID != "TOAD-42" {
		t.Errorf("Ref = %+v, want ID TOAD-42", result.Ref)
	}

	if len(tracker.createCalls) != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", len(tracker.createCalls))
	}
	created := tracker.createCalls[0]
	if created.Title != "Export fails for empty accounts." {
		t.Errorf("Title = %q, want derived first sentence", created.Title)
	}
	if created.Category != "bug" {
		t.Errorf("Category = %q, want bug (sentry-corroborated)", created.Category)
	}
	if created.StateID != "state-uuid" {
		t.Errorf("StateID = %q, want cfg.TriageStateID passed verbatim", created.StateID)
	}

	entry, err := store.GetTicketIndex("sentry:BILLING-2291")
	if err != nil {
		t.Fatalf("GetTicketIndex() error = %v", err)
	}
	if entry == nil {
		t.Fatal("expected ticket index entry to be created")
	}
	if entry.IssueID != "TOAD-42" || entry.InvestigationID != "inv-1" {
		t.Errorf("entry = %+v, want IssueID TOAD-42, InvestigationID inv-1", entry)
	}

	if len(tracker.commentCalls) != 0 {
		t.Errorf("PostComment calls = %d, want 0 on the create path", len(tracker.commentCalls))
	}
}

func TestFileOrUpdate_Idempotent(t *testing.T) {
	tracker := &fakeTracker{nextID: "TOAD-7"}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{}, fixedPermalink("https://slack.example.com/thread"))

	f := investigation.Findings{
		Problem:        "Export fails for empty accounts.",
		SentryIssueIDs: []string{"BILLING-2291"},
		Reasoning:      "Seen again in #billing-alerts.",
	}

	first, err := e.FileOrUpdate(context.Background(), f, "C123", "1722.0001", "inv-1", SourceAuto)
	if err != nil {
		t.Fatalf("first FileOrUpdate() error = %v", err)
	}
	if first.AlreadyExisted {
		t.Fatalf("first call: AlreadyExisted = true, want false")
	}
	if len(tracker.createCalls) != 1 {
		t.Fatalf("after first call: CreateIssue calls = %d, want 1", len(tracker.createCalls))
	}

	second, err := e.FileOrUpdate(context.Background(), f, "C123", "1722.0001", "inv-2", SourceDigest)
	if err != nil {
		t.Fatalf("second FileOrUpdate() error = %v", err)
	}
	if !second.AlreadyExisted {
		t.Errorf("second call: AlreadyExisted = false, want true")
	}
	if second.Ref == nil || second.Ref.ID != "TOAD-7" {
		t.Errorf("second call: Ref = %+v, want ID TOAD-7 (existing ticket)", second.Ref)
	}

	// The second call must not create a duplicate — only comment.
	if len(tracker.createCalls) != 1 {
		t.Errorf("CreateIssue calls = %d after second FileOrUpdate, want still 1 (no duplicate)", len(tracker.createCalls))
	}
	if len(tracker.commentCalls) != 1 {
		t.Fatalf("PostComment calls = %d, want 1", len(tracker.commentCalls))
	}
	comment := tracker.commentCalls[0]
	if comment.ref.ID != "TOAD-7" {
		t.Errorf("comment posted to ref ID %q, want TOAD-7", comment.ref.ID)
	}
	for _, want := range []string{"**Toad re-observed this issue**", "Seen again in #billing-alerts.", "https://slack.example.com/thread"} {
		if !strings.Contains(comment.body, want) {
			t.Errorf("comment body = %q, missing %q", comment.body, want)
		}
	}

	entry, err := store.GetTicketIndex("sentry:BILLING-2291")
	if err != nil {
		t.Fatalf("GetTicketIndex() error = %v", err)
	}
	if entry.IssueID != "TOAD-7" {
		t.Errorf("entry.IssueID = %q, want TOAD-7 unchanged", entry.IssueID)
	}
}

func TestFileOrUpdate_PermalinkErrorIsBestEffort(t *testing.T) {
	tracker := &fakeTracker{nextID: "TOAD-1"}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{}, func(string, string) (string, error) {
		return "", errors.New("slack unavailable")
	})

	f := investigation.Findings{Problem: "Something broke.", SentryIssueIDs: []string{"X-1"}}

	result, err := e.FileOrUpdate(context.Background(), f, "C1", "1.0", "inv-1", SourceAuto)
	if err != nil {
		t.Fatalf("FileOrUpdate() error = %v, want nil (permalink failure must not fail the flow)", err)
	}
	if result.AlreadyExisted {
		t.Errorf("AlreadyExisted = true, want false")
	}
}
