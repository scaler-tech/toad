// internal/personality/translator_test.go
package personality

import (
	"strings"
	"testing"
)

func TestPromptFragmentsRibbit(t *testing.T) {
	m := NewManager(DefaultTraits())
	frags := m.PromptFragments(ModeRibbit)
	if len(frags) == 0 {
		t.Error("expected prompt fragments for ribbit mode")
	}
	found := false
	for _, f := range frags {
		if len(f) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("expected non-empty prompt fragments")
	}
}

func TestConfigOverridesDigest(t *testing.T) {
	m := NewManager(DefaultTraits())
	ov := m.ConfigOverrides(ModeDigest)
	if ov.MinConfidence == nil {
		t.Error("expected MinConfidence override for digest mode")
	}
}

func TestPromptFragmentsTadpole(t *testing.T) {
	traits := DefaultTraits()
	traits.TestAffinity = 0.90
	m := NewManager(traits)
	frags := m.PromptFragments(ModeTadpole)
	hasTestInstruction := false
	for _, f := range frags {
		if contains(f, "test") {
			hasTestInstruction = true
		}
	}
	if !hasTestInstruction {
		t.Error("high test_affinity should produce test-related prompt fragment")
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && strings.Contains(strings.ToLower(s), substr)
}
