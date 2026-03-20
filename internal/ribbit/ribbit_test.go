package ribbit

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/triage"
)

func TestRespond_RunOptsWiring(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: "The bug is in `handler.go:42` — the nil check is missing.",
		},
	}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 10},
	}
	e := New(mock, cfg, nil, nil)

	tr := &triage.Result{
		Summary:  "nil pointer",
		Category: "bug",
		Keywords: []string{"nil"},
	}
	repoPaths := map[string]string{
		"/repo/main":  "main-app",
		"/repo/tools": "tools",
	}
	resp, err := e.Respond(context.Background(), "where is the nil pointer?", tr, nil, "/repo/main", repoPaths)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Text == "" {
		t.Error("expected non-empty response text")
	}

	// Verify RunOpts
	if len(mock.RunCalls) != 1 {
		t.Fatalf("expected 1 Run call, got %d", len(mock.RunCalls))
	}
	opts := mock.RunCalls[0]

	if opts.Model != "sonnet" {
		t.Errorf("expected model 'sonnet', got %q", opts.Model)
	}
	if opts.MaxTurns != 10 {
		t.Errorf("expected MaxTurns=10, got %d", opts.MaxTurns)
	}
	if opts.Timeout != 10*time.Minute {
		t.Errorf("expected Timeout=10m, got %v", opts.Timeout)
	}
	if opts.Permissions != agent.PermissionReadOnly {
		t.Errorf("expected PermissionReadOnly, got %d", opts.Permissions)
	}
	if opts.WorkDir != "/repo/main" {
		t.Errorf("expected WorkDir '/repo/main', got %q", opts.WorkDir)
	}
	// AdditionalDirs should contain both repo paths
	sort.Strings(opts.AdditionalDirs)
	if len(opts.AdditionalDirs) != 2 {
		t.Fatalf("expected 2 AdditionalDirs, got %d", len(opts.AdditionalDirs))
	}
	if opts.AdditionalDirs[0] != "/repo/main" || opts.AdditionalDirs[1] != "/repo/tools" {
		t.Errorf("unexpected AdditionalDirs: %v", opts.AdditionalDirs)
	}
}

func TestRespond_EmptyResult(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: "   "},
	}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 5},
	}
	e := New(mock, cfg, nil, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "test", tr, nil, "/repo", nil)
	if err == nil {
		t.Fatal("expected error for empty result")
	}
}

func TestRespond_ProviderError(t *testing.T) {
	mock := &agent.MockProvider{
		RunErr: context.DeadlineExceeded,
	}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 5},
	}
	e := New(mock, cfg, nil, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "test", tr, nil, "/repo", nil)
	if err == nil {
		t.Fatal("expected error when provider fails")
	}
}

func TestRespond_VCSBashWiring(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: "answer"},
	}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 5},
		VCS:    config.VCSConfig{Platform: "github"},
	}
	e := New(mock, cfg, nil, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "what is this PR?", tr, nil, "/repo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	opts := mock.LastRunOpts()

	if len(opts.AllowedBashCommands) == 0 {
		t.Fatal("expected AllowedBashCommands to be set")
	}
	// All commands should start with "gh " and be read-only subcommands
	for _, cmd := range opts.AllowedBashCommands {
		if !strings.HasPrefix(cmd, "gh ") {
			t.Errorf("expected all commands to start with 'gh ', got %q", cmd)
		}
	}
	// Verify no broad "gh" entry (would allow writes)
	for _, cmd := range opts.AllowedBashCommands {
		if cmd == "gh" {
			t.Error("AllowedBashCommands should not contain broad 'gh', only specific subcommands")
		}
	}
}

func TestRespond_VCSBashWiring_GitLab(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: "answer"},
	}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 5},
		VCS:    config.VCSConfig{Platform: "gitlab"},
	}
	e := New(mock, cfg, nil, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "what is this MR?", tr, nil, "/repo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	opts := mock.LastRunOpts()

	if len(opts.AllowedBashCommands) == 0 {
		t.Fatal("expected AllowedBashCommands to be set for gitlab")
	}
	for _, cmd := range opts.AllowedBashCommands {
		if !strings.HasPrefix(cmd, "glab ") {
			t.Errorf("expected all commands to start with 'glab ', got %q", cmd)
		}
	}
}

func TestRespond_PriorContext(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: "Follow-up answer here.",
		},
	}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 5},
	}
	e := New(mock, cfg, nil, nil)

	tr := &triage.Result{Summary: "follow-up"}
	prior := &PriorContext{
		Summary:  "nil pointer in handler",
		Response: "It's in handler.go:42",
	}
	_, err := e.Respond(context.Background(), "can you show the full function?", tr, prior, "/repo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the prompt includes prior context
	opts := mock.LastRunOpts()
	if opts.Prompt == "" {
		t.Error("expected non-empty prompt")
	}
}

func TestRespond_IssueTrackerEnrichment(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: "The ticket describes a nil pointer."},
	}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 5},
	}
	tracker := &mockTracker{
		refs: []*issuetracker.IssueRef{{Provider: "linear", ID: "PLF-123"}},
		details: &issuetracker.IssueDetails{
			ID:          "PLF-123",
			Title:       "Nil pointer in handler",
			Description: "When calling /api/foo, a nil pointer panic occurs.",
			Comments: []issuetracker.IssueComment{
				{Author: "Alice", Body: "Reproduced on staging"},
			},
		},
	}
	e := New(mock, cfg, nil, tracker)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "what's going on with PLF-123?", tr, nil, "/repo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	opts := mock.LastRunOpts()
	if !strings.Contains(opts.Prompt, "PLF-123") {
		t.Error("expected prompt to contain issue ID")
	}
	if !strings.Contains(opts.Prompt, "Nil pointer in handler") {
		t.Error("expected prompt to contain issue title")
	}
	if !strings.Contains(opts.Prompt, "Reproduced on staging") {
		t.Error("expected prompt to contain comment")
	}
}

func TestRespond_NilTracker(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: "answer"},
	}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 5},
	}
	e := New(mock, cfg, nil, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "what about PLF-999?", tr, nil, "/repo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

type mockTracker struct {
	refs    []*issuetracker.IssueRef
	details *issuetracker.IssueDetails
}

func (m *mockTracker) ExtractIssueRef(text string) *issuetracker.IssueRef {
	if len(m.refs) > 0 {
		return m.refs[0]
	}
	return nil
}

func (m *mockTracker) ExtractAllIssueRefs(text string) []*issuetracker.IssueRef {
	return m.refs
}

func (m *mockTracker) GetIssueDetails(ctx context.Context, ref *issuetracker.IssueRef) (*issuetracker.IssueDetails, error) {
	return m.details, nil
}

func (m *mockTracker) CreateIssue(ctx context.Context, opts issuetracker.CreateIssueOpts) (*issuetracker.IssueRef, error) {
	return nil, nil
}

func (m *mockTracker) ShouldCreateIssues() bool { return false }

func (m *mockTracker) GetIssueStatus(ctx context.Context, ref *issuetracker.IssueRef) (*issuetracker.IssueStatus, error) {
	return nil, nil
}

func (m *mockTracker) PostComment(ctx context.Context, ref *issuetracker.IssueRef, body string) error {
	return nil
}
