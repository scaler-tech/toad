package ticket

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
//
// ShouldCreateIssues defaults to true (overriding the embedded NoopTracker's
// false) since every pre-existing test in this file exercises a tracker
// that's meant to be capable of filing — set cannotCreate to exercise the
// "tracker can't create issues" guard in file() instead (see
// TestFileOrUpdate_ShouldCreateIssuesFalse below).
type fakeTracker struct {
	issuetracker.NoopTracker
	createCalls  []issuetracker.CreateIssueOpts
	commentCalls []struct {
		ref  *issuetracker.IssueRef
		body string
	}
	nextID       string
	createErr    error
	postErr      error
	cannotCreate bool
	nilRef       bool
}

func (t *fakeTracker) ShouldCreateIssues() bool {
	return !t.cannotCreate
}

func (t *fakeTracker) CreateIssue(_ context.Context, opts issuetracker.CreateIssueOpts) (*issuetracker.IssueRef, error) {
	if t.createErr != nil {
		return nil, t.createErr
	}
	if t.nilRef {
		return nil, nil
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

	t.Run("all blank sentry ids fall back to thread key", func(t *testing.T) {
		f := investigation.Findings{SentryIssueIDs: []string{"", "   "}}
		got := ExternalKey(f, "C123", "1722.000100")
		want := "thread:C123:1722.000100"
		if got != want {
			t.Errorf("ExternalKey() = %q, want %q (blank ids must not produce a degenerate \"sentry:\" key)", got, want)
		}
	})

	t.Run("mixed blank and real sentry ids use first non-blank", func(t *testing.T) {
		f := investigation.Findings{SentryIssueIDs: []string{"", "  ", "BILLING-2291"}}
		got := ExternalKey(f, "C123", "1722.000100")
		want := "sentry:BILLING-2291"
		if got != want {
			t.Errorf("ExternalKey() = %q, want %q", got, want)
		}
	})
}

func TestDecide_BlankSentryIDs(t *testing.T) {
	t.Run("all blank ids do not count as corroboration", func(t *testing.T) {
		e := New(&fakeTracker{}, newFakeStore(), config.TicketConfig{
			AutoFile:           true,
			AutoFileConfidence: 0.85,
		}, nil)
		f := investigation.Findings{
			Feasible:       true,
			Confidence:     0.9,
			SentryIssueIDs: []string{"", "   "},
		}
		if got := e.Decide(f); got != DecisionPropose {
			t.Errorf("Decide() = %v, want DecisionPropose (all-blank sentry ids)", got)
		}
	})

	t.Run("mixed blank and real ids still auto-files", func(t *testing.T) {
		e := New(&fakeTracker{}, newFakeStore(), config.TicketConfig{
			AutoFile:           true,
			AutoFileConfidence: 0.85,
		}, nil)
		f := investigation.Findings{
			Feasible:       true,
			Confidence:     0.9,
			SentryIssueIDs: []string{"", "BILLING-2291"},
		}
		if got := e.Decide(f); got != DecisionAutoFile {
			t.Errorf("Decide() = %v, want DecisionAutoFile", got)
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

func TestFileOrUpdate_ConcurrentSameKeySerializes(t *testing.T) {
	tracker := &fakeTracker{nextID: "TOAD-99"}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{}, nil)

	f := investigation.Findings{
		Problem:        "Duplicate Sentry deliveries racing to file the same ticket.",
		SentryIssueIDs: []string{"RACE-1"},
	}

	const n = 2
	results := make([]*FileResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			results[i], errs[i] = e.FileOrUpdate(context.Background(), f, "C1", "1.0", "inv-1", SourceAuto)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: FileOrUpdate() error = %v", i, err)
		}
	}

	if len(tracker.createCalls) != 1 {
		t.Fatalf("CreateIssue calls = %d, want exactly 1 (duplicate-filing race must be closed)", len(tracker.createCalls))
	}

	existedCount := 0
	for i, r := range results {
		if r == nil {
			t.Fatalf("goroutine %d: result is nil", i)
		}
		if r.AlreadyExisted {
			existedCount++
		}
	}
	if existedCount != 1 {
		t.Errorf("results with AlreadyExisted=true = %d, want exactly 1 (one winner creates, one loser comments)", existedCount)
	}
	if len(tracker.commentCalls) != 1 {
		t.Errorf("PostComment calls = %d, want exactly 1 (the loser's re-observation)", len(tracker.commentCalls))
	}
}

func TestFileOrUpdate_EmptyCategoryWhenNoSentryCorroboration(t *testing.T) {
	tracker := &fakeTracker{nextID: "TOAD-5"}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{}, nil)

	// No SentryIssueIDs at all: this is the documented trade-off in file()'s
	// Category derivation — a proposed, non-Sentry-corroborated finding gets
	// no bug/feature label rather than a guessed one.
	f := investigation.Findings{Problem: "A feature request with no Sentry backing."}

	result, err := e.FileOrUpdate(context.Background(), f, "C1", "1.0", "inv-1", SourceCTA)
	if err != nil {
		t.Fatalf("FileOrUpdate() error = %v", err)
	}
	if result.AlreadyExisted {
		t.Errorf("AlreadyExisted = true, want false")
	}
	if len(tracker.createCalls) != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", len(tracker.createCalls))
	}
	if got := tracker.createCalls[0].Category; got != "" {
		t.Errorf("Category = %q, want empty (no Sentry corroboration, no category label)", got)
	}
}

// Regression for the critical nil-deref: issuetracker.NewTracker returns a
// NoopTracker (CreateIssue -> (nil, nil), ShouldCreateIssues -> false)
// whenever issue_tracker.enabled is false — the default for a stock install.
// Before the ShouldCreateIssues guard in file(), the first ticket request on
// such an install would panic dereferencing a nil ref.ID. This must instead
// return a clean error, and never call CreateIssue at all.
func TestFileOrUpdate_ShouldCreateIssuesFalseReturnsCleanError(t *testing.T) {
	tracker := &fakeTracker{cannotCreate: true}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{}, nil)

	f := investigation.Findings{Problem: "Something broke.", SentryIssueIDs: []string{"X-1"}}

	result, err := e.FileOrUpdate(context.Background(), f, "C1", "1.0", "inv-1", SourceAuto)
	if err == nil {
		t.Fatal("expected an error when the tracker cannot create issues, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result on error, got %+v", result)
	}
	if len(tracker.createCalls) != 0 {
		t.Errorf("expected CreateIssue never called, got %d calls", len(tracker.createCalls))
	}
}

