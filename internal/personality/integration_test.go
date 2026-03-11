package personality

import "testing"

func TestFullLifecycle(t *testing.T) {
	db := testDB(t)
	base := DefaultTraits()

	// 1. Create persistent manager
	m, err := NewPersistentManager(db, base)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Apply emoji feedback
	m.ProcessEmoji("rabbit", "shallow ribbit")
	m.ProcessEmoji("turtle", "too slow investigation")

	// 3. Apply outcome
	m.ProcessOutcome(OutcomeSignal{Type: "pr_merged", PRURL: "test"})

	// 4. Check effective values changed
	eff := m.Effective()
	if eff == base {
		t.Error("effective should differ from base after feedback")
	}

	// 5. Export
	pf, err := m.Export("test-export", "integration test")
	if err != nil {
		t.Fatal(err)
	}
	if pf.Traits == base {
		t.Error("exported traits should reflect learned adjustments")
	}

	// 6. Import resets
	m.Import(&PersonalityFile{Version: 1, Name: "fresh", Traits: DefaultTraits()})
	eff2 := m.Effective()
	if eff2 != DefaultTraits() {
		t.Error("import should reset to fresh base")
	}

	// 7. Translation produces output
	frags := m.PromptFragments(ModeRibbit)
	if len(frags) == 0 {
		t.Error("should produce prompt fragments")
	}
	ov := m.ConfigOverrides(ModeTadpole)
	if ov.MaxRetries == nil {
		t.Error("should produce config overrides")
	}
}
