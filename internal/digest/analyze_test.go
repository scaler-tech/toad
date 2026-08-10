package digest

import (
	"context"
	"strings"
	"testing"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
)

func TestAnalyze_RunOptsWiring(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `[{"summary":"null pointer in handler","confidence":0.9,"category":"bug","estimated_size":"small","keywords":["nil","handler"],"files_hint":["handler.go"],"message_ids":[0]}]`,
		},
	}

	e := &Engine{
		cfg:   &config.DigestConfig{},
		agent: mock,
		model: "haiku",
	}

	msgs := []Message{
		{Text: "nil pointer crash", ChannelName: "errors", User: "alice"},
	}
	opps, err := e.analyze(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(opps) != 1 {
		t.Fatalf("expected 1 opportunity, got %d", len(opps))
	}
	if opps[0].Summary != "null pointer in handler" {
		t.Errorf("expected summary 'null pointer in handler', got %q", opps[0].Summary)
	}

	// Verify RunOpts
	if len(mock.RunCalls) != 1 {
		t.Fatalf("expected 1 Run call, got %d", len(mock.RunCalls))
	}
	opts := mock.RunCalls[0]

	if opts.Model != "haiku" {
		t.Errorf("expected model 'haiku', got %q", opts.Model)
	}
	if opts.Permissions != agent.PermissionNone {
		t.Errorf("expected PermissionNone, got %d", opts.Permissions)
	}
	if opts.Prompt == "" {
		t.Error("expected non-empty prompt")
	}
}

func TestPrompt_IncludesProseStyleRules(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: `[]`},
	}
	e := &Engine{
		cfg:   &config.DigestConfig{},
		agent: mock,
		model: "haiku",
	}
	msgs := []Message{{Text: "test message", ChannelName: "general", User: "bob"}}
	if _, err := e.analyze(context.Background(), msgs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := mock.RunCalls[0].Prompt
	if !strings.Contains(prompt, agent.ProseStyleRulesSlim) {
		t.Error("expected the shared (slim) prose style rules in the digest prompt")
	}
}

func TestAnalyze_ProvenanceLabelsInPrompt(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: `[]`},
	}
	e := &Engine{
		cfg:   &config.DigestConfig{},
		agent: mock,
		model: "haiku",
	}
	msgs := []Message{
		{Text: "a human message", ChannelName: "general", User: "alice"},
		{Text: "deploy finished", ChannelName: "general", User: "deploybot", BotID: "B_DEPLOY"},
		{Text: "NullPointerException at handler.go:42", ChannelName: "errors", User: "sentry", BotID: "B_SENTRY", IsMonitoringBot: true},
	}
	if _, err := e.analyze(context.Background(), msgs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := mock.RunCalls[0].Prompt

	if !strings.Contains(prompt, "[0][user] #general @alice: a human message") {
		t.Error("expected [user] provenance label on the human message")
	}
	if !strings.Contains(prompt, "[1][bot] #general @deploybot: deploy finished") {
		t.Error("expected [bot] provenance label on the non-monitoring bot message")
	}
	if !strings.Contains(prompt, "[2][monitoring] #errors @sentry: NullPointerException at handler.go:42") {
		t.Error("expected [monitoring] provenance label on the monitoring-bot message")
	}
}

func TestPrompt_IncludesProvenanceRules(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: `[]`},
	}
	e := &Engine{
		cfg:   &config.DigestConfig{},
		agent: mock,
		model: "haiku",
	}
	msgs := []Message{{Text: "test message", ChannelName: "general", User: "bob"}}
	if _, err := e.analyze(context.Background(), msgs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := mock.RunCalls[0].Prompt

	if !strings.Contains(prompt, "Message provenance") {
		t.Error("expected the prompt to include the message provenance rules section")
	}
	if !strings.Contains(prompt, "[bot]") || !strings.Contains(prompt, "NEVER an opportunity") {
		t.Error("expected the prompt to explain that [bot] operational chatter is never an opportunity")
	}
	if !strings.Contains(prompt, "client account, customer, deal, RFP") {
		t.Error("expected the prompt to include the channel-audience conservatism rule")
	}
}

func TestAnalyze_ProviderError(t *testing.T) {
	mock := &agent.MockProvider{
		RunErr: context.DeadlineExceeded,
	}

	e := &Engine{
		cfg:   &config.DigestConfig{},
		agent: mock,
		model: "haiku",
	}

	msgs := []Message{
		{Text: "test message", ChannelName: "general", User: "bob"},
	}
	_, err := e.analyze(context.Background(), msgs)
	if err == nil {
		t.Fatal("expected error when provider fails")
	}
}
