package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/digest"
	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	islack "github.com/scaler-tech/toad/internal/slack"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/ticket"
	"github.com/scaler-tech/toad/internal/triage"
)

// ticketflowTrackerFake is a small hand-written issuetracker.Tracker fake —
// same spirit as internal/ticket/ticket_test.go's fakeTracker — that counts
// CreateIssue/PostComment calls so tests can assert which path fired without
// a live Linear API.
type ticketflowTrackerFake struct {
	issuetracker.NoopTracker
	createCalls  []issuetracker.CreateIssueOpts
	commentCalls int
	nextID       string
	// createErr, when set, is returned by CreateIssue instead of a ref —
	// exercises the FileOrUpdate create-failure path surfacing through
	// runTriggeredInvestigation as outcomeFilingFailed.
	createErr error
	// postErr, when set, is returned by PostComment instead of nil —
	// exercises preInvestigationTicketCheck's re-observation comment failing
	// without crashing the flow (it's logged and swallowed).
	postErr error
}

// ShouldCreateIssues overrides the embedded NoopTracker's false — every test
// in this file exercises a tracker meant to be capable of filing.
func (f *ticketflowTrackerFake) ShouldCreateIssues() bool { return true }

func (f *ticketflowTrackerFake) CreateIssue(_ context.Context, opts issuetracker.CreateIssueOpts) (*issuetracker.IssueRef, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.createCalls = append(f.createCalls, opts)
	id := f.nextID
	if id == "" {
		id = "TOAD-1"
	}
	return &issuetracker.IssueRef{Provider: "linear", ID: id, URL: "https://linear.app/toad/issue/" + id, Title: opts.Title}, nil
}

func (f *ticketflowTrackerFake) PostComment(context.Context, *issuetracker.IssueRef, string) error {
	f.commentCalls++
	if f.postErr != nil {
		return f.postErr
	}
	return nil
}

func newTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("opening in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestStateManager(t *testing.T, db *state.DB) *state.Manager {
	t.Helper()
	m, err := state.NewPersistentManager(db, 50)
	if err != nil {
		t.Fatalf("creating persistent manager: %v", err)
	}
	return m
}

func newTestResolver(t *testing.T) *config.Resolver {
	t.Helper()
	repo := config.RepoConfig{Name: "svc", Path: t.TempDir(), DefaultBranch: "main"}
	return config.NewResolver(nil, []config.RepoConfig{repo})
}

// setupTriggeredTest wires a minimal, fully in-memory/fake environment for
// exercising runTriggeredInvestigation directly — no live Slack client and
// no live issue tracker, per Task 6's precedent that this package has no
// Slack test double and the decision logic in ticketflow.go is deliberately
// kept Slack-client-free so it can be tested this way.
func setupTriggeredTest(t *testing.T, findingsJSON string, cfg config.TicketConfig) (
	*state.DB, *state.Manager, *ticketflowTrackerFake, *agent.MockProvider, *investigation.Runner, *ticket.Engine, chan struct{},
) {
	t.Helper()
	db := newTestDB(t)
	stateManager := newTestStateManager(t, db)
	tracker := &ticketflowTrackerFake{}
	mockProvider := &agent.MockProvider{RunResult: &agent.RunResult{Result: findingsJSON}}
	investRunner := investigation.NewRunner(mockProvider, "sonnet", "", nil, nil, nil)
	ticketEngine := ticket.New(tracker, db, cfg, nil)
	investigateSem := make(chan struct{}, 2)
	return db, stateManager, tracker, mockProvider, investRunner, ticketEngine, investigateSem
}

const highConfidenceSentryFindings = `{"feasible":true,"title":"Refund export double-counts partial refunds","problem":"p","root_cause":"rc","evidence":[],"scope":["s"],"non_goals":[],"acceptance_criteria":["ac"],"confidence":0.92,"repo":"svc","sentry_issue_ids":["BILLING-42"],"issue_id":"","files_found":[],"reasoning":"Found the root cause via Sentry stack trace."}`

const lowConfidenceSentryFindings = `{"feasible":true,"title":"Maybe a bug","problem":"p","root_cause":"rc","evidence":[],"scope":["s"],"non_goals":[],"acceptance_criteria":["ac"],"confidence":0.5,"repo":"svc","sentry_issue_ids":["BILLING-42"],"issue_id":"","files_found":[],"reasoning":"Not fully confident this is the root cause."}`

func autoFileCfg() config.TicketConfig {
	return config.TicketConfig{AutoFile: true, AutoFileConfidence: 0.85}
}

// TestComposeFiledReply_AlreadyExistedUsesLinkedWording exercises
// composeFiledReply directly (no investigation/state/tracker scaffolding
// needed): when FileResult.AlreadyExisted is true, the reply must say
// "Already tracked as", not "Filed" — wording it as a fresh filing would be
// misleading for a re-observation of a ticket that already tracked this
// exact problem (see the function's doc comment).
func TestComposeFiledReply_AlreadyExistedUsesLinkedWording(t *testing.T) {
	findings := investigation.Findings{Title: "Refund export double-counts partial refunds", Reasoning: "root cause confirmed"}
	fileResult := &ticket.FileResult{
		Ref:            &issuetracker.IssueRef{Provider: "linear", ID: "TOAD-EXISTING", URL: "https://linear.app/toad/issue/TOAD-EXISTING", Title: "Refund export double-counts partial refunds"},
		AlreadyExisted: true,
	}

	got := composeFiledReply(findings, fileResult)

	if !strings.Contains(got, "Already tracked as") {
		t.Errorf("expected reply to use the linked-existing wording, got %q", got)
	}
	if strings.Contains(got, "Filed ") {
		t.Errorf("expected reply NOT to say 'Filed' for an already-existing ticket, got %q", got)
	}
	if !strings.Contains(got, "https://linear.app/toad/issue/TOAD-EXISTING") {
		t.Errorf("expected reply to contain the existing ticket URL, got %q", got)
	}
}

