package linearagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/state"
)

// ActivityPoster is the slice of Client the processor needs.
type ActivityPoster interface {
	CreateActivity(ctx context.Context, sessionID, activityType, body string) error
}

// ProcessorOpts wires the processor to the daemon (callback style, like
// digest.EngineOpts).
type ProcessorOpts struct {
	Poster      ActivityPoster
	DB          *state.DB
	Claim       func(key, scope string) bool
	Unclaim     func(key, scope string)
	Investigate func(ctx context.Context, w Work) (*investigation.Findings, error)
	Timeout     time.Duration
}

// Processor answers one session's unhandled work: ack, claim, investigate
// (or reuse), respond. Sessions never file tickets and never mutate issues.
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

	findings, err := p.findFindings(ctx, w)
	if err != nil {
		// The investigation may have failed because ctx's timeout expired —
		// post the error on a fresh short context so the post itself doesn't
		// silently fail on a dead context, and record the session as handled
		// so a failing investigation is answered (with an error) at most
		// once per user prompt; the human's next prompt retriggers.
		postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if postErr := p.opts.Poster.CreateActivity(postCtx, w.Session.ID, "error", "The investigation failed: "+firstLine(err.Error())); postErr != nil {
			slog.Warn("posting session activity", "session", w.Session.ID, "type", "error", "error", postErr)
			return // error post failed -> handled record unwritten, retried next poll
		}
		p.recordHandled(w)
		return
	}

	if err := p.opts.Poster.CreateActivity(ctx, w.Session.ID, "response", composeResponse(findings)); err != nil {
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

// findFindings returns stored findings when they are fresh and the ask is
// not a follow-up; otherwise it runs a new investigation and persists it.
func (p *Processor) findFindings(ctx context.Context, w Work) (*investigation.Findings, error) {
	// Same-trigger retry: a prior Handle() for THIS exact Work (same
	// session, same w.TriggeredAt) already investigated and persisted
	// findings under the deterministic ID investigationID(w), but failed to
	// post the response (e.g. Linear API hiccup) before the handled record
	// was written, so the poller re-detected identical Work. Reuse rather
	// than re-investigate.
	//
	// This is intentionally an ID match, not a CreatedAt-vs-TriggeredAt time
	// comparison: a follow-up prompt sent WHILE a prior investigation for
	// this session is still running has a TriggeredAt that is *earlier* than
	// the prior investigation's completion-time CreatedAt, so a time-based
	// "was this produced after the prompt arrived" check would wrongly
	// reuse stale pre-follow-up findings. Deriving the record's ID from
	// w.TriggeredAt instead means a genuine follow-up (new TriggeredAt)
	// always misses this lookup and falls through to re-investigate,
	// regardless of how the two calls' wall-clock times relate.
	wantID := investigationID(w)
	if rec, err := p.opts.DB.GetInvestigationByThread("linear-session:" + w.Session.ID); err == nil && rec != nil {
		if rec.ID == wantID {
			var f investigation.Findings
			if err := json.Unmarshal([]byte(rec.FindingsJSON), &f); err == nil && f.Feasible {
				slog.Info("linear session reusing same-trigger findings", "session", w.Session.ID, "investigation", rec.ID)
				return &f, nil
			}
		}
	}

	if !w.FollowUp && w.Session.IssueIdentifier != "" {
		if rec, err := p.opts.DB.FindInvestigationByTicket(w.Session.IssueIdentifier); err == nil && rec != nil {
			if rec.CreatedAt.After(w.TriggeredAt.Add(-24 * time.Hour)) {
				var f investigation.Findings
				if err := json.Unmarshal([]byte(rec.FindingsJSON), &f); err == nil && f.Feasible {
					slog.Info("linear session reusing stored findings", "session", w.Session.ID, "investigation", rec.ID)
					return &f, nil
				}
			}
		}
	}

	f, err := p.opts.Investigate(ctx, w)
	if err != nil {
		return nil, err
	}
	fj, _ := json.Marshal(f)
	rec := &state.InvestigationRecord{
		ID:           investigationID(w),
		ThreadTS:     "linear-session:" + w.Session.ID,
		Channel:      "linear",
		Repo:         f.Repo,
		FindingsJSON: string(fj),
		CreatedAt:    time.Now().UTC(),
	}
	if err := p.opts.DB.SaveInvestigation(rec); err != nil {
		slog.Warn("persisting session investigation", "session", w.Session.ID, "error", err)
	}
	return f, nil
}

func (p *Processor) post(ctx context.Context, sessionID, activityType, body string) {
	if err := p.opts.Poster.CreateActivity(ctx, sessionID, activityType, body); err != nil {
		slog.Warn("posting session activity", "session", sessionID, "type", activityType, "error", err)
	}
}

// composeResponse renders findings as a session response: the reasoning
// prose first (it already follows the STE style rules — the investigation
// prompt injects them), then evidence references.
func composeResponse(f *investigation.Findings) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(f.Reasoning))
	if len(f.Evidence) > 0 {
		b.WriteString("\n\nEvidence:\n")
		for _, e := range f.Evidence {
			if e.Note != "" {
				fmt.Fprintf(&b, "- `%s` — %s\n", e.Ref, e.Note)
			} else {
				fmt.Fprintf(&b, "- `%s`\n", e.Ref)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
