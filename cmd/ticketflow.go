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
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/config"
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

// composeTicketReply renders the Slack reply text for a ticket-engine
// decision — shared by the triggered bug/feature flow and the CTA/escalation
// flow so both produce identical wording for the same outcome.
func composeTicketReply(decision ticket.Decision, findings investigation.Findings, fileResult *ticket.FileResult) string {
	if decision != ticket.DecisionAutoFile {
		return findings.Reasoning
	}

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
) triggeredOutcome {
	defer stateManager.Unclaim(threadTS)

	db := stateManager.DB()

	// Idempotency pre-check: skip a fresh, expensive investigation entirely
	// when a ticket already tracks this exact problem.
	if existing := preInvestigationTicketCheck(ctx, db, tracker, msg.SentryRefs, msg.Channel, threadTS); existing != nil {
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

	kind := outcomeProposed
	if decision == ticket.DecisionAutoFile {
		kind = outcomeFiled
	}
	return triggeredOutcome{
		Kind:      kind,
		ReplyText: composeTicketReply(decision, *findings, fileResult),
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
	Decision  ticket.Decision
	ReplyText string
	Err       error
}

// runTicketRequest is the shared core of the CTA (:frog: reaction / ticket
// button) and triage-escalation entry points: reuse a recent investigation
// for this thread if one exists, otherwise run a fresh one, then gate it
// through the ticket engine. Unlike runTriggeredInvestigation this never
// falls through to ribbit on an infeasible verdict — an infeasible finding
// simply fails Decide's AutoFile conditions and gets proposed to a human
// instead, which is the right outcome for an explicit "please file a
// ticket" request.
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
) ticketRequestOutcome {
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

	decision, fileResult, err := fileOrProposeFromFindings(ctx, ticketEngine, *findings, msg.Channel, threadTS, recordID, src)
	if err != nil {
		return ticketRequestOutcome{Err: fmt.Errorf("filing ticket: %w", err)}
	}
	return ticketRequestOutcome{
		Decision:  decision,
		ReplyText: composeTicketReply(decision, *findings, fileResult),
	}
}

// reuseRecentInvestigation returns a previously-saved investigation's
// findings and ID when one exists for threadTS and is younger than
// investigationReuseWindow. Returns nil, "" (never an error) on any miss —
// a DB error, no saved investigation, an unparseable record, or one that's
// gone stale all just mean "run a fresh investigation instead", logged for
// visibility but never blocking the caller.
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
) {
	threadTS := msg.ThreadTS()

	// The CTA/reaction path already set a "Spawning tadpole..." (legacy
	// name) thread status before dispatching here (internal/slack/interactive.go).
	// ReplyInThread does not clear it, so it must be cleared before we're
	// done, regardless of which branch below is taken.
	defer slackClient.ClearStatus(msg.Channel, threadTS)
	slackClient.SetStatus(msg.Channel, threadTS, "Filing a ticket...")

	// Reuse already-fetched thread context when the caller already has it
	// (the escalation branch reuses msg from handleTriggered, which already
	// populated and enriched ThreadContext); a bare CTA click has none yet.
	if len(msg.ThreadContext) == 0 && msg.ThreadTimestamp != "" {
		if tc, err := slackClient.FetchThreadMessages(msg.Channel, msg.ThreadTimestamp); err != nil {
			slog.Warn("failed to fetch thread context for ticket request", "error", err, "thread", threadTS)
		} else {
			msg.ThreadContext = tc
		}
	}
	msg.ThreadContext = enrichWithIssueDetails(ctx, tracker, msg.Text, msg.ThreadContext)

	outcome := runTicketRequest(ctx, msg, result, stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, channelName, threadTS, src)
	if outcome.Err != nil {
		slog.Error("ticket request failed", "error", outcome.Err, "thread", threadTS)
		slackClient.ReplyInThread(msg.Channel, threadTS, ":x: Sorry, I couldn't file a ticket: "+outcome.Err.Error())
		return
	}

	if outcome.Decision == ticket.DecisionPropose {
		blocks := islack.TicketBlocks(outcome.ReplyText, threadTS)
		if _, err := slackClient.ReplyInThreadWithBlocks(msg.Channel, threadTS, outcome.ReplyText, blocks); err != nil {
			slog.Warn("ticket request reply failed", "error", err)
		}
		return
	}
	if _, err := slackClient.ReplyInThread(msg.Channel, threadTS, outcome.ReplyText); err != nil {
		slog.Warn("ticket request reply failed", "error", err)
	}
}
