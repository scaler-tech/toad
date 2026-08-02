// ticketflow.go implements the v2 investigate-and-file flow: the bug/feature
// branch of handleTriggered, the :ticket:/:frog: CTA handler, and the shared
// helpers both (and the escalation branch) build on.
//
// Design note: the decision logic in this file (idempotency pre-check,
// running an investigation, gating via the ticket engine, composing reply
// text) deliberately never touches *islack.Client directly. cmd/handlers.go
// and handleTicketRequest below are the only places that call SetStatus /
// ClearStatus / ReplyInThread — thin, untested glue, consistent with Task
// 6's precedent of not adding Slack test-double scaffolding for one-line
// status calls. Keeping the decision functions Slack-client-free is what
// lets ticketflow_test.go exercise them directly with a fake tracker and an
// in-memory state DB.
package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/digest"
	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	islack "github.com/scaler-tech/toad/internal/slack"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/ticket"
	"github.com/scaler-tech/toad/internal/triage"
)

// staleFlagKey is the context key wrapSync (root.go) uses to report a repo
// sync failure back to the specific investigation call that triggered it.
// A context value (rather than a field on investigation.Runner) keeps this
// goroutine-local: concurrent investigations never share the flag, even
// though they share one Runner and one wrapped RepoSyncer.
type staleFlagKey struct{}

// withStaleTracking returns a context carrying a fresh staleness flag for a
// single investigation call, and a pointer to read it afterward.
func withStaleTracking(ctx context.Context) (context.Context, *bool) {
	stale := new(bool)
	return context.WithValue(ctx, staleFlagKey{}, stale), stale
}

// staleCaveat is appended to Findings.Reasoning when the repo sync failed
// before an investigation ran against it (Task 9's carried finding:
// investigation.Runner itself only slog.Warns on a sync failure and
// otherwise proceeds silently against a possibly-stale checkout — this is
// the user-visible caveat that was missing).
const staleCaveat = "\n\n_note: repo sync failed before this investigation; line references may be stale_"

