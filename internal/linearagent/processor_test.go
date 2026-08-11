package linearagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/responder"
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

type appliedUpdate struct {
	Issue  string
	Update responder.TicketUpdate
}

func quickEnvelope(reply string) *responder.Envelope { return &responder.Envelope{Reply: reply} }

func newTestProcessor(db *state.DB, poster *fakePoster, respond func(ctx context.Context, w Work) (*responder.Envelope, error)) (*Processor, *[]appliedUpdate) {
	claims := map[string]bool{}
	var updates []appliedUpdate
	p := NewProcessor(ProcessorOpts{
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
		Unclaim: func(key, scope string) { delete(claims, key+"/"+scope) },
		Respond: respond,
		UpdateTicket: func(ctx context.Context, issue string, u responder.TicketUpdate) error {
			updates = append(updates, appliedUpdate{issue, u})
			return nil
		},
		Timeout: time.Minute,
	})
	return p, &updates
}

func TestHandle_AckThenRespondThenRecord(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p, _ := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		return &responder.Envelope{Reply: "The export double-counts partial refunds.",
			DidInvestigate: true, FindingsSummary: "aggregate() sums superseded rows"}, nil
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
	// Investigated envelope persisted for later same-trigger reuse.
	inv, _ := db.GetInvestigationByThread("linear-session:sess-1")
	if inv == nil {
		t.Error("envelope not persisted")
	}
}

func TestHandle_ErrorPathPostsErrorActivity(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p, _ := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		return nil, errors.New("agent exploded")
	})
	w := work()
	p.Handle(context.Background(), w)

	last := poster.posted[len(poster.posted)-1]
	if last.Type != "error" {
		t.Errorf("last activity = %+v, want error", last)
	}
	if !strings.Contains(last.Body, "I could not answer:") {
		t.Errorf("error body = %q, want the new prefix", last.Body)
	}
	// Once the error activity has posted, the session is answered for this
	// prompt: the handled record must be written so a failing response is
	// retried at most once per prompt, not on every poll tick.
	rec, _ := db.GetAgentSession("sess-1")
	if rec == nil || !rec.LastHandledActivityAt.Equal(w.TriggeredAt) {
		t.Errorf("record = %+v, want handled record written after the error activity posts", rec)
	}
}

func TestHandle_FailedErrorPostLeavesRecordUnwritten(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{failOn: "error"}
	p, _ := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
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
	p, _ := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		t.Fatal("must not respond on claim conflict")
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

func TestHandle_ResponsePostFailureDoesNotWriteHandledRecord(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{failOn: "response"}
	p, _ := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		return quickEnvelope("The export double-counts partial refunds."), nil
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

func TestHandle_AckFailureAbortsWithoutResponding(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{failOn: "thought"}
	responded := false
	p, _ := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		responded = true
		return quickEnvelope("The export double-counts partial refunds."), nil
	})
	p.Handle(context.Background(), work())

	if responded {
		t.Error("a session we cannot ack must not be responded to (e.g. Linear rejects the session: foreign, dismissed, or stale)")
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

func TestHandle_FollowUpDuringInProgressCallsRespondEvenWithPersistedEnvelope(t *testing.T) {
	db := procDB(t)
	env := &responder.Envelope{Reply: "stale prior answer", DidInvestigate: true, FindingsSummary: "s"}
	ej, _ := json.Marshal(env)

	// T1: the original trigger, whose response is still running.
	// T2: a follow-up prompt arrives WHILE T1's response is in flight, so
	// T2 > T1's TriggeredAt. T1's envelope only finishes (and is persisted)
	// after T2 has already arrived, so its CreatedAt is after T2 too — that
	// ordering is exactly the trap a CreatedAt-based heuristic would fall
	// into. The ID-based guard must not make that mistake: T1's record's ID
	// is keyed to T1's TriggeredAt, so it never matches investigationID(w)
	// for a Work whose TriggeredAt is T2.
	t1 := work()
	t1.TriggeredAt = time.Now().Add(-10 * time.Minute)
	t2 := time.Now().Add(-5 * time.Minute)

	db.SaveInvestigation(&state.InvestigationRecord{
		ID: investigationID(t1), ThreadTS: "linear-session:sess-1", Channel: "linear",
		FindingsJSON: string(ej), CreatedAt: t2.Add(2 * time.Minute), // finishes after T2 arrived
	})

	respondCalls := 0
	p, _ := newTestProcessor(db, &fakePoster{}, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		respondCalls++
		return quickEnvelope("fresh follow-up answer"), nil
	})

	w := work()
	w.FollowUp = true
	w.TriggeredAt = t2

	p.Handle(context.Background(), w)

	if respondCalls != 1 {
		t.Errorf("respondCalls = %d, want 1 (a follow-up sent mid-response must call Respond, not reuse the prior trigger's persisted envelope)", respondCalls)
	}
}

func TestHandle_TicketUpdateAppliedBeforeReply(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p, updates := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		return &responder.Envelope{Reply: "Updated the ticket.",
			TicketUpdate: &responder.TicketUpdate{Comment: "summary comment"}}, nil
	})
	p.Handle(context.Background(), work())

	if len(*updates) != 1 || (*updates)[0].Issue != "PLF-9" {
		t.Fatalf("updates = %+v (empty Issue must default to the session's own ticket)", *updates)
	}
	last := poster.posted[len(poster.posted)-1]
	if last.Type != "response" || last.Body != "Updated the ticket." {
		t.Errorf("reply = %+v", last)
	}
}