// (a) sentry-corroborated (allowlisted-bot-sourced), high-confidence finding
// -> auto-file, ticket_index row created, and the composed Slack reply text
// contains the ticket URL. Fix 2 (security): corroboration requires the
// report to have arrived from an allowlisted monitoring bot — msg carries
// IsBot/BotID and sentryCorroborated=true reflects that a caller already
// checked BotID against cfg.Intake.BotAllowlist.
func TestRunTriggeredInvestigation_AutoFilesHighConfidenceSentryFinding(t *testing.T) {
	db, stateManager, tracker, _, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, highConfidenceSentryFindings, autoFileCfg())
	resolver := newTestResolver(t)

	msg := &islack.IncomingMessage{Channel: "C1", Timestamp: "100.1", SentryRefs: []string{"BILLING-42"}, Text: "users report double refunds", IsBot: true, BotID: "B_SENTRY"}
	result := &triage.Result{Category: "bug", Confidence: 0.9, Summary: "double refunds", Actionable: true}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Fatal("expected claim to succeed on a fresh thread")
	}

	outcome := runTriggeredInvestigation(context.Background(), msg, result, "eng-alerts", msg.ThreadTS(),
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, true /* sentryCorroborated */)

	if outcome.Kind != outcomeFiled {
		t.Fatalf("expected outcomeFiled, got %v (reply: %q)", outcome.Kind, outcome.ReplyText)
	}
	if !strings.Contains(outcome.ReplyText, "https://linear.app/toad/issue/TOAD-1") {
		t.Errorf("expected reply to contain the filed ticket URL, got %q", outcome.ReplyText)
	}
	if len(tracker.createCalls) != 1 {
		t.Fatalf("expected exactly 1 CreateIssue call, got %d", len(tracker.createCalls))
	}

	entry, err := db.GetTicketIndex("sentry:BILLING-42")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected a ticket_index row for sentry:BILLING-42, got none")
	}
	if entry.IssueID != "TOAD-1" {
		t.Errorf("expected ticket_index.issue_id = TOAD-1, got %q", entry.IssueID)
	}
	if entry.Source != string(ticket.SourceAuto) {
		t.Errorf("expected ticket_index.source = %q for the triggered auto-file path, got %q", ticket.SourceAuto, entry.Source)
	}

	// Claim must be released on the filed-and-replied path.
	if !stateManager.Claim(msg.ThreadTS()) {
		t.Error("expected claim to be released after a filed outcome")
	}
}

// (b) low-confidence finding -> proposed to a human, with TicketBlocks
// producing real Slack blocks for the composed reply.
func TestRunTriggeredInvestigation_ProposesLowConfidenceFinding(t *testing.T) {
	_, stateManager, tracker, _, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, lowConfidenceSentryFindings, autoFileCfg())
	resolver := newTestResolver(t)

	msg := &islack.IncomingMessage{Channel: "C2", Timestamp: "200.1", SentryRefs: []string{"BILLING-42"}, Text: "maybe a bug?"}
	result := &triage.Result{Category: "bug", Confidence: 0.9, Summary: "maybe a bug", Actionable: true}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Fatal("expected claim to succeed on a fresh thread")
	}

	outcome := runTriggeredInvestigation(context.Background(), msg, result, "eng-alerts", msg.ThreadTS(),
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, false /* sentryCorroborated */)

	if outcome.Kind != outcomeProposed {
		t.Fatalf("expected outcomeProposed, got %v (reply: %q)", outcome.Kind, outcome.ReplyText)
	}
	if len(tracker.createCalls) != 0 {
		t.Errorf("expected no CreateIssue call on a proposed outcome, got %d", len(tracker.createCalls))
	}
	// TicketBlocks always renders a fixed 2-block layout regardless of input,
	// so asserting len(blocks) != 0 on it is vacuous — assert the actual
	// reply text (what the propose path is responsible for composing).
	if outcome.ReplyText != "Not fully confident this is the root cause." {
		t.Errorf("expected reply text to be the finding's Reasoning, got %q", outcome.ReplyText)
	}
}

// (c) a duplicate sentry key already tracked in the ticket index short-
// circuits before any investigation runs: no second (or first) agent Run
// call, a re-observation comment is posted, and the reply links the
// existing ticket. Requires bot corroboration (Fix 2): the sentry-key
// idempotency pre-check is only trusted for an allowlisted-bot-sourced
// report — see TestRunTriggeredInvestigation_HumanPastedSentryIDSkipsIdempotencyPreCheck
// for the non-corroborated case.
func TestRunTriggeredInvestigation_DuplicateSentryKeySkipsInvestigation(t *testing.T) {
	db, stateManager, tracker, mockProvider, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, highConfidenceSentryFindings, autoFileCfg())
	resolver := newTestResolver(t)

	if err := db.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey: "sentry:BILLING-42",
		IssueID:     "TOAD-EXISTING",
		IssueURL:    "https://linear.app/toad/issue/TOAD-EXISTING",
		Source:      "auto",
		CreatedAt:   time.Now(),
		LastSeenAt:  time.Now(),
	}); err != nil {
		t.Fatalf("seeding ticket index: %v", err)
	}

	msg := &islack.IncomingMessage{Channel: "C3", Timestamp: "300.1", SentryRefs: []string{"BILLING-42"}, Text: "same issue again", IsBot: true, BotID: "B_SENTRY"}
	result := &triage.Result{Category: "bug", Confidence: 0.9, Summary: "same issue again", Actionable: true}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Fatal("expected claim to succeed on a fresh thread")
	}

	outcome := runTriggeredInvestigation(context.Background(), msg, result, "eng-alerts", msg.ThreadTS(),
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, true /* sentryCorroborated */)

	if outcome.Kind != outcomeIdempotentHit {
		t.Fatalf("expected outcomeIdempotentHit, got %v (reply: %q)", outcome.Kind, outcome.ReplyText)
	}
	if !strings.Contains(outcome.ReplyText, "TOAD-EXISTING") {
		t.Errorf("expected reply to link the existing ticket, got %q", outcome.ReplyText)
	}
	if len(mockProvider.RunCalls) != 0 {
		t.Errorf("expected no investigation agent Run call on an idempotency hit, got %d", len(mockProvider.RunCalls))
	}
	if tracker.commentCalls != 1 {
		t.Errorf("expected exactly 1 re-observation PostComment call, got %d", tracker.commentCalls)
	}
	if len(tracker.createCalls) != 0 {
		t.Errorf("expected no CreateIssue call on an idempotency hit, got %d", len(tracker.createCalls))
	}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Error("expected claim to be released after an idempotent-hit outcome")
	}
}