// runInvestigation gates an investigation run behind investigateSem (bounded
// concurrent investigations — separate from ribbitSem since investigations
// are slow, minutes not seconds) and appends a staleness caveat to the
// findings when the repo sync failed beforehand.
func runInvestigation(ctx context.Context, investRunner *investigation.Runner, investigateSem chan struct{},
	req investigation.Request) (*investigation.Findings, error) {
	select {
	case investigateSem <- struct{}{}:
		defer func() { <-investigateSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	staleCtx, stale := withStaleTracking(ctx)
	findings, err := investRunner.Run(staleCtx, req)
	if err != nil {
		return nil, err
	}
	if *stale {
		findings.Reasoning = strings.TrimRight(findings.Reasoning, "\n") + staleCaveat
	}
	return findings, nil
}

// randomHex mirrors the v1 tadpole runner's ID-suffix generator (deleted in
// Task 6 along with internal/tadpole) — kept as the same small helper here
// since investigation record IDs follow the same "<prefix>-<ms>-<hex>" shape.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// generateInvestigationID builds an "invest-<ms>-<hex>" ID for a new
// InvestigationRecord. A randomHex failure (practically never, short of an
// exhausted entropy source) falls back to a nanotime-derived suffix rather
// than failing the whole investigation flow over a non-critical display ID.
func generateInvestigationID() string {
	suffix, err := randomHex(4)
	if err != nil {
		suffix = fmt.Sprintf("%x", time.Now().UnixNano())[:8]
	}
	return fmt.Sprintf("invest-%d-%s", time.Now().UnixMilli(), suffix)
}

// buildTicketContextBlock formats any issue-tracker references found in text
// or threadContext into the "<linked_tickets>" block investigation.Request's
// TicketContext field expects (internal/investigation/prompt.go writes it
// into the prompt verbatim — wrapping it is this function's job, mirroring
// the deleted v1 cmd/investigation.go's formatTicketContext). Reuses the
// same extraction as enrichWithIssueDetails (cmd/helpers.go) but renders a
// separate tagged block instead of appending plain entries to a context
// slice, since investigation.Request keeps ticket context distinct from raw
// thread conversation. Returns "" when no issue references are found.
func buildTicketContextBlock(ctx context.Context, tracker issuetracker.Tracker, text string, threadContext []string) string {
	allText := text
	for _, tc := range threadContext {
		allText += "\n" + tc
	}
	refs := tracker.ExtractAllIssueRefs(allText)
	if len(refs) == 0 {
		return ""
	}

	limit := 3
	if len(refs) < limit {
		limit = len(refs)
	}

	var b strings.Builder
	b.WriteString("<linked_tickets>\n")
	found := false
	for _, ref := range refs[:limit] {
		details, err := tracker.GetIssueDetails(ctx, ref)
		if err != nil {
			slog.Warn("failed to fetch issue details for ticket context", "issue", ref.ID, "error", err)
			continue
		}
		if details == nil {
			continue
		}
		found = true
		fmt.Fprintf(&b, "## %s\n", details.ID)
		if details.Title != "" {
			fmt.Fprintf(&b, "Title: %s\n", details.Title)
		}
		if details.Description != "" {
			desc := details.Description
			if len(desc) > 2000 {
				desc = desc[:2000] + "..."
			}
			fmt.Fprintf(&b, "Description:\n%s\n", desc)
		}
		b.WriteString("\n")
	}
	if !found {
		return ""
	}
	b.WriteString("</linked_tickets>\n")
	return b.String()
}

// preInvestigationTicketCheck looks up the ticket index for the external key
// this report would map to — using only intake-time Sentry refs, before any
// investigation runs. A hit means a ticket already tracks this exact
// problem, so a fresh (expensive) investigation would be redundant: this
// posts a re-observation comment on the existing ticket and returns its
// index entry so the caller can reply with the link and skip investigating
// entirely.
//
// A GetTicketIndex error (a DB hiccup, as opposed to a clean miss which
// returns nil, nil) does NOT block the caller from proceeding to a fresh
// investigation — it's logged clearly and treated as "no known
// corroboration". This is the mitigation for the residual partial-failure
// window carried from Task 12's review (CreateIssue can succeed while the
// index upsert fails): the worst case here is a possible duplicate ticket,
// never a silently dropped report.
func preInvestigationTicketCheck(ctx context.Context, db *state.DB, tracker issuetracker.Tracker,
	sentryRefs []string, channel, threadTS string) *state.TicketIndexEntry {
	if db == nil {
		return nil
	}

	key := ticket.ExternalKey(investigation.Findings{SentryIssueIDs: sentryRefs}, channel, threadTS)
	existing, err := db.GetTicketIndex(key)
	if err != nil {
		slog.Warn("ticket index pre-check failed, proceeding with investigation without corroboration of index state",
			"key", key, "error", err)
		return nil
	}
	if existing == nil {
		return nil
	}

	ref := &issuetracker.IssueRef{Provider: "linear", ID: existing.IssueID, URL: existing.IssueURL}
	comment := fmt.Sprintf("**Toad re-observed this issue**\n\nAnother report came in on this thread before a fresh investigation ran — see %s.", existing.IssueID)
	if err := tracker.PostComment(ctx, ref, comment); err != nil {
		slog.Warn("failed to post re-observation comment during idempotency pre-check", "issue", existing.IssueID, "error", err)
	}
	return existing
}

// fileOrProposeFromFindings applies the ticket gate (Decide) to f and, on
// DecisionAutoFile, files or re-observes the ticket via ticketEngine. It
// performs NO thread-claim bookkeeping of its own — every caller owns claim
// lifecycle end to end.
//
// Contract for future callers (e.g. Task 16's digest-path proposeFromDigest,
// which is expected to build on this helper): by the time this function
// returns, the ticket-filing side effect (if any) has already happened —
// a caller holding a scoped claim across the investigation MUST release it
// once this returns, on BOTH the DecisionAutoFile and DecisionPropose
// outcomes. This differs from digest.go's legacy Propose wiring, which only
// unclaimed on failure and left every successful proposal holding its claim
// forever (a carried finding from Task 6's review) — do not repeat that
// pattern here. The Slack-thread flow in this file (runTriggeredInvestigation)
// follows this contract via an unconditional defer around its whole body.
func fileOrProposeFromFindings(ctx context.Context, ticketEngine *ticket.Engine, f investigation.Findings,
	channel, threadTS, investigationID string, src ticket.Source) (ticket.Decision, *ticket.FileResult, error) {
	decision := ticketEngine.Decide(f)
	if decision != ticket.DecisionAutoFile {
		return decision, nil, nil
	}
	res, err := ticketEngine.FileOrUpdate(ctx, f, channel, threadTS, investigationID, src)
	return decision, res, err
}

// composeFiledReply renders the Slack reply text for a successful
// ticketEngine.FileOrUpdate call — shared by the triggered auto-file path
// and the CTA/escalation direct-file path so both produce identical wording
// for the same outcome. FileResult.AlreadyExisted distinguishes a brand new
// filing from a re-observation of a ticket that already tracked this exact
// problem — wording it as "Filed" in the latter case would be misleading
// (review finding: this must say linked-existing, not "Filed").
func composeFiledReply(findings investigation.Findings, fileResult *ticket.FileResult) string {
	title := findings.Title
	url := ""
	if fileResult != nil && fileResult.Ref != nil {
		if fileResult.Ref.Title != "" {
			title = fileResult.Ref.Title
		}
		url = fileResult.Ref.URL
	}
	if title == "" {
		title = "(untitled)"
	}
	if fileResult != nil && fileResult.AlreadyExisted {
		return fmt.Sprintf(":ticket: Already tracked as %s — *%s*\n\n%s", url, title, findings.Reasoning)
	}
	return fmt.Sprintf(":ticket: Filed %s — *%s*\n\n%s", url, title, findings.Reasoning)
}

// triggeredOutcomeKind enumerates what the bug/feature investigate-and-file
// branch of handleTriggered decided to do.
type triggeredOutcomeKind int

const (
	// outcomeFallThrough means the report was found infeasible (or the
	// investigation itself errored) — the caller should ignore ReplyText
	// and continue into the unchanged v1 ribbit path below.
	outcomeFallThrough triggeredOutcomeKind = iota
	// outcomeIdempotentHit means the pre-investigation ticket-index check
	// found an existing ticket for this report; no investigation ran.
	outcomeIdempotentHit
	// outcomeFiled means the ticket engine auto-filed (or re-observed) a
	// ticket for a fresh investigation's findings.
	outcomeFiled
	// outcomeProposed means the finding was gated to a human via Slack
	// (send ReplyText with TicketBlocks, not a plain reply).
	outcomeProposed
	// outcomeFilingFailed means investigation succeeded but FileOrUpdate
	// itself errored.
	outcomeFilingFailed
)

// triggeredOutcome is what runTriggeredInvestigation decided to do,
// expressed as plain data with no Slack dependency so it's unit-testable —
// see the package doc comment at the top of this file.
type triggeredOutcome struct {
	Kind      triggeredOutcomeKind
	ReplyText string
	Findings  *investigation.Findings
}

// runTriggeredInvestigation is the bug/feature branch of handleTriggered. It
// assumes the caller has already claimed the thread (stateManager.Claim) and
// unconditionally releases that claim via defer before returning, regardless
// of outcome — the "airtight" claim handling called for by Task 6's review
// (claim before investigate, unclaim on every non-filed exit, claim released
// after ticket filed/proposed): since nothing here creates a long-lived Run
// to hand the claim off to, every exit path releases it.
//
// sentryCorroborated must be true only when this report arrived from an
// allowlisted monitoring bot (see the caller's computation of the flag) —
// NEVER when a Sentry reference merely appears in human-pasted text or in
// the investigation agent's own output. Without this gate, any channel user
// could paste (or guess) a Sentry issue key to: (1) use
// preInvestigationTicketCheck as a ticket-existence oracle — a
// differently-worded reply reveals whether that key is already tracked —
// and spam re-observation comments onto an arbitrary existing ticket, or
// (2) get a fabricated/model-inferred Sentry ID auto-filed and dedup-keyed
// as if it were real external corroboration. When false, this both skips
// the sentry-key path in preInvestigationTicketCheck entirely (passing nil
// refs — the thread-keyed fallback in ExternalKey still applies) and clears
// the investigation's own Findings.SentryIssueIDs before Decide/
// ExternalKey/FileOrUpdate/the ticket footer ever see them. The refs may
// still flow into the investigation PROMPT as leads (req.SentryRefs below)
// and the model may cite them in its reasoning text — that's fine, they
// just must never drive automation or identity keys.
func runTriggeredInvestigation(
	ctx context.Context,
	msg *islack.IncomingMessage,
	result *triage.Result,
	channelName, threadTS string,
	stateManager *state.Manager,
	tracker issuetracker.Tracker,
	resolver *config.Resolver,
	investRunner *investigation.Runner,
	ticketEngine *ticket.Engine,
	investigateSem chan struct{},
	sentryCorroborated bool,
) triggeredOutcome {
	defer stateManager.Unclaim(threadTS)

	db := stateManager.DB()

	sentryRefsForCheck := msg.SentryRefs
	if !sentryCorroborated {
		sentryRefsForCheck = nil
	}

	// Idempotency pre-check: skip a fresh, expensive investigation entirely
	// when a ticket already tracks this exact problem.
	if existing := preInvestigationTicketCheck(ctx, db, tracker, sentryRefsForCheck, msg.Channel, threadTS); existing != nil {
		return triggeredOutcome{
			Kind:      outcomeIdempotentHit,
			ReplyText: fmt.Sprintf(":ticket: Already tracked as %s: %s", existing.IssueID, existing.IssueURL),
		}
	}

	repo := resolver.Resolve(result.Repo, result.FilesHint)
	if repo == nil {
		slog.Info("could not resolve a repo for investigation, falling through to ribbit", "summary", result.Summary)
		return triggeredOutcome{Kind: outcomeFallThrough}
	}

	req := investigation.Request{
		Text:          msg.Text,
		ThreadContext: msg.ThreadContext,
		Category:      result.Category,
		Confidence:    result.Confidence,
		Summary:       result.Summary,
		ChannelName:   channelName,
		Keywords:      result.Keywords,
		FilesHint:     result.FilesHint,
		SentryRefs:    msg.SentryRefs,
		TicketContext: buildTicketContextBlock(ctx, tracker, msg.Text, msg.ThreadContext),
		Repo:          repo,
		Timeout:       4 * time.Minute,
	}

	findings, err := runInvestigation(ctx, investRunner, investigateSem, req)
	if err != nil {
		slog.Warn("triggered investigation failed, falling through to ribbit", "error", err, "summary", result.Summary)
		return triggeredOutcome{Kind: outcomeFallThrough}
	}
	if !sentryCorroborated {
		// Strip any Sentry IDs the model surfaced (from req.SentryRefs leads,
		// or inferred from thread text) before they can drive Decide,
		// ExternalKey, FileOrUpdate, or the ticket footer — see this
		// function's doc comment.
		findings.SentryIssueIDs = nil
	}

	recordID := generateInvestigationID()
	saveInvestigationRecord(db, recordID, threadTS, msg.Channel, findings)

	if !findings.Feasible {
		slog.Info("investigation says not feasible, falling through to ribbit",
			"reasoning", findings.Reasoning, "summary", result.Summary)
		return triggeredOutcome{Kind: outcomeFallThrough, Findings: findings}
	}

	decision, fileResult, fileErr := fileOrProposeFromFindings(ctx, ticketEngine, *findings, msg.Channel, threadTS, recordID, ticket.SourceAuto)
	if fileErr != nil {
		slog.Error("ticket filing failed", "error", fileErr, "summary", result.Summary)
		return triggeredOutcome{
			Kind:      outcomeFilingFailed,
			ReplyText: fmt.Sprintf(":x: I found a fix but couldn't file a ticket: %s\n\n%s", fileErr.Error(), findings.Reasoning),
			Findings:  findings,
		}
	}

	if decision == ticket.DecisionAutoFile {
		return triggeredOutcome{
			Kind:      outcomeFiled,
			ReplyText: composeFiledReply(*findings, fileResult),
			Findings:  findings,
		}
	}
	return triggeredOutcome{
		Kind:      outcomeProposed,
		ReplyText: findings.Reasoning,
		Findings:  findings,
	}
}

// saveInvestigationRecord persists findings for later reuse (the CTA/
// escalation flow's <24h reuse window in runTicketRequest). Errors are
// logged, never fatal to the calling flow — a failed save just means a
// later CTA click re-investigates instead of reusing.
func saveInvestigationRecord(db *state.DB, id, threadTS, channel string, findings *investigation.Findings) {
	if db == nil {
		return
	}
	fjson, err := json.Marshal(findings)
	if err != nil {
		slog.Warn("failed to marshal findings for investigation record", "error", err)
		return
	}
	if err := db.SaveInvestigation(&state.InvestigationRecord{
		ID:           id,
		ThreadTS:     threadTS,
		Channel:      channel,
		Repo:         findings.Repo,
		FindingsJSON: string(fjson),
		CreatedAt:    time.Now(),
	}); err != nil {
		slog.Warn("failed to save investigation record", "error", err, "id", id)
	}
}

// investigationReuseWindow bounds how old a saved investigation can be and
// still be reused by the CTA/escalation flow instead of re-investigating.
const investigationReuseWindow = 24 * time.Hour

// ticketRequestOutcome is what runTicketRequest decided — see triggeredOutcome's
// doc comment for why this stays Slack-client-free.
type ticketRequestOutcome struct {
	// Conflict is true when the thread was already claimed (a concurrent
	// escalation/CTA/investigation in flight) — the caller should reply
	// with a conflict message and do nothing else; ReplyText/Err are unset.
	Conflict  bool
	ReplyText string
	Err       error
}

// runTicketRequest is the shared core of the CTA (:frog: reaction / ticket
// button) and triage-escalation entry points: claim the thread, reuse a
// recent investigation for it if one exists, otherwise run a fresh one, and
// file it directly.
//
// This deliberately does NOT gate through ticketEngine.Decide — Decide is
// the AUTO-file confidence/corroboration gate for *unattended* filing
// (runTriggeredInvestigation); an explicit human request (a CTA click, or
// triage's Escalate flag) already IS the sign-off. Routing this path through
// Decide as well made the CTA button a permanent no-op for any non-Sentry-
// corroborated or lower-confidence finding: Decide is pure over the same
// reused Findings, so a click always re-produced DecisionPropose — the very
// state the button was meant to resolve (review Critical finding). Calling
// FileOrUpdate directly here — for both a fresh finding and a reused one —
// is what actually files (or, if a ticket already exists for the derived
// external key, re-observes) it.
//
// Claim/Unclaim bookkeeping mirrors runTriggeredInvestigation: a claim
// conflict short-circuits before any work (Conflict: true, no investigation
// run), and a successful claim is released via defer regardless of outcome.
//
// sentryCorroborated follows the same rule as runTriggeredInvestigation's
// (see its doc comment): true only when this request arrived from an
// allowlisted monitoring bot, never from a human click/reaction or
// human-pasted text. It gates a FRESH investigation's Findings.SentryIssueIDs
// the same way — this path bypasses Decide entirely (an explicit human
// request already IS the sign-off, so auto-file's confidence/corroboration
// gate doesn't apply), but FileOrUpdate's ExternalKey/footer must still never
// trust an unverified Sentry ID as the ticket's identity key. A REUSED saved
// finding is left untouched here regardless of the current request's
// corroboration — it was already gated correctly at the point it was
// produced (by runTriggeredInvestigation or a prior investigateFromDigest),
// and reusing it doesn't rerun that judgment.
func runTicketRequest(
	ctx context.Context,
	msg *islack.IncomingMessage,
	result *triage.Result, // nil for a bare CTA click with no fresh triage
	stateManager *state.Manager,
	tracker issuetracker.Tracker,
	resolver *config.Resolver,
	investRunner *investigation.Runner,
	ticketEngine *ticket.Engine,
	investigateSem chan struct{},
	channelName, threadTS string,
	src ticket.Source,
	sentryCorroborated bool,
) ticketRequestOutcome {
	if !stateManager.Claim(threadTS) {
		return ticketRequestOutcome{Conflict: true}
	}
	defer stateManager.Unclaim(threadTS)

	db := stateManager.DB()

	findings, recordID := reuseRecentInvestigation(db, threadTS)
	if findings == nil {
		repoHint, filesHint := "", []string(nil)
		category, confidence, summary, keywords := "", 0.0, "", []string(nil)
		if result != nil {
			repoHint, filesHint = result.Repo, result.FilesHint
			category, confidence, summary, keywords = result.Category, result.Confidence, result.Summary, result.Keywords
		}

		repo := resolver.Resolve(repoHint, filesHint)
		if repo == nil {
			return ticketRequestOutcome{Err: errors.New("could not resolve a repo for this thread")}
		}

		req := investigation.Request{
			Text:          msg.Text,
			ThreadContext: msg.ThreadContext,
			Category:      category,
			Confidence:    confidence,
			Summary:       summary,
			ChannelName:   channelName,
			Keywords:      keywords,
			FilesHint:     filesHint,
			SentryRefs:    msg.SentryRefs,
			TicketContext: buildTicketContextBlock(ctx, tracker, msg.Text, msg.ThreadContext),
			Repo:          repo,
			Timeout:       4 * time.Minute,
		}

		f, err := runInvestigation(ctx, investRunner, investigateSem, req)
		if err != nil {
			return ticketRequestOutcome{Err: fmt.Errorf("investigation failed: %w", err)}
		}
		findings = f
		recordID = generateInvestigationID()
		saveInvestigationRecord(db, recordID, threadTS, msg.Channel, findings)
	}

	// Re-apply the corroboration clear here, unconditionally, on the FINAL
	// findings value — whether it just came from a fresh investigation above
	// or was reused from a saved record. A reused record may predate
	// corroboration filtering, or may have been saved by a different path
	// with different (or missing) gating at save time — e.g. investigateFromDigest,
	// whose saveInvestigationRecord call has no reliable relationship to
	// THIS request's sentryCorroborated value. Never trust a persisted
	// record's SentryIssueIDs as already-clean; always re-derive from the
	// current request's own corroboration.
	if !sentryCorroborated {
		findings.SentryIssueIDs = nil
	}

	fileResult, err := ticketEngine.FileOrUpdate(ctx, *findings, msg.Channel, threadTS, recordID, src)
	if err != nil {
		return ticketRequestOutcome{Err: fmt.Errorf("filing ticket: %w", err)}
	}
	return ticketRequestOutcome{ReplyText: composeFiledReply(*findings, fileResult)}
}

// reuseRecentInvestigation returns a previously-saved investigation's
// findings and ID when one exists for threadTS, is younger than
// investigationReuseWindow, AND was feasible. Returns nil, "" (never an
// error) on any miss — a DB error, no saved investigation, an unparseable
// record, one that's gone stale, or one that was infeasible all just mean
// "run a fresh investigation instead", logged for visibility but never
// blocking the caller.
//
// The infeasible check matters: saveInvestigationRecord persists a finding
// regardless of Feasible (runTriggeredInvestigation saves before checking
// it, for audit visibility), so an infeasible record can be sitting in the
// DB when a human clicks the CTA button afterward. Reusing it as-is would
// file a ticket from a finding that explicitly said "no real fix found here"
// — this forces a fresh investigation instead (review Critical finding).
func reuseRecentInvestigation(db *state.DB, threadTS string) (*investigation.Findings, string) {
	if db == nil {
		return nil, ""
	}
	rec, err := db.GetInvestigationByThread(threadTS)
	if err != nil {
		slog.Warn("failed to look up existing investigation, running a fresh one", "error", err, "thread", threadTS)
		return nil, ""
	}
	if rec == nil || time.Since(rec.CreatedAt) >= investigationReuseWindow {
		return nil, ""
	}
	var f investigation.Findings
	if err := json.Unmarshal([]byte(rec.FindingsJSON), &f); err != nil {
		slog.Warn("failed to parse saved investigation, running a fresh one", "error", err, "thread", threadTS)
		return nil, ""
	}
	if !f.Feasible {
		slog.Debug("saved investigation was infeasible, running a fresh one instead", "thread", threadTS)
		return nil, ""
	}
	return &f, rec.ID
}

// handleTicketRequest is the :ticket:/:frog: CTA entry point, and — via the
// escalation branch in handleTriggered — the triage Escalate==true entry
// point too. Both funnel into runTicketRequest, distinguished only by src.
func handleTicketRequest(
	ctx context.Context,
	msg *islack.IncomingMessage,
	slackClient *islack.Client,
	result *triage.Result,
	stateManager *state.Manager,
	tracker issuetracker.Tracker,
	resolver *config.Resolver,
	investRunner *investigation.Runner,
	ticketEngine *ticket.Engine,
	investigateSem chan struct{},
	channelName string,
	src ticket.Source,
	sentryCorroborated bool,
) {
	threadTS := msg.ThreadTS()

	// The CTA/reaction path already set a "Spawning tadpole..." (legacy
	// name) thread status before dispatching here (internal/slack/interactive.go).
	// ReplyInThread does not clear it, so it must be cleared before we're
	// done, regardless of which branch below is taken.
	defer slackClient.ClearStatus(msg.Channel, threadTS)
	slackClient.SetStatus(msg.Channel, threadTS, "Filing a ticket...")

	// Reuse already-fetched, already-enriched thread context when the caller
	// already has it (the escalation branch reuses msg from handleTriggered,
	// which already populated AND enriched ThreadContext via
	// enrichWithIssueDetails) — the guard covers both the fetch and the
	// enrich call, so a bare CTA click (no context yet) does both exactly
	// once, and the escalation path does neither again (review minor: this
	// guard previously covered only the fetch, so escalation threads got
	// enriched twice).
	if len(msg.ThreadContext) == 0 {
		if msg.ThreadTimestamp != "" {
			if tc, err := slackClient.FetchThreadMessages(msg.Channel, msg.ThreadTimestamp); err != nil {
				slog.Warn("failed to fetch thread context for ticket request", "error", err, "thread", threadTS)
			} else {
				msg.ThreadContext = tc
			}
		}
		msg.ThreadContext = enrichWithIssueDetails(ctx, tracker, msg.Text, msg.ThreadContext)
	}

	outcome := runTicketRequest(ctx, msg, result, stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, channelName, threadTS, src, sentryCorroborated)
	if outcome.Conflict {
		slackClient.ReplyInThread(msg.Channel, threadTS, ":frog: Already working on this thread")
		return
	}
	if outcome.Err != nil {
		slog.Error("ticket request failed", "error", outcome.Err, "thread", threadTS)
		slackClient.ReplyInThread(msg.Channel, threadTS, ":x: Sorry, I couldn't file a ticket: "+outcome.Err.Error())
		return
	}
	if _, err := slackClient.ReplyInThread(msg.Channel, threadTS, outcome.ReplyText); err != nil {
		slog.Warn("ticket request reply failed", "error", err)
	}
}

// runBotIntake is the pure decision core of the allowlisted-bot intake path
// (controller ruling from Task 15's review: bot messages feed intake, never
// conversation). It triages the message directly — no ribbitSem, no
// "ask for clarification" reply, no ribbit fallback — and proceeds into the
// same investigate-and-file flow as handleTriggered's bug/feature branch
// (bounded by investigateSem as always) ONLY for an actionable bug/feature.
// Everything else (question/other, non-actionable, low confidence, a claim
// conflict, an infeasible/errored investigation) is silently dropped: this
// returns nil and the caller posts no reply. Never touches ribbit or
// *islack.Client, so the "drop a question" behavior is directly testable.
func runBotIntake(
	ctx context.Context,
	msg *islack.IncomingMessage,
	triageEngine *triage.Engine,
	channelName string,
	stateManager *state.Manager,
	tracker issuetracker.Tracker,
	resolver *config.Resolver,
	investRunner *investigation.Runner,
	ticketEngine *ticket.Engine,
	investigateSem chan struct{},
	sentryCorroborated bool,
) *triggeredOutcome {
	result, err := triageEngine.Classify(ctx, msg, channelName)
	if err != nil {
		slog.Debug("bot intake: triage failed, dropping", "error", err, "bot_id", msg.BotID)
		return nil
	}

	daemonCounters.triages.Add(1)
	switch result.Category {
	case "bug":
		daemonCounters.triageBug.Add(1)
	case "feature":
		daemonCounters.triageFeature.Add(1)
	case "question":
		daemonCounters.triageQuestion.Add(1)
	default:
		daemonCounters.triageOther.Add(1)
	}

	if !result.Actionable || result.Confidence < 0.5 || (result.Category != "bug" && result.Category != "feature") {
		slog.Debug("bot intake: not an actionable bug/feature, dropping",
			"actionable", result.Actionable, "category", result.Category, "confidence", result.Confidence, "bot_id", msg.BotID)
		return nil
	}

	threadTS := msg.ThreadTS()
	if !stateManager.Claim(threadTS) {
		slog.Debug("bot intake: thread already claimed, dropping", "thread", threadTS, "bot_id", msg.BotID)
		return nil
	}

	slog.Info("bot intake investigating", "summary", result.Summary, "category", result.Category, "bot_id", msg.BotID)
	outcome := runTriggeredInvestigation(ctx, msg, result, channelName, threadTS,
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, sentryCorroborated)

	if outcome.Kind == outcomeFallThrough {
		slog.Debug("bot intake: investigation fell through (infeasible or errored), dropping", "bot_id", msg.BotID)
		return nil
	}
	return &outcome
}

// handleBotIntake is the thin Slack-touching wrapper around runBotIntake:
// it fetches thread context (for investigation quality) and posts a reply
// only when runBotIntake actually decided to file or propose a ticket.
func handleBotIntake(
	ctx context.Context,
	msg *islack.IncomingMessage,
	triageEngine *triage.Engine,
	slackClient *islack.Client,
	stateManager *state.Manager,
	channelName string,
	tracker issuetracker.Tracker,
	resolver *config.Resolver,
	investRunner *investigation.Runner,
	ticketEngine *ticket.Engine,
	investigateSem chan struct{},
	sentryCorroborated bool,
) {
	if msg.ThreadTimestamp != "" {
		if tc, err := slackClient.FetchThreadMessages(msg.Channel, msg.ThreadTimestamp); err != nil {
			slog.Debug("bot intake: failed to fetch thread context", "error", err)
		} else {
			msg.ThreadContext = tc
		}
	}
	msg.ThreadContext = enrichWithIssueDetails(ctx, tracker, msg.Text, msg.ThreadContext)

	outcome := runBotIntake(ctx, msg, triageEngine, channelName, stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, sentryCorroborated)
	if outcome == nil {
		return
	}

	threadTS := msg.ThreadTS()
	// The CTA button is suppressed when the tracker can't actually create
	// issues — post plain text instead of a button that always errors when
	// clicked.
	if outcome.Kind == outcomeProposed && ticketEngine.ShouldCreateIssues() {
		blocks := islack.TicketBlocks(outcome.ReplyText, threadTS)
		if _, err := slackClient.ReplyInThreadWithBlocks(msg.Channel, threadTS, outcome.ReplyText, blocks); err != nil {
			slog.Warn("bot intake investigation reply failed", "error", err)
		}
		return
	}
	if _, err := slackClient.ReplyInThread(msg.Channel, threadTS, outcome.ReplyText); err != nil {
		slog.Warn("bot intake investigation reply failed", "error", err)
	}
}

// digestPostFunc posts a Slack thread reply for the digest propose path,
// optionally with Block Kit blocks (nil for a plain reply) attached. Kept as
// an injectable function value — rather than a *islack.Client parameter —
// so proposeFromDigest's Decide/FileOrUpdate/compose decision logic stays
// directly unit-testable with a fake, consistent with this file's
// Slack-client-free design (see the package doc comment at the top). Task
// 15's root.go wires the real implementation over slackClient.ReplyInThread/
// ReplyInThreadWithBlocks.
type digestPostFunc func(channel, threadTS, text string, blocks []slack.Block) (string, error)

// composeDigestProposalText renders the Slack reply text for a digest-
// sourced finding that didn't clear the auto-file gate (ticket.DecisionPropose).
// This replaces the v1 ":crown:" tadpole-spawn announcement — nothing is
// being spawned here, so the copy is rewritten to describe what actually
// happened: Toad King noticed something worth a look while passively
// monitoring, and here's the investigation's reasoning.
func composeDigestProposalText(f investigation.Findings) string {
	body := f.Reasoning
	if strings.TrimSpace(body) == "" {
		body = f.Problem
	}
	return ":crown: Spotted while monitoring — here's what I found:\n\n" + body
}

// proposeFromDigest is digest.ProposeFunc's real implementation (modulo the
// injected digestPostFunc — see its doc comment): it applies the same
// Decide-then-file-or-propose gate the Slack-thread flows use
// (fileOrProposeFromFindings) and posts the corresponding thread reply.
//
// No claim bookkeeping happens here — per fileOrProposeFromFindings's
// contract comment, and per the digest.go fix (Task 16's carried finding 1):
// the scoped claim is released by the caller (processOpportunities /
// ResumeInvestigations in internal/digest/digest.go) on BOTH success and
// failure now, not just failure. This function only needs to return an
// error on failure — digest.go logs it and does the unclaim + failure
// notice.
//
// investigationID is resolved via digestInvestigationID (a best-effort
// lookup of the InvestigationRecord investigateFromDigest saves for this
// same thread) rather than threaded straight through from that call —
// review finding (round 2): a ticket_index row filed from digest previously
// carried no investigation backlink at all (investigationID was
// hard-coded ""), unlike every Slack-thread-originated ticket, silently
// degrading FindInvestigationByTicket/the upcoming MCP investigations tool
// for the digest path. See digestInvestigationID's doc comment for why this
// is a lookup rather than a parameter threaded through digest.ProposeFunc's
// signature.
//
// sentryCorroborated follows the same rule as runTriggeredInvestigation's
// (see its doc comment): true only when msg.BotID is an allowlisted
// monitoring bot, computed by the caller (root.go, from cfg.Intake.
// BotAllowlist) — never merely because a Sentry reference appears in the
// message text. When false, f.SentryIssueIDs is cleared before it can drive
// Decide, ExternalKey, FileOrUpdate, or the ticket footer — a digest message
// with a non-allowlisted BotID (or no BotID, i.e. a human message the digest
// batched) must never auto-file or dedup-key on an unverified Sentry ID.
func proposeFromDigest(ctx context.Context, ticketEngine *ticket.Engine, db *state.DB, post digestPostFunc,
	f investigation.Findings, msg digest.Message, sentryCorroborated bool) error {
	if !sentryCorroborated {
		f.SentryIssueIDs = nil
	}

	investigationID := digestInvestigationID(db, msg.ThreadTS)
	decision, fileResult, err := fileOrProposeFromFindings(ctx, ticketEngine, f, msg.Channel, msg.ThreadTS, investigationID, ticket.SourceDigest)
	if err != nil {
		return err
	}

	if decision == ticket.DecisionAutoFile {
		_, err := post(msg.Channel, msg.ThreadTS, composeFiledReply(f, fileResult), nil)
		return err
	}

	text := composeDigestProposalText(f)
	// The CTA button is suppressed when the tracker can't actually create
	// issues — post plain text instead of a button that always errors when
	// clicked.
	var blocks []slack.Block
	if ticketEngine.ShouldCreateIssues() {
		blocks = islack.TicketBlocks(text, msg.ThreadTS)
	}
	_, err = post(msg.Channel, msg.ThreadTS, text, blocks)
	return err
}

// digestInvestigationID looks up the freshest InvestigationRecord saved for
// msg's thread — saved by investigateFromDigest right after a successful
// runInvestigation, keyed on the same (channel, threadTS) — and returns its
// ID for FileOrUpdate's ticket-body backlink footer. Best-effort: a nil db,
// a DB error, or no saved record all return "" (ComposeBody's footer simply
// omits the "toad:investigation <id>" line when blank) rather than failing
// the propose flow — this is an audit-trail nicety, not a correctness
// dependency, so it fails open like every other best-effort DB lookup in
// this package (see preInvestigationTicketCheck's doc comment for the same
// pattern).
//
// This is a lookup rather than a value threaded straight through from
// investigateFromDigest's return because digest.ProposeFunc's signature
// (func(ctx, investigation.Findings, digest.Message) error) is shared,
// exported package API — internal/digest.EngineOpts.Propose — with dozens
// of existing test fixtures across internal/digest/digest_test.go
// constructing it inline at that exact 3-arg shape. Widening it to also
// carry an investigation ID would be a much larger, cross-package
// signature change purely to plumb one backlink field. Looking the record
// up fresh here is correct in the common case (one investigated opportunity
// per thread per flush) and only imprecise in the rare case of multiple
// distinct opportunities landing on the same thread within one flush,
// where the most-recently-saved record wins — an acceptable trade-off for
// a nice-to-have backlink.
func digestInvestigationID(db *state.DB, threadTS string) string {
	if db == nil {
		return ""
	}
	rec, err := db.GetInvestigationByThread(threadTS)
	if err != nil || rec == nil {
		return ""
	}
	return rec.ID
}

// buildDigestTicketContextBlock formats digest's pre-fetched ticket details
// (fetched in internal/digest/digest.go's processOpportunities, before the
// investigation gate runs) into the same "<linked_tickets>" block shape
// buildTicketContextBlock produces for the Slack-thread flows — including
// any fetched comments, which buildTicketContextBlock's own tracker-driven
// fetch doesn't carry. Returns "" when there are no tickets with a
// non-blank ID.
func buildDigestTicketContextBlock(tickets []digest.TicketContext) string {
	if len(tickets) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<linked_tickets>\n")
	found := false
	for _, tc := range tickets {
		if tc.ID == "" {
			continue
		}
		found = true
		fmt.Fprintf(&b, "## %s\n", tc.ID)
		if tc.Title != "" {
			fmt.Fprintf(&b, "Title: %s\n", tc.Title)
		}
		if tc.Description != "" {
			desc := tc.Description
			if len(desc) > 2000 {
				desc = desc[:2000] + "..."
			}
			fmt.Fprintf(&b, "Description:\n%s\n", desc)
		}
		for _, c := range tc.Comments {
			fmt.Fprintf(&b, "Comment (%s): %s\n", c.Author, c.Body)
		}
		b.WriteString("\n")
	}
	if !found {
		return ""
	}
	b.WriteString("</linked_tickets>\n")
	return b.String()
}

// investigateFromDigest is digest.InvestigateFunc's real implementation: it
// resolves the opportunity's repo hint, builds an investigation.Request from
// the opportunity + message + pre-fetched ticket context, and runs it
// through the same bounded investigateSem helper (runInvestigation) the
// Slack-thread flows use — the digest investigate path must respect the
// same concurrent-investigation limit, not spawn unbounded agent runs of its
// own.
//
// An unresolvable repo returns an error without ever calling investRunner —
// mirrors runTicketRequest's identical "could not resolve a repo" guard for
// the CTA path.
//
// On a successful run, this also saves an InvestigationRecord keyed on the
// digest message's thread (same generateInvestigationID/saveInvestigationRecord
// helpers the Slack-thread flows use) — review finding (round 2): before
// this, digest-originated findings never got a saved InvestigationRecord at
// all, so proposeFromDigest had nothing to backlink a filed ticket to (it
// hard-coded investigationID=""), unlike every Slack-thread-originated
// ticket. proposeFromDigest (this file) reads it back via
// digestInvestigationID.
//
// Fix 2 (security) hardening: sentryCorroborated is computed here from
// msg.BotID against botAllowlist (cfg.Intake.BotAllowlist, passed in by the
// root.go closure that wires this as digest.EngineOpts.Investigate) using
// the same rule as everywhere else in this file — true only when the digest
// message came from an allowlisted monitoring bot. When false,
// findings.SentryIssueIDs is cleared BEFORE saveInvestigationRecord, not
// just at the point some later consumer happens to use it: a persisted
// InvestigationRecord is later reused verbatim by runTicketRequest's CTA
// path (reuseRecentInvestigation), so an uncleared record here would let a
// non-corroborated Sentry ID survive to a subsequent CTA click and drive
// FileOrUpdate's identity key — the exact spoofing the corroboration rule
// exists to prevent. (runTicketRequest also re-applies the clear
// unconditionally on reuse as defense in depth, but the record must not be
// tainted at the source either.)
func investigateFromDigest(
	ctx context.Context,
	resolver *config.Resolver,
	investRunner *investigation.Runner,
	investigateSem chan struct{},
	db *state.DB,
	timeoutSecs int,
	opp digest.Opportunity,
	msg digest.Message,
	tickets []digest.TicketContext,
	botAllowlist []string,
) (*investigation.Findings, error) {
	repo := resolver.Resolve(opp.Repo, opp.FilesHint)
	if repo == nil {
		return nil, errors.New("cannot resolve repo")
	}

	timeout := time.Duration(timeoutSecs) * time.Second
	if timeout <= 0 {
		// Matches config.DigestConfig's documented default (600s / 10m) —
		// a defensive fallback in case a zero/negative value ever reaches
		// here (e.g. a test or a future config-loading gap), never the
		// expected path in production.
		timeout = 10 * time.Minute
	}

	req := investigation.Request{
		Text:          msg.Text,
		Category:      opp.Category,
		Confidence:    opp.Confidence,
		Summary:       opp.Summary,
		ChannelName:   msg.ChannelName,
		Keywords:      opp.Keywords,
		FilesHint:     opp.FilesHint,
		SentryRefs:    islack.ExtractSentryRefs(msg.Text),
		TicketContext: buildDigestTicketContextBlock(tickets),
		Repo:          repo,
		Timeout:       timeout,
	}
	findings, err := runInvestigation(ctx, investRunner, investigateSem, req)
	if err != nil {
		return nil, err
	}

	sentryCorroborated := msg.BotID != "" && slices.Contains(botAllowlist, msg.BotID)
	if !sentryCorroborated {
		findings.SentryIssueIDs = nil
	}

	saveInvestigationRecord(db, generateInvestigationID(), msg.ThreadTS, msg.Channel, findings)

	return findings, nil
}
