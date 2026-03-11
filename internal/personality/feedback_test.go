// internal/personality/feedback_test.go
package personality

import "testing"

func TestProcessEmoji(t *testing.T) {
	m := NewManager(DefaultTraits())
	err := m.ProcessEmoji("rabbit", "ribbit reply about auth")
	if err != nil {
		t.Fatal(err)
	}
	eff := m.Effective()
	if eff.Thoroughness <= 0.70 {
		t.Errorf("Thoroughness = %v, should have increased from 0.70", eff.Thoroughness)
	}
	if eff.ContextHunger <= 0.50 {
		t.Errorf("ContextHunger = %v, should have increased from 0.50", eff.ContextHunger)
	}
}

func TestProcessEmojiUnknown(t *testing.T) {
	m := NewManager(DefaultTraits())
	err := m.ProcessEmoji("sparkles", "some context")
	if err == nil {
		t.Error("expected error for unknown emoji")
	}
}

func TestProcessOutcomePRMerged(t *testing.T) {
	m := NewManager(DefaultTraits())
	err := m.ProcessOutcome(OutcomeSignal{
		Type:  "pr_merged",
		PRURL: "https://github.com/org/repo/pull/42",
	})
	if err != nil {
		t.Fatal(err)
	}
	eff := m.Effective()
	if eff.RiskTolerance < 0.30 {
		t.Errorf("RiskTolerance = %v, should not decrease after merge", eff.RiskTolerance)
	}
}

func TestProcessOutcomePRClosed(t *testing.T) {
	m := NewManager(DefaultTraits())
	err := m.ProcessOutcome(OutcomeSignal{
		Type:  "pr_closed",
		PRURL: "https://github.com/org/repo/pull/43",
	})
	if err != nil {
		t.Fatal(err)
	}
	eff := m.Effective()
	if eff.RiskTolerance >= 0.30 {
		t.Errorf("RiskTolerance = %v, should decrease after close", eff.RiskTolerance)
	}
}
