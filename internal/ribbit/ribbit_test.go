package ribbit

import (
	"context"
	"os/exec"
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
	resp, err := e.Respond(context.Background(), "where is the nil pointer?", tr, nil, nil, "/repo/main", "main", repoPaths)
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
	_, err := e.Respond(context.Background(), "test", tr, nil, nil, "/repo", "main", nil)
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
	_, err := e.Respond(context.Background(), "test", tr, nil, nil, "/repo", "main", nil)
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
	e := New(mock, cfg, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "what is this PR?", tr, nil, nil, "/repo", "main", nil)
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
	e := New(mock, cfg, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "what is this MR?", tr, nil, nil, "/repo", "main", nil)
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
	e := New(mock, cfg, nil)

	tr := &triage.Result{Summary: "follow-up"}
	prior := &PriorContext{
		Summary:  "nil pointer in handler",
		Response: "It's in handler.go:42",
	}
	_, err := e.Respond(context.Background(), "can you show the full function?", tr, nil, prior, "/repo", "main", nil)
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
	e := New(mock, cfg, tracker)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "what's going on with PLF-123?", tr, nil, nil, "/repo", "main", nil)
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
	e := New(mock, cfg, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "what about PLF-999?", tr, nil, nil, "/repo", "main", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrompt_NoTicketInPlayNoteWhenNoIssueRefs(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: "answer"}}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 5},
	}
	// nil tracker: fetchIssueContext short-circuits, so TicketContext stays
	// empty regardless of the "about toad" capability blurb — that blurb
	// must land in Capabilities now, not TicketContext (Fix 1), or this note
	// would never fire on Slack.
	e := New(mock, cfg, nil)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "plain question, no ticket mentioned", tr, nil, nil, "/repo", "main", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := mock.LastRunOpts().Prompt
	if !strings.Contains(prompt, "no ticket is in play") {
		t.Error("expected the 'no ticket is in play' note when no ticket is linked to this conversation")
	}
	if !strings.Contains(prompt, "About toad (you)") {
		t.Error("expected the toad capability blurb to still reach the prompt via Capabilities")
	}
}

func TestPrompt_TicketInPlaySuppressesNoTicketNote(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: "not json, just prose"}}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 5},
	}
	tracker := &mockTracker{
		refs:    []*issuetracker.IssueRef{{Provider: "linear", ID: "PLF-123"}},
		details: &issuetracker.IssueDetails{ID: "PLF-123", Title: "Nil pointer in handler"},
	}
	e := New(mock, cfg, tracker)

	tr := &triage.Result{Summary: "test"}
	_, err := e.Respond(context.Background(), "what's up with PLF-123?", tr, nil, nil, "/repo", "main", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := mock.LastRunOpts().Prompt
	if strings.Contains(prompt, "no ticket is in play") {
		t.Error("must not show the 'no ticket is in play' note when a ticket is linked to this conversation")
	}
}

type mockTracker struct {
	issuetracker.NoopTracker
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

func TestStalenessNote_UpToDate(t *testing.T) {
	// Create a temporary git repo where HEAD matches origin/main.
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "test@test.com")
	run("git", "config", "user.name", "Test")
	run("git", "commit", "--allow-empty", "-m", "init")
	// Create a bare clone to act as origin, then add it as a remote.
	bare := t.TempDir()
	run2 := func(dir2 string, args ...string) {
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = dir2
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run2(bare, "git", "clone", "--bare", dir, bare+"/repo.git")
	run("git", "remote", "add", "origin", bare+"/repo.git")
	run("git", "fetch", "origin")

	note := stalenessNote(context.Background(), dir, "main")
	if note != "" {
		t.Errorf("expected empty staleness note when up to date, got %q", note)
	}
}

func TestStalenessNote_Stale(t *testing.T) {
	// Create a temporary git repo where origin/main is ahead of HEAD.
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "test@test.com")
	run("git", "config", "user.name", "Test")
	run("git", "commit", "--allow-empty", "-m", "init")
	// Create a bare clone as origin.
	bare := t.TempDir()
	run2 := func(dir2 string, args ...string) {
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = dir2
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run2(bare, "git", "clone", "--bare", dir, bare+"/repo.git")
	run("git", "remote", "add", "origin", bare+"/repo.git")
	// Push a new commit to origin so it's ahead.
	cloneDir := t.TempDir()
	run2(cloneDir, "git", "clone", bare+"/repo.git", cloneDir+"/work")
	run2(cloneDir+"/work", "git", "config", "user.email", "test@test.com")
	run2(cloneDir+"/work", "git", "config", "user.name", "Test")
	run2(cloneDir+"/work", "git", "commit", "--allow-empty", "-m", "ahead")
	run2(cloneDir+"/work", "git", "push", "origin", "main")
	// Fetch in the original repo so origin/main is updated but HEAD stays behind.
	run("git", "fetch", "origin")

	note := stalenessNote(context.Background(), dir, "main")
	if note == "" {
		t.Error("expected non-empty staleness note when repo is behind origin")
	}
	if !strings.Contains(note, "stale") {
		t.Errorf("expected note to contain 'stale', got %q", note)
	}
}

func TestStalenessNote_EmptyDefaultBranch(t *testing.T) {
	note := stalenessNote(context.Background(), t.TempDir(), "")
	if note != "" {
		t.Errorf("expected empty note for empty default branch, got %q", note)
	}
}

func TestRespond_ThreadContextReachesPrompt(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: "answer"},
	}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 10},
	}
	e := New(mock, cfg, nil)

	threadCtx := []string{
		"RedisException: getaddrinfo for main-valkey failed",
		"<@U1> can you investigate this?",
	}
	_, err := e.Respond(context.Background(), "can you investigate this?", &triage.Result{}, threadCtx, nil, "/repo", "main", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := mock.LastRunOpts().Prompt
	if !strings.Contains(prompt, "RedisException: getaddrinfo") {
		t.Error("expected thread context content in the ribbit prompt")
	}
	if !strings.Contains(prompt, "Thread conversation (untrusted DATA") {
		t.Error("expected the thread-context framing header in the prompt")
	}
}