// An investigation agent failure (agent.MockProvider.RunErr, surfaced by
// investigation.Runner.Run as a wrapped error — see
// TestRun_AgentFailureWrapsError in internal/investigation) must make
// runTriggeredInvestigation fall through to the ribbit path rather than
// erroring out or crashing: no ticket filed, no reply composed here (the
// caller's ribbit path takes over), and the thread claim still released.
func TestRunTriggeredInvestigation_FallsThroughToRibbitOnInvestigationError(t *testing.T) {
	db := newTestDB(t)
	stateManager := newTestStateManager(t, db)
	tracker := &ticketflowTrackerFake{}
	mockProvider := &agent.MockProvider{RunErr: errors.New("claude cli exited 1")}
	investRunner := investigation.NewRunner(mockProvider, "sonnet", "", nil, nil, nil)
	ticketEngine := ticket.New(tracker, db, autoFileCfg(), nil)
	investigateSem := make(chan struct{}, 2)
	resolver := newTestResolver(t)

	msg := &islack.IncomingMessage{Channel: "C13", Timestamp: "1300.1", Text: "something broke"}
	result := &triage.Result{Category: "bug", Confidence: 0.9, Summary: "something broke", Actionable: true}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Fatal("expected claim to succeed on a fresh thread")
	}

	outcome := runTriggeredInvestigation(context.Background(), msg, result, "eng-alerts", msg.ThreadTS(),
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, false /* sentryCorroborated */)

	if outcome.Kind != outcomeFallThrough {
		t.Fatalf("expected outcomeFallThrough on an investigation agent error, got %v (reply: %q)", outcome.Kind, outcome.ReplyText)
	}
	if len(tracker.createCalls) != 0 {
		t.Errorf("expected no CreateIssue call when the investigation itself errored, got %d", len(tracker.createCalls))
	}
	if !stateManager.Claim(msg.ThreadTS()) {
		t.Error("expected claim to be released after a fall-through outcome")
	}
}

// (d) FileOrUpdate's CreateIssue failure must surface as outcomeFilingFailed
// with a user-facing ":x: ... couldn't file" reply that carries both the
// underlying error and the investigation's own reasoning — not a crash and
// not a silently-dropped report.
func TestRunTriggeredInvestigation_CreateIssueErrorSurfacesAsFilingFailed(t *testing.T) {
	_, stateManager, tracker, _, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, highConfidenceSentryFindings, autoFileCfg())
	resolver := newTestResolver(t)
	tracker.createErr = errors.New("linear api unavailable")

	msg := &islack.IncomingMessage{Channel: "C11", Timestamp: "1100.1", SentryRefs: []string{"BILLING-42"}, Text: "users report double refunds", IsBot: true, BotID: "B_SENTRY"}
	result := &triage.Result{Category: "bug", Confidence: 0.9, Summary: "double refunds", Actionable: true}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Fatal("expected claim to succeed on a fresh thread")
	}

	outcome := runTriggeredInvestigation(context.Background(), msg, result, "eng-alerts", msg.ThreadTS(),
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, true /* sentryCorroborated */)

	if outcome.Kind != outcomeFilingFailed {
		t.Fatalf("expected outcomeFilingFailed, got %v (reply: %q)", outcome.Kind, outcome.ReplyText)
	}
	if !strings.Contains(outcome.ReplyText, ":x:") || !strings.Contains(outcome.ReplyText, "couldn't file a ticket") {
		t.Errorf("expected a user-facing filing-failure reply, got %q", outcome.ReplyText)
	}
	if !strings.Contains(outcome.ReplyText, "linear api unavailable") {
		t.Errorf("expected the reply to surface the underlying error, got %q", outcome.ReplyText)
	}
	if !strings.Contains(outcome.ReplyText, "Found the root cause via Sentry stack trace.") {
		t.Errorf("expected the reply to still carry the investigation's reasoning, got %q", outcome.ReplyText)
	}
	if outcome.Findings == nil {
		t.Error("expected Findings to be populated on a filing-failed outcome")
	}

	// Claim must still be released even though filing failed.
	if !stateManager.Claim(msg.ThreadTS()) {
		t.Error("expected claim to be released after a filing-failed outcome")
	}
}

// (e) A PostComment failure during the idempotency pre-check's re-observation
// comment must not crash or otherwise derail the flow — preInvestigationTicketCheck
// only logs it and still returns the existing entry, so the caller must still
// report an idempotent hit linking the existing ticket.
func TestRunTriggeredInvestigation_ReObservationPostCommentErrorDoesNotCrashFlow(t *testing.T) {
	db, stateManager, tracker, mockProvider, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, highConfidenceSentryFindings, autoFileCfg())
	resolver := newTestResolver(t)
	tracker.postErr = errors.New("linear comment api unavailable")

	if err := db.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey: "sentry:BILLING-42",
		IssueID:     "TOAD-EXISTING",
		IssueURL:    "https://linear.app/toad/issue/TOAD-EXISTING",
		Source:      "auto",
		CreatedAt:   time.Now(),
		LastSeenAt:  time.Now(),
	}); err != nil {
		t.Fatalf("seeding ticket index: %v", err)
	}

	msg := &islack.IncomingMessage{Channel: "C12", Timestamp: "1200.1", SentryRefs: []string{"BILLING-42"}, Text: "same issue again", IsBot: true, BotID: "B_SENTRY"}
	result := &triage.Result{Category: "bug", Confidence: 0.9, Summary: "same issue again", Actionable: true}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Fatal("expected claim to succeed on a fresh thread")
	}

	outcome := runTriggeredInvestigation(context.Background(), msg, result, "eng-alerts", msg.ThreadTS(),
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, true /* sentryCorroborated */)

	if outcome.Kind != outcomeIdempotentHit {
		t.Fatalf("expected outcomeIdempotentHit despite the PostComment error, got %v (reply: %q)", outcome.Kind, outcome.ReplyText)
	}
	if !strings.Contains(outcome.ReplyText, "TOAD-EXISTING") {
		t.Errorf("expected reply to still link the existing ticket, got %q", outcome.ReplyText)
	}
	if len(mockProvider.RunCalls) != 0 {
		t.Errorf("expected no investigation agent Run call on an idempotency hit, got %d", len(mockProvider.RunCalls))
	}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Error("expected claim to be released even though the re-observation comment failed")
	}
}

