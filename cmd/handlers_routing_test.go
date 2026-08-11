package cmd

import (
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/triage"
)

func TestShouldInvestigateFirstTouch(t *testing.T) {
	cases := []struct {
		name   string
		result triage.Result
		want   bool
	}{
		{"bug report investigates", triage.Result{Category: "bug", Confidence: 0.8, Intent: "report"}, true},
		{"bug question converses", triage.Result{Category: "bug", Confidence: 0.8, Intent: "question"}, false},
		{"bug action converses", triage.Result{Category: "bug", Confidence: 0.8, Intent: "action"}, false},
		{"missing intent falls back to report", triage.Result{Category: "bug", Confidence: 0.8}, true},
		{"feature report investigates", triage.Result{Category: "feature", Confidence: 0.6, Intent: "report"}, true},
		{"low confidence never investigates", triage.Result{Category: "bug", Confidence: 0.3, Intent: "report"}, false},
		{"question category never investigates", triage.Result{Category: "question", Confidence: 0.9, Intent: "report"}, false},
	}
	for _, c := range cases {
		if got := shouldInvestigateFirstTouch(&c.result); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHasPriorThreadState(t *testing.T) {
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer db.Close()

	if hasPriorThreadState(db, "t1") {
		t.Error("fresh thread has no prior state")
	}
	if err := db.SaveThreadMemory("t1", "C1", "summary", "response"); err != nil {
		t.Fatalf("SaveThreadMemory: %v", err)
	}
	if !hasPriorThreadState(db, "t1") {
		t.Error("thread memory counts as prior state")
	}

	if err := db.SaveInvestigation(&state.InvestigationRecord{
		ID: "i1", ThreadTS: "t2", Channel: "C1", FindingsJSON: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveInvestigation: %v", err)
	}
	if !hasPriorThreadState(db, "t2") {
		t.Error("an investigation record counts as prior state")
	}

	if hasPriorThreadState(nil, "t1") {
		t.Error("nil DB has no prior state")
	}

	// An investigation record older than priorThreadStateWindow (7 days)
	// must not count — a stale thread gets a fresh triage-routed look
	// instead of converging forever (Fix 6). SaveInvestigation preserves
	// the given CreatedAt verbatim, so this back-dates the row directly.
	if err := db.SaveInvestigation(&state.InvestigationRecord{
		ID: "i2", ThreadTS: "t3", Channel: "C1", FindingsJSON: "{}",
		CreatedAt: time.Now().Add(-priorThreadStateWindow - time.Hour),
	}); err != nil {
		t.Fatalf("SaveInvestigation (expired): %v", err)
	}
	if hasPriorThreadState(db, "t3") {
		t.Error("an investigation record older than priorThreadStateWindow must not count as prior state")
	}
}
