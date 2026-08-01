package state

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRecoverOnStartup_MarksActiveRunFailedAndReturnsStaleOpportunities seeds a
// file-backed temp DB with a run stuck "investigating" (simulating a crash
// mid-run) plus a digest opportunity stuck "investigating", then verifies
// RecoverOnStartup marks the run failed with the crash message and surfaces
// the stale opportunity for the caller to resume.
func TestRecoverOnStartup_MarksActiveRunFailedAndReturnsStaleOpportunities(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db, err := OpenDBAt(dbPath)
	if err != nil {
		t.Fatalf("OpenDBAt: %v", err)
	}
	defer db.Close()

	// Seed an active run left mid-flight by a crash.
	run := &Run{
		ID:            "run-crashed-1",
		Status:        "investigating",
		SlackChannel:  "C123",
		SlackThreadTS: "111.222",
		Task:          "investigate the billing bug",
		StartedAt:     time.Now(),
	}
	if err := db.SaveRun(run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	// Seed a completed run, which must NOT be touched by recovery.
	done := &Run{
		ID:        "run-done-1",
		Status:    "done",
		StartedAt: time.Now(),
	}
	if err := db.SaveRun(done); err != nil {
		t.Fatalf("SaveRun (done): %v", err)
	}

	// Seed a stale digest opportunity (investigation stuck mid-flight).
	opp := &DigestOpportunity{
		Summary:       "billing double-charges users",
		Category:      "bug",
		Confidence:    0.9,
		EstSize:       "small",
		Channel:       "errors",
		ChannelID:     "C123",
		ThreadTS:      "111.222",
		Message:       "users are being double charged",
		Investigating: true,
		CreatedAt:     time.Now(),
	}
	if err := db.SaveDigestOpportunity(opp); err != nil {
		t.Fatalf("SaveDigestOpportunity: %v", err)
	}

	// Seed a resolved digest opportunity, which must NOT be returned as stale.
	resolved := &DigestOpportunity{
		Summary:       "resolved thing",
		Category:      "bug",
		Confidence:    0.9,
		EstSize:       "small",
		Channel:       "errors",
		Investigating: false,
		CreatedAt:     time.Now(),
	}
	if err := db.SaveDigestOpportunity(resolved); err != nil {
		t.Fatalf("SaveDigestOpportunity (resolved): %v", err)
	}

	result, err := RecoverOnStartup(db)
	if err != nil {
		t.Fatalf("RecoverOnStartup: %v", err)
	}

	if result.StaleRuns != 1 {
		t.Errorf("expected StaleRuns = 1, got %d", result.StaleRuns)
	}
	if result.StaleInvestigations != 1 {
		t.Errorf("expected StaleInvestigations = 1, got %d", result.StaleInvestigations)
	}
	if len(result.StaleOpportunities) != 1 {
		t.Fatalf("expected 1 stale opportunity, got %d", len(result.StaleOpportunities))
	}
	if result.StaleOpportunities[0].Summary != "billing double-charges users" {
		t.Errorf("unexpected stale opportunity returned: %+v", result.StaleOpportunities[0])
	}

	// Verify the crashed run was actually marked failed in the DB, with the
	// crash message, and left recoverable via History (not silently dropped).
	history, err := db.History(10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	var found *Run
	for _, r := range history {
		if r.ID == "run-crashed-1" {
			found = r
		}
	}
	if found == nil {
		t.Fatalf("expected run-crashed-1 to appear in history after recovery")
	}
	if found.Status != "failed" {
		t.Errorf("expected status failed, got %q", found.Status)
	}
	if found.Result == nil || found.Result.Success {
		t.Fatalf("expected a failed RunResult, got %+v", found.Result)
	}
	if found.Result.Error != "toad crashed during execution" {
		t.Errorf("expected crash message, got %q", found.Result.Error)
	}

	// The already-done run must be untouched by recovery.
	active, err := db.ActiveRuns()
	if err != nil {
		t.Fatalf("ActiveRuns: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("expected 0 active runs after recovery, got %d", len(active))
	}

	// The stale digest opportunity row must still be present in the DB
	// (recovery does not delete it — the caller resumes it and updates it).
	counts, err := db.DigestOpportunityCounts()
	if err != nil {
		t.Fatalf("DigestOpportunityCounts: %v", err)
	}
	if counts.Investigating != 1 {
		t.Errorf("expected 1 opportunity still marked investigating, got %d", counts.Investigating)
	}
}

// TestRecoverOnStartup_NoStaleState_IsNoOp verifies a healthy DB with no
// stale runs or investigations produces a zero-value result and no errors.
func TestRecoverOnStartup_NoStaleState_IsNoOp(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db, err := OpenDBAt(dbPath)
	if err != nil {
		t.Fatalf("OpenDBAt: %v", err)
	}
	defer db.Close()

	if err := db.SaveRun(&Run{ID: "run-done-1", Status: "done", StartedAt: time.Now()}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	result, err := RecoverOnStartup(db)
	if err != nil {
		t.Fatalf("RecoverOnStartup: %v", err)
	}
	if result.StaleRuns != 0 {
		t.Errorf("expected StaleRuns = 0, got %d", result.StaleRuns)
	}
	if result.StaleInvestigations != 0 {
		t.Errorf("expected StaleInvestigations = 0, got %d", result.StaleInvestigations)
	}
	if len(result.StaleOpportunities) != 0 {
		t.Errorf("expected no stale opportunities, got %d", len(result.StaleOpportunities))
	}
}
