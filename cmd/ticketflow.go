// ticketflow.go implements the v2 investigate-and-file flow: the bug/feature
// branch of handleTriggered, the :ticket: CTA handler, and the shared
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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/digest"
	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	islack "github.com/scaler-tech/toad/internal/slack"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/ticket"
	"github.com/scaler-tech/toad/internal/triage"
)

// Triage categories emitted by internal/triage's classifier; the prompt
// pins the category vocabulary to exactly bug/feature/question/other.
const (
	categoryBug     = "bug"
	categoryFeature = "feature"
)

// ticketRequestRe matches the explicit "make/create/add/file/open a
// ticket/issue" phrasings that the triage prompt defines for its escalate
// flag. It exists so an explicit ticket request never depends on the triage
// model call succeeding: a timed-out triage falls back to escalate=false
// and would otherwise silently answer with Q&A instead of filing.
var ticketRequestRe = regexp.MustCompile(`(?i)\b(?:make|create|add|file|open)\b[^.!?\n]{0,40}\b(?:ticket|issue)s?\b`)

// isExplicitTicketRequest reports whether the message text explicitly asks
// for a ticket/issue to be created.
func isExplicitTicketRequest(text string) bool {
	return ticketRequestRe.MatchString(text)
}

// shouldForceEscalateForTicketRequest reports whether handleTriggered's
// deterministic ticket-request backstop should override triage's Escalate
// flag for msg (see isExplicitTicketRequest's doc comment above for why the
// backstop exists at all).
//
// !msg.IsBot is the Critical fix here: without it, bot boilerplate that
// happens to contain a ticket-request phrase (e.g. "...create an issue to
// track...") combined with a triage timeout would
// force an unreviewed SourceEscalation filing with no human ever having
// asked for a ticket. SourceEscalation's entire justification is that an
// explicit HUMAN request already IS the filing sign-off (see
// runTicketRequest's doc comment) — that does not hold for bot-authored
// text, so the backstop must never fire on it. FetchMessage
// (internal/slack/client.go) sets IsBot correctly (BotID != "") even though
// it leaves BotID itself unset on that path, so this check is reliable
// regardless of which entry path produced msg.
func shouldForceEscalateForTicketRequest(msg *islack.IncomingMessage, escalate bool) bool {
	return !msg.IsBot && !escalate && isExplicitTicketRequest(msg.Text)
}