// Belt-and-suspenders: even when ShouldCreateIssues reports true, a Tracker
// implementation (like NoopTracker.CreateIssue) could still return a nil ref
// with no error — file() must catch this explicitly rather than
// dereferencing ref.ID/ref.URL and panicking.
func TestFileOrUpdate_NilRefFromCreateIssueReturnsCleanError(t *testing.T) {
	tracker := &fakeTracker{nilRef: true}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{}, nil)

	f := investigation.Findings{Problem: "Something broke.", SentryIssueIDs: []string{"X-1"}}

	result, err := e.FileOrUpdate(context.Background(), f, "C1", "1.0", "inv-1", SourceAuto)
	if err == nil {
		t.Fatal("expected an error when CreateIssue returns a nil ref, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result on error, got %+v", result)
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

// TestFileOrUpdate_ReobserveHitPostCommentErrorPropagates covers reobserve's
// (the hit-path helper inside FileOrUpdate) DIFFERENT-from-cmd-package
// semantics: unlike cmd/ticketflow.go's preInvestigationTicketCheck, which
// swallows a PostComment failure during its own idempotency pre-check and
// logs it, reobserve here PROPAGATES the error wrapped with "posting
// re-observation comment on %s: %w" — this is what makes FileOrUpdate return
// an error on this path, which callers (e.g. fileOrProposeFromFindings) turn
// into outcomeFilingFailed. Retires fakeTracker.postErr, previously defined
// but never set by any test in this file.
func TestFileOrUpdate_ReobserveHitPostCommentErrorPropagates(t *testing.T) {
	tracker := &fakeTracker{nextID: "TOAD-7"}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{}, nil)

	// Seed the index directly so FileOrUpdate takes the hit (reobserve)
	// branch on its very first call, rather than needing two calls.
	seeded := &state.TicketIndexEntry{
		ExternalKey: "sentry:BILLING-2291",
		IssueID:     "TOAD-EXISTING",
		IssueURL:    "https://linear.app/toad/issue/TOAD-EXISTING",
		Source:      string(SourceAuto),
		CreatedAt:   time.Now(),
		LastSeenAt:  time.Now(),
	}
	if err := store.UpsertTicketIndex(seeded); err != nil {
		t.Fatalf("seeding ticket index: %v", err)
	}

	wantErr := errors.New("linear comment api unavailable")
	tracker.postErr = wantErr

	f := investigation.Findings{
		Problem:        "Export fails again for empty accounts.",
		SentryIssueIDs: []string{"BILLING-2291"},
	}

	result, err := e.FileOrUpdate(context.Background(), f, "C123", "1722.0001", "inv-2", SourceDigest)
	if err == nil {
		t.Fatal("expected FileOrUpdate to propagate the PostComment error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("expected the returned error to wrap postErr (errors.Is), got %v", err)
	}
	if !strings.Contains(err.Error(), "posting re-observation comment on TOAD-EXISTING") {
		t.Errorf("expected error to describe the re-observation-comment failure point, got %q", err.Error())
	}
	if result != nil {
		t.Errorf("expected nil result on error, got %+v", result)
	}
	if len(tracker.createCalls) != 0 {
		t.Errorf("expected no CreateIssue call on the re-observe (hit) path, got %d", len(tracker.createCalls))
	}

	// The index entry must be left as seeded — the failed comment must not
	// have been followed by a bump to LastSeenAt (UpsertTicketIndex happens
	// AFTER PostComment in reobserve, so it's never reached on this error).
	entry, err := store.GetTicketIndex("sentry:BILLING-2291")
	if err != nil {
		t.Fatalf("GetTicketIndex() error = %v", err)
	}
	if entry.IssueID != "TOAD-EXISTING" {
		t.Errorf("entry.IssueID = %q, want unchanged TOAD-EXISTING", entry.IssueID)
	}
}

// TestFileOrUpdate_CreateIssueErrorPropagates covers the miss-path sibling of
// the reobserve test above: file()'s CreateIssue failure is wrapped with
// "creating issue for %s: %w" and propagated out of FileOrUpdate. Retires
// fakeTracker.createErr, previously defined but never set by any test in
// this file.
func TestFileOrUpdate_CreateIssueErrorPropagates(t *testing.T) {
	tracker := &fakeTracker{}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{}, nil)

	wantErr := errors.New("linear api unavailable")
	tracker.createErr = wantErr

	f := investigation.Findings{
		Problem:        "Export fails for empty accounts.",
		SentryIssueIDs: []string{"BILLING-3001"},
	}

	result, err := e.FileOrUpdate(context.Background(), f, "C1", "1.0", "inv-1", SourceAuto)
	if err == nil {
		t.Fatal("expected FileOrUpdate to propagate the CreateIssue error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("expected the returned error to wrap createErr (errors.Is), got %v", err)
	}
	if !strings.Contains(err.Error(), "creating issue for sentry:BILLING-3001") {
		t.Errorf("expected error to describe the create-issue failure point, got %q", err.Error())
	}
	if result != nil {
		t.Errorf("expected nil result on error, got %+v", result)
	}
	if len(tracker.commentCalls) != 0 {
		t.Errorf("expected no PostComment call on the create (miss) path, got %d", len(tracker.commentCalls))
	}

	entry, err := store.GetTicketIndex("sentry:BILLING-3001")
	if err != nil {
		t.Fatalf("GetTicketIndex() error = %v", err)
	}
	if entry != nil {
		t.Errorf("expected no ticket index entry to be created on a CreateIssue failure, got %+v", entry)
	}
}

func TestFileOrUpdate_PassesRequestedTeamAndProject(t *testing.T) {
	tracker := &fakeTracker{nextID: "ANA-7"}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{}, fixedPermalink("https://slack.example.com/x"))

	f := investigation.Findings{
		Problem:       "Cost page uses unreliable cost-based metrics.",
		LinearTeam:    "ANA",
		LinearProject: "Biome",
	}

	if _, err := e.FileOrUpdate(context.Background(), f, "C123", "1722.0002", "inv-2", SourceCTA); err != nil {
		t.Fatalf("FileOrUpdate() error = %v", err)
	}
	if len(tracker.createCalls) != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", len(tracker.createCalls))
	}
	created := tracker.createCalls[0]
	if created.Team != "ANA" {
		t.Errorf("Team = %q, want the findings' explicitly requested team passed verbatim", created.Team)
	}
	if created.Project != "Biome" {
		t.Errorf("Project = %q, want the findings' explicitly requested project passed verbatim", created.Project)
	}
}

func TestFileOrUpdate_PassesResolvedAssignees(t *testing.T) {
	tracker := &fakeTracker{nextID: "ANA-8"}
	store := newFakeStore()
	e := New(tracker, store, config.TicketConfig{}, fixedPermalink("https://slack.example.com/x"))

	f := investigation.Findings{
		Problem:                 "User changes should be added to the audit logs.",
		LinearResolvedAssignees: []string{"alice@example.com", "biome"},
	}

	if _, err := e.FileOrUpdate(context.Background(), f, "C123", "1722.0003", "inv-3", SourceCTA); err != nil {
		t.Fatalf("FileOrUpdate() error = %v", err)
	}
	if len(tracker.createCalls) != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", len(tracker.createCalls))
	}
	created := tracker.createCalls[0]
	want := []string{"alice@example.com", "biome"}
	if len(created.Assignees) != len(want) {
		t.Fatalf("Assignees = %v, want %v", created.Assignees, want)
	}
	for i, w := range want {
		if created.Assignees[i] != w {
			t.Errorf("Assignees[%d] = %q, want %q", i, created.Assignees[i], w)
		}
	}
}