func TestHandle_TicketUpdateForOtherIssueRefused(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p, updates := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		return &responder.Envelope{Reply: "Done.",
			TicketUpdate: &responder.TicketUpdate{Issue: "OTHER-1", Comment: "c"}}, nil
	})
	p.Handle(context.Background(), work())

	if len(*updates) != 0 {
		t.Fatalf("cross-ticket update must not be applied, got %+v", *updates)
	}
	last := poster.posted[len(poster.posted)-1]
	if !strings.Contains(last.Body, "only update the ticket this session is on") {
		t.Errorf("reply should explain the refusal, got %q", last.Body)
	}
}

func TestHandle_TicketUpdateFailurePrependsNoteAndStillReplies(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	claims := map[string]bool{}
	p := NewProcessor(ProcessorOpts{
		Poster: poster, DB: db,
		Claim: func(k, s string) bool {
			key := k + "/" + s
			if claims[key] {
				return false
			}
			claims[key] = true
			return true
		},
		Unclaim: func(k, s string) { delete(claims, k+"/"+s) },
		Respond: func(ctx context.Context, w Work) (*responder.Envelope, error) {
			return &responder.Envelope{Reply: "Here is the summary.",
				TicketUpdate: &responder.TicketUpdate{Description: "new body"}}, nil
		},
		UpdateTicket: func(ctx context.Context, issue string, u responder.TicketUpdate) error {
			return errors.New("linear rejected the update")
		},
		Timeout: time.Minute,
	})
	p.Handle(context.Background(), work())

	last := poster.posted[len(poster.posted)-1]
	if last.Type != "response" || !strings.Contains(last.Body, "could not update the ticket") ||
		!strings.Contains(last.Body, "Here is the summary.") {
		t.Errorf("reply = %+v", last)
	}
	if rec, _ := db.GetAgentSession("sess-1"); rec == nil {
		t.Error("update failure must not block the handled record (the reply posted)")
	}
}

func TestHandle_PersistsOnlyInvestigatedEnvelopes(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p, _ := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		return quickEnvelope("quick answer"), nil
	})
	p.Handle(context.Background(), work())
	if rec, _ := db.GetInvestigationByThread("linear-session:sess-1"); rec != nil {
		t.Error("non-investigative reply must not persist a record")
	}

	p2, _ := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		return &responder.Envelope{Reply: "deep answer", DidInvestigate: true, FindingsSummary: "the cap is gone"}, nil
	})
	w2 := work()
	w2.Session.ID = "sess-2"
	w2.TriggeredAt = w2.TriggeredAt.Add(time.Minute)
	p2.Handle(context.Background(), w2)
	rec, _ := db.GetInvestigationByThread("linear-session:sess-2")
	if rec == nil {
		t.Fatal("investigated reply must persist")
	}
	var env responder.Envelope
	if err := json.Unmarshal([]byte(rec.FindingsJSON), &env); err != nil || env.FindingsSummary != "the cap is gone" {
		t.Errorf("persisted envelope = %+v err=%v", env, err)
	}
}