// Fix 2 (security) regression: a plain human message that happens to
// mention/paste a Sentry-looking reference — with an investigation whose
// Findings.SentryIssueIDs the model populated the same way a genuinely
// corroborated run would — must NOT be treated as externally corroborated.
// A human user must not be able to use toad as a ticket-existence oracle by
// guessing/pasting a Sentry key (the idempotency pre-check must skip the
// sentry-key path entirely), and a subsequent explicit human filing (CTA)
// of the resulting proposal must dedup-key on the Slack thread, never on
// the unverified Sentry ID.
func TestRunTriggeredInvestigation_HumanPastedSentryIDDoesNotAutoFile(t *testing.T) {
	db, stateManager, tracker, _, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, highConfidenceSentryFindings, autoFileCfg())
	resolver := newTestResolver(t)

	msg := &islack.IncomingMessage{Channel: "C10", Timestamp: "1000.1", SentryRefs: []string{"BILLING-42"}, Text: "hey saw BILLING-42 again, users report double refunds"}
	result := &triage.Result{Category: "bug", Confidence: 0.9, Summary: "double refunds", Actionable: true}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Fatal("expected claim to succeed on a fresh thread")
	}

	outcome := runTriggeredInvestigation(context.Background(), msg, result, "eng-alerts", msg.ThreadTS(),
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, false /* sentryCorroborated */)

	if outcome.Kind != outcomeProposed {
		t.Fatalf("expected outcomeProposed for a non-corroborated Sentry ID, got %v (reply: %q)", outcome.Kind, outcome.ReplyText)
	}
	if len(tracker.createCalls) != 0 {
		t.Errorf("expected no CreateIssue call for a non-corroborated finding, got %d", len(tracker.createCalls))
	}

	// A human later explicitly files it via the CTA, reusing the saved
	// finding — must dedup-key on the thread, not the unverified sentry ID.
	outcome2 := runTicketRequest(context.Background(), msg, nil, stateManager, tracker, resolver,
		investRunner, ticketEngine, investigateSem, "eng-alerts", msg.ThreadTS(), ticket.SourceCTA, false /* sentryCorroborated */)
	if outcome2.Err != nil {
		t.Fatalf("unexpected error filing via CTA: %v", outcome2.Err)
	}

	if entry, err := db.GetTicketIndex("sentry:BILLING-42"); err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	} else if entry != nil {
		t.Errorf("expected no ticket_index row keyed by the unverified sentry ID, got %+v", entry)
	}

	threadKey := "thread:" + msg.Channel + ":" + msg.ThreadTS()
	entry, err := db.GetTicketIndex(threadKey)
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatalf("expected a ticket_index row keyed by thread %q, got none", threadKey)
	}
}

// Task 9's carried finding: investigation.Runner only slog.Warns on a repo
// sync failure and otherwise proceeds silently against a possibly-stale
// checkout. Runner.Run is supposed to surface that via the returned
// Findings.RepoSyncFailed so runInvestigation can append a one-line
// staleness caveat to the findings text posted to Slack — this exercises
// the real SyncRepoNow against a real (non-git) temp dir, so its "git fetch"
// genuinely fails rather than faking the syncer.
func TestRunInvestigation_AppendsStalenessCaveatOnSyncFailure(t *testing.T) {
	mockProvider := &agent.MockProvider{RunResult: &agent.RunResult{Result: highConfidenceSentryFindings}}
	investRunner := investigation.NewRunner(mockProvider, "sonnet", "", nil, SyncRepoNow, nil)
	investigateSem := make(chan struct{}, 1)
	repo := config.RepoConfig{Name: "svc", Path: t.TempDir(), DefaultBranch: "main"} // not a git repo -> fetch fails
	req := investigation.Request{Text: "x", Repo: &repo, Timeout: time.Minute}

	findings, err := runInvestigation(context.Background(), investRunner, investigateSem, req)
	if err != nil {
		t.Fatalf("runInvestigation: %v", err)
	}
	if !findings.RepoSyncFailed {
		t.Errorf("expected findings.RepoSyncFailed to be true after a failed sync")
	}
	if !strings.Contains(findings.Reasoning, "repo sync failed before this investigation") {
		t.Errorf("expected staleness caveat appended to Reasoning, got %q", findings.Reasoning)
	}
}

