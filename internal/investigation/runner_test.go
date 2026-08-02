package investigation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
)

func testRepo() *config.RepoConfig {
	return &config.RepoConfig{Name: "billing-service", Path: "/repos/billing-service"}
}

func validFindingsJSON() string {
	return `{"feasible": true, "title": "t", "problem": "p", "root_cause": "r", ` +
		`"evidence": [{"kind":"file","ref":"a.go:1","note":"n"}], "scope": ["s"], ` +
		`"non_goals": ["n"], "acceptance_criteria": ["ac"], "confidence": 0.7, ` +
		`"repo": "billing-service", "sentry_issue_ids": [], "issue_id": "", ` +
		`"files_found": [], "reasoning": "because b.go:2 was involved too"}`
}

// recordingProvider wraps agent.MockProvider so tests can observe ordering
// between a RepoSyncer call and the agent.Run call.
type recordingProvider struct {
	*agent.MockProvider
	order *[]string
}

func (r *recordingProvider) Run(ctx context.Context, opts agent.RunOpts) (*agent.RunResult, error) {
	*r.order = append(*r.order, "run")
	return r.MockProvider.Run(ctx, opts)
}

func TestRun_RepoRequired(t *testing.T) {
	mock := &agent.MockProvider{}
	r := NewRunner(mock, "claude-x", "", nil, nil, nil)

	_, err := r.Run(context.Background(), Request{Text: "hi"})
	if err == nil {
		t.Fatal("expected error when req.Repo is nil")
	}
	if len(mock.RunCalls) != 0 {
		t.Error("expected agent not to be called when repo is missing")
	}
}

func TestRun_CallsSyncerBeforeAgent(t *testing.T) {
	var order []string
	sync := func(ctx context.Context, repo config.RepoConfig) error {
		order = append(order, "sync")
		if repo.Name != "billing-service" {
			t.Errorf("expected syncer to receive resolved repo, got %q", repo.Name)
		}
		return nil
	}

	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: validFindingsJSON()}}
	rec := &recordingProvider{MockProvider: mock, order: &order}
	r := NewRunner(rec, "claude-x", "", nil, sync, nil)

	if _, err := r.Run(context.Background(), Request{Text: "hi", Repo: testRepo()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(order) != 2 || order[0] != "sync" || order[1] != "run" {
		t.Fatalf("expected [sync run], got %v", order)
	}
}

func TestRun_NilSyncerSkipsSyncWithoutError(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: validFindingsJSON()}}
	r := NewRunner(mock, "claude-x", "", nil, nil, nil)

	if _, err := r.Run(context.Background(), Request{Text: "hi", Repo: testRepo()}); err != nil {
		t.Fatalf("unexpected error with nil syncer: %v", err)
	}
	if len(mock.RunCalls) != 1 {
		t.Fatalf("expected agent to run once, got %d calls", len(mock.RunCalls))
	}
}

func TestRun_BuildsExpectedRunOpts(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: validFindingsJSON()}}
	repoPaths := map[string]string{
		"/repos/billing-service": "billing-service",
		"/repos/web-app":         "web-app",
	}
	r := NewRunner(mock, "claude-x", "/tmp/mcp-config.json", []string{"mcp__sentry__*"}, nil, repoPaths)

	req := Request{
		Text:    "investigate this",
		Repo:    testRepo(),
		Timeout: 4 * time.Minute,
	}
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	opts := mock.LastRunOpts()

	if opts.Permissions != agent.PermissionReadOnly {
		t.Errorf("expected PermissionReadOnly, got %v", opts.Permissions)
	}
	if opts.WorkDir != testRepo().Path {
		t.Errorf("expected WorkDir %q, got %q", testRepo().Path, opts.WorkDir)
	}
	if opts.Timeout != 4*time.Minute {
		t.Errorf("expected Timeout 4m, got %v", opts.Timeout)
	}
	if opts.MCPConfigPath != "/tmp/mcp-config.json" {
		t.Errorf("expected MCPConfigPath passed through, got %q", opts.MCPConfigPath)
	}
	if len(opts.AllowedMCPTools) != 1 || opts.AllowedMCPTools[0] != "mcp__sentry__*" {
		t.Errorf("expected AllowedMCPTools passed through, got %v", opts.AllowedMCPTools)
	}

	wantDirs := map[string]bool{"/repos/billing-service": true, "/repos/web-app": true}
	if len(opts.AdditionalDirs) != len(wantDirs) {
		t.Fatalf("expected %d additional dirs, got %v", len(wantDirs), opts.AdditionalDirs)
	}
	for _, d := range opts.AdditionalDirs {
		if !wantDirs[d] {
			t.Errorf("unexpected additional dir %q", d)
		}
	}
}

func TestBuildPrompt_SentryBlockGatedOnRefs(t *testing.T) {
	withRefs := buildPrompt(Request{Text: "x", SentryRefs: []string{"PROJ-123"}})
	if !strings.Contains(withRefs, "sentry MCP tools") {
		t.Error("expected sentry MCP instruction present when SentryRefs is non-empty")
	}
	if !strings.Contains(withRefs, "PROJ-123") {
		t.Error("expected sentry ref value included in the prompt")
	}

	withoutRefs := buildPrompt(Request{Text: "x"})
	if strings.Contains(withoutRefs, "sentry MCP tools") {
		t.Error("expected no sentry MCP instruction when SentryRefs is empty")
	}
	if strings.Contains(withoutRefs, "<sentry_refs>") {
		t.Error("expected no sentry_refs section when SentryRefs is empty")
	}
}

