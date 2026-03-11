// internal/personality/manager_test.go
package personality

import "testing"

func TestNewManager(t *testing.T) {
	m := NewManager(DefaultTraits())
	eff := m.Effective()
	if eff.Thoroughness != 0.70 {
		t.Errorf("Thoroughness = %v, want 0.70", eff.Thoroughness)
	}
}

func TestManagerAdjust(t *testing.T) {
	m := NewManager(DefaultTraits())
	err := m.applyAdjustment("thoroughness", 0.05, "emoji", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	eff := m.Effective()
	if eff.Thoroughness <= 0.70 {
		t.Errorf("Thoroughness = %v, should have increased from 0.70", eff.Thoroughness)
	}
	base := m.Base()
	if base.Thoroughness != 0.70 {
		t.Errorf("Base Thoroughness = %v, want 0.70 (should not change)", base.Thoroughness)
	}
}

func TestManagerDampening(t *testing.T) {
	base := DefaultTraits()
	base.Thoroughness = 0.95
	m := NewManager(base)
	err := m.applyAdjustment("thoroughness", 0.05, "emoji", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	eff := m.Effective()
	if eff.Thoroughness > 0.96 {
		t.Errorf("Thoroughness = %v, expected dampening near extreme", eff.Thoroughness)
	}
}

func TestManagerLearnedCap(t *testing.T) {
	m := NewManager(DefaultTraits())
	for i := 0; i < 100; i++ {
		m.applyAdjustment("risk_tolerance", 0.05, "outcome", "test", "")
	}
	eff := m.Effective()
	base := m.Base()
	diff := eff.RiskTolerance - base.RiskTolerance
	if diff > 0.36 {
		t.Errorf("learned adjustment = %v, should not exceed 0.35 cap", diff)
	}
}

func TestManagerManualAdjust(t *testing.T) {
	m := NewManager(DefaultTraits())
	err := m.ManualAdjust("verbosity", 0.80, "user prefers verbose")
	if err != nil {
		t.Fatal(err)
	}
	eff := m.Effective()
	if eff.Verbosity != 0.80 {
		t.Errorf("Verbosity = %v, want 0.80", eff.Verbosity)
	}
}

func TestManagerPersistent(t *testing.T) {
	db := testDB(t)
	base := DefaultTraits()
	m, err := NewPersistentManager(db, base)
	if err != nil {
		t.Fatal(err)
	}
	m.applyAdjustment("tone", 0.05, "emoji", "test", "")

	m2, err := NewPersistentManager(db, base)
	if err != nil {
		t.Fatal(err)
	}
	eff := m2.Effective()
	if eff.Tone <= 0.60 {
		t.Errorf("Tone = %v, should be > 0.60 (hydrated from DB)", eff.Tone)
	}
}