// (a) A CTA click that reuses a previously-saved, non-corroborated finding
// (one that would have been DecisionPropose under the auto-file gate) must
// still file a ticket directly — the click itself is the human sign-off, so
// runTicketRequest must NOT re-run Decide (review Critical finding: routing
// this through Decide made the button a permanent no-op for any finding
// that wasn't already Sentry-corroborated and high-confidence).
func TestRunTicketRequest_FilesDirectlyForPreviouslyProposedFinding(t *testing.T) {
	db, stateManager, tracker, mockProvider, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, "", autoFileCfg())
	resolver := newTestResolver(t)

	findings := &investigation.Findings{
		Feasible: true, Title: "Some bug", Problem: "p", RootCause: "rc",
		Confidence: 0.5, Repo: "svc", Reasoning: "root cause found, no Sentry corroboration",
	}
	msg := &islack.IncomingMessage{Channel: "C5", Timestamp: "500.1", Text: "please file a ticket for this"}
	saveInvestigationRecord(db, "invest-test-1", msg.ThreadTS(), msg.Channel, findings)

	outcome := runTicketRequest(context.Background(), msg, nil, stateManager, tracker, resolver,
		investRunner, ticketEngine, investigateSem, "eng-alerts", msg.ThreadTS(), ticket.SourceCTA, false /* sentryCorroborated */)

	if outcome.Err != nil {
		t.Fatalf("unexpected error: %v", outcome.Err)
	}
	if outcome.Conflict {
		t.Fatal("did not expect a claim conflict on a fresh thread")
	}
	if len(tracker.createCalls) != 1 {
		t.Fatalf("expected exactly 1 CreateIssue call, got %d", len(tracker.createCalls))
	}
	if !strings.Contains(outcome.ReplyText, "https://linear.app/toad/issue/TOAD-1") {
		t.Errorf("expected reply to contain the filed ticket URL, got %q", outcome.ReplyText)
	}
	// The saved (reused) finding was used directly — no fresh investigation
	// agent run.
	if len(mockProvider.RunCalls) != 0 {
		t.Errorf("expected no investigation agent Run call when reusing a saved finding, got %d", len(mockProvider.RunCalls))
	}

	entry, err := db.GetTicketIndex("thread:" + msg.Channel + ":" + msg.ThreadTS())
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected a ticket_index row for the filed ticket")
	}
	if entry.Source != string(ticket.SourceCTA) {
		t.Errorf("expected ticket_index.source = %q for the CTA path, got %q", ticket.SourceCTA, entry.Source)
	}
}

// (b) An escalation (or CTA) request on a thread that's already claimed
// (a concurrent investigation/ticket-request in flight) must conflict
// immediately — no investigation run, no ticket filed.
func TestRunTicketRequest_ConflictWhenThreadAlreadyClaimed(t *testing.T) {
	_, stateManager, tracker, mockProvider, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, highConfidenceSentryFindings, autoFileCfg())
	resolver := newTestResolver(t)

	msg := &islack.IncomingMessage{Channel: "C6", Timestamp: "600.1", Text: "escalate this"}
	threadTS := msg.ThreadTS()
	if !stateManager.Claim(threadTS) {
		t.Fatal("expected initial claim to succeed")
	}
	// Deliberately not unclaiming, to simulate a concurrent flow already
	// holding this thread's claim.

	outcome := runTicketRequest(context.Background(), msg, nil, stateManager, tracker, resolver,
		investRunner, ticketEngine, investigateSem, "eng-alerts", threadTS, ticket.SourceEscalation, false /* sentryCorroborated */)

	if !outcome.Conflict {
		t.Fatalf("expected Conflict outcome, got %+v", outcome)
	}
	if len(mockProvider.RunCalls) != 0 {
		t.Errorf("expected no investigation agent Run call on a claim conflict, got %d", len(mockProvider.RunCalls))
	}
	if len(tracker.createCalls) != 0 {
		t.Errorf("expected no CreateIssue call on a claim conflict, got %d", len(tracker.createCalls))
	}
}

// (f) The success path for allowlisted-bot intake: an actionable bug,
// triaged above the confidence floor, with a bot-corroborated Sentry
// reference, must run the investigate-and-file flow end to end — a single
// investigation agent Run call, an auto-filed ticket, and a sentry-keyed
// ticket_index row (not just a thread-keyed one, since this report IS
// externally corroborated).
func TestRunBotIntake_ActionableCorroboratedBugAutoFiles(t *testing.T) {
	db, stateManager, tracker, mockProvider, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, highConfidenceSentryFindings, autoFileCfg())
	resolver := newTestResolver(t)

	triageProvider := &agent.MockProvider{RunResult: &agent.RunResult{
		Result: `{"actionable":true,"confidence":0.9,"summary":"double refunds","category":"bug","estimated_size":"small"}`,
	}}
	triageEngine := triage.New(triageProvider, "haiku", nil)

	msg := &islack.IncomingMessage{Channel: "C14", Timestamp: "1400.1", SentryRefs: []string{"BILLING-42"}, Text: "users report double refunds", IsBot: true, BotID: "B_SENTRY"}

	outcome := runBotIntake(context.Background(), msg, triageEngine, "eng-alerts",
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, true /* sentryCorroborated */)

	if outcome == nil {
		t.Fatal("expected a non-nil outcome for an actionable, corroborated bug")
	}
	if outcome.Kind != outcomeFiled {
		t.Fatalf("expected outcomeFiled, got %v (reply: %q)", outcome.Kind, outcome.ReplyText)
	}
	if len(mockProvider.RunCalls) != 1 {
		t.Errorf("expected exactly 1 investigation agent Run call, got %d", len(mockProvider.RunCalls))
	}
	if len(tracker.createCalls) != 1 {
		t.Fatalf("expected exactly 1 CreateIssue call, got %d", len(tracker.createCalls))
	}

	entry, err := db.GetTicketIndex("sentry:BILLING-42")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected a sentry-keyed ticket_index row for a corroborated bot-intake filing, got none")
	}
	if entry.Source != string(ticket.SourceAuto) {
		t.Errorf("expected ticket_index.source = %q for bot-intake auto-filing, got %q", ticket.SourceAuto, entry.Source)
	}
}

