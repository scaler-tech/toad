package triage

import (
	"context"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	islack "github.com/scaler-tech/toad/internal/slack"
)

func TestClassify_RunOptsWiring(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `{"actionable":true,"confidence":0.85,"summary":"test bug","category":"bug","estimated_size":"small","keywords":["test"],"files_hint":["main.go"]}`,
		},
	}
	e := New(mock, "haiku", nil)

	msg := &islack.IncomingMessage{Text: "there's a bug in main.go"}
	result, err := e.Classify(context.Background(), msg, "general")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the result was parsed correctly
	if !result.Actionable {
		t.Error("expected actionable=true")
	}
	if result.Category != "bug" {
		t.Errorf("expected category 'bug', got %q", result.Category)
	}

	// Verify RunOpts passed to the provider
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
	if opts.Prompt == "" {
		t.Error("expected non-empty prompt")
	}
}

func TestClassify_ProviderError(t *testing.T) {
	mock := &agent.MockProvider{
		RunErr: context.DeadlineExceeded,
	}
	e := New(mock, "haiku", nil)

	msg := &islack.IncomingMessage{Text: "test"}
	_, err := e.Classify(context.Background(), msg, "general")
	if err == nil {
		t.Fatal("expected error when provider fails")
	}
}
