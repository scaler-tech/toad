package linearagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/state"
)

type postedActivity struct{ Type, Body string }

type fakePoster struct {
	posted []postedActivity
	failOn string // if non-empty, return error for activities matching this type
}

func (f *fakePoster) CreateActivity(ctx context.Context, sessionID, activityType, body string) error {
	if f.failOn == activityType {
		return errors.New("poster simulated failure")
	}
	f.posted = append(f.posted, postedActivity{activityType, body})
	return nil
}

func testFindings() *investigation.Findings {
	return &investigation.Findings{
		Feasible: true, Title: "Export double-counts refunds",
		Problem:   "Totals are 2x for partial refunds.",
		RootCause: "aggregate() sums superseded rows (billing/export/aggregate.py:118).",
		Evidence: []investigation.Evidence{
			{Kind: "file", Ref: "billing/export/aggregate.py:118", Note: "no supersede filter"},
		},
		Confidence: 0.8, Repo: "billing",
		Reasoning: "The export double-counts partial refunds. The fix is one file.",
	}
}

func procDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func work() Work {
	return Work{
		Session: Session{ID: "sess-1", Status: "pending", CreatedAt: time.Now().Add(-time.Minute),
			IssueID: "uuid-1", IssueIdentifier: "PLF-9", IssueTitle: "Exports are slow"},
		Prompt:      "@toad why are exports slow?",
		TriggeredAt: time.Now().Add(-time.Minute),
	}
}

func newTestProcessor(db *state.DB, poster *fakePoster, investigate func(ctx context.Context, w Work) (*investigation.Findings, error)) *Processor {
	claims := map[string]bool{}
	return NewProcessor(ProcessorOpts{
		Poster: poster,
		DB:     db,
		Claim: func(key, scope string) bool {
			k := key + "/" + scope
			if claims[k] {
				return false
			}
			claims[k] = true
			return true
		},
		Unclaim:     func(key, scope string) { delete(claims, key+"/"+scope) },
		Investigate: investigate,
		Timeout:     time.Minute,
	})
}

func TestHandle_AckThenInvestigateThenRespond(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		return testFindings(), nil
	})
	w := work()
	p.Handle(context.Background(), w)

	if len(poster.posted) != 2 {
		t.Fatalf("posted %d activities: %+v", len(poster.posted), poster.posted)
	}
	if poster.posted[0].Type != "thought" {
		t.Errorf("first activity = %+v, want thought ack", poster.posted[0])
	}
	if poster.posted[1].Type != "response" || !strings.Contains(poster.posted[1].Body, "double-counts") {
		t.Errorf("second activity = %+v", poster.posted[1])
	}
	// Handled record written with the trigger time.
	rec, _ := db.GetAgentSession("sess-1")
	if rec == nil || !rec.LastHandledActivityAt.Equal(w.TriggeredAt) {
		t.Errorf("record = %+v", rec)
	}
	// Findings persisted for later reuse.
	inv, _ := db.GetInvestigationByThread("linear-session:sess-1")
	if inv == nil {
		t.Error("investigation not persisted")
	}
}

func TestHandle_InvestigationErrorPostsErrorActivity(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		return nil, errors.New("agent exploded")
	})
	w := work()
	p.Handle(context.Background(), w)

	last := poster.posted[len(poster.posted)-1]
	if last.Type != "error" {
		t.Errorf("last activity = %+v, want error", last)
	}
	// Once the error activity has posted, the session is answered for this
	// prompt: the handled record must be written so a failing investigation
	// is retried at most once per prompt, not on every poll tick.
	rec, _ := db.GetAgentSession("sess-1")
	if rec == nil || !rec.LastHandledActivityAt.Equal(w.TriggeredAt) {
		t.Errorf("record = %+v, want handled record written after the error activity posts", rec)
	}
}

func TestHandle_FailedErrorPostLeavesRecordUnwritten(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{failOn: "error"}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		return nil, errors.New("agent exploded")
	})
	p.Handle(context.Background(), work())

	if rec, _ := db.GetAgentSession("sess-1"); rec != nil {
		t.Errorf("record = %+v, want unwritten when the error activity itself fails to post (retry next poll)", rec)
	}
}