// (c) An allowlisted bot message that triages as "question" (not bug/
// feature) must be silently dropped: no investigation run.
func TestRunBotIntake_DropsQuestionCategoryNoInvestigation(t *testing.T) {
	_, stateManager, tracker, mockProvider, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, highConfidenceSentryFindings, autoFileCfg())
	resolver := newTestResolver(t)

	triageProvider := &agent.MockProvider{RunResult: &agent.RunResult{
		Result: `{"actionable":true,"confidence":0.9,"summary":"just curious","category":"question","estimated_size":"small"}`,
	}}
	triageEngine := triage.New(triageProvider, "haiku", nil)

	msg := &islack.IncomingMessage{Channel: "C7", Timestamp: "700.1", BotID: "B123", IsBot: true, Text: "what's the deploy status?"}

	outcome := runBotIntake(context.Background(), msg, triageEngine, "eng-alerts",
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, true /* sentryCorroborated */)

	if outcome != nil {
		t.Fatalf("expected nil outcome (dropped) for a question-triaged bot message, got %+v", outcome)
	}
	if len(mockProvider.RunCalls) != 0 {
		t.Errorf("expected no investigation agent Run call for a dropped bot message, got %d", len(mockProvider.RunCalls))
	}
	if len(tracker.createCalls) != 0 {
		t.Errorf("expected no CreateIssue call for a dropped bot message, got %d", len(tracker.createCalls))
	}
}

// reuseRecentInvestigation must not resurrect an infeasible saved finding —
// a CTA click after an infeasible fallthrough must trigger a fresh
// investigation instead of filing a ticket from a "no real fix found here"
// verdict (review Critical finding, second half).
func TestReuseRecentInvestigation_SkipsInfeasibleFindings(t *testing.T) {
	db := newTestDB(t)
	findings := &investigation.Findings{Feasible: false, Reasoning: "could not find a root cause"}
	saveInvestigationRecord(db, "invest-infeasible-1", "800.1", "C8", findings)

	f, id := reuseRecentInvestigation(db, "800.1")
	if f != nil || id != "" {
		t.Fatalf("expected an infeasible saved finding to be skipped, got findings=%+v id=%q", f, id)
	}
}

// digestPostCall records a single invocation of a fake digestPostFunc, so
// tests can assert on what proposeFromDigest posted without a live Slack
// client.
type digestPostCall struct {
	channel, threadTS, text string
	showCTA                 bool
}

// (a) A sentry-corroborated, high-confidence finding must auto-file (via
// ticketEngine.FileOrUpdate with source digest) and post a plain thread
// notice containing the filed ticket's URL — no TicketBlocks, since there's
// no human decision left to make. Fix 2 (security): corroboration requires
// msg.BotID to be an allowlisted monitoring bot — the caller (root.go)
// computes sentryCorroborated from that, passed here as true.
func TestProposeFromDigest_AutoFilesSentryCorroboratedFinding(t *testing.T) {
	db := newTestDB(t)
	tracker := &ticketflowTrackerFake{}
	ticketEngine := ticket.New(tracker, db, autoFileCfg(), nil)

	f := investigation.Findings{
		Feasible:       true,
		Title:          "Refund export double-counts partial refunds",
		Problem:        "p",
		RootCause:      "rc",
		Confidence:     0.92,
		Repo:           "svc",
		SentryIssueIDs: []string{"BILLING-42"},
		Reasoning:      "Found the root cause via Sentry stack trace.",
	}
	msg := digest.Message{Channel: "C1", ThreadTS: "100.1", Text: "double refunds", BotID: "B_SENTRY"}

	var calls []digestPostCall
	post := func(channel, threadTS, text string, showCTA bool) (string, error) {
		calls = append(calls, digestPostCall{channel, threadTS, text, showCTA})
		return "999.1", nil
	}

	if err := proposeFromDigest(context.Background(), ticketEngine, db, post, f, msg, true /* sentryCorroborated */); err != nil {
		t.Fatalf("proposeFromDigest: %v", err)
	}

	if len(tracker.createCalls) != 1 {
		t.Fatalf("expected exactly 1 CreateIssue call, got %d", len(tracker.createCalls))
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 posted notice, got %d", len(calls))
	}
	if !strings.Contains(calls[0].text, "https://linear.app/toad/issue/TOAD-1") {
		t.Errorf("expected posted notice to contain the filed ticket URL, got %q", calls[0].text)
	}
	if calls[0].showCTA {
		t.Errorf("expected a plain reply (no TicketBlocks/CTA) for an auto-filed notice")
	}

	entry, err := db.GetTicketIndex("sentry:BILLING-42")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected a ticket_index row for sentry:BILLING-42, got none")
	}
	if entry.Source != string(ticket.SourceDigest) {
		t.Errorf("expected ticket_index.source = %q for the digest auto-file path, got %q", ticket.SourceDigest, entry.Source)
	}
}

// (b) A non-corroborated (no Sentry IDs, or below the confidence floor)
// finding must NOT auto-file: no CreateIssue call, and the posted notice
// carries TicketBlocks (the CTA button) with digest-appropriate "spotted
// while monitoring" copy — the v1 ":crown:" spawn-announcement text this
// replaces described a tadpole being spawned, which no longer happens here.
func TestProposeFromDigest_ProposesNonCorroboratedFinding(t *testing.T) {
	db := newTestDB(t)
	tracker := &ticketflowTrackerFake{}
	ticketEngine := ticket.New(tracker, db, autoFileCfg(), nil)

	f := investigation.Findings{
		Feasible:   true,
		Title:      "Maybe a bug",
		Problem:    "p",
		RootCause:  "rc",
		Confidence: 0.5,
		Repo:       "svc",
		Reasoning:  "Not fully confident this is the root cause.",
	}
	msg := digest.Message{Channel: "C2", ThreadTS: "200.1", Text: "maybe a bug?"}

	var calls []digestPostCall
	post := func(channel, threadTS, text string, showCTA bool) (string, error) {
		calls = append(calls, digestPostCall{channel, threadTS, text, showCTA})
		return "999.2", nil
	}

	if err := proposeFromDigest(context.Background(), ticketEngine, db, post, f, msg, false /* sentryCorroborated */); err != nil {
		t.Fatalf("proposeFromDigest: %v", err)
	}

	if len(tracker.createCalls) != 0 {
		t.Errorf("expected no CreateIssue call for a non-corroborated finding, got %d", len(tracker.createCalls))
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 posted notice, got %d", len(calls))
	}
	if !calls[0].showCTA {
		t.Error("expected TicketBlocks/CTA to be attached to the proposed notice")
	}
	if !strings.Contains(calls[0].text, "Spotted while monitoring") {
		t.Errorf("expected digest-appropriate copy replacing the v1 :crown: spawn announcement, got %q", calls[0].text)
	}
	if !strings.Contains(calls[0].text, "Not fully confident this is the root cause.") {
		t.Errorf("expected the finding's reasoning in the posted text, got %q", calls[0].text)
	}
}

