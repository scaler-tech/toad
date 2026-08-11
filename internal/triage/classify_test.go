package triage

import (
	"context"
	"strings"
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
	if opts.Timeout != 60*time.Second {
		t.Errorf("expected Timeout=60s, got %v", opts.Timeout)
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

func TestTriagePrompt_ContainsSentryRule(t *testing.T) {
	if !strings.Contains(triagePrompt, "Sentry") {
		t.Error("triagePrompt should mention Sentry monitoring bot rule")
	}
	if !strings.Contains(triagePrompt, "error/stack trace") {
		t.Error("triagePrompt should mention error/stack trace for Sentry rule")
	}
}

func TestTriagePrompt_ContainsEscalateRule(t *testing.T) {
	if !strings.Contains(triagePrompt, "escalate") {
		t.Error("triagePrompt should mention escalate rule")
	}
	if !strings.Contains(triagePrompt, "create/file a ticket") {
		t.Error("triagePrompt should mention creating/filing a ticket in escalate rule")
	}
}

func TestTriagePrompt_ContainsEscalateInTemplate(t *testing.T) {
	if !strings.Contains(triagePrompt, `"escalate":`) {
		t.Error("triagePrompt JSON template should include escalate field")
	}
}

func TestParseResult_RoundTripsEscalateTrue(t *testing.T) {
	jsonData := []byte(`{"actionable":true,"confidence":0.85,"summary":"test","category":"bug","estimated_size":"small","keywords":["test"],"files_hint":["main.go"],"escalate":true}`)
	result, err := parseResult(jsonData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Escalate {
		t.Error("expected escalate=true, got false")
	}
}

func TestParseResult_DefaultsEscalateFalse(t *testing.T) {
	jsonData := []byte(`{"actionable":false,"confidence":0.5,"summary":"test","category":"other","estimated_size":"small","keywords":[],"files_hint":[]}`)
	result, err := parseResult(jsonData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Escalate {
		t.Error("expected escalate=false by default, got true")
	}
}

func TestTriagePrompt_ContainsIntentInTemplate(t *testing.T) {
	if !strings.Contains(triagePrompt, `"intent":`) {
		t.Error("triagePrompt JSON template should include intent field")
	}
}

func TestTriagePrompt_ContainsIntentDefinitions(t *testing.T) {
	for _, want := range []string{`"report"`, `"question"`, `"action"`, `"chatter"`, "Intent definitions"} {
		if !strings.Contains(triagePrompt, want) {
			t.Errorf("triagePrompt missing intent definition element %q", want)
		}
	}
}

func TestParseResult_RoundTripsIntent(t *testing.T) {
	jsonData := []byte(`{"actionable":true,"confidence":0.85,"summary":"t","category":"bug","estimated_size":"small","keywords":[],"files_hint":[],"escalate":false,"intent":"Question"}`)
	result, err := parseResult(jsonData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Intent != "question" {
		t.Errorf("Intent = %q, want normalized %q", result.Intent, "question")
	}
}

func TestParseResult_DefaultsIntentEmpty(t *testing.T) {
	jsonData := []byte(`{"actionable":true,"confidence":0.85,"summary":"t","category":"bug","estimated_size":"small","keywords":[],"files_hint":[],"escalate":false}`)
	result, err := parseResult(jsonData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Intent != "" {
		t.Errorf("Intent = %q, want empty when omitted", result.Intent)
	}
}