func TestHandle_ClaimConflictStopsWithoutHandledRecord(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		t.Fatal("must not investigate on claim conflict")
		return nil, nil
	})
	// Occupy the claim.
	if !p.opts.Claim("PLF-9", "linear-agent") {
		t.Fatal("setup claim failed")
	}
	p.Handle(context.Background(), work())
	if rec, _ := db.GetAgentSession("sess-1"); rec != nil {
		t.Error("claim conflict must not write the handled record")
	}
}

func TestHandle_ReusesFreshFindings(t *testing.T) {
	db := procDB(t)
	f := testFindings()
	fj, _ := json.Marshal(f)
	// A prior filing on this issue links ticket_index -> investigations.
	db.SaveInvestigation(&state.InvestigationRecord{
		ID: "inv-1", ThreadTS: "slack-thread", Channel: "C1",
		FindingsJSON: string(fj), CreatedAt: time.Now(),
	})
	db.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey: "thread:C1:slack-thread", IssueID: "PLF-9",
		InvestigationID: "inv-1", CreatedAt: time.Now(), LastSeenAt: time.Now(),
	})

	poster := &fakePoster{}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		t.Fatal("fresh findings exist; must not re-investigate")
		return nil, nil
	})
	p.Handle(context.Background(), work())

	last := poster.posted[len(poster.posted)-1]
	if last.Type != "response" || !strings.Contains(last.Body, "double-counts") {
		t.Errorf("last = %+v", last)
	}
}

func TestComposeResponse_LeadsWithReasoningAndCitesEvidence(t *testing.T) {
	body := composeResponse(testFindings())
	if !strings.HasPrefix(body, "The export double-counts partial refunds.") {
		t.Errorf("must lead with reasoning, got: %q", body)
	}
	if !strings.Contains(body, "billing/export/aggregate.py:118") {
		t.Error("must cite evidence refs")
	}
}

func TestComposeResponse_InfeasibleStatesItPlainly(t *testing.T) {
	f := testFindings()
	f.Feasible = false
	f.Reasoning = "I could not confirm the root cause. I searched the export and billing packages."
	body := composeResponse(f)
	if !strings.Contains(body, "could not confirm") {
		t.Errorf("infeasible response must carry the reasoning, got: %q", body)
	}
}

func TestHandle_ReusesFreshnessRelativeToTriggerTime(t *testing.T) {
	db := procDB(t)
	f := testFindings()
	fj, _ := json.Marshal(f)
	now := time.Now()

	// Record created 25 hours ago
	recordCreatedAt := now.Add(-25 * time.Hour)
	db.SaveInvestigation(&state.InvestigationRecord{
		ID: "inv-1", ThreadTS: "slack-thread", Channel: "C1",
		FindingsJSON: string(fj), CreatedAt: recordCreatedAt,
	})
	db.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey: "thread:C1:slack-thread", IssueID: "PLF-9",
		InvestigationID: "inv-1", CreatedAt: recordCreatedAt, LastSeenAt: recordCreatedAt,
	})

	// Work triggered 26 hours ago. Under the brief's formula:
	// record.CreatedAt.After(triggeredAt.Add(-24h))
	// triggeredAt.Add(-24h) = (now-26h) - 24h = now-50h
	// record.CreatedAt = now-25h, and now-25h is after now-50h → true, MUST reuse
	//
	// A naive wall-clock freshness check (fresh iff time.Since(CreatedAt) < 24h)
	// would instead see time.Since(now-25h) = 25h, which is NOT < 24h, and
	// would incorrectly re-investigate. The trigger-relative formula avoids
	// that: it only cares whether the record predates *this prompt*, not
	// whether it predates "now" — a record can be older than 24h wall-clock
	// and still be the right answer to an old, just-detected prompt.
	w := work()
	w.TriggeredAt = now.Add(-26 * time.Hour)

	poster := &fakePoster{}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		t.Fatal("record is fresh relative to trigger time; must not re-investigate")
		return nil, nil
	})
	p.Handle(context.Background(), w)

	last := poster.posted[len(poster.posted)-1]
	if last.Type != "response" || !strings.Contains(last.Body, "double-counts") {
		t.Errorf("must reuse and respond, got: %+v", last)
	}
}

