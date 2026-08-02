package state

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRecoverOnStartup_ReturnsStaleOpportunities seeds a file-backed temp DB
// with a digest opportunity stuck "investigating" (simulating a crash
// mid-investigation), then verifies RecoverOnStartup surfaces it for the
// caller to resume, while leaving a resolved opportunity untouched.
func TestRecoverOnStartup_ReturnsStaleOpportunities(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db, err := OpenDBAt(dbPath)
	if err != nil {
		t.Fatalf("OpenDBAt: %v", err)
	}
	defer db.Close()

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

	if result.StaleInvestigations != 1 {
		t.Errorf("expected StaleInvestigations = 1, got %d", result.StaleInvestigations)
	}
	if len(result.StaleOpportunities) != 1 {
		t.Fatalf("expected 1 stale opportunity, got %d", len(result.StaleOpportunities))
	}
	if result.StaleOpportunities[0].Summary != "billing double-charges users" {
		t.Errorf("unexpected stale opportunity returned: %+v", result.StaleOpportunities[0])
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
// stale investigations produces a zero-value result and no errors.
func TestRecoverOnStartup_NoStaleState_IsNoOp(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db, err := OpenDBAt(dbPath)
	if err != nil {
		t.Fatalf("OpenDBAt: %v", err)
	}
	defer db.Close()

	result, err := RecoverOnStartup(db)
	if err != nil {
		t.Fatalf("RecoverOnStartup: %v", err)
	}
	if result.StaleInvestigations != 0 {
		t.Errorf("expected StaleInvestigations = 0, got %d", result.StaleInvestigations)
	}
	if len(result.StaleOpportunities) != 0 {
		t.Errorf("expected no stale opportunities, got %d", len(result.StaleOpportunities))
	}
}
