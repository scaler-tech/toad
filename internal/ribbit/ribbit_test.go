package ribbit

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
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
	e := New(mock, cfg, nil)

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
	e := New(mock, cfg, nil)

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
	e := New(mock, cfg, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "test", tr, nil, "/repo", nil)
	if err == nil {
		t.Fatal("expected error when provider fails")
	}
}

func TestRespond_VCSAndMCPWiring(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: "answer"},
	}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 5},
		VCS:    config.VCSConfig{Platform: "github"},
		IssueTracker: config.IssueTrackerConfig{
			Enabled:  true,
			Provider: "linear",
		},
		MCP: config.MCPConfig{
			Enabled: true,
			Host:    "localhost",
			Port:    8099,
		},
	}
	e := New(mock, cfg, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "what is linear?", tr, nil, "/repo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	opts := mock.LastRunOpts()

	if len(opts.AllowedBashCommands) != 1 || opts.AllowedBashCommands[0] != "gh" {
		t.Errorf("expected AllowedBashCommands=[gh], got %v", opts.AllowedBashCommands)
	}
	if len(opts.MCPServers) != 1 {
		t.Fatalf("expected 1 MCPServer, got %d", len(opts.MCPServers))
	}
	if opts.MCPServers[0].Name != "linear" {
		t.Errorf("expected MCPServer name 'linear', got %q", opts.MCPServers[0].Name)
	}
	if opts.MCPServers[0].URL != "http://localhost:8099" {
		t.Errorf("expected MCPServer URL 'http://localhost:8099', got %q", opts.MCPServers[0].URL)
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
	e := New(mock, cfg, nil)

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