func TestHandle_RetryAfterResponsePostFailureReusesSamePromptFindings(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{failOn: "response"}
	investigateCalls := 0
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		investigateCalls++
		return testFindings(), nil
	})
	w := work()

	// First attempt: investigates, persists findings, but the response post
	// fails -> handled record stays unwritten, so the poller re-detects the
	// same Work (identical TriggeredAt) next tick.
	p.Handle(context.Background(), w)
	if investigateCalls != 1 {
		t.Fatalf("first Handle: investigateCalls = %d, want 1", investigateCalls)
	}
	if rec, _ := db.GetAgentSession("sess-1"); rec != nil {
		t.Fatalf("handled record must stay unwritten after a failed response post, got %+v", rec)
	}

	// Retry with the same Work: must reuse the persisted findings from the
	// first attempt (GetInvestigationByThread) rather than re-investigating.
	poster.failOn = ""
	p.Handle(context.Background(), w)
	if investigateCalls != 1 {
		t.Errorf("retry Handle: investigateCalls = %d, want still 1 (must reuse same-prompt findings)", investigateCalls)
	}
	rec, _ := db.GetAgentSession("sess-1")
	if rec == nil || !rec.LastHandledActivityAt.Equal(w.TriggeredAt) {
		t.Errorf("record = %+v, want handled record written after the retry succeeds", rec)
	}
}

func TestHandle_FollowUpDuringInProgressInvestigationDoesNotReuseStaleFindings(t *testing.T) {
	db := procDB(t)
	f := testFindings()
	fj, _ := json.Marshal(f)

	// T1: the original trigger, whose investigation is still running.
	// T2: a follow-up prompt arrives WHILE T1's investigation is in flight,
	// so T2 > T1's TriggeredAt. T1's investigation only finishes (and is
	// persisted) after T2 has already arrived, so its CreatedAt is after T2
	// too — that ordering is exactly the trap the CreatedAt-based heuristic
	// fell into: rec.CreatedAt.After(t2) would be true, wrongly reusing T1's
	// pre-follow-up findings for the follow-up. The ID-based guard must not
	// make that mistake: T1's record's ID is keyed to T1's TriggeredAt, so
	// it never matches investigationID(w) for a Work whose TriggeredAt is
	// T2.
	t1 := work()
	t1.TriggeredAt = time.Now().Add(-10 * time.Minute)
	t2 := time.Now().Add(-5 * time.Minute)

	db.SaveInvestigation(&state.InvestigationRecord{
		ID: investigationID(t1), ThreadTS: "linear-session:sess-1", Channel: "linear",
		FindingsJSON: string(fj), CreatedAt: t2.Add(2 * time.Minute), // finishes after T2 arrived
	})

	investigateCalls := 0
	p := newTestProcessor(db, &fakePoster{}, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		investigateCalls++
		return testFindings(), nil
	})

	w := work()
	w.FollowUp = true
	w.TriggeredAt = t2

	p.Handle(context.Background(), w)

	if investigateCalls != 1 {
		t.Errorf("investigateCalls = %d, want 1 (a follow-up sent mid-investigation must re-investigate, not reuse the prior trigger's stale findings)", investigateCalls)
	}
}

func TestHandle_ResponsePostFailureDoesNotWriteHandledRecord(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{failOn: "response"}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		return testFindings(), nil
	})
	p.Handle(context.Background(), work())

	// Verify that the handled record was NOT written (response post failed).
	rec, _ := db.GetAgentSession("sess-1")
	if rec != nil {
		t.Errorf("response post failure must not write handled record, but got: %+v", rec)
	}

	// Verify thought ack was posted, but response was not.
	thoughtCount := 0
	responseCount := 0
	for _, a := range poster.posted {
		if a.Type == "thought" {
			thoughtCount++
		}
		if a.Type == "response" {
			responseCount++
		}
	}
	if thoughtCount == 0 {
		t.Error("thought ack must be posted")
	}
	if responseCount != 0 {
		t.Error("response should not be posted on failure")
	}
}

func TestHandle_AckFailureAbortsWithoutInvestigating(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{failOn: "thought"}
	investigated := false
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		investigated = true
		return testFindings(), nil
	})
	p.Handle(context.Background(), work())

	if investigated {
		t.Error("a session we cannot ack must not be investigated (e.g. Linear rejects the session: foreign, dismissed, or stale)")
	}
	if rec, _ := db.GetAgentSession("sess-1"); rec != nil {
		t.Error("aborted session must not write the handled record (transient ack failures retry next poll)")
	}
	for _, a := range poster.posted {
		if a.Type == "response" || a.Type == "error" {
			t.Errorf("no further activities after failed ack, got %+v", a)
		}
	}
}