// (c) The digest Investigate closure must resolve a repo before ever
// running the investigation agent — an unresolvable repo (no configured
// repos) returns an error, with zero agent Run calls, mirroring
// runTicketRequest's identical "could not resolve a repo" guard for the
// CTA path.
func TestInvestigateFromDigest_UnresolvableRepoReturnsError(t *testing.T) {
	resolver := config.NewResolver(nil, nil) // no repos configured at all
	mockProvider := &agent.MockProvider{RunResult: &agent.RunResult{Result: highConfidenceSentryFindings}}
	investRunner := investigation.NewRunner(mockProvider, "sonnet", "", nil, nil, nil)
	investigateSem := make(chan struct{}, 1)

	opp := digest.Opportunity{Summary: "fix it", Category: "bug", Confidence: 0.9}
	msg := digest.Message{Channel: "C3", ThreadTS: "300.1", Text: "something broke"}

	findings, err := investigateFromDigest(context.Background(), resolver, investRunner, investigateSem, nil, 600, opp, msg, nil, nil)
	if err == nil {
		t.Fatal("expected an error for an unresolvable repo")
	}
	if findings != nil {
		t.Errorf("expected nil findings on error, got %+v", findings)
	}
	if len(mockProvider.RunCalls) != 0 {
		t.Errorf("expected no investigation agent Run call when the repo can't be resolved, got %d", len(mockProvider.RunCalls))
	}
}

// Review round 2 finding: a ticket_index row filed from the digest
// auto-file path previously carried no investigation backlink at all
// (proposeFromDigest hard-coded investigationID=""), unlike every
// Slack-thread-originated ticket — silently degrading
// FindInvestigationByTicket (and the upcoming MCP investigations tool) for
// the digest path. This exercises investigateFromDigest and
// proposeFromDigest together, sharing one db and thread, and asserts the
// filed ticket's investigation_id is non-empty and resolves back to the
// investigation that produced it.
func TestDigestFlow_AutoFiledTicketBacklinksInvestigation(t *testing.T) {
	db := newTestDB(t)
	tracker := &ticketflowTrackerFake{}
	ticketEngine := ticket.New(tracker, db, autoFileCfg(), nil)
	resolver := newTestResolver(t)

	mockProvider := &agent.MockProvider{RunResult: &agent.RunResult{Result: highConfidenceSentryFindings}}
	investRunner := investigation.NewRunner(mockProvider, "sonnet", "", nil, nil, nil)
	investigateSem := make(chan struct{}, 1)

	opp := digest.Opportunity{Summary: "fix refunds", Category: "bug", Confidence: 0.99, Repo: "svc"}
	msg := digest.Message{Channel: "C9", ThreadTS: "900.1", ChannelName: "errors", Text: "users report double refunds", BotID: "B_SENTRY"}

	// B_SENTRY is allowlisted, so investigateFromDigest must NOT clear
	// findings.SentryIssueIDs before saving the InvestigationRecord.
	findings, err := investigateFromDigest(context.Background(), resolver, investRunner, investigateSem, db, 600, opp, msg, nil, []string{"B_SENTRY"})
	if err != nil {
		t.Fatalf("investigateFromDigest: %v", err)
	}

	var calls []digestPostCall
	post := func(channel, threadTS, text string, showCTA bool) (string, error) {
		calls = append(calls, digestPostCall{channel, threadTS, text, showCTA})
		return "999.9", nil
	}

	// sentryCorroborated=true: msg.BotID is the allowlisted monitoring bot
	// that produced this report (Fix 2).
	if err := proposeFromDigest(context.Background(), ticketEngine, db, post, *findings, msg, true /* sentryCorroborated */); err != nil {
		t.Fatalf("proposeFromDigest: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 posted notice, got %d", len(calls))
	}
	if len(tracker.createCalls) != 1 {
		t.Fatalf("expected exactly 1 CreateIssue call, got %d", len(tracker.createCalls))
	}

	entry, err := db.GetTicketIndex("sentry:BILLING-42")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected a ticket_index row for sentry:BILLING-42, got none")
	}
	if entry.InvestigationID == "" {
		t.Fatal("expected ticket_index.investigation_id to be non-empty for a digest-filed ticket")
	}

	rec, err := db.FindInvestigationByTicket(entry.IssueID)
	if err != nil {
		t.Fatalf("FindInvestigationByTicket: %v", err)
	}
	if rec == nil {
		t.Fatal("expected FindInvestigationByTicket to resolve the investigation behind the filed ticket, got nil")
	}
	if rec.ThreadTS != msg.ThreadTS {
		t.Errorf("expected the resolved investigation's ThreadTS = %q, got %q", msg.ThreadTS, rec.ThreadTS)
	}
}

