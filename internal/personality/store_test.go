// internal/personality/store_test.go
package personality

import (
	"testing"

	"github.com/scaler-tech/toad/internal/state"
)

func testDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestStoreInsertAndSum(t *testing.T) {
	db := testDB(t)
	s := NewStore(db)

	err := s.Insert(Adjustment{
		Trait:       "thoroughness",
		Delta:       0.05,
		Source:      "emoji",
		BeforeValue: 0.70,
		AfterValue:  0.75,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.Insert(Adjustment{
		Trait:       "thoroughness",
		Delta:       -0.03,
		Source:      "outcome",
		BeforeValue: 0.75,
		AfterValue:  0.72,
	})
	if err != nil {
		t.Fatal(err)
	}

	deltas, err := s.SumDeltas()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := deltas["thoroughness"]
	const eps = 1e-9
	if !ok || got < 0.02-eps || got > 0.02+eps {
		t.Errorf("thoroughness delta = %v, want 0.02", got)
	}
}

func TestStoreRecent(t *testing.T) {
	db := testDB(t)
	s := NewStore(db)

	s.Insert(Adjustment{Trait: "tone", Delta: 0.01, Source: "emoji", BeforeValue: 0.6, AfterValue: 0.61})
	s.Insert(Adjustment{Trait: "verbosity", Delta: -0.02, Source: "manual", BeforeValue: 0.35, AfterValue: 0.33})

	recent, err := s.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("got %d adjustments, want 2", len(recent))
	}
	// Most recent first (ordered by id DESC for reliable ordering)
	if recent[0].Trait != "verbosity" {
		t.Errorf("first adjustment trait = %q, want %q", recent[0].Trait, "verbosity")
	}
}

func TestStoreClearAll(t *testing.T) {
	db := testDB(t)
	s := NewStore(db)

	s.Insert(Adjustment{Trait: "tone", Delta: 0.01, Source: "emoji", BeforeValue: 0.6, AfterValue: 0.61})

	if err := s.ClearAll(); err != nil {
		t.Fatal(err)
	}

	deltas, err := s.SumDeltas()
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 0 {
		t.Errorf("expected empty deltas after clear, got %v", deltas)
	}
}
