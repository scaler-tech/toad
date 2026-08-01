// Package ticket is the single author of every toad-filed Linear ticket. It
// decides whether a Findings verdict should be auto-filed or proposed to a
// human, composes the ticket body, and de-duplicates against previously
// filed tickets via the ticket index (re-observing an existing ticket posts
// a comment instead of creating a duplicate).
package ticket

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/state"
)

// Decision is the outcome of gating a Findings verdict: file it
// automatically, or propose it to a human via a Slack CTA.
type Decision int

const (
	// DecisionPropose surfaces the finding to a human (Slack CTA) rather
	// than filing a ticket outright.
	DecisionPropose Decision = iota
	// DecisionAutoFile files the ticket immediately, no human in the loop.
	DecisionAutoFile
)

// Source identifies what triggered a ticket to be filed or updated.
type Source string

const (
	SourceAuto       Source = "auto"
	SourceCTA        Source = "cta"
	SourceDigest     Source = "digest"
	SourceEscalation Source = "escalation"
)

// Store is the persistence dependency for the ticket index, implemented by
// *state.DB. It maps an external event key (a Sentry issue, or a Slack
// thread) to the tracking ticket filed for it.
type Store interface {
	UpsertTicketIndex(e *state.TicketIndexEntry) error
	GetTicketIndex(externalKey string) (*state.TicketIndexEntry, error)
}

// Engine composes, gates, and files tickets against an issue tracker,
// de-duplicating via a Store-backed index.
type Engine struct {
	tracker   issuetracker.Tracker
	store     Store
	cfg       config.TicketConfig
	permalink func(channel, ts string) (string, error)
}

// New constructs an Engine. permalink resolves a Slack message to a
// permanent URL for inclusion in ticket bodies and comments; it may be nil,
// in which case permalinks are simply omitted.
func New(tr issuetracker.Tracker, store Store, cfg config.TicketConfig,
	permalink func(channel, ts string) (string, error)) *Engine {
	return &Engine{tracker: tr, store: store, cfg: cfg, permalink: permalink}
}

// Decide reports whether a Findings verdict should be filed automatically
// or proposed to a human. Auto-filing requires the auto-file flag, at least
// one corroborating Sentry issue, confidence clearing the configured floor,
// and a feasible verdict — anything short of all four is proposed instead.
func (e *Engine) Decide(f investigation.Findings) Decision {
	if e.cfg.AutoFile && len(f.SentryIssueIDs) > 0 &&
		f.Confidence >= e.cfg.AutoFileConfidence && f.Feasible {
		return DecisionAutoFile
	}
	return DecisionPropose
}

// ExternalKey derives the de-duplication key for a Findings verdict: the
// first corroborating Sentry issue when present (the strongest, most
// stable identity for a recurring problem), otherwise the Slack thread it
// was raised in.
func ExternalKey(f investigation.Findings, channel, threadTS string) string {
	if len(f.SentryIssueIDs) > 0 {
		return "sentry:" + f.SentryIssueIDs[0]
	}
	return "thread:" + channel + ":" + threadTS
}

// FileResult is the outcome of FileOrUpdate.
type FileResult struct {
	Ref            *issuetracker.IssueRef
	AlreadyExisted bool
}

// FileOrUpdate files a new ticket for f, or — if a ticket was already filed
// for this external key — posts a re-observation comment on the existing
// ticket instead of creating a duplicate.
func (e *Engine) FileOrUpdate(ctx context.Context, f investigation.Findings,
	channel, threadTS, investigationID string, src Source) (*FileResult, error) {
	key := ExternalKey(f, channel, threadTS)
	permalink := e.resolvePermalink(channel, threadTS)

	existing, err := e.store.GetTicketIndex(key)
	if err != nil {
		return nil, fmt.Errorf("looking up ticket index for %s: %w", key, err)
	}
	if existing != nil {
		return e.reobserve(ctx, existing, f, key, permalink, src)
	}
	return e.file(ctx, f, key, investigationID, permalink, src)
}