// Fix 2 defense-in-depth regression (re-review finding: "live resurrection
// gap"): a digest-batched, non-bot message that happens to reference a
// Sentry ID must not have that ID survive from investigateFromDigest's
// saved InvestigationRecord through to a later CTA click. Two layers are
// exercised end to end here: (1) investigateFromDigest must clear
// findings.SentryIssueIDs before saveInvestigationRecord when the digest
// message isn't bot-corroborated, and (2) runTicketRequest must re-apply
// the clear on a REUSED record regardless (a record could predate/bypass
// layer 1, or arrive from a differently-gated path) — this test exercises
// both by not disabling either layer, so a regression in just one would
// still be caught by the other, but the assertions target the final
// observable outcome: no sentry-keyed ticket_index row, only a thread-keyed
// one.
func TestDigestSaveThenCTAReuse_NonCorroboratedClearsSentryID(t *testing.T) {
	db := newTestDB(t)
	tracker := &ticketflowTrackerFake{}
	ticketEngine := ticket.New(tracker, db, autoFileCfg(), nil)
	resolver := newTestResolver(t)
	investigateSem := make(chan struct{}, 1)

	mockProvider := &agent.MockProvider{RunResult: &agent.RunResult{Result: highConfidenceSentryFindings}}
	investRunner := investigation.NewRunner(mockProvider, "sonnet", "", nil, nil, nil)

	opp := digest.Opportunity{Summary: "double refunds", Category: "bug", Confidence: 0.99, Repo: "svc"}
	// No BotID: a human message the digest batched, not an allowlisted-bot
	// report — even though its text pastes a Sentry-looking reference and
	// the investigation's Findings (highConfidenceSentryFindings) surfaces
	// sentry_issue_ids the same way a genuinely corroborated run would.
	digestMsg := digest.Message{Channel: "C20", ThreadTS: "2000.1", ChannelName: "eng-alerts", Text: "hey saw BILLING-42 again, users report double refunds"}

	// B_SENTRY is allowlisted, but digestMsg.BotID is empty, so this report
	// stays non-corroborated regardless.
	findings, err := investigateFromDigest(context.Background(), resolver, investRunner, investigateSem, db, 600, opp, digestMsg, nil, []string{"B_SENTRY"})
	if err != nil {
		t.Fatalf("investigateFromDigest: %v", err)
	}
	if findings.SentryIssueIDs != nil {
		t.Fatalf("expected investigateFromDigest to clear SentryIssueIDs for a non-corroborated digest message, got %+v", findings.SentryIssueIDs)
	}
	if len(mockProvider.RunCalls) != 1 {
		t.Fatalf("expected exactly 1 investigation agent Run call from investigateFromDigest, got %d", len(mockProvider.RunCalls))
	}

	// A human later clicks the CTA on the same thread; reuseRecentInvestigation
	// (inside runTicketRequest) picks up the record investigateFromDigest just
	// saved instead of running a fresh investigation.
	ctaMsg := &islack.IncomingMessage{Channel: digestMsg.Channel, Timestamp: digestMsg.ThreadTS}
	outcome := runTicketRequest(context.Background(), ctaMsg, nil, newTestStateManager(t, db), tracker, resolver,
		investRunner, ticketEngine, investigateSem, "eng-alerts", ctaMsg.ThreadTS(), ticket.SourceCTA, false /* sentryCorroborated */)
	if outcome.Err != nil {
		t.Fatalf("unexpected error filing via CTA: %v", outcome.Err)
	}
	if len(mockProvider.RunCalls) != 1 {
		t.Errorf("expected the CTA click to reuse the saved investigation (no second Run call), got %d total", len(mockProvider.RunCalls))
	}

	if entry, err := db.GetTicketIndex("sentry:BILLING-42"); err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	} else if entry != nil {
		t.Errorf("expected no ticket_index row keyed by the unverified sentry ID, got %+v", entry)
	}

	threadKey := "thread:" + ctaMsg.Channel + ":" + ctaMsg.ThreadTS()
	entry, err := db.GetTicketIndex(threadKey)
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatalf("expected a ticket_index row keyed by thread %q, got none", threadKey)
	}
}

// Inverse of the above: an allowlisted-bot-sourced digest message's Sentry
// ID DOES survive into the saved record (layer 1 keeps it), and DOES drive
// a sentry-keyed ticket when later filed via a corroborated CTA request
// (layer 2 doesn't strip what's genuinely corroborated).
func TestDigestSaveThenCTAReuse_CorroboratedKeepsSentryID(t *testing.T) {
	db := newTestDB(t)
	tracker := &ticketflowTrackerFake{}
	ticketEngine := ticket.New(tracker, db, autoFileCfg(), nil)
	resolver := newTestResolver(t)
	investigateSem := make(chan struct{}, 1)

	mockProvider := &agent.MockProvider{RunResult: &agent.RunResult{Result: highConfidenceSentryFindings}}
	investRunner := investigation.NewRunner(mockProvider, "sonnet", "", nil, nil, nil)

	opp := digest.Opportunity{Summary: "double refunds", Category: "bug", Confidence: 0.99, Repo: "svc"}
	digestMsg := digest.Message{Channel: "C21", ThreadTS: "2100.1", ChannelName: "eng-alerts", Text: "double refunds", BotID: "B_SENTRY"}

	findings, err := investigateFromDigest(context.Background(), resolver, investRunner, investigateSem, db, 600, opp, digestMsg, nil, []string{"B_SENTRY"})
	if err != nil {
		t.Fatalf("investigateFromDigest: %v", err)
	}
	if len(findings.SentryIssueIDs) == 0 {
		t.Fatal("expected investigateFromDigest to keep SentryIssueIDs for an allowlisted-bot-sourced message")
	}

	ctaMsg := &islack.IncomingMessage{Channel: digestMsg.Channel, Timestamp: digestMsg.ThreadTS, IsBot: true, BotID: "B_SENTRY"}
	outcome := runTicketRequest(context.Background(), ctaMsg, nil, newTestStateManager(t, db), tracker, resolver,
		investRunner, ticketEngine, investigateSem, "eng-alerts", ctaMsg.ThreadTS(), ticket.SourceCTA, true /* sentryCorroborated */)
	if outcome.Err != nil {
		t.Fatalf("unexpected error filing via CTA: %v", outcome.Err)
	}

	entry, err := db.GetTicketIndex("sentry:BILLING-42")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected a ticket_index row keyed sentry:BILLING-42 for a corroborated report, got none")
	}
}