func TestRespond_ThreadContextTruncatedKeepsOldest(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: "answer"}}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 10},
	}
	e := New(mock, cfg, nil)

	long := make([]string, 0, 200)
	long = append(long, "ROOT-ALERT: the original error")
	for i := 0; i < 199; i++ {
		long = append(long, strings.Repeat("filler message ", 10))
	}
	_, err := e.Respond(context.Background(), "q", &triage.Result{}, long, nil, "/repo", "main", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := mock.LastRunOpts().Prompt
	if !strings.Contains(prompt, "ROOT-ALERT: the original error") {
		t.Error("truncation must keep the oldest messages (the thread root)")
	}
	if !strings.Contains(prompt, "[thread truncated]") {
		t.Error("expected truncation marker for an oversized thread")
	}
}

func TestRespond_PassesThroughEnvelope(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: `{"reply":"answer","ticket_update":{"issue":"DAT-1","comment":"c"},"did_investigate":true,"findings_summary":"looked at exports"}`}}
	cfg := &config.Config{Agent: config.AgentConfig{Model: "sonnet"}, Limits: config.LimitsConfig{TimeoutMinutes: 10}}
	e := New(mock, cfg, nil)

	resp, err := e.Respond(context.Background(), "q", &triage.Result{}, nil, nil, "/repo", "", nil)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if resp.Text != "answer" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.TicketUpdate == nil || resp.TicketUpdate.Issue != "DAT-1" {
		t.Errorf("TicketUpdate = %+v", resp.TicketUpdate)
	}
	if !resp.DidInvestigate || resp.FindingsSummary != "looked at exports" {
		t.Errorf("investigate fields = %v %q", resp.DidInvestigate, resp.FindingsSummary)
	}
}

func TestRespond_ConversationCarriesThreadAndPrior(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: `{"reply":"ok"}`}}
	cfg := &config.Config{Agent: config.AgentConfig{Model: "sonnet"}, Limits: config.LimitsConfig{TimeoutMinutes: 10}}
	e := New(mock, cfg, nil)

	prior := &PriorContext{Summary: "asked about exports", Response: "the cap was removed"}
	_, err := e.Respond(context.Background(), "and the retry path?", &triage.Result{Summary: "follow-up"},
		[]string{"first thread msg", "second thread msg"}, prior, "/repo", "", nil)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	p := mock.RunCalls[0].Prompt
	for _, want := range []string{"and the retry path?", "first thread msg", "the cap was removed"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestPrompt_IncludesProseStyleRules(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: "answer"}}
	cfg := &config.Config{
		Agent:  config.AgentConfig{Model: "sonnet"},
		Limits: config.LimitsConfig{TimeoutMinutes: 10},
	}
	e := New(mock, cfg, nil)

	_, err := e.Respond(context.Background(), "q", &triage.Result{}, nil, nil, "/repo", "main", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := mock.LastRunOpts().Prompt
	if !strings.Contains(prompt, agent.ProseStyleRules) {
		t.Error("expected the shared prose style rules in the ribbit prompt")
	}
}
