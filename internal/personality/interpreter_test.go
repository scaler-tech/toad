package personality

import (
	"context"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
)

func TestInterpreterBasic(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `[{"trait": "thoroughness", "delta": 0.03, "reasoning": "user wants more depth"}]`,
		},
	}

	interp := NewInterpreter(mock)
	adjs, err := interp.Interpret(context.Background(), "dig deeper next time", "ch:C1 ts:1", DefaultTraits())
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 {
		t.Fatalf("expected 1 adjustment, got %d", len(adjs))
	}
	if adjs[0].Trait != "thoroughness" {
		t.Errorf("expected trait thoroughness, got %s", adjs[0].Trait)
	}
	if adjs[0].Delta != 0.03 {
		t.Errorf("expected delta 0.03, got %f", adjs[0].Delta)
	}

	// Verify the agent was called with the right model
	opts := mock.LastRunOpts()
	if opts.Model != interpretModel {
		t.Errorf("expected model %s, got %s", interpretModel, opts.Model)
	}
	if opts.Permissions != agent.PermissionNone {
		t.Errorf("expected no permissions, got %d", opts.Permissions)
	}
}

func TestInterpreterDebounce(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `[{"trait": "verbosity", "delta": -0.02, "reasoning": "too much"}]`,
		},
	}

	interp := NewInterpreter(mock)
	threadKey := "ch:C1 ts:1"

	// First call should succeed
	adjs, err := interp.Interpret(context.Background(), "too verbose", threadKey, DefaultTraits())
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 {
		t.Fatalf("expected 1 adjustment on first call, got %d", len(adjs))
	}

	// Second call within debounce window should return nil
	adjs, err = interp.Interpret(context.Background(), "still too verbose", threadKey, DefaultTraits())
	if err != nil {
		t.Fatal(err)
	}
	if adjs != nil {
		t.Errorf("expected nil adjustments on debounced call, got %v", adjs)
	}

	// Different thread should not be debounced
	adjs, err = interp.Interpret(context.Background(), "too verbose", "ch:C2 ts:2", DefaultTraits())
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 {
		t.Errorf("expected 1 adjustment on different thread, got %d", len(adjs))
	}
}

func TestInterpreterEmptyText(t *testing.T) {
	mock := &agent.MockProvider{}
	interp := NewInterpreter(mock)

	adjs, err := interp.Interpret(context.Background(), "  ", "ch:C1 ts:1", DefaultTraits())
	if err != nil {
		t.Fatal(err)
	}
	if adjs != nil {
		t.Errorf("expected nil for empty text, got %v", adjs)
	}
	if len(mock.RunCalls) != 0 {
		t.Error("should not have called agent for empty text")
	}
}

func TestInterpreterEmptyArray(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: `[]`},
	}

	interp := NewInterpreter(mock)
	adjs, err := interp.Interpret(context.Background(), "nice work", "ch:C1 ts:1", DefaultTraits())
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 0 {
		t.Errorf("expected 0 adjustments for generic praise, got %d", len(adjs))
	}
}

func TestInterpreterClampsDeltas(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `[{"trait": "verbosity", "delta": 0.99, "reasoning": "extreme"}]`,
		},
	}

	interp := NewInterpreter(mock)
	adjs, err := interp.Interpret(context.Background(), "way more verbose", "ch:C1 ts:1", DefaultTraits())
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 {
		t.Fatalf("expected 1 adjustment, got %d", len(adjs))
	}
	if adjs[0].Delta != 0.05 {
		t.Errorf("expected delta clamped to 0.05, got %f", adjs[0].Delta)
	}
}

func TestInterpreterFiltersUnknownTraits(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `[{"trait": "nonexistent", "delta": 0.03, "reasoning": "test"}, {"trait": "verbosity", "delta": 0.02, "reasoning": "ok"}]`,
		},
	}

	interp := NewInterpreter(mock)
	adjs, err := interp.Interpret(context.Background(), "some feedback", "ch:C1 ts:1", DefaultTraits())
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 {
		t.Fatalf("expected 1 valid adjustment (unknown filtered), got %d", len(adjs))
	}
	if adjs[0].Trait != "verbosity" {
		t.Errorf("expected verbosity, got %s", adjs[0].Trait)
	}
}

func TestInterpreterAgentError(t *testing.T) {
	mock := &agent.MockProvider{
		RunErr: context.DeadlineExceeded,
	}

	interp := NewInterpreter(mock)
	_, err := interp.Interpret(context.Background(), "feedback", "ch:C1 ts:1", DefaultTraits())
	if err == nil {
		t.Error("expected error when agent fails")
	}
}

func TestParseAdjustmentsMarkdownFences(t *testing.T) {
	raw := "```json\n[{\"trait\": \"creativity\", \"delta\": 0.02, \"reasoning\": \"good\"}]\n```"
	adjs, err := parseAdjustments(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(adjs) != 1 || adjs[0].Trait != "creativity" {
		t.Errorf("unexpected: %+v", adjs)
	}
}

func TestProcessTextIntegration(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `[{"trait": "thoroughness", "delta": 0.03, "reasoning": "user wants more depth"}]`,
		},
	}

	mgr := NewManager(DefaultTraits())
	mgr.SetInterpreter(NewInterpreter(mock))

	baseThoroughness, _ := mgr.Base().Get("thoroughness")

	err := mgr.ProcessText(context.Background(), "dig deeper", "ch:C1 ts:1")
	if err != nil {
		t.Fatal(err)
	}

	eff := mgr.Effective()
	effThoroughness, _ := eff.Get("thoroughness")
	if effThoroughness <= baseThoroughness {
		t.Errorf("expected thoroughness to increase, base=%f effective=%f", baseThoroughness, effThoroughness)
	}
}

func TestProcessTextNoInterpreter(t *testing.T) {
	mgr := NewManager(DefaultTraits())
	// No interpreter set — should silently skip
	err := mgr.ProcessText(context.Background(), "some feedback", "ch:C1 ts:1")
	if err != nil {
		t.Errorf("expected nil error without interpreter, got %v", err)
	}
}

func TestProcessTextLearningDisabled(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{
			Result: `[{"trait": "verbosity", "delta": 0.03, "reasoning": "test"}]`,
		},
	}

	mgr := NewManager(DefaultTraits())
	mgr.SetInterpreter(NewInterpreter(mock))
	mgr.SetLearning(false)

	err := mgr.ProcessText(context.Background(), "some feedback", "ch:C1 ts:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.RunCalls) != 0 {
		t.Error("should not call agent when learning is disabled")
	}
}

// Verify debounce state is not corrupted by the time package
func TestInterpreterDebounceExpiry(t *testing.T) {
	mock := &agent.MockProvider{
		RunResult: &agent.RunResult{Result: `[]`},
	}

	interp := NewInterpreter(mock)
	threadKey := "ch:C1 ts:expire"

	// First call
	_, _ = interp.Interpret(context.Background(), "test", threadKey, DefaultTraits())

	// Manually expire the debounce
	interp.mu.Lock()
	interp.lastCall[threadKey] = time.Now().Add(-interpretDebounce - time.Second)
	interp.mu.Unlock()

	// Should now be allowed again
	_, err := interp.Interpret(context.Background(), "test again", threadKey, DefaultTraits())
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.RunCalls) != 2 {
		t.Errorf("expected 2 agent calls after debounce expiry, got %d", len(mock.RunCalls))
	}
}
