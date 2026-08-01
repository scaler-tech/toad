package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
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

	blocks := islack.TicketBlocks(outcome.ReplyText, msg.ThreadTS())
	if len(blocks) == 0 {
		t.Error("expected TicketBlocks to produce at least one Slack block for the proposed reply")
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

// The claim-conflict path (already claimed) is handled by the caller
// (handleTriggered), not runTriggeredInvestigation itself — this asserts
// that contract directly: Claim fails while another claim is outstanding.
func TestClaim_ConflictsWhileHeld(t *testing.T) {
	stateManager := state.NewManager()
	threadTS := "400.1"
	if !stateManager.Claim(threadTS) {
		t.Fatal("expected first claim to succeed")
	}
	if stateManager.Claim(threadTS) {
		t.Error("expected second claim on the same thread to fail while the first is held")
	}
	stateManager.Unclaim(threadTS)
	if !stateManager.Claim(threadTS) {
		t.Error("expected claim to succeed again after unclaim")
	}
}
