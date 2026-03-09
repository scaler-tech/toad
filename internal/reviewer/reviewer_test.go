package reviewer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/vcs"
)

// stubVCS implements only the methods needed for triageComments testing.
type stubVCS struct{ vcs.Provider }

func (stubVCS) PRNoun() string                                            { return "PR" }
func (stubVCS) PostPRComment(_ context.Context, _ int, _, _ string) error { return nil }

func TestTriageComments_RunOptsWiring(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `{"actionable":true,"summary":"fix the nil check","reason":"reviewer requested a code change"}`,
		},
	}

	w := &Watcher{
		agent:       mock,
		triageModel: "haiku",
	}

	comments := []vcs.PRComment{
		{UserLogin: "reviewer", Body: "This needs a nil check", Source: "review"},
	}
	result, err := w.triageComments(context.Background(), stubVCS{}, 42, comments, comments)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Actionable {
		t.Error("expected actionable=true")
	}
	if result.Summary != "fix the nil check" {
		t.Errorf("expected summary 'fix the nil check', got %q", result.Summary)
	}
	// TaskDescription should be built for actionable results
	if result.TaskDescription == "" {
		t.Error("expected non-empty TaskDescription for actionable result")
	}

	// Verify RunOpts
	if len(mock.RunCalls) != 1 {
		t.Fatalf("expected 1 Run call, got %d", len(mock.RunCalls))
	}
	opts := mock.RunCalls[0]

	if opts.Model != "haiku" {
		t.Errorf("expected model 'haiku', got %q", opts.Model)
	}
	if opts.MaxTurns != 1 {
		t.Errorf("expected MaxTurns=1, got %d", opts.MaxTurns)
	}
	if opts.Timeout != 30*time.Second {
		t.Errorf("expected Timeout=30s, got %v", opts.Timeout)
	}
	if opts.Permissions != agent.PermissionNone {
		t.Errorf("expected PermissionNone, got %d", opts.Permissions)
	}
}

func TestTriageComments_NotActionable(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `{"actionable":false,"summary":"","reason":"just an approval"}`,
		},
	}

	w := &Watcher{
		agent:       mock,
		triageModel: "haiku",
	}

	comments := []vcs.PRComment{
		{UserLogin: "reviewer", Body: "LGTM!", Source: "review"},
	}
	result, err := w.triageComments(context.Background(), stubVCS{}, 10, comments, comments)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Actionable {
		t.Error("expected actionable=false")
	}
	if result.TaskDescription != "" {
		t.Error("expected empty TaskDescription for non-actionable result")
	}
}

func TestTriageComments_ProviderError(t *testing.T) {
	mock := &agent.MockProvider{
		RunErr: context.DeadlineExceeded,
	}

	w := &Watcher{
		agent:       mock,
		triageModel: "haiku",
	}

	comments := []vcs.PRComment{
		{UserLogin: "reviewer", Body: "fix this", Source: "review"},
	}
	_, err := w.triageComments(context.Background(), stubVCS{}, 1, comments, comments)
	if err == nil {
		t.Fatal("expected error when provider fails")
	}
}

func TestTriageComments_BotCommentsInContext(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `{"actionable":true,"summary":"fix lint issues","reason":"human requested fix"}`,
		},
	}

	w := &Watcher{
		agent:       mock,
		triageModel: "haiku",
	}

	// Human says "fix this" referencing a bot's lint comment
	newComments := []vcs.PRComment{
		{UserLogin: "alice", Body: "fix this", Source: "review", UserType: "User"},
	}
	allComments := []vcs.PRComment{
		{UserLogin: "lint-bot", Body: "unused variable on line 42", Source: "review", UserType: "Bot", Path: "main.go"},
		{UserLogin: "alice", Body: "fix this", Source: "review", UserType: "User"},
	}

	result, err := w.triageComments(context.Background(), stubVCS{}, 99, newComments, allComments)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Actionable {
		t.Fatal("expected actionable=true")
	}

	// Task description should contain both the human comment and the bot context
	if !strings.Contains(result.TaskDescription, "fix this") {
		t.Error("expected task description to contain human comment")
	}
	if !strings.Contains(result.TaskDescription, "lint-bot") {
		t.Error("expected task description to contain bot comment for context")
	}
	if !strings.Contains(result.TaskDescription, "[bot]") {
		t.Error("expected bot comments to be labeled [bot]")
	}

	// Triage prompt should only contain new human comments, not bot ones
	opts := mock.RunCalls[0]
	if strings.Contains(opts.Prompt, "lint-bot") {
		t.Error("triage prompt should not contain bot comments")
	}
}

func TestTriageComments_ReviewBotIncluded(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `{"actionable":true,"summary":"fix security issue","reason":"bot found real bug"}`,
		},
	}

	w := &Watcher{
		agent:       mock,
		triageModel: "haiku",
		reviewBots:  map[string]bool{"greptile[bot]": true},
	}

	// Review bot is in allowed list — should appear in triage prompt with [review-bot] label
	newComments := []vcs.PRComment{
		{UserLogin: "greptile[bot]", Body: "SQL injection on line 42", Source: "review", UserType: "Bot", Path: "handler.go"},
	}

	result, err := w.triageComments(context.Background(), stubVCS{}, 99, newComments, newComments)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	opts := mock.RunCalls[0]
	if !strings.Contains(opts.Prompt, "greptile[bot]") {
		t.Error("triage prompt should contain allowed review bot")
	}
	if !strings.Contains(opts.Prompt, "[review-bot]") {
		t.Error("review bot should be labeled [review-bot] in triage prompt")
	}

	// Task description should include the bot caveat
	if !strings.Contains(result.TaskDescription, "[review-bot]") {
		t.Error("task description should label review bot comments")
	}
	if !strings.Contains(result.TaskDescription, "overconfident") {
		t.Error("task description should warn about bot reliability")
	}
}

func TestFilterComments_ReviewBotPassesThrough(t *testing.T) {
	w := &Watcher{
		reviewBots: map[string]bool{"greptile[bot]": true},
	}

	allComments := []vcs.PRComment{
		{ID: 1, UserLogin: "greptile[bot]", Body: "found a bug", Source: "review", UserType: "Bot"},
		{ID: 2, UserLogin: "random-bot", Body: "lint warning", Source: "review", UserType: "Bot"},
	}

	var newComments []vcs.PRComment
	for _, c := range allComments {
		if c.UserType == "Bot" && !w.reviewBots[c.UserLogin] {
			continue
		}
		newComments = append(newComments, c)
	}

	if len(newComments) != 1 {
		t.Fatalf("expected 1 comment (allowed review bot only), got %d", len(newComments))
	}
	if newComments[0].UserLogin != "greptile[bot]" {
		t.Errorf("expected greptile[bot], got %q", newComments[0].UserLogin)
	}
}

func TestTriageComments_CodeFencedJSON(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: "```json\n{\"actionable\":true,\"summary\":\"add test\",\"reason\":\"reviewer asked\"}\n```",
		},
	}

	w := &Watcher{
		agent:       mock,
		triageModel: "haiku",
	}

	comments := []vcs.PRComment{
		{UserLogin: "reviewer", Body: "add a test", Source: "review"},
	}
	result, err := w.triageComments(context.Background(), stubVCS{}, 5, comments, comments)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Actionable {
		t.Error("expected actionable=true after stripping code fences")
	}
}
