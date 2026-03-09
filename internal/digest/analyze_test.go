package digest

import (
	"context"
	"testing"

	"github.com/scaler-tech/toad/internal/agent"
)

func TestAnalyze_RunOptsWiring(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `[{"summary":"null pointer in handler","confidence":0.9,"category":"bug","estimated_size":"small","keywords":["nil","handler"],"files_hint":["handler.go"],"message_ids":[0]}]`,
		},
	}

	e := &Engine{
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
	if opts.MaxTurns != 1 {
		t.Errorf("expected MaxTurns=1, got %d", opts.MaxTurns)
	}
	if opts.Permissions != agent.PermissionNone {
		t.Errorf("expected PermissionNone, got %d", opts.Permissions)
	}
	if opts.Prompt == "" {
		t.Error("expected non-empty prompt")
	}
}

func TestAnalyze_ProviderError(t *testing.T) {
	mock := &agent.MockProvider{
		RunErr: context.DeadlineExceeded,
	}

	e := &Engine{
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