func TestBuildPrompt_MatchesFindingsSchema(t *testing.T) {
	prompt := buildPrompt(Request{Text: "x"})
	for _, tag := range []string{
		`"feasible"`, `"title"`, `"problem"`, `"root_cause"`, `"evidence"`,
		`"kind"`, `"ref"`, `"note"`, `"scope"`, `"non_goals"`,
		`"acceptance_criteria"`, `"confidence"`, `"repo"`, `"sentry_issue_ids"`,
		`"issue_id"`, `"files_found"`, `"reasoning"`,
	} {
		if !strings.Contains(prompt, tag) {
			t.Errorf("expected prompt schema to mention %s", tag)
		}
	}
}

func TestRun_ParsesFindingsFromAgentResult(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: validFindingsJSON()}}
	r := NewRunner(mock, "claude-x", "", nil, nil, nil)

	findings, err := r.Run(context.Background(), Request{Text: "investigate", Repo: testRepo()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings == nil {
		t.Fatal("expected non-nil findings")
	}
	if !findings.Feasible {
		t.Error("expected feasible=true")
	}
	if findings.Title != "t" {
		t.Errorf("expected title 't', got %q", findings.Title)
	}
}

func TestRun_DefaultsRepoNameWhenModelOmitsIt(t *testing.T) {
	result := `{"feasible": true, "title": "t", "problem": "p", "root_cause": "r", ` +
		`"evidence": [], "scope": [], "non_goals": [], "acceptance_criteria": [], ` +
		`"confidence": 0.5, "repo": "", "sentry_issue_ids": [], "issue_id": "", ` +
		`"files_found": [], "reasoning": "r"}`
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: result}}
	r := NewRunner(mock, "claude-x", "", nil, nil, nil)

	findings, err := r.Run(context.Background(), Request{Text: "investigate", Repo: testRepo()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings.Repo != "billing-service" {
		t.Errorf("expected repo defaulted to %q, got %q", "billing-service", findings.Repo)
	}
}

func TestRun_FilesFoundUnionsExtractedPathsFromFullResult(t *testing.T) {
	// "billing/export/aggregate.py" appears twice in the full transcript: once
	// in exploration prose (outside the JSON entirely, so ParseFindings never
	// sees it) and once in root_cause (inside the JSON, so ParseFindings's own
	// extraction from Problem/RootCause/Reasoning already finds it there).
	// The union must still dedupe down to a single entry.
	result := `Exploring the repo, I found billing/export/aggregate.py handles this.
{"feasible": true, "title": "t", "problem": "p", "root_cause": "billing/export/aggregate.py is the culprit", ` +
		`"evidence": [], "scope": [], "non_goals": [], "acceptance_criteria": [], ` +
		`"confidence": 0.5, "repo": "billing-service", "sentry_issue_ids": [], "issue_id": "", ` +
		`"files_found": [], "reasoning": "see above"}`
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: result}}
	r := NewRunner(mock, "claude-x", "", nil, nil, nil)

	findings, err := r.Run(context.Background(), Request{Text: "investigate", Repo: testRepo()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := 0
	for _, f := range findings.FilesFound {
		if f == "billing/export/aggregate.py" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected billing/export/aggregate.py exactly once in FilesFound (deduped), got %d occurrences in %v", count, findings.FilesFound)
	}
}

func TestRun_ParseFailureWrapsError(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: "not json at all"}}
	r := NewRunner(mock, "claude-x", "", nil, nil, nil)

	_, err := r.Run(context.Background(), Request{Text: "investigate", Repo: testRepo()})
	if err == nil {
		t.Fatal("expected error when agent result has no parseable findings")
	}
}

// TestRun_AgentFailureWrapsError covers the other Run error path: the agent
// provider itself failing (process error, CLI failure, etc.), as opposed to
// TestRun_ParseFailureWrapsError's successful-but-unparseable result. The
// underlying error must be preserved (via %w) and identifiable via
// errors.Is/errors.Unwrap, not just swallowed into an opaque message.
func TestRun_AgentFailureWrapsError(t *testing.T) {
	underlying := errors.New("claude cli exited 1")
	mock := &agent.MockProvider{RunErr: underlying}
	r := NewRunner(mock, "claude-x", "", nil, nil, nil)

	_, err := r.Run(context.Background(), Request{Text: "investigate", Repo: testRepo()})
	if err == nil {
		t.Fatal("expected error when the agent provider itself fails")
	}
	if !errors.Is(err, underlying) {
		t.Errorf("expected wrapped error to unwrap to the underlying agent error, got %v", err)
	}
	if !strings.Contains(err.Error(), "investigation agent run failed") {
		t.Errorf("expected error message to describe the failure point, got %q", err.Error())
	}
}