// flowDeps bundles the six dependencies shared by every investigate-and-file
// entry point in this package: handleMessage, handleTriggered,
// handleTicketRequest, handleBotIntake, runTriggeredInvestigation,
// runTicketRequest, and runBotIntake all thread the exact same six values
// through, previously as six separate positional parameters apiece.
// Constructed once in root.go and passed down as a single value. Deliberately
// does NOT also carry slackClient, triageEngine, botAllowlist, channelName,
// or the semaphores/engines only some of these callers need (ribbitSem,
// digestEngine, repoPaths) — the goal is fewer positional params, not a god
// object holding everything.
type flowDeps struct {
	stateManager   *state.Manager
	tracker        issuetracker.Tracker
	resolver       *config.Resolver
	investRunner   *investigation.Runner
	ticketEngine   *ticket.Engine
	investigateSem chan struct{}

	// delegates is cfg.IssueTracker.Delegates — a requested-name -> Linear
	// label map consulted by applyLinearAssigneeMapping. May be nil (no
	// delegation configured).
	delegates map[string]string

	// resolveRequesterIdentity resolves a Slack user ID to the best
	// available identity for Linear assignee resolution (email preferred,
	// falling back to display name; "" if neither is available) — wired in
	// root.go over slackClient.GetUserEmail/GetUserDisplayName. Kept as an
	// injectable function value rather than a *islack.Client field, same
	// rationale as digestPostFunc below: it keeps this file's decision
	// functions (runTriggeredInvestigation, runTicketRequest) directly
	// unit-testable with a fake instead of a live Slack client. May be nil
	// (e.g. in tests that don't exercise assignee resolution) —
	// applyLinearAssigneeMapping treats a nil func the same as an empty
	// requesterID: any "requester" entry simply drops.
	resolveRequesterIdentity func(userID string) string

	// investigateTimeout bounds a single message-path investigation run
	// (triggered bug/feature and CTA/escalation requests). Sourced from
	// limits.timeout_minutes in root.go — the same knob that bounds ribbit —
	// replacing an old hardcoded 4m that regularly expired once the
	// workspace grew to several repos (digest investigations already had a
	// configurable 10m via digest.investigate_timeout_secs).
	investigateTimeout time.Duration
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
// findings when the repo sync failed beforehand. Staleness is read off the
// returned Findings.RepoSyncFailed (set by Runner.Run itself) rather than
// smuggled back through ctx — see investigation.Findings' doc comment on
// that field.
func runInvestigation(ctx context.Context, investRunner *investigation.Runner, investigateSem chan struct{},
	req investigation.Request) (*investigation.Findings, error) {
	select {
	case investigateSem <- struct{}{}:
		defer func() { <-investigateSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	findings, err := investRunner.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	if findings.RepoSyncFailed {
		findings.Reasoning = strings.TrimRight(findings.Reasoning, "\n") + staleCaveat
	}
	return findings, nil
}

// isSentryCorroborated reports whether a Sentry reference may be trusted as
// genuine external corroboration: only when the message that carried it
// arrived from an allowlisted monitoring bot, identified by botID. Every
// IncomingMessage/digest.Message construction path sets IsBot as exactly
// `BotID != ""` (verified against internal/slack/events.go and client.go),
// so checking botID directly here is equivalent to checking IsBot and also
// works for callers that only have a raw bot ID string (e.g. digest.Message,
// which carries no IsBot field of its own).
//
// This is the single mechanism for the corroboration gate, combined with
// enforceCorroboration's ref-scoping: a Sentry ID must never drive ticket
// identity, auto-file, or the ticket footer merely because it appears in
// human-pasted text or an investigation agent's own output — only because
// it arrived from a bot on the configured allowlist AND matches one of the
// Sentry refs extracted from that bot message's own text. isSentryCorroborated
// answers "did this arrive from an allowlisted bot at all"; it is not by
// itself sufficient to trust an arbitrary model-emitted Sentry ID — see
// enforceCorroboration for the ref-intersection that closes that gap.
func isSentryCorroborated(botID string, allowlist []string) bool {
	return botID != "" && slices.Contains(allowlist, botID)
}

// enforceCorroboration is the single enforcement point for the corroboration
// rule (see isSentryCorroborated's doc comment for the full rationale): a
// Sentry reference may only drive Decide/ExternalKey/FileOrUpdate/the ticket
// footer once it's been confirmed to have arrived from an allowlisted
// monitoring bot, never from human-pasted text or model output.
//
// When corroborated is false, f.SentryIssueIDs is cleared entirely.
//
// When corroborated is true, f.SentryIssueIDs is intersected against
// trustedRefs — the Sentry refs extracted directly from the corroborating
// bot message's own text (via islack.ExtractSentryRefs), NOT the model's
// freeform output. f.SentryIssueIDs is itself model output, and the model
// sees the whole thread — including any human-pasted text — so "corroborated
// == true" alone is not enough to trust whichever IDs the model happened to
// emit: a human reply pasting an arbitrary Sentry key into a thread rooted by
// an allowlisted bot could otherwise steer ticket identity (ExternalKey ->
// sentry:<id>) and the auto-file gate onto a fabricated ID. Only IDs that
// also appear in trustedRefs survive; anything else the model emitted is
// dropped, same as the non-corroborated case, and ExternalKey falls back to
// the thread key.
//
// Nil-safe — a no-op when f is nil — so callers can pass a possibly-nil
// *Findings without a separate guard.
func enforceCorroboration(f *investigation.Findings, corroborated bool, trustedRefs []string) {
	if f == nil {
		return
	}
	if !corroborated {
		f.SentryIssueIDs = nil
		return
	}
	f.SentryIssueIDs = intersectTrustedSentryRefs(f.SentryIssueIDs, trustedRefs)
}

// intersectTrustedSentryRefs returns the subset of ids that also appears in
// trusted, preserving ids' order. Returns nil (not an empty slice) when
// either input is empty or nothing survives, matching enforceCorroboration's
// prior "cleared means nil" convention that callers (e.g. ticket.ExternalKey)
// already rely on.
func intersectTrustedSentryRefs(ids, trusted []string) []string {
	if len(ids) == 0 || len(trusted) == 0 {
		return nil
	}
	trustedSet := make(map[string]struct{}, len(trusted))
	for _, r := range trusted {
		trustedSet[r] = struct{}{}
	}
	var kept []string
	for _, id := range ids {
		if _, ok := trustedSet[id]; ok {
			kept = append(kept, id)
		}
	}
	return kept
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

// ticketItem is the common shape both <linked_tickets> renderers below (the
// Slack-thread path's buildTicketContextBlock and the digest path's
// buildDigestTicketContextBlock) reduce their differently-sourced input to
// before sharing renderTicketContextBlock. buildTicketContextBlock fetches
// details live via issuetracker.Tracker, which this file's rendering has
// never surfaced comments for; buildDigestTicketContextBlock works from
// digest's own pre-fetched tickets, which do carry comments. Comments is
// left empty by the Slack-thread path rather than forcing artificial parity
// between the two callers — the shared renderer just takes what it's given.
type ticketItem struct {
	ID          string
	Title       string
	Description string
	Comments    []string // pre-formatted lines, one per comment
}

// renderTicketContextBlock formats items into the "<linked_tickets>" block
// investigation.Request's TicketContext field expects
// (internal/investigation/prompt.go writes it into the prompt verbatim).
// Returns "" when no item has a non-blank ID.
func renderTicketContextBlock(items []ticketItem) string {
	var b strings.Builder
	b.WriteString("<linked_tickets>\n")
	found := false
	for _, it := range items {
		if it.ID == "" {
			continue
		}
		found = true
		fmt.Fprintf(&b, "## %s\n", it.ID)
		if it.Title != "" {
			fmt.Fprintf(&b, "Title: %s\n", it.Title)
		}
		if it.Description != "" {
			desc := it.Description
			if len(desc) > 2000 {
				desc = desc[:2000] + "..."
			}
			fmt.Fprintf(&b, "Description:\n%s\n", desc)
		}
		for _, c := range it.Comments {
			fmt.Fprintf(&b, "%s\n", c)
		}
		b.WriteString("\n")
	}
	if !found {
		return ""
	}
	b.WriteString("</linked_tickets>\n")
	return b.String()
}

// buildTicketContextBlock formats already-fetched issue-tracker details into
// the "<linked_tickets>" block (mirroring the deleted v1
// cmd/investigation.go's formatTicketContext). details comes from the
// caller's own enrichWithIssueDetails call (cmd/helpers.go) — that function
// already extracts issue references from the same text/threadContext this
// function used to re-extract from, and fetches their details over the
// network; re-extracting and re-fetching here duplicated that work (up to 2
// GetIssueDetails calls per ticket per flow) for the identical result, since
// every caller of this function enriches its message before calling it.
// Returns "" when details is empty.
func buildTicketContextBlock(details []issuetracker.IssueDetails) string {
	items := make([]ticketItem, 0, len(details))
	for _, d := range details {
		items = append(items, ticketItem{ID: d.ID, Title: d.Title, Description: d.Description})
	}
	return renderTicketContextBlock(items)
}

// preInvestigationTicketCheck looks up the ticket index for the external key
// this report would map to, before any investigation runs — keyed by
// intake-time Sentry refs when sentryRefs is non-empty, or (via
// ticket.ExternalKey's own fallback) by the channel+threadTS pair when it's
// empty, e.g. because the caller hasn't corroborated a Sentry reference for
// this report. A hit means a ticket already tracks this exact
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

// mapLinearAssignees is the pure decision core behind applyLinearAssigneeMapping:
// resolve each raw name in names (Findings.LinearAssignees — copied verbatim
// by the model from an explicit reporter request) into either the single
// Linear assignee candidate or a delegate label, following toad's
// issue_tracker.delegates config. It is Slack-client-free and
// network-free — the caller resolves "requester" to requesterIdentity (a
// Slack email/display name, or "" if unavailable) before calling this.
//
// Resolution order per name (case-insensitive, in names' order):
//  1. the literal token "requester" -> requesterIdentity (dropped
//     entirely — not even counted as a candidate — if requesterIdentity
//     is "")
//  2. a name matching a delegates key -> its configured label, collected
//     into labels
//  3. anything else -> an assignee candidate
//
// The FIRST assignee candidate found wins — Linear has a single assignee
// slot. Any later candidates are returned in dropped, for the caller to log
// (never silently discarded from visibility, just from the actual filing).
func mapLinearAssignees(names []string, delegates map[string]string, requesterIdentity string) (assignee string, labels []string, dropped []string) {
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}

		if strings.EqualFold(name, "requester") {
			if requesterIdentity == "" {
				continue
			}
			assignee, dropped = addCandidate(assignee, dropped, requesterIdentity)
			continue
		}

		if label, ok := lookupDelegateLabel(delegates, name); ok {
			labels = append(labels, label)
			continue
		}

		assignee, dropped = addCandidate(assignee, dropped, name)
	}
	return assignee, labels, dropped
}

// addCandidate returns (assignee, dropped) updated with candidate: it fills
// assignee if empty (first candidate wins), otherwise appends to dropped —
// shared by both branches of mapLinearAssignees that can produce an
// assignee candidate ("requester" and a plain name).
func addCandidate(assignee string, dropped []string, candidate string) (string, []string) {
	if assignee == "" {
		return candidate, dropped
	}
	return assignee, append(dropped, candidate)
}

// lookupDelegateLabel looks up name in delegates case-insensitively — the
// map itself is user-authored YAML config (issue_tracker.delegates), so a
// literal, case-sensitive Go map lookup would make "Biome" vs "biome" a
// silent miss; case folding it here is cheap given delegates is always a
// small, human-sized list.
func lookupDelegateLabel(delegates map[string]string, name string) (string, bool) {
	for k, v := range delegates {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// applyLinearAssigneeMapping resolves f.LinearAssignees — the model's raw,
// verbatim names — into the RESOLVED-input fields ticket.Engine.file reads
// (f.LinearAssignee, f.LinearExtraLabels), mutating f in place. It is the
// single call site for this resolution, invoked immediately before every
// FileOrUpdate call in this file (runTriggeredInvestigation's auto-file
// branch, runTicketRequest's direct-file path, and proposeFromDigest before
// its own fileOrProposeFromFindings call) — never cached or reused across
// calls, so a re-derivation always reflects the CURRENT request's own
// requester, never a persisted/reused investigation record's original
// reporter (mirrors enforceCorroboration's re-derive-every-time rule, and
// for the same reason: "assign to me" means something different depending
// on who is asking right now).
//
// requesterID is the Slack user ID of whoever made THIS specific request; it
// may be "" (digest paths have no requester at all — see flowDeps.
// resolveRequesterIdentity's doc comment), in which case a "requester" entry
// in f.LinearAssignees simply drops (logged at Info, same as any other
// dropped extra candidate).
func applyLinearAssigneeMapping(f *investigation.Findings, delegates map[string]string, requesterID string, resolveIdentity func(string) string) {
	if f == nil || len(f.LinearAssignees) == 0 {
		return
	}

	identity := ""
	if requesterID != "" && resolveIdentity != nil {
		identity = resolveIdentity(requesterID)
	}
	if containsFold(f.LinearAssignees, "requester") && identity == "" {
		slog.Info("could not resolve requester identity for Linear assignment, dropping 'requester'", "requester", requesterID)
	}

	assignee, labels, dropped := mapLinearAssignees(f.LinearAssignees, delegates, identity)
	f.LinearAssignee = assignee
	f.LinearExtraLabels = labels
	if len(dropped) > 0 {
		slog.Info("dropping extra Linear assignee candidates (Linear allows only one)", "kept", assignee, "dropped", dropped)
	}
}

// containsFold reports whether names contains s, case-insensitively.
func containsFold(names []string, s string) bool {
	for _, n := range names {
		if strings.EqualFold(strings.TrimSpace(n), s) {
			return true
		}
	}
	return false
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
	assigneeUnresolved := ""
	if fileResult != nil && fileResult.Ref != nil {
		if fileResult.Ref.Title != "" {
			title = fileResult.Ref.Title
		}
		url = fileResult.Ref.URL
		assigneeUnresolved = fileResult.Ref.AssigneeUnresolved
	}
	if title == "" {
		title = "(untitled)"
	}
	if fileResult != nil && fileResult.AlreadyExisted {
		return fmt.Sprintf(":ticket: Already tracked as %s — *%s*\n\n%s", url, title, findings.Reasoning)
	}
	return fmt.Sprintf(":ticket: Filed %s — *%s*\n\n%s%s", url, title, findings.Reasoning,
		assigneeReplySuffix(findings, assigneeUnresolved))
}

// assigneeReplySuffix renders the trailing "assigned to X" / "couldn't
// resolve assignee" / "delegated via LABEL" line(s) appended to a
// newly-filed ticket's reply — never for a re-observed (AlreadyExisted)
// ticket, since assignment/labeling only ever happens on the CreateIssue
// path composeFiledReply's caller took (reobserve only posts a comment, it
// never touches the existing ticket's assignee or labels). assigneeUnresolved
// comes from the filed IssueRef (empty unless CreateIssue actually tried and
// failed to resolve an assignee); findings carries the RESOLVED-input
// LinearAssignee/LinearExtraLabels fields applyLinearAssigneeMapping set
// before filing.
func assigneeReplySuffix(findings investigation.Findings, assigneeUnresolved string) string {
	var b strings.Builder
	switch {
	case assigneeUnresolved != "":
		fmt.Fprintf(&b, "\n\n_couldn't resolve assignee %q — filed unassigned_", assigneeUnresolved)
	case findings.LinearAssignee != "":
		fmt.Fprintf(&b, "\n\n_assigned to %s_", findings.LinearAssignee)
	}
	if len(findings.LinearExtraLabels) > 0 {
		fmt.Fprintf(&b, "\n\n_delegated via %s_", strings.Join(findings.LinearExtraLabels, ", "))
	}
	return b.String()
}

// maxInfeasibleReasoningLen bounds how much of an infeasible finding's
// Reasoning gets embedded in composeInfeasibleTicketReply's Slack reply —
// long enough to be useful context, short enough not to dump a whole
// multi-paragraph investigation into a one-line "couldn't confirm a fix"
// message.
const maxInfeasibleReasoningLen = 300

// trimForReply truncates s to maxLen characters (appending "..." when
// truncated), trimming surrounding whitespace either way.
func trimForReply(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return strings.TrimSpace(s[:maxLen]) + "..."
}

// composeInfeasibleTicketReply renders the Slack reply for an explicit
// ticket request (CTA click, triage escalate, or the phrase backstop) whose
// FRESH investigation came back infeasible — Important fix (I1). An
// explicit request is sign-off to ATTEMPT filing, not sign-off to file a
// finding the investigation itself said has no concrete fix; siblings
// (runTriggeredInvestigation's auto-file branch) already fall through
// instead of filing on !Feasible, and this path must not skip that check
// just because a human explicitly asked.
//
// Deliberately does NOT implement an override phrase (e.g. "file it
// anyway"): the spec called that support optional, and skipping it here
// keeps this a small, easily-verified change — so the reply asks the user
// to add detail and re-request rather than promising an override path that
// doesn't exist yet.
func composeInfeasibleTicketReply(findings *investigation.Findings) string {
	reasoning := trimForReply(findings.Reasoning, maxInfeasibleReasoningLen)
	return fmt.Sprintf(
		":warning: I investigated but couldn't confirm a concrete fix: %s\n\nNo ticket filed — reply with more detail and ask again.",
		reasoning,
	)
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
//
// Even when sentryCorroborated is true, enforceCorroboration further scopes
// findings.SentryIssueIDs to msg.SentryRefs (the refs extracted from this
// message's own text): the model sees the whole thread, so a human reply
// pasting an arbitrary Sentry key into a thread rooted by an allowlisted bot
// must not be able to steer ticket identity onto that pasted ID merely
// because the thread as a whole is corroborated.
func runTriggeredInvestigation(
	ctx context.Context,
	msg *islack.IncomingMessage,
	result *triage.Result,
	channelName, threadTS string,
	deps flowDeps,
	issueDetails []issuetracker.IssueDetails,
	sentryCorroborated bool,
) triggeredOutcome {
	defer deps.stateManager.Unclaim(threadTS)

	db := deps.stateManager.DB()

	sentryRefsForCheck := msg.SentryRefs
	if !sentryCorroborated {
		sentryRefsForCheck = nil
	}

	// Idempotency pre-check: skip a fresh, expensive investigation entirely
	// when a ticket already tracks this exact problem.
	if existing := preInvestigationTicketCheck(ctx, db, deps.tracker, sentryRefsForCheck, msg.Channel, threadTS); existing != nil {
		return triggeredOutcome{
			Kind:      outcomeIdempotentHit,
			ReplyText: fmt.Sprintf(":ticket: Already tracked as %s: %s", existing.IssueID, existing.IssueURL),
		}
	}

	repo := deps.resolver.Resolve(result.Repo, result.FilesHint)
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
		TicketContext: buildTicketContextBlock(issueDetails),
		Repo:          repo,
		Timeout:       deps.investigateTimeout,
	}

	findings, err := runInvestigation(ctx, deps.investRunner, deps.investigateSem, req)
	if err != nil {
		slog.Warn("triggered investigation failed, falling through to ribbit", "error", err, "summary", result.Summary)
		return triggeredOutcome{Kind: outcomeFallThrough}
	}
	// See this function's doc comment, and enforceCorroboration's, for the
	// rule: any Sentry IDs the model surfaced (from req.SentryRefs leads, or
	// inferred from thread text) must never drive Decide/ExternalKey/
	// FileOrUpdate/the ticket footer unless sentryCorroborated is true AND
	// the ID also appears in msg.SentryRefs — the refs extracted from this
	// report's own (corroborating, when sentryCorroborated) message text —
	// not merely whatever the model happened to emit.
	enforceCorroboration(findings, sentryCorroborated, msg.SentryRefs)

	recordID := generateInvestigationID()
	saveInvestigationRecord(db, recordID, threadTS, msg.Channel, findings)

	if !findings.Feasible {
		slog.Info("investigation says not feasible, falling through to ribbit",
			"reasoning", findings.Reasoning, "summary", result.Summary)
		return triggeredOutcome{Kind: outcomeFallThrough, Findings: findings}
	}

	// Resolve any explicit assign/delegate request against msg.User — the
	// Slack user who sent THIS message — before the ticket might be filed.
	// See applyLinearAssigneeMapping's doc comment for why this always
	// happens fresh here rather than being trusted from anywhere else.
	applyLinearAssigneeMapping(findings, deps.delegates, msg.User, deps.resolveRequesterIdentity)

	decision, fileResult, fileErr := fileOrProposeFromFindings(ctx, deps.ticketEngine, *findings, msg.Channel, threadTS, recordID, ticket.SourceAuto)
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

// runTicketRequest is the shared core of the CTA (ticket button) and
// triage-escalation entry points: claim the thread, reuse a
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
// allowlisted monitoring bot, never from a human click or
// human-pasted text. It gates both a FRESH investigation's
// Findings.SentryIssueIDs and a REUSED saved finding's the same way — this
// path bypasses Decide entirely (an explicit human request already IS the
// sign-off, so auto-file's confidence/corroboration gate doesn't apply), but
// FileOrUpdate's ExternalKey/footer must still never trust an unverified (or
// no-longer-current-request-scoped) Sentry ID as the ticket's identity key.
// See the enforceCorroboration call below, which re-applies unconditionally
// to whichever findings value this function ends up with — fresh or reused —
// against THIS request's own trustedRefs, never a reused record's own
// (possibly stale, possibly differently-gated) history.
func runTicketRequest(
	ctx context.Context,
	msg *islack.IncomingMessage,
	result *triage.Result, // nil for a bare CTA click with no fresh triage
	deps flowDeps,
	issueDetails []issuetracker.IssueDetails,
	channelName, threadTS string,
	src ticket.Source,
	sentryCorroborated bool,
) ticketRequestOutcome {
	if !deps.stateManager.Claim(threadTS) {
		return ticketRequestOutcome{Conflict: true}
	}
	defer deps.stateManager.Unclaim(threadTS)

	db := deps.stateManager.DB()

	findings, recordID := reuseRecentInvestigation(db, threadTS)
	if findings == nil {
		repoHint, filesHint := "", []string(nil)
		category, confidence, summary, keywords := "", 0.0, "", []string(nil)
		if result != nil {
			repoHint, filesHint = result.Repo, result.FilesHint
			category, confidence, summary, keywords = result.Category, result.Confidence, result.Summary, result.Keywords
		}

		repo := deps.resolver.Resolve(repoHint, filesHint)
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
			TicketContext: buildTicketContextBlock(issueDetails),
			Repo:          repo,
			Timeout:       deps.investigateTimeout,
		}

		f, err := runInvestigation(ctx, deps.investRunner, deps.investigateSem, req)
		if err != nil {
			return ticketRequestOutcome{Err: fmt.Errorf("investigation failed: %w", err)}
		}
		recordID = generateInvestigationID()
		// Save before the Feasible check (mirrors runTriggeredInvestigation)
		// so an infeasible fresh investigation is still visible in the
		// audit trail even though it won't be filed below.
		saveInvestigationRecord(db, recordID, threadTS, msg.Channel, f)

		// Important fix (I1): an explicit ticket request (this whole
		// function) must not file a FRESH investigation's infeasible
		// finding just because the request was explicit — see
		// composeInfeasibleTicketReply's doc comment. A REUSED record never
		// reaches here infeasible (reuseRecentInvestigation already filters
		// those out above), so this check only applies to a fresh run.
		if !f.Feasible {
			return ticketRequestOutcome{ReplyText: composeInfeasibleTicketReply(f)}
		}
		findings = f
	}

	// Re-apply enforceCorroboration here, unconditionally, on the FINAL
	// findings value — whether it just came from a fresh investigation above
	// or was reused from a saved record. A reused record may predate
	// corroboration filtering, or may have been saved by a different path
	// with different (or missing) gating at save time — e.g. investigateFromDigest,
	// whose saveInvestigationRecord call has no reliable relationship to
	// THIS request's sentryCorroborated value. Never trust a persisted
	// record's SentryIssueIDs as already-clean; always re-derive from the
	// current request's own corroboration AND intersect against msg.SentryRefs
	// — the CURRENT request's own trusted refs, not whatever refs (if any)
	// were in scope when the reused record was first produced.
	enforceCorroboration(findings, sentryCorroborated, msg.SentryRefs)

	// Re-derive the assignee/delegate mapping too, for the same reason as
	// enforceCorroboration above: msg.User here is whoever made THIS
	// request (the CTA clicker, or the escalating message's author) — not
	// necessarily the same person who originally reported the problem when
	// findings came from a reused record. See applyLinearAssigneeMapping's
	// doc comment.
	applyLinearAssigneeMapping(findings, deps.delegates, msg.User, deps.resolveRequesterIdentity)

	fileResult, err := deps.ticketEngine.FileOrUpdate(ctx, *findings, msg.Channel, threadTS, recordID, src)
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

// handleTicketRequest is the :ticket: CTA entry point, and — via the
// escalation branch in handleTriggered — the triage Escalate==true entry
// point too. Both funnel into runTicketRequest, distinguished only by src.
func handleTicketRequest(
	ctx context.Context,
	msg *islack.IncomingMessage,
	slackClient *islack.Client,
	result *triage.Result,
	deps flowDeps,
	issueDetails []issuetracker.IssueDetails,
	channelName string,
	src ticket.Source,
	sentryCorroborated bool,
) {
	threadTS := msg.ThreadTS()

	// The CTA path already set a ":ticket: Creating ticket..."
	// thread status before dispatching here (internal/slack/interactive.go).
	// ReplyInThread does not clear it, so it must be cleared before we're
	// done, regardless of which branch below is taken.
	defer slackClient.ClearStatus(msg.Channel, threadTS)
	slackClient.SetStatus(msg.Channel, threadTS, "Filing a ticket...")

	// Reuse already-fetched, already-enriched thread context (and its
	// fetched issueDetails) when the caller already has it — the escalation
	// branch reuses msg from handleTriggered, which already populated AND
	// enriched ThreadContext via enrichWithIssueDetails, passing its
	// issueDetails through as this function's own parameter. The guard
	// covers both the fetch and the enrich call, so a bare CTA click (no
	// context yet) does both exactly once, and the escalation path does
	// neither again (review minor: this guard previously covered only the
	// fetch, so escalation threads got enriched twice).
	if len(msg.ThreadContext) == 0 {
		if msg.ThreadTimestamp != "" {
			if tc, err := slackClient.FetchThreadMessages(msg.Channel, msg.ThreadTimestamp); err != nil {
				slog.Warn("failed to fetch thread context for ticket request", "error", err, "thread", threadTS)
			} else {
				msg.ThreadContext = tc
			}
		}
		msg.ThreadContext, issueDetails = enrichWithIssueDetails(ctx, deps.tracker, msg.Text, msg.ThreadContext)
	}

	outcome := runTicketRequest(ctx, msg, result, deps, issueDetails, channelName, threadTS, src, sentryCorroborated)
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

// firstLine returns the first non-blank line of text, trimmed, for use as a
// short synthetic summary — text may be multi-paragraph bot alert content
// (stack traces, formatted fields), and a full-text summary would be far
// noisier than the first line (typically the alert title).
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return strings.TrimSpace(text)
}

// syntheticBotIntakeResult builds a fallback triage.Result for an
// allowlisted-bot message when Classify has failed twice in a row but the
// message carries Sentry references — i.e. it's already externally
// corroborated as a real signal, just one triage couldn't classify. Rather
// than drop a corroborated signal outright, this "fails toward
// investigation": Actionable=true, Category="bug", a modest 0.55 confidence
// (above runBotIntake's 0.5 investigate-gate, but low enough that
// ticket.Engine's Decide gate won't auto-file from this alone without real
// investigation findings backing it up). The downstream investigation +
// Decide gate is the real filter here, not this synthetic classification —
// see runBotIntake's doc comment on this substitution.
func syntheticBotIntakeResult(msg *islack.IncomingMessage) *triage.Result {
	return &triage.Result{
		Actionable: true,
		Category:   categoryBug,
		Confidence: 0.55,
		Summary:    firstLine(msg.Text),
		Keywords:   msg.SentryRefs,
	}
}

// runBotIntake is the pure decision core of the allowlisted-bot intake path
// (controller ruling from Task 15's review: bot messages feed intake, never
// conversation). It triages the message directly — no ribbitSem, no
// "ask for clarification" reply, no ribbit fallback — and proceeds into the
// same investigate-and-file flow as handleTriggered's bug/feature branch
// (bounded by investigateSem as always) ONLY for an actionable bug/feature.
// Everything else (question/other, non-actionable, low confidence, a claim
// conflict, an infeasible/errored investigation) is dropped: this returns nil
// and the caller posts no reply. Never touches ribbit or *islack.Client, so
// the "drop a question" behavior is directly testable.
//
// Critical fix (C3): every drop point used to log at Debug and return nil,
// which for an allowlisted monitoring bot (i.e. already-corroborated
// signals like Sentry alerts) meant real incoming problems could vanish
// with no operational visibility at all. Every drop now logs at Warn with
// enough context to investigate after the fact (bot_id, channel, sentry ref
// count, reason) and increments daemonCounters.botIntakeDropped (surfaced on
// the dashboard). Additionally, a triage failure specifically is no longer
// an automatic drop: it retries Classify once, and if that also fails AND
// the message carries Sentry refs (external corroboration that this is a
// real signal, not noise), it proceeds with a synthetic triage result
// instead of dropping — see syntheticBotIntakeResult's doc comment. A
// triage failure with no Sentry refs still drops (there's no external
// signal to fail toward investigating on, and this package's investigation
// path requires *some* triage.Result to build a Request from).
func runBotIntake(
	ctx context.Context,
	msg *islack.IncomingMessage,
	triageEngine *triage.Engine,
	channelName string,
	deps flowDeps,
	issueDetails []issuetracker.IssueDetails,
	sentryCorroborated bool,
) *triggeredOutcome {
	result, err := triageEngine.Classify(ctx, msg, channelName)
	if err != nil {
		slog.Warn("bot intake: triage failed, retrying once", "error", err, "bot_id", msg.BotID, "channel", msg.Channel)
		result, err = triageEngine.Classify(ctx, msg, channelName)
	}
	if err != nil {
		if len(msg.SentryRefs) == 0 {
			daemonCounters.botIntakeDropped.Add(1)
			slog.Warn("bot intake: triage failed twice with no Sentry refs to fail toward, dropping",
				"error", err, "bot_id", msg.BotID, "channel", msg.Channel, "sentry_refs", 0)
			return nil
		}
		slog.Warn("bot intake: triage failed twice but message has Sentry refs, proceeding with synthetic classification",
			"error", err, "bot_id", msg.BotID, "channel", msg.Channel, "sentry_refs", len(msg.SentryRefs))
		result = syntheticBotIntakeResult(msg)
	}

	daemonCounters.triages.Add(1)
	switch result.Category {
	case categoryBug:
		daemonCounters.triageBug.Add(1)
	case categoryFeature:
		daemonCounters.triageFeature.Add(1)
	case "question":
		daemonCounters.triageQuestion.Add(1)
	default:
		daemonCounters.triageOther.Add(1)
	}

	if !result.Actionable || result.Confidence < 0.5 || (result.Category != categoryBug && result.Category != categoryFeature) {
		daemonCounters.botIntakeDropped.Add(1)
		slog.Warn("bot intake: not an actionable bug/feature, dropping",
			"actionable", result.Actionable, "category", result.Category, "confidence", result.Confidence,
			"bot_id", msg.BotID, "channel", msg.Channel, "sentry_refs", len(msg.SentryRefs))
		return nil
	}

	threadTS := msg.ThreadTS()
	if !deps.stateManager.Claim(threadTS) {
		daemonCounters.botIntakeDropped.Add(1)
		slog.Warn("bot intake: thread already claimed, dropping",
			"thread", threadTS, "bot_id", msg.BotID, "channel", msg.Channel, "sentry_refs", len(msg.SentryRefs))
		return nil
	}

	slog.Info("bot intake investigating", "summary", result.Summary, "category", result.Category, "bot_id", msg.BotID)
	outcome := runTriggeredInvestigation(ctx, msg, result, channelName, threadTS, deps, issueDetails, sentryCorroborated)

	if outcome.Kind == outcomeFallThrough {
		daemonCounters.botIntakeDropped.Add(1)
		slog.Warn("bot intake: investigation fell through (infeasible or errored), dropping",
			"bot_id", msg.BotID, "channel", msg.Channel, "sentry_refs", len(msg.SentryRefs))
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
	deps flowDeps,
	channelName string,
	sentryCorroborated bool,
) {
	if msg.ThreadTimestamp != "" {
		if tc, err := slackClient.FetchThreadMessages(msg.Channel, msg.ThreadTimestamp); err != nil {
			slog.Debug("bot intake: failed to fetch thread context", "error", err)
		} else {
			msg.ThreadContext = tc
		}
	}
	var issueDetails []issuetracker.IssueDetails
	msg.ThreadContext, issueDetails = enrichWithIssueDetails(ctx, deps.tracker, msg.Text, msg.ThreadContext)

	outcome := runBotIntake(ctx, msg, triageEngine, channelName, deps, issueDetails, sentryCorroborated)
	if outcome == nil {
		return
	}

	threadTS := msg.ThreadTS()
	// See ReplyWithOptionalCTA's doc comment: the button is suppressed when
	// the tracker can't actually create issues.
	showCTA := outcome.Kind == outcomeProposed && deps.ticketEngine.ShouldCreateIssues()
	if _, err := slackClient.ReplyWithOptionalCTA(msg.Channel, threadTS, outcome.ReplyText, showCTA); err != nil {
		slog.Warn("bot intake investigation reply failed", "error", err)
	}
}

// digestPostFunc posts a Slack thread reply for the digest propose path,
// showing the ticket-creation CTA button when showCTA is true. Kept as an
// injectable function value — rather than a *islack.Client parameter — so
// proposeFromDigest's Decide/FileOrUpdate/compose decision logic stays
// directly unit-testable with a fake, consistent with this file's
// Slack-client-free design (see the package doc comment at the top).
// root.go wires the real implementation over
// slackClient.ReplyWithOptionalCTA.
type digestPostFunc func(channel, threadTS, text string, showCTA bool) (string, error)

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
// When true, f.SentryIssueIDs is still intersected against the Sentry refs
// found in msg.Text itself (digest.Message carries no pre-extracted
// SentryRefs field the way islack.IncomingMessage does, so this re-derives
// them here with the same islack.ExtractSentryRefs used everywhere else) —
// a model-emitted ID that isn't actually present in the corroborating
// message's own text must not survive, even on an otherwise-corroborated
// thread.
// delegates is cfg.IssueTracker.Delegates, threaded through for
// applyLinearAssigneeMapping below. requesterID is always passed as ""
// (digest paths have no requester — see flowDeps.resolveRequesterIdentity's
// doc comment): a "requester" entry in a digest-sourced Findings.LinearAssignees
// simply drops, but a delegates match (e.g. "biome") still applies.
func proposeFromDigest(ctx context.Context, ticketEngine *ticket.Engine, db *state.DB, post digestPostFunc,
	f investigation.Findings, msg digest.Message, sentryCorroborated bool, delegates map[string]string) error {
	enforceCorroboration(&f, sentryCorroborated, islack.ExtractSentryRefs(msg.Text))
	applyLinearAssigneeMapping(&f, delegates, "", nil)

	investigationID := digestInvestigationID(db, msg.ThreadTS)
	decision, fileResult, err := fileOrProposeFromFindings(ctx, ticketEngine, f, msg.Channel, msg.ThreadTS, investigationID, ticket.SourceDigest)
	if err != nil {
		return err
	}

	if decision == ticket.DecisionAutoFile {
		_, err := post(msg.Channel, msg.ThreadTS, composeFiledReply(f, fileResult), false)
		return err
	}

	// See ReplyWithOptionalCTA's doc comment: the button is suppressed when
	// the tracker can't actually create issues.
	text := composeDigestProposalText(f)
	_, err = post(msg.Channel, msg.ThreadTS, text, ticketEngine.ShouldCreateIssues())
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
// fetch doesn't carry (see ticketItem's doc comment). Returns "" when there
// are no tickets with a non-blank ID.
func buildDigestTicketContextBlock(tickets []digest.TicketContext) string {
	if len(tickets) == 0 {
		return ""
	}

	items := make([]ticketItem, 0, len(tickets))
	for _, tc := range tickets {
		var comments []string
		for _, c := range tc.Comments {
			comments = append(comments, fmt.Sprintf("Comment (%s): %s", c.Author, c.Body))
		}
		items = append(items, ticketItem{
			ID:          tc.ID,
			Title:       tc.Title,
			Description: tc.Description,
			Comments:    comments,
		})
	}
	return renderTicketContextBlock(items)
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
// tainted at the source either.) When true, findings.SentryIssueIDs is still
// intersected against sentryRefs (the refs extracted from msg.Text) rather
// than kept verbatim — see enforceCorroboration's doc comment for why
// corroborated alone is not sufficient to trust arbitrary model output.
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

	// sentryRefs is the set of Sentry refs found in the digest message's own
	// text — used both as investigation leads (req.SentryRefs) and, below, as
	// enforceCorroboration's trustedRefs: the same extraction, not a second
	// independent one, so "what the model was told" and "what's trusted"
	// never drift apart.
	sentryRefs := islack.ExtractSentryRefs(msg.Text)

	req := investigation.Request{
		Text:          msg.Text,
		Category:      opp.Category,
		Confidence:    opp.Confidence,
		Summary:       opp.Summary,
		ChannelName:   msg.ChannelName,
		Keywords:      opp.Keywords,
		FilesHint:     opp.FilesHint,
		SentryRefs:    sentryRefs,
		TicketContext: buildDigestTicketContextBlock(tickets),
		Repo:          repo,
		Timeout:       timeout,
	}
	findings, err := runInvestigation(ctx, investRunner, investigateSem, req)
	if err != nil {
		return nil, err
	}

	sentryCorroborated := isSentryCorroborated(msg.BotID, botAllowlist)
	enforceCorroboration(findings, sentryCorroborated, sentryRefs)

	saveInvestigationRecord(db, generateInvestigationID(), msg.ThreadTS, msg.Channel, findings)

	return findings, nil
}
