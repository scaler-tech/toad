package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

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
}

func (f *ticketflowTrackerFake) CreateIssue(_ context.Context, opts issuetracker.CreateIssueOpts) (*issuetracker.IssueRef, error) {
	f.createCalls = append(f.createCalls, opts)
	id := f.nextID
	if id == "" {
		id = "TOAD-1"
	}
	return &issuetracker.IssueRef{Provider: "linear", ID: id, URL: "https://linear.app/toad/issue/" + id, Title: opts.Title}, nil
}

func (f *ticketflowTrackerFake) PostComment(context.Context, *issuetracker.IssueRef, string) error {
	f.commentCalls++
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

// (a) sentry-corroborated, high-confidence finding -> auto-file, ticket_index
// row created, and the composed Slack reply text contains the ticket URL.
func TestRunTriggeredInvestigation_AutoFilesHighConfidenceSentryFinding(t *testing.T) {
	db, stateManager, tracker, _, investRunner, ticketEngine, investigateSem := setupTriggeredTest(t, highConfidenceSentryFindings, autoFileCfg())
	resolver := newTestResolver(t)

	msg := &islack.IncomingMessage{Channel: "C1", Timestamp: "100.1", SentryRefs: []string{"BILLING-42"}, Text: "users report double refunds"}
	result := &triage.Result{Category: "bug", Confidence: 0.9, Summary: "double refunds", Actionable: true}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Fatal("expected claim to succeed on a fresh thread")
	}

	outcome := runTriggeredInvestigation(context.Background(), msg, result, "eng-alerts", msg.ThreadTS(),
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem)

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
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem)

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
// existing ticket.
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

	msg := &islack.IncomingMessage{Channel: "C3", Timestamp: "300.1", SentryRefs: []string{"BILLING-42"}, Text: "same issue again"}
	result := &triage.Result{Category: "bug", Confidence: 0.9, Summary: "same issue again", Actionable: true}

	if !stateManager.Claim(msg.ThreadTS()) {
		t.Fatal("expected claim to succeed on a fresh thread")
	}

	outcome := runTriggeredInvestigation(context.Background(), msg, result, "eng-alerts", msg.ThreadTS(),
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem)

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

// Task 9's carried finding: investigation.Runner only slog.Warns on a repo
// sync failure and otherwise proceeds silently against a possibly-stale
// checkout. wrapSync (root.go) is supposed to surface that as a one-line
// staleness caveat on the findings text posted to Slack — this exercises
// the real wrapSync against a real (non-git) temp dir, so SyncRepoNow's
// "git fetch" genuinely fails rather than faking the syncer.
func TestRunInvestigation_AppendsStalenessCaveatOnSyncFailure(t *testing.T) {
	mockProvider := &agent.MockProvider{RunResult: &agent.RunResult{Result: highConfidenceSentryFindings}}
	investRunner := investigation.NewRunner(mockProvider, "sonnet", "", nil, wrapSync(&config.Config{}), nil)
	investigateSem := make(chan struct{}, 1)
	repo := config.RepoConfig{Name: "svc", Path: t.TempDir(), DefaultBranch: "main"} // not a git repo -> fetch fails
	req := investigation.Request{Text: "x", Repo: &repo, Timeout: time.Minute}

	findings, err := runInvestigation(context.Background(), investRunner, investigateSem, req)
	if err != nil {
		t.Fatalf("runInvestigation: %v", err)
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
		investRunner, ticketEngine, investigateSem, "eng-alerts", msg.ThreadTS(), ticket.SourceCTA)

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
		investRunner, ticketEngine, investigateSem, "eng-alerts", threadTS, ticket.SourceEscalation)

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
		stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem)

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
	blocks                  []slack.Block
}

// (a) A sentry-corroborated, high-confidence finding must auto-file (via
// ticketEngine.FileOrUpdate with source digest) and post a plain thread
// notice containing the filed ticket's URL — no TicketBlocks, since there's
// no human decision left to make.
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
	msg := digest.Message{Channel: "C1", ThreadTS: "100.1", Text: "double refunds"}

	var calls []digestPostCall
	post := func(channel, threadTS, text string, blocks []slack.Block) (string, error) {
		calls = append(calls, digestPostCall{channel, threadTS, text, blocks})
		return "999.1", nil
	}

	if err := proposeFromDigest(context.Background(), ticketEngine, post, f, msg); err != nil {
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
	if calls[0].blocks != nil {
		t.Errorf("expected a plain reply (no TicketBlocks) for an auto-filed notice, got %d blocks", len(calls[0].blocks))
	}

	entry, err := db.GetTicketIndex("sentry:BILLING-42")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected a ticket_index row for sentry:BILLING-42, got none")
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
	post := func(channel, threadTS, text string, blocks []slack.Block) (string, error) {
		calls = append(calls, digestPostCall{channel, threadTS, text, blocks})
		return "999.2", nil
	}

	if err := proposeFromDigest(context.Background(), ticketEngine, post, f, msg); err != nil {
		t.Fatalf("proposeFromDigest: %v", err)
	}

	if len(tracker.createCalls) != 0 {
		t.Errorf("expected no CreateIssue call for a non-corroborated finding, got %d", len(tracker.createCalls))
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 posted notice, got %d", len(calls))
	}
	if len(calls[0].blocks) == 0 {
		t.Error("expected TicketBlocks to be attached to the proposed notice")
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

	findings, err := investigateFromDigest(context.Background(), resolver, investRunner, investigateSem, 600, opp, msg, nil)
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
