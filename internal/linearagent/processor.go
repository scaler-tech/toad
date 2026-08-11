package linearagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/responder"
	"github.com/scaler-tech/toad/internal/state"
)

// ActivityPoster is the slice of Client the processor needs.
type ActivityPoster interface {
	CreateActivity(ctx context.Context, sessionID, activityType, body string) error
}

// ProcessorOpts wires the processor to the daemon (callback style, like
// digest.EngineOpts).
type ProcessorOpts struct {
	Poster       ActivityPoster
	DB           *state.DB
	Claim        func(key, scope string) bool
	Unclaim      func(key, scope string)
	Respond      func(ctx context.Context, w Work) (*responder.Envelope, error)
	UpdateTicket func(ctx context.Context, issueIdentifier string, u responder.TicketUpdate) error
	Timeout      time.Duration
}

// Processor answers one session's unhandled work: ack, claim, respond (or
// reuse a same-trigger envelope), apply any ticket update, reply. Sessions
// never file tickets and never mutate issues beyond the safe title/
// description/comment subset a TicketUpdate carries.
type Processor struct {
	opts ProcessorOpts
}

func NewProcessor(opts ProcessorOpts) *Processor { return &Processor{opts: opts} }

const claimScope = "linear-agent"

// Handle processes one Work item. It is the poller's handle callback.
func (p *Processor) Handle(ctx context.Context, w Work) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	// Ack immediately — Linear marks silent sessions unresponsive. A failed
	// ack aborts the session: if we cannot post to it, we cannot answer it
	// either (Linear rejects foreign, dismissed, or stale sessions with
	// "Invalid agent session"), so investigating would be pure waste. The
	// handled record stays unwritten, so a transient failure retries next poll.
	if err := p.opts.Poster.CreateActivity(ctx, w.Session.ID, "thought", "Reading the ticket and the code."); err != nil {
		slog.Warn("posting session ack; skipping session", "session", w.Session.ID, "error", err)
		return
	}

	claimKey := w.Session.IssueIdentifier
	if claimKey == "" {
		claimKey = w.Session.ID
	}
	if !p.opts.Claim(claimKey, claimScope) {
		// Another flow is already investigating this issue. Say so; the
		// handled record stays unwritten so a later prompt retriggers. ctx
		// may already be near its deadline (or expired) by the time we get
		// here, so post on a fresh short context rather than the one Handle
		// wrapped with the investigation timeout.
		postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		p.post(postCtx, w.Session.ID, "response", "An investigation is already running for this issue. Ask again in a few minutes.")
		return
	}
	defer p.opts.Unclaim(claimKey, claimScope)

	env, fromStore, err := p.findEnvelope(ctx, w)
	if err != nil {
		// The response may have failed because ctx's timeout expired — post
		// the error on a fresh short context so the post itself doesn't
		// silently fail on a dead context, and record the session as handled
		// so a failing response is answered (with an error) at most once per
		// user prompt; the human's next prompt retriggers.
		postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if postErr := p.opts.Poster.CreateActivity(postCtx, w.Session.ID, "error", "I could not answer: "+firstLine(err.Error())); postErr != nil {
			slog.Warn("posting session activity", "session", w.Session.ID, "type", "error", "error", postErr)
			return // error post failed -> handled record unwritten, retried next poll
		}
		p.recordHandled(w)
		return
	}

	reply := env.Reply
	if fromStore {
		// The ticket update (if any) was already applied on the attempt that
		// produced this envelope — see findEnvelope's doc comment. Applying
		// it again here would duplicate it (e.g. a Linear comment posted
		// twice), so a same-trigger retry only re-posts the stored reply.
	} else {
		// updateWasApplied tracks whether UpdateTicket actually succeeded
		// this attempt — NOT merely whether env carried a TicketUpdate. A
		// refused cross-ticket update (the switch's first case) or a failed
		// UpdateTicket call were never applied, so they must not force a
		// persist below: there is nothing there a retry could duplicate.
		updateWasApplied := false
		if !env.TicketUpdate.IsZero() {
			target := env.TicketUpdate.Issue
			switch {
			case target != "" && target != w.Session.IssueIdentifier:
				reply = "(I only update the ticket this session is on — ask me on " + target + " directly.)\n\n" + reply
			default:
				if target == "" {
					target = w.Session.IssueIdentifier
				}
				if err := p.opts.UpdateTicket(ctx, target, *env.TicketUpdate); err != nil {
					slog.Warn("applying ticket update", "session", w.Session.ID, "issue", target, "error", err)
					reply = "(I could not update the ticket: " + firstLine(err.Error()) + ")\n\n" + reply
				} else {
					updateWasApplied = true
					slog.Info("applied ticket update from session", "session", w.Session.ID, "issue", target,
						"title", env.TicketUpdate.Title != "", "description", env.TicketUpdate.Description != "", "comment", env.TicketUpdate.Comment != "")
				}
			}
		}

		// Persist BEFORE posting (expensive-envelope retry guarantee: if the
		// post below fails, a retry with identical Work reuses this record
		// instead of re-running Respond). The persisted envelope carries the
		// FINAL reply text (including any refusal/failure note) and a nil
		// TicketUpdate, so a retry's fromStore path above never re-applies
		// the update — see findEnvelope's doc comment for the consumed-update
		// invariant this preserves.
		//
		// updateWasApplied is included alongside the DidInvestigate gate:
		// an envelope that only applied a ticket update (no investigation,
		// e.g. "add a comment saying X") used to persist nothing here, so a
		// response-post failure's retry fell into this same else-branch
		// again and called UpdateTicket a second time — a duplicate Linear
		// comment. Persisting on either condition means such a retry takes
		// the fromStore branch above instead, which only re-posts.
		if (env.DidInvestigate && env.FindingsSummary != "") || updateWasApplied {
			p.persistEnvelope(w, &responder.Envelope{
				Reply: reply, DidInvestigate: env.DidInvestigate, FindingsSummary: env.FindingsSummary,
			})
		}
	}

	if err := p.opts.Poster.CreateActivity(ctx, w.Session.ID, "response", reply); err != nil {
		slog.Warn("posting session response", "session", w.Session.ID, "error", err)
		return // handled record unwritten -> retried next poll
	}

	p.recordHandled(w)
}