func TestHandle_SameTriggerRetryRepostsWithoutRespond(t *testing.T) {
	db := procDB(t)
	respondCalls := 0
	// First pass: respond succeeds, applies a ticket update, persists the
	// FINAL reply (investigated), but the response post fails.
	poster1 := &fakePoster{failOn: "response"}
	p1, updates1 := newTestProcessor(db, poster1, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		respondCalls++
		return &responder.Envelope{Reply: "expensive answer", DidInvestigate: true, FindingsSummary: "s",
			TicketUpdate: &responder.TicketUpdate{Comment: "c"}}, nil
	})
	w := work()
	p1.Handle(context.Background(), w)
	if rec, _ := db.GetAgentSession("sess-1"); rec != nil {
		t.Fatal("failed post must leave record unwritten")
	}

	// Retry with identical Work: must re-post from the stored envelope
	// without calling Respond OR UpdateTicket again — the update was already
	// applied on the first attempt, and the stored envelope's Reply already
	// reflects it, so replaying UpdateTicket would duplicate it (e.g. a
	// Linear comment posted twice).
	poster2 := &fakePoster{}
	p2, updates2 := newTestProcessor(db, poster2, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		respondCalls++
		return quickEnvelope("should not run"), nil
	})
	p2.Handle(context.Background(), w)
	if respondCalls != 1 {
		t.Errorf("Respond ran %d times, want 1 (retry reuses the stored envelope)", respondCalls)
	}
	if total := len(*updates1) + len(*updates2); total != 1 {
		t.Errorf("UpdateTicket called %d times across both attempts, want exactly 1 (applied once, on the first attempt)", total)
	}
	last := poster2.posted[len(poster2.posted)-1]
	if last.Body != "expensive answer" {
		t.Errorf("retry reply = %q, want the first attempt's final reply", last.Body)
	}
}

func TestHandle_NonInvestigatedQuickEditRetryDoesNotDoubleApply(t *testing.T) {
	db := procDB(t)
	respondCalls := 0
	// First pass: a quick edit (no investigation) applies its ticket update
	// and produces a final reply, but the response post itself fails.
	poster1 := &fakePoster{failOn: "response"}
	p1, updates1 := newTestProcessor(db, poster1, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		respondCalls++
		return &responder.Envelope{Reply: "Added the comment.",
			TicketUpdate: &responder.TicketUpdate{Comment: "quick note"}}, nil
	})
	w := work()
	p1.Handle(context.Background(), w)
	if rec, _ := db.GetAgentSession("sess-1"); rec != nil {
		t.Fatal("failed post must leave record unwritten")
	}

	// Retry with identical Work: must re-post the stored final reply
	// without calling Respond OR UpdateTicket again — before this fix, a
	// non-investigated envelope (DidInvestigate=false) was never persisted,
	// so this retry fell through to a fresh Respond call and re-applied the
	// ticket update, duplicating the comment.
	poster2 := &fakePoster{}
	p2, updates2 := newTestProcessor(db, poster2, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		respondCalls++
		return quickEnvelope("should not run"), nil
	})
	p2.Handle(context.Background(), w)

	if respondCalls != 1 {
		t.Errorf("Respond ran %d times, want 1 (retry must reuse the persisted quick-edit envelope)", respondCalls)
	}
	if total := len(*updates1) + len(*updates2); total != 1 {
		t.Errorf("UpdateTicket called %d times across both attempts, want exactly 1", total)
	}
	last := poster2.posted[len(poster2.posted)-1]
	if last.Body != "Added the comment." {
		t.Errorf("retry reply = %q, want the first attempt's final reply", last.Body)
	}
}

func TestHandle_TicketUpdateIssueOnlyIsNotAnUpdate(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	// A TicketUpdate with only Issue set carries no title/description/comment
	// content, so TicketUpdate.IsZero() is true — this is not an update at
	// all, just (at most) a stray issue reference. No UpdateTicket call, no
	// refusal note, the reply posts unmodified.
	p, updates := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*responder.Envelope, error) {
		return &responder.Envelope{Reply: "Just an answer.",
			TicketUpdate: &responder.TicketUpdate{Issue: "PLF-9"}}, nil
	})
	p.Handle(context.Background(), work())

	if len(*updates) != 0 {
		t.Fatalf("Issue-only TicketUpdate carries no content; must not call UpdateTicket, got %+v", *updates)
	}
	last := poster.posted[len(poster.posted)-1]
	if last.Type != "response" || last.Body != "Just an answer." {
		t.Errorf("reply = %+v, want the plain reply with no refusal note", last)
	}
}