// reobserve handles the hit path: an existing ticket already covers this
// external key, so toad posts a comment noting it recurred rather than
// filing a duplicate. LastStatus and InvestigationID are left at their zero
// value in the upsert — Store's COALESCE guard preserves whatever is
// already stored for those fields; only identity and last-seen are meant to
// move here.
func (e *Engine) reobserve(ctx context.Context, existing *state.TicketIndexEntry,
	f investigation.Findings, key, permalink string, src Source) (*FileResult, error) {
	ref := &issuetracker.IssueRef{
		Provider: "linear",
		ID:       existing.IssueID,
		URL:      existing.IssueURL,
	}

	if err := e.tracker.PostComment(ctx, ref, reobserveComment(f, permalink)); err != nil {
		return nil, fmt.Errorf("posting re-observation comment on %s: %w", existing.IssueID, err)
	}

	if err := e.store.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey: key,
		IssueID:     existing.IssueID,
		IssueURL:    existing.IssueURL,
		Source:      string(src),
		CreatedAt:   existing.CreatedAt,
		LastSeenAt:  time.Now(),
	}); err != nil {
		return nil, fmt.Errorf("bumping ticket index for %s: %w", key, err)
	}

	return &FileResult{Ref: ref, AlreadyExisted: true}, nil
}

// file handles the miss path: no ticket has been filed for this external
// key yet, so toad creates one and records it in the index.
func (e *Engine) file(ctx context.Context, f investigation.Findings, key, investigationID,
	permalink string, src Source) (*FileResult, error) {
	title := f.Title
	if strings.TrimSpace(title) == "" {
		title = firstSentence(f.Problem)
	}

	// Category drives Linear label mapping (linear.go only maps "bug" and
	// "feature"). Findings doesn't carry the triage category, so this uses
	// Sentry corroboration as a proxy: a Sentry-backed finding is treated as
	// a bug for labeling purposes, everything else gets no category label.
	// Revisit if label fidelity matters more than this task's scope.
	category := ""
	if len(f.SentryIssueIDs) > 0 {
		category = "bug"
	}

	ref, err := e.tracker.CreateIssue(ctx, issuetracker.CreateIssueOpts{
		Title:       title,
		Description: ComposeBody(f, permalink, investigationID),
		Category:    category,
		StateID:     e.cfg.TriageStateID,
	})
	if err != nil {
		return nil, fmt.Errorf("creating issue for %s: %w", key, err)
	}

	now := time.Now()
	if err := e.store.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey:     key,
		IssueID:         ref.ID,
		IssueURL:        ref.URL,
		Source:          string(src),
		InvestigationID: investigationID,
		CreatedAt:       now,
		LastSeenAt:      now,
	}); err != nil {
		return nil, fmt.Errorf("indexing new ticket %s: %w", ref.ID, err)
	}

	return &FileResult{Ref: ref, AlreadyExisted: false}, nil
}

// resolvePermalink resolves a Slack permalink, treating any failure (or a
// nil permalink func) as "no permalink available" rather than failing the
// filing flow — the ticket is still worth filing without it.
func (e *Engine) resolvePermalink(channel, threadTS string) string {
	if e.permalink == nil {
		return ""
	}
	link, err := e.permalink(channel, threadTS)
	if err != nil {
		return ""
	}
	return link
}

// reobserveComment renders the comment posted when an existing ticket is
// re-observed: a fixed header, the fresh investigation's reasoning, and the
// Slack permalink when available.
func reobserveComment(f investigation.Findings, permalink string) string {
	var b strings.Builder
	b.WriteString("**Toad re-observed this issue**")
	if strings.TrimSpace(f.Reasoning) != "" {
		b.WriteString("\n\n")
		b.WriteString(f.Reasoning)
	}
	if permalink != "" {
		b.WriteString("\n\n")
		b.WriteString(permalink)
	}
	return b.String()
}

// firstSentence derives a title from the first sentence of a problem
// statement, used when Findings.Title is empty.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Untitled issue"
	}
	if idx := strings.IndexAny(s, ".!?"); idx >= 0 {
		return strings.TrimSpace(s[:idx+1])
	}
	return s
}