// recordHandled writes the handled-session record so this user prompt is not
// re-answered on the next poll. Shared by the success path and the
// error-activity-posted path (a failed investigation is still answered, with
// an error, at most once per prompt — the human's next prompt retriggers).
func (p *Processor) recordHandled(w Work) {
	if err := p.opts.DB.UpsertAgentSession(&state.AgentSessionRecord{
		SessionID: w.Session.ID, IssueID: w.Session.IssueID,
		IssueIdentifier: w.Session.IssueIdentifier, Status: w.Session.Status,
		LastHandledActivityAt: w.TriggeredAt, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		slog.Warn("recording handled session", "session", w.Session.ID, "error", err)
	}
}

// investigationID derives the deterministic investigation record ID for a
// given trigger: it identifies the (session, prompt) pair, not wall-clock
// time. Two Handle() calls for the SAME Work (a retry after a failed
// response post, where w.TriggeredAt is unchanged) produce the same ID, so
// SaveInvestigation's INSERT OR REPLACE makes the retry idempotent instead
// of accumulating rows. A follow-up prompt always carries a new
// w.TriggeredAt (see DetectWork), so it always derives a different ID.
func investigationID(w Work) string {
	return fmt.Sprintf("linv-%s-%d", w.Session.ID, w.TriggeredAt.UnixNano())
}

// findEnvelope returns the envelope to post: the persisted one on a
// same-trigger retry (fromStore=true — a prior Handle for THIS Work applied
// any ticket update and/or investigated, persisted the result, and failed
// only at the response post), else a fresh Respond run (fromStore=false;
// Handle applies the ticket update and persists it).
//
// Invariant: a persisted envelope never carries a TicketUpdate — updates are
// applied exactly once, on the attempt that produced the envelope, and the
// stored copy holds the FINAL reply text (post-update-note) with
// TicketUpdate cleared. This is what makes fromStore safe to skip the
// ticket-update block entirely: the update was already applied before this
// record was written, so replaying it on retry would duplicate it (e.g. a
// Linear comment posted twice). A crash between applying the update and
// persisting the record can still duplicate it on the next retry — an
// accepted at-least-once trade-off, same as the rest of this pipeline.
//
// Handle persists whenever EITHER the envelope investigated (DidInvestigate
// && FindingsSummary != "") OR its ticket update was actually applied this
// attempt (updateWasApplied) — not only on investigation. A quick-edit
// envelope ("add a comment saying X", no code reading involved) that applied
// its update must also be persisted: otherwise a response-post failure's
// retry falls through to a fresh, non-fromStore Handle for the same Work,
// which would call UpdateTicket a second time and duplicate the edit.
//
// The same-trigger match is intentionally an ID match, not a
// CreatedAt-vs-TriggeredAt time comparison: a follow-up prompt sent WHILE a
// prior response for this session is still running has a TriggeredAt that is
// *earlier* than the prior response's completion-time CreatedAt, so a
// time-based "was this produced after the prompt arrived" check would
// wrongly reuse a stale pre-follow-up envelope. Deriving the record's ID
// from w.TriggeredAt instead means a genuine follow-up (new TriggeredAt)
// always misses this lookup and falls through to Respond again, regardless
// of how the two calls' wall-clock times relate.
func (p *Processor) findEnvelope(ctx context.Context, w Work) (env *responder.Envelope, fromStore bool, err error) {
	wantID := investigationID(w)
	if rec, err := p.opts.DB.GetInvestigationByThread("linear-session:" + w.Session.ID); err == nil && rec != nil && rec.ID == wantID {
		var stored responder.Envelope
		if err := json.Unmarshal([]byte(rec.FindingsJSON), &stored); err == nil && stored.Reply != "" {
			slog.Info("linear session reusing same-trigger envelope", "session", w.Session.ID, "investigation", rec.ID)
			return &stored, true, nil
		}
	}

	fresh, err := p.opts.Respond(ctx, w)
	if err != nil {
		return nil, false, err
	}
	return fresh, false, nil
}

// persistEnvelope saves env (already the final, post-update reply — see
// findEnvelope's consumed-update invariant) under investigationID(w) so a
// retry of this exact Work reuses it instead of re-running Respond.
func (p *Processor) persistEnvelope(w Work, env *responder.Envelope) {
	ej, _ := json.Marshal(env)
	rec := &state.InvestigationRecord{
		ID:           investigationID(w),
		ThreadTS:     "linear-session:" + w.Session.ID,
		Channel:      "linear",
		FindingsJSON: string(ej),
		CreatedAt:    time.Now().UTC(),
	}
	if err := p.opts.DB.SaveInvestigation(rec); err != nil {
		slog.Warn("persisting session envelope", "session", w.Session.ID, "error", err)
	}
}

func (p *Processor) post(ctx context.Context, sessionID, activityType, body string) {
	if err := p.opts.Poster.CreateActivity(ctx, sessionID, activityType, body); err != nil {
		slog.Warn("posting session activity", "session", sessionID, "type", activityType, "error", err)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
