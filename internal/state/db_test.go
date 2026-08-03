package state

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDB_ThreadMemory(t *testing.T) {
	db := openTestDB(t)

	if err := db.SaveThreadMemory("ts-1", "C123", `{"summary":"test"}`, "Here's my answer"); err != nil {
		t.Fatal(err)
	}

	mem, err := db.GetThreadMemory("ts-1")
	if err != nil {
		t.Fatal(err)
	}
	if mem == nil {
		t.Fatal("expected thread memory")
	}
	if mem.Channel != "C123" {
		t.Errorf("channel: got %q, want %q", mem.Channel, "C123")
	}
	if mem.Response != "Here's my answer" {
		t.Errorf("response: got %q", mem.Response)
	}
}

func TestDB_ThreadMemory_NotFound(t *testing.T) {
	db := openTestDB(t)
	mem, err := db.GetThreadMemory("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if mem != nil {
		t.Error("expected nil for nonexistent thread")
	}
}

func TestDB_PruneThreadMemory(t *testing.T) {
	db := openTestDB(t)
	// Insert directly with old timestamp
	db.db.Exec(
		"INSERT INTO thread_memory (thread_ts, channel, triage_json, response, created_at) VALUES (?, ?, ?, ?, ?)",
		"old-ts", "C123", "{}", "old response", time.Now().Add(-48*time.Hour),
	)
	db.SaveThreadMemory("new-ts", "C123", "{}", "new response")

	pruned, err := db.PruneThreadMemory(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Errorf("expected 1 pruned, got %d", pruned)
	}

	// New one should still exist
	mem, _ := db.GetThreadMemory("new-ts")
	if mem == nil {
		t.Error("new thread memory should survive pruning")
	}
}

func TestDB_ThreadMemoryCount(t *testing.T) {
	db := openTestDB(t)

	db.SaveThreadMemory("ts-1", "C1", "{}", "resp1")
	db.SaveThreadMemory("ts-2", "C1", "{}", "resp2")
	db.SaveThreadMemory("ts-3", "C2", "{}", "resp3")

	count, err := db.ThreadMemoryCount()
	if err != nil {
		t.Fatalf("ThreadMemoryCount(): %v", err)
	}
	if count != 3 {
		t.Errorf("ThreadMemoryCount: got %d, want 3", count)
	}
}

func TestDB_ThreadMemoryCount_Empty(t *testing.T) {
	db := openTestDB(t)
	count, err := db.ThreadMemoryCount()
	if err != nil {
		t.Fatalf("ThreadMemoryCount(): %v", err)
	}
	if count != 0 {
		t.Errorf("ThreadMemoryCount: got %d, want 0", count)
	}
}

func TestDB_DaemonStats(t *testing.T) {
	db := openTestDB(t)

	// Should be nil when never written
	ds, err := db.ReadDaemonStats()
	if err != nil {
		t.Fatalf("ReadDaemonStats: %v", err)
	}
	if ds != nil {
		t.Fatal("expected nil when never written")
	}

	// Write stats
	now := time.Now()
	stats := &DaemonStats{
		Heartbeat:        now,
		StartedAt:        now.Add(-1 * time.Hour),
		PID:              12345,
		Ribbits:          42,
		Triages:          100,
		TriageByCategory: map[string]int64{"bug": 30, "feature": 20, "question": 50},
		DigestEnabled:    true,
		DigestBuffer:     5,
		DigestProcessed:  200,
		DigestOpps:       3,
		DigestSpawns:     2,
	}
	if err := db.WriteDaemonStats(stats); err != nil {
		t.Fatalf("WriteDaemonStats: %v", err)
	}

	// Read back
	got, err := db.ReadDaemonStats()
	if err != nil {
		t.Fatalf("ReadDaemonStats: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil stats")
	}
	if got.PID != 12345 {
		t.Errorf("PID: got %d, want 12345", got.PID)
	}
	if got.Ribbits != 42 {
		t.Errorf("Ribbits: got %d, want 42", got.Ribbits)
	}
	if got.TriageByCategory["bug"] != 30 {
		t.Errorf("TriageByCategory[bug]: got %d, want 30", got.TriageByCategory["bug"])
	}
	if !got.DigestEnabled {
		t.Error("DigestEnabled: got false, want true")
	}
	if got.DigestProcessed != 200 {
		t.Errorf("DigestProcessed: got %d, want 200", got.DigestProcessed)
	}

	// Clear
	if err := db.ClearDaemonStats(); err != nil {
		t.Fatalf("ClearDaemonStats: %v", err)
	}
	got, err = db.ReadDaemonStats()
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("expected nil after clear")
	}
}

func TestDB_DigestOpportunities(t *testing.T) {
	db := openTestDB(t)

	// Empty at start
	opps, err := db.RecentDigestOpportunities(10)
	if err != nil {
		t.Fatalf("RecentDigestOpportunities: %v", err)
	}
	if len(opps) != 0 {
		t.Errorf("expected 0 opportunities, got %d", len(opps))
	}

	// Save a dry-run opportunity
	now := time.Now()
	err = db.SaveDigestOpportunity(&DigestOpportunity{
		Summary:    "Fix null pointer in handler",
		Category:   "bug",
		Confidence: 0.97,
		EstSize:    "small",
		Channel:    "C123",
		Message:    "there's a nil pointer crash in the handler",
		Keywords:   "nil,pointer,handler",
		DryRun:     true,
		CreatedAt:  now,
	})
	if err != nil {
		t.Fatalf("SaveDigestOpportunity: %v", err)
	}

	// Save a spawned opportunity
	err = db.SaveDigestOpportunity(&DigestOpportunity{
		Summary:    "Add missing validation",
		Category:   "bug",
		Confidence: 0.99,
		EstSize:    "tiny",
		Channel:    "C456",
		Message:    "validation is missing on the input field",
		Keywords:   "validation,input",
		DryRun:     false,
		CreatedAt:  now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("SaveDigestOpportunity: %v", err)
	}

	// Save a dismissed opportunity
	err = db.SaveDigestOpportunity(&DigestOpportunity{
		Summary:    "Refactor auth flow",
		Category:   "feature",
		Confidence: 0.96,
		EstSize:    "small",
		Channel:    "C789",
		Message:    "the auth flow is messy",
		Keywords:   "auth,refactor",
		DryRun:     false,
		Dismissed:  true,
		Reasoning:  "too complex, spans multiple services",
		CreatedAt:  now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("SaveDigestOpportunity (dismissed): %v", err)
	}

	// Retrieve — newest first
	opps, err = db.RecentDigestOpportunities(10)
	if err != nil {
		t.Fatalf("RecentDigestOpportunities: %v", err)
	}
	if len(opps) != 3 {
		t.Fatalf("expected 3 opportunities, got %d", len(opps))
	}

	// First should be the newest (dismissed)
	if opps[0].Summary != "Refactor auth flow" {
		t.Errorf("first opportunity summary: got %q, want %q", opps[0].Summary, "Refactor auth flow")
	}
	if !opps[0].Dismissed {
		t.Error("first opportunity should be dismissed")
	}
	if opps[0].Reasoning != "too complex, spans multiple services" {
		t.Errorf("first opportunity reasoning: got %q", opps[0].Reasoning)
	}

	// Second should be the spawned one
	if opps[1].Summary != "Add missing validation" {
		t.Errorf("second opportunity summary: got %q, want %q", opps[1].Summary, "Add missing validation")
	}
	if opps[1].DryRun {
		t.Error("second opportunity should not be dry-run")
	}
	if opps[1].Dismissed {
		t.Error("second opportunity should not be dismissed")
	}
	if opps[1].Confidence != 0.99 {
		t.Errorf("second opportunity confidence: got %f, want 0.99", opps[1].Confidence)
	}

	// Third should be the older one (dry-run)
	if opps[2].Summary != "Fix null pointer in handler" {
		t.Errorf("third opportunity summary: got %q", opps[2].Summary)
	}
	if !opps[2].DryRun {
		t.Error("third opportunity should be dry-run")
	}
	if opps[2].Channel != "C123" {
		t.Errorf("third opportunity channel: got %q, want %q", opps[2].Channel, "C123")
	}

	// Limit works
	opps, err = db.RecentDigestOpportunities(1)
	if err != nil {
		t.Fatalf("RecentDigestOpportunities(1): %v", err)
	}
	if len(opps) != 1 {
		t.Errorf("expected 1 opportunity with limit=1, got %d", len(opps))
	}
}

func TestDB_DigestOpportunity_InvestigatingLifecycle(t *testing.T) {
	db := openTestDB(t)

	// Save an investigating opportunity (pre-investigation)
	opp := &DigestOpportunity{
		Summary:       "Fix login crash",
		Category:      "bug",
		Confidence:    0.98,
		EstSize:       "small",
		Channel:       "C123",
		Message:       "login is crashing",
		Keywords:      "login,crash",
		DryRun:        false,
		Investigating: true,
		CreatedAt:     time.Now(),
	}
	if err := db.SaveDigestOpportunity(opp); err != nil {
		t.Fatalf("SaveDigestOpportunity: %v", err)
	}
	if opp.ID == 0 {
		t.Error("expected ID to be set after save")
	}

	// Verify it appears as investigating
	opps, _ := db.RecentDigestOpportunities(10)
	if len(opps) != 1 || !opps[0].Investigating {
		t.Fatalf("expected 1 investigating opportunity, got %d", len(opps))
	}

	// Counts should show 1 investigating
	counts, err := db.DigestOpportunityCounts()
	if err != nil {
		t.Fatalf("DigestOpportunityCounts: %v", err)
	}
	if counts.Investigating != 1 {
		t.Errorf("expected 1 investigating, got %d", counts.Investigating)
	}
	if counts.Approved != 0 || counts.Dismissed != 0 {
		t.Errorf("expected 0 approved/dismissed, got %d/%d", counts.Approved, counts.Dismissed)
	}

	// Complete investigation — approved
	opp.Investigating = false
	opp.Reasoning = "clear fix, single file"
	if err := db.UpdateDigestOpportunity(opp); err != nil {
		t.Fatalf("UpdateDigestOpportunity: %v", err)
	}

	// Verify updated state
	opps, _ = db.RecentDigestOpportunities(10)
	if opps[0].Investigating {
		t.Error("expected investigating to be false after update")
	}
	if opps[0].Reasoning != "clear fix, single file" {
		t.Errorf("expected reasoning to be updated, got %q", opps[0].Reasoning)
	}

	// Counts should show 1 approved, 0 investigating
	counts, _ = db.DigestOpportunityCounts()
	if counts.Approved != 1 {
		t.Errorf("expected 1 approved, got %d", counts.Approved)
	}
	if counts.Investigating != 0 {
		t.Errorf("expected 0 investigating, got %d", counts.Investigating)
	}

	// Save and dismiss another
	opp2 := &DigestOpportunity{
		Summary:       "Refactor utils",
		Category:      "feature",
		Confidence:    0.96,
		EstSize:       "small",
		Channel:       "C456",
		Investigating: true,
		CreatedAt:     time.Now(),
	}
	db.SaveDigestOpportunity(opp2)
	opp2.Investigating = false
	opp2.Dismissed = true
	opp2.Reasoning = "too complex"
	db.UpdateDigestOpportunity(opp2)

	counts, _ = db.DigestOpportunityCounts()
	if counts.Approved != 1 || counts.Dismissed != 1 || counts.Investigating != 0 {
		t.Errorf("counts: approved=%d dismissed=%d investigating=%d", counts.Approved, counts.Dismissed, counts.Investigating)
	}
}

func TestDB_StaleInvestigations(t *testing.T) {
	db := openTestDB(t)

	// Create two stuck investigating opportunities
	for _, summary := range []string{"stuck-1", "stuck-2"} {
		opp := &DigestOpportunity{
			Summary:       summary,
			Category:      "bug",
			Confidence:    0.9,
			EstSize:       "small",
			Channel:       "errors",
			ChannelID:     "C123",
			ThreadTS:      "111.222",
			Message:       "something broke: " + summary,
			Investigating: true,
			CreatedAt:     time.Now(),
		}
		if err := db.SaveDigestOpportunity(opp); err != nil {
			t.Fatalf("SaveDigestOpportunity: %v", err)
		}
	}

	// Create one completed opportunity (should not be returned)
	done := &DigestOpportunity{
		Summary:       "done",
		Category:      "bug",
		Confidence:    0.9,
		EstSize:       "small",
		Channel:       "errors",
		Investigating: false,
		CreatedAt:     time.Now(),
	}
	if err := db.SaveDigestOpportunity(done); err != nil {
		t.Fatalf("SaveDigestOpportunity: %v", err)
	}

	opps, err := db.StaleInvestigations()
	if err != nil {
		t.Fatalf("StaleInvestigations: %v", err)
	}
	if len(opps) != 2 {
		t.Fatalf("expected 2 stale, got %d", len(opps))
	}

	// Verify returned rows have enough data for resume
	for _, opp := range opps {
		if opp.ID == 0 {
			t.Error("expected ID to be set")
		}
		if opp.ChannelID != "C123" {
			t.Errorf("expected channel_id C123, got %q", opp.ChannelID)
		}
		if opp.Message == "" {
			t.Error("expected message to be populated")
		}
	}

	// Rows should still be in DB (not deleted)
	counts, _ := db.DigestOpportunityCounts()
	if counts.Investigating != 2 {
		t.Errorf("expected 2 still investigating, got %d", counts.Investigating)
	}

	// Simulate resume completing: update one row
	opps[0].Investigating = false
	opps[0].Reasoning = "approved after resume"
	if err := db.UpdateDigestOpportunity(opps[0]); err != nil {
		t.Fatalf("UpdateDigestOpportunity: %v", err)
	}

	// Now only 1 should be stale
	remaining, _ := db.StaleInvestigations()
	if len(remaining) != 1 {
		t.Errorf("expected 1 stale after partial resume, got %d", len(remaining))
	}
}

func TestDB_HasRecentOpportunity(t *testing.T) {
	db := openTestDB(t)

	// No opportunities yet
	has, err := db.HasRecentOpportunity("fix the bug", "", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no recent opportunity")
	}

	// Save one
	opp := &DigestOpportunity{
		Summary:   "fix the bug",
		Category:  "bug",
		Channel:   "C123",
		CreatedAt: time.Now(),
	}
	if err := db.SaveDigestOpportunity(opp); err != nil {
		t.Fatal(err)
	}

	// Exact summary match still works
	has, err = db.HasRecentOpportunity("fix the bug", "", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected to find recent opportunity by exact summary")
	}

	// Different summary, no keywords — should not match
	has, err = db.HasRecentOpportunity("different bug", "", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no match for different summary without keywords")
	}
}

func TestDB_HasRecentOpportunity_KeywordOverlap(t *testing.T) {
	db := openTestDB(t)

	// Save opportunity with keywords
	opp := &DigestOpportunity{
		Summary:   "Red dot indicator misaligned with actual alert severity in meter details",
		Category:  "bug",
		Keywords:  "meter,alert,red dot,indicator,severity,misalignment",
		Channel:   "C123",
		CreatedAt: time.Now(),
	}
	if err := db.SaveDigestOpportunity(opp); err != nil {
		t.Fatal(err)
	}

	// Different summary but overlapping keywords should match
	has, err := db.HasRecentOpportunity(
		"Red dot indicator misaligned with actual alert severity in meter alert view",
		"meter alert,red dot indicator,misaligned,alert severity",
		1*time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected keyword overlap to detect duplicate")
	}

	// Completely different keywords should not match
	has, err = db.HasRecentOpportunity(
		"Fix login page CSS",
		"login,css,styling,page",
		1*time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no match for unrelated keywords")
	}
}

// TestDB_HasRecentOpportunity_InvestigationErrorNotSuppressed is the I3 fix:
// an opportunity dismissed only because the investigation call itself
// errored (Reasoning prefixed with InvestigationErrorPrefix) must NOT
// suppress a similar new opportunity within the dedup window — a
// transient investigation failure shouldn't silence a genuinely recurring
// alert for up to an hour. Covers both the exact-summary fast path and the
// keyword-overlap fuzzy path.
func TestDB_HasRecentOpportunity_InvestigationErrorNotSuppressed(t *testing.T) {
	t.Run("exact summary match", func(t *testing.T) {
		db := openTestDB(t)

		opp := &DigestOpportunity{
			Summary:   "fix the bug",
			Category:  "bug",
			Channel:   "C123",
			Reasoning: InvestigationErrorPrefix + "context deadline exceeded",
			Dismissed: true,
			CreatedAt: time.Now(),
		}
		if err := db.SaveDigestOpportunity(opp); err != nil {
			t.Fatal(err)
		}

		has, err := db.HasRecentOpportunity("fix the bug", "", 1*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if has {
			t.Error("expected investigation-error row to NOT suppress an exact-summary match")
		}
	})

	t.Run("keyword overlap match", func(t *testing.T) {
		db := openTestDB(t)

		opp := &DigestOpportunity{
			Summary:   "Red dot indicator misaligned with actual alert severity in meter details",
			Category:  "bug",
			Keywords:  "meter,alert,red dot,indicator,severity,misalignment",
			Channel:   "C123",
			Reasoning: InvestigationErrorPrefix + "connection refused",
			Dismissed: true,
			CreatedAt: time.Now(),
		}
		if err := db.SaveDigestOpportunity(opp); err != nil {
			t.Fatal(err)
		}

		has, err := db.HasRecentOpportunity(
			"Red dot indicator misaligned with actual alert severity in meter alert view",
			"meter alert,red dot indicator,misaligned,alert severity",
			1*time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		if has {
			t.Error("expected investigation-error row to NOT suppress a keyword-overlap match")
		}
	})
}

// TestDB_HasRecentOpportunity_GenuinelyDismissedStillSuppresses guards
// against over-correcting I3: a genuinely infeasible opportunity (dismissed
// with ordinary reasoning, not the investigation-error prefix) must still
// suppress a similar new opportunity within the window, exactly as before.
func TestDB_HasRecentOpportunity_GenuinelyDismissedStillSuppresses(t *testing.T) {
	t.Run("exact summary match", func(t *testing.T) {
		db := openTestDB(t)

		opp := &DigestOpportunity{
			Summary:   "fix the bug",
			Category:  "bug",
			Channel:   "C123",
			Reasoning: "not feasible: this is already handled by existing validation",
			Dismissed: true,
			CreatedAt: time.Now(),
		}
		if err := db.SaveDigestOpportunity(opp); err != nil {
			t.Fatal(err)
		}

		has, err := db.HasRecentOpportunity("fix the bug", "", 1*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Error("expected a genuinely dismissed row to still suppress an exact-summary match")
		}
	})

	t.Run("keyword overlap match", func(t *testing.T) {
		db := openTestDB(t)

		opp := &DigestOpportunity{
			Summary:   "Red dot indicator misaligned with actual alert severity in meter details",
			Category:  "bug",
			Keywords:  "meter,alert,red dot,indicator,severity,misalignment",
			Channel:   "C123",
			Reasoning: "not feasible: working as intended",
			Dismissed: true,
			CreatedAt: time.Now(),
		}
		if err := db.SaveDigestOpportunity(opp); err != nil {
			t.Fatal(err)
		}

		has, err := db.HasRecentOpportunity(
			"Red dot indicator misaligned with actual alert severity in meter alert view",
			"meter alert,red dot indicator,misaligned,alert severity",
			1*time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Error("expected a genuinely dismissed row to still suppress a keyword-overlap match")
		}
	})
}

func TestKeywordOverlap(t *testing.T) {
	tests := []struct {
		name   string
		a, b   string
		expect float64
		above  float64
	}{
		{
			name:  "identical",
			a:     "meter,alert,red dot",
			b:     "meter,alert,red dot",
			above: 0.99,
		},
		{
			name:  "high overlap with different phrasing",
			a:     "meter,alert,red dot,indicator,severity,misalignment",
			b:     "meter alert,red dot indicator,misaligned,alert severity",
			above: 0.5,
		},
		{
			name:   "no overlap",
			a:      "login,css,styling",
			b:      "meter,alert,severity",
			expect: 0,
		},
		{
			name:  "useBreadcrumb duplicates",
			a:     "useBreadcrumb_experimental,breadcrumb,company,null,initialization",
			b:     "useBreadcrumb_experimental,company,null,initialization,breadcrumb",
			above: 0.99,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := keywordOverlap(normalizeKeywords(tt.a), normalizeKeywords(tt.b))
			if tt.above > 0 && score < tt.above {
				t.Errorf("expected overlap >= %.2f, got %.2f", tt.above, score)
			}
			if tt.expect == 0 && tt.above == 0 && score != 0 {
				t.Errorf("expected overlap == 0, got %.2f", score)
			}
		})
	}
}

func TestDB_GitHubSlackMapping_AddAndLookup(t *testing.T) {
	db := openTestDB(t)

	if err := db.AddGitHubMapping("U123", "johndoe"); err != nil {
		t.Fatalf("AddGitHubMapping: %v", err)
	}

	slackID, err := db.LookupSlackByGitHub("johndoe")
	if err != nil {
		t.Fatalf("LookupSlackByGitHub: %v", err)
	}
	if slackID != "U123" {
		t.Errorf("expected U123, got %q", slackID)
	}

	// Case-insensitive lookup
	slackID, _ = db.LookupSlackByGitHub("JohnDoe")
	if slackID != "U123" {
		t.Errorf("case-insensitive: expected U123, got %q", slackID)
	}

	// Unknown login returns empty
	slackID, _ = db.LookupSlackByGitHub("unknown")
	if slackID != "" {
		t.Errorf("expected empty, got %q", slackID)
	}
}

func TestDB_GitHubSlackMapping_MultiplePerUser(t *testing.T) {
	db := openTestDB(t)

	db.AddGitHubMapping("U123", "johndoe")
	db.AddGitHubMapping("U123", "john-work")

	logins, err := db.ListGitHubMappings("U123")
	if err != nil {
		t.Fatalf("ListGitHubMappings: %v", err)
	}
	if len(logins) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(logins))
	}
}

func TestDB_GitHubSlackMapping_UniqueGitHub(t *testing.T) {
	db := openTestDB(t)

	db.AddGitHubMapping("U123", "johndoe")
	err := db.AddGitHubMapping("U456", "johndoe")
	if err == nil {
		t.Fatal("expected error for duplicate github login")
	}
}

func TestDB_GitHubSlackMapping_Remove(t *testing.T) {
	db := openTestDB(t)

	db.AddGitHubMapping("U123", "johndoe")
	if err := db.RemoveGitHubMapping("U123", "johndoe"); err != nil {
		t.Fatalf("RemoveGitHubMapping: %v", err)
	}

	slackID, _ := db.LookupSlackByGitHub("johndoe")
	if slackID != "" {
		t.Errorf("expected empty after remove, got %q", slackID)
	}
}

func TestDB_MigratesToSchemaVersion12(t *testing.T) {
	db := openTestDB(t)

	version, err := db.GetSetting("schema_version")
	if err != nil {
		t.Fatalf("GetSetting(schema_version): %v", err)
	}
	if version != "12" {
		t.Errorf("schema_version: got %q, want %q", version, "12")
	}

	// Tables introduced in migration 10 must exist and be queryable.
	if _, err := db.db.Exec("SELECT external_key, investigation_id FROM ticket_index"); err != nil {
		t.Errorf("ticket_index table (with investigation_id column) not usable: %v", err)
	}
	if _, err := db.db.Exec("SELECT id, thread_ts, channel, repo, findings_json, created_at FROM investigations"); err != nil {
		t.Errorf("investigations table not usable: %v", err)
	}

	// Columns introduced in migration 11 must exist and be queryable.
	if _, err := db.db.Exec("SELECT token, expires_at FROM mcp_tokens"); err != nil {
		t.Errorf("mcp_tokens.expires_at column not usable: %v", err)
	}
	if _, err := db.db.Exec("SELECT external_key, last_state_type FROM ticket_index"); err != nil {
		t.Errorf("ticket_index.last_state_type column not usable: %v", err)
	}

	// Table/column introduced in migration 12 must exist and be queryable.
	if _, err := db.db.Exec("SELECT bucket, name, count FROM metrics_hourly"); err != nil {
		t.Errorf("metrics_hourly table not usable: %v", err)
	}
	if _, err := db.db.Exec("SELECT id, duration_ms FROM investigations"); err != nil {
		t.Errorf("investigations.duration_ms column not usable: %v", err)
	}
}

// TestDB_MigrationV12_AddsMetricsAndDuration exercises the real upgrade
// path for v12: a file-backed DB built with the genuine pre-v12 physical
// schema (no metrics_hourly table, investigations with no duration_ms
// column, schema_version explicitly at "11"), migrated via the package's
// actual migration entry point. Mirrors the fixture-based pattern used by
// TestDB_MigrationV11_ForcesTokenRotation for the same reason: a fresh
// openTestDB already has both the table and column via the base schema
// block, so a genuine pre-v12 fixture is needed to exercise the ALTER/CREATE
// statements themselves.
func TestDB_MigrationV12_AddsMetricsAndDuration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening raw fixture db: %v", err)
	}
	defer rawDB.Close()

	// Genuine pre-v12 physical schema: investigations with no duration_ms,
	// no metrics_hourly table at all.
	if _, err := rawDB.Exec(`
		CREATE TABLE investigations (
			id            TEXT PRIMARY KEY,
			thread_ts     TEXT,
			channel       TEXT,
			repo          TEXT,
			findings_json TEXT NOT NULL,
			created_at    DATETIME NOT NULL
		);
		CREATE TABLE settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`); err != nil {
		t.Fatalf("creating pre-v12 fixture schema: %v", err)
	}
	if _, err := rawDB.Exec(`INSERT INTO settings (key, value) VALUES ('schema_version', '11')`); err != nil {
		t.Fatalf("seeding schema_version=11: %v", err)
	}
	if _, err := rawDB.Exec(
		`INSERT INTO investigations (id, thread_ts, channel, repo, findings_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"invest-1", "111.222", "C123", "app", `{"title":"x"}`, time.Now(),
	); err != nil {
		t.Fatalf("seeding pre-v12 investigations row: %v", err)
	}

	// Confirm the fixture genuinely predates v12 before migrating.
	if cols := tableColumns(t, rawDB, "investigations"); cols["duration_ms"] {
		t.Fatal("fixture bug: investigations already has duration_ms before migrate")
	}
	var metricsTableCount int
	if err := rawDB.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='metrics_hourly'`,
	).Scan(&metricsTableCount); err != nil {
		t.Fatalf("checking metrics_hourly existence: %v", err)
	}
	if metricsTableCount != 0 {
		t.Fatal("fixture bug: metrics_hourly already exists before migrate")
	}

	if err := migrate(rawDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if cols := tableColumns(t, rawDB, "investigations"); !cols["duration_ms"] {
		t.Error("expected investigations.duration_ms to exist after migrate")
	}
	var durationMs sql.NullInt64
	if err := rawDB.QueryRow(
		`SELECT duration_ms FROM investigations WHERE id = ?`, "invest-1",
	).Scan(&durationMs); err != nil {
		t.Fatalf("reading duration_ms on surviving row: %v", err)
	}

	if _, err := rawDB.Exec(
		`INSERT INTO metrics_hourly (bucket, name, count) VALUES (?, ?, ?)`,
		"2026-08-02T14", "intake", 3,
	); err != nil {
		t.Fatalf("inserting into metrics_hourly after migrate: %v", err)
	}

	var version string
	if err := rawDB.QueryRow(`SELECT value FROM settings WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("reading schema_version: %v", err)
	}
	if version != "12" {
		t.Errorf("schema_version after migrate: got %q, want %q", version, "12")
	}
}

// tableColumns returns the column names of a table via PRAGMA table_info,
// for asserting a column does or doesn't exist yet.
func tableColumns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning pragma_table_info(%s): %v", table, err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating pragma_table_info(%s): %v", table, err)
	}
	return cols
}

// TestDB_MigrationV11_ForcesTokenRotation exercises the real upgrade path: a
// file-backed DB built with the genuine pre-v11 physical schema (mcp_tokens
// with no expires_at column, ticket_index with no last_state_type column,
// schema_version explicitly at "10"), seeded with a plaintext-era token row
// and a ticket_index row, then migrated via the package's actual migration
// entry point.
//
// Building this fixture via openTestDB (a fresh OpenDBAt(":memory:")) would
// be circular: the unconditional base-schema CREATE TABLE statements already
// include expires_at/last_state_type (see the v11 migration comment on why
// fresh DBs need those columns in the base schema, not just the ALTERs), so
// a fresh DB already has both columns before migrate() ever reaches the
// numbered-migrations loop. Forcing schema_version back to "10" on such a
// DB doesn't undo that — it just makes the v11 ALTER TABLE ... ADD COLUMN
// statements fail with "duplicate column name", which the migration loop
// only logs (slog.Warn) rather than returning as an error. The test would
// then still pass on a completely broken ALTER statement, since DELETE FROM
// mcp_tokens and the schema_version bump don't depend on the ALTERs having
// worked. Hence the raw CREATE TABLE fixture below, plus an explicit
// assertion that no such warning occurred.
func TestDB_MigrationV11_ForcesTokenRotation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening raw fixture db: %v", err)
	}
	defer rawDB.Close()

	// Genuine pre-v11 physical schema: no expires_at, no last_state_type.
	// investigations is included (pre-v12, no duration_ms) purely so the
	// unconditional base-schema block in migrate() treats it as already
	// existing (CREATE TABLE IF NOT EXISTS is a no-op) rather than creating
	// it fresh with duration_ms already baked in — which would make v12's
	// own ALTER TABLE ADD COLUMN fail with "duplicate column name".
	if _, err := rawDB.Exec(`
		CREATE TABLE mcp_tokens (
			token TEXT PRIMARY KEY,
			slack_user_id TEXT NOT NULL,
			slack_user TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user',
			created_at DATETIME NOT NULL,
			last_used_at DATETIME
		);
		CREATE TABLE ticket_index (
			external_key      TEXT PRIMARY KEY,
			issue_id          TEXT NOT NULL,
			issue_url         TEXT,
			source            TEXT DEFAULT '',
			investigation_id  TEXT DEFAULT '',
			created_at        DATETIME NOT NULL,
			last_seen_at      DATETIME NOT NULL,
			last_status       TEXT DEFAULT '',
			status_checked_at DATETIME
		);
		CREATE TABLE settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE investigations (
			id            TEXT PRIMARY KEY,
			thread_ts     TEXT,
			channel       TEXT,
			repo          TEXT,
			findings_json TEXT NOT NULL,
			created_at    DATETIME NOT NULL
		);
	`); err != nil {
		t.Fatalf("creating pre-v11 fixture schema: %v", err)
	}
	if _, err := rawDB.Exec(`INSERT INTO settings (key, value) VALUES ('schema_version', '10')`); err != nil {
		t.Fatalf("seeding schema_version=10: %v", err)
	}
	if _, err := rawDB.Exec(
		`INSERT INTO mcp_tokens (token, slack_user_id, slack_user, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		"plaintext-legacy-token", "U1", "alice", "user", time.Now(),
	); err != nil {
		t.Fatalf("seeding legacy plaintext token: %v", err)
	}
	if _, err := rawDB.Exec(
		`INSERT INTO ticket_index (external_key, issue_id, source, created_at, last_seen_at, last_status) VALUES (?, ?, ?, ?, ?, ?)`,
		"sentry:BILLING-2291", "SCL-100", "auto", time.Now(), time.Now(), "In Progress",
	); err != nil {
		t.Fatalf("seeding pre-v11 ticket_index row: %v", err)
	}

	// Confirm the fixture genuinely predates v11 before migrating.
	if cols := tableColumns(t, rawDB, "mcp_tokens"); cols["expires_at"] {
		t.Fatal("fixture bug: mcp_tokens already has expires_at before migrate")
	}
	if cols := tableColumns(t, rawDB, "ticket_index"); cols["last_state_type"] {
		t.Fatal("fixture bug: ticket_index already has last_state_type before migrate")
	}

	// Capture slog output so a swallowed "duplicate column name" warning
	// (which would mean the ALTER statements didn't actually apply) fails
	// the test instead of passing silently.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	if err := migrate(rawDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if strings.Contains(strings.ToLower(logBuf.String()), "duplicate column") {
		t.Errorf("migrate logged a duplicate-column warning — the v11 ALTER statements did not apply cleanly:\n%s", logBuf.String())
	}

	// mcp_tokens is wiped (forced rotation).
	var count int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM mcp_tokens`).Scan(&count); err != nil {
		t.Fatalf("counting mcp_tokens: %v", err)
	}
	if count != 0 {
		t.Errorf("expected v11 migration to wipe mcp_tokens, found %d rows", count)
	}

	// expires_at now exists and is queryable.
	if cols := tableColumns(t, rawDB, "mcp_tokens"); !cols["expires_at"] {
		t.Error("expected mcp_tokens.expires_at to exist after migrate")
	}

	// last_state_type now exists, and the surviving ticket_index row picked
	// up the '' default.
	if cols := tableColumns(t, rawDB, "ticket_index"); !cols["last_state_type"] {
		t.Fatal("expected ticket_index.last_state_type to exist after migrate")
	}
	var existingType string
	if err := rawDB.QueryRow(
		`SELECT last_state_type FROM ticket_index WHERE external_key = ?`, "sentry:BILLING-2291",
	).Scan(&existingType); err != nil {
		t.Fatalf("reading last_state_type on surviving row: %v", err)
	}
	if existingType != "" {
		t.Errorf("surviving ticket_index row: last_state_type = %q, want \"\"", existingType)
	}

	// Re-insert and read back a fresh row to confirm the column's default
	// applies going forward too, not just as an ALTER-time backfill.
	if _, err := rawDB.Exec(
		`INSERT INTO ticket_index (external_key, issue_id, source, created_at, last_seen_at, last_status) VALUES (?, ?, ?, ?, ?, ?)`,
		"sentry:BILLING-3000", "SCL-200", "auto", time.Now(), time.Now(), "Todo",
	); err != nil {
		t.Fatalf("inserting post-migration ticket_index row: %v", err)
	}
	var newType string
	if err := rawDB.QueryRow(
		`SELECT last_state_type FROM ticket_index WHERE external_key = ?`, "sentry:BILLING-3000",
	).Scan(&newType); err != nil {
		t.Fatalf("reading last_state_type on new row: %v", err)
	}
	if newType != "" {
		t.Errorf("new ticket_index row: last_state_type = %q, want \"\"", newType)
	}

	var version string
	if err := rawDB.QueryRow(`SELECT value FROM settings WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("reading schema_version: %v", err)
	}
	if version != "12" {
		t.Errorf("schema_version after migrate: got %q, want %q", version, "12")
	}
}

// TestDB_PreVersionedDB_ProbeFreezesToV8AndRunsLaterMigrations exercises the
// fix to migrate()'s pre-versioned-DB fallback: a real 2026-03-09..11-era DB
// (created before schema_version tracking existed at all) has
// pr_watches.original_summary — the v8-era ad-hoc marker this probe checks —
// but predates every migration added after schema_version tracking began:
// v9's runs.claim_scope, v10's ticket_index/investigations tables, v11's
// mcp_tokens hardening, v12's metrics_hourly/duration_ms.
//
// Before this fix, the probe set currentVersion = len(migrations) whenever
// it fired. Because len(migrations) is evaluated against the CURRENT
// (ever-growing) migrations slice, not whatever count existed when this
// fallback was originally written, that unconditionally jumped such a DB
// straight to "fully migrated" — silently skipping v9 through v12 forever,
// even though none of them ever ran. Concretely, this meant a DB from that
// window would never get mcp_tokens.expires_at or the plaintext-token wipe,
// and would hit a runtime "no such column" error the first time
// SaveMCPToken/ValidateMCPToken ran against it.
//
// This seeds a genuine pre-v9 physical schema (no schema_version row at
// all — the ad-hoc-migration-era condition) with a legacy plaintext mcp
// token, migrates it via the real probe path, and asserts v9, v11, and v12
// all actually ran rather than being skipped.
func TestDB_PreVersionedDB_ProbeFreezesToV8AndRunsLaterMigrations(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening raw fixture db: %v", err)
	}
	defer rawDB.Close()

	// Genuine v8-era physical schema: pr_watches carries the probe's marker
	// column (original_summary), but runs.claim_scope (v9) and
	// mcp_tokens.expires_at (v11) do not exist yet, and there is no
	// schema_version row anywhere (the ad-hoc code predates that tracking).
	if _, err := rawDB.Exec(`
		CREATE TABLE pr_watches (
			pr_number            INTEGER PRIMARY KEY,
			pr_url               TEXT NOT NULL,
			branch               TEXT NOT NULL,
			run_id               TEXT NOT NULL,
			original_summary     TEXT DEFAULT '',
			original_description TEXT DEFAULT ''
		);
		CREATE TABLE runs (
			id            TEXT PRIMARY KEY,
			status        TEXT NOT NULL,
			slack_channel TEXT,
			slack_thread  TEXT,
			branch        TEXT,
			worktree_path TEXT,
			task          TEXT,
			repo_name     TEXT DEFAULT '',
			started_at    DATETIME NOT NULL,
			result_json   TEXT,
			updated_at    DATETIME NOT NULL
		);
		CREATE TABLE settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE mcp_tokens (
			token         TEXT PRIMARY KEY,
			slack_user_id TEXT NOT NULL,
			slack_user    TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'user',
			created_at    DATETIME NOT NULL,
			last_used_at  DATETIME
		);
	`); err != nil {
		t.Fatalf("creating v8-era fixture schema: %v", err)
	}
	if _, err := rawDB.Exec(
		`INSERT INTO mcp_tokens (token, slack_user_id, slack_user, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		"plaintext-legacy-token", "U1", "alice", "user", time.Now(),
	); err != nil {
		t.Fatalf("seeding legacy plaintext token: %v", err)
	}

	// Confirm the fixture genuinely predates v9/v11 and carries no
	// schema_version row, so migrate() takes the pre-versioned-DB probe path.
	if cols := tableColumns(t, rawDB, "runs"); cols["claim_scope"] {
		t.Fatal("fixture bug: runs already has claim_scope before migrate")
	}
	if cols := tableColumns(t, rawDB, "mcp_tokens"); cols["expires_at"] {
		t.Fatal("fixture bug: mcp_tokens already has expires_at before migrate")
	}
	var versionRowCount int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'schema_version'`).Scan(&versionRowCount); err != nil {
		t.Fatalf("checking schema_version row: %v", err)
	}
	if versionRowCount != 0 {
		t.Fatal("fixture bug: schema_version row already present before migrate")
	}

	if err := migrate(rawDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// v9 ran: runs.claim_scope exists.
	if cols := tableColumns(t, rawDB, "runs"); !cols["claim_scope"] {
		t.Error("expected runs.claim_scope to exist after migrate (v9 must not be skipped)")
	}
	// v11 ran: mcp_tokens was force-rotated (wiped) and expires_at exists.
	var tokenCount int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM mcp_tokens`).Scan(&tokenCount); err != nil {
		t.Fatalf("counting mcp_tokens: %v", err)
	}
	if tokenCount != 0 {
		t.Errorf("expected v11 to wipe mcp_tokens (not be skipped), found %d rows", tokenCount)
	}
	if cols := tableColumns(t, rawDB, "mcp_tokens"); !cols["expires_at"] {
		t.Error("expected mcp_tokens.expires_at to exist after migrate (v11 must not be skipped)")
	}
	// v12 ran: metrics_hourly exists and investigations.duration_ms exists.
	var metricsTableCount int
	if err := rawDB.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='metrics_hourly'`,
	).Scan(&metricsTableCount); err != nil {
		t.Fatalf("checking metrics_hourly existence: %v", err)
	}
	if metricsTableCount == 0 {
		t.Error("expected metrics_hourly to exist after migrate (v12 must not be skipped)")
	}
	if cols := tableColumns(t, rawDB, "investigations"); !cols["duration_ms"] {
		t.Error("expected investigations.duration_ms to exist after migrate (v12 must not be skipped)")
	}

	var version string
	if err := rawDB.QueryRow(`SELECT value FROM settings WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("reading schema_version: %v", err)
	}
	if version != "12" {
		t.Errorf("schema_version after migrate: got %q, want %q", version, "12")
	}
}

// TestApplyMigrations_GenuineFailureDoesNotAdvanceVersion is the "otherwise
// return the error without persisting the new version" half of the
// migration-robustness fix: a statement failing for a reason OTHER than
// "the change already exists" (isBenignMigrationError) must abort the whole
// migration run, leaving the returned version at the last one that actually
// completed — not the failing migration's version, and not any migration
// after it. Exercised directly against applyMigrations with a synthetic
// migrations slice (per the migrations slice being a fixed, package-internal
// list inside migrate() otherwise) rather than trying to make one of the
// real migrations fail for a non-benign reason.
func TestApplyMigrations_GenuineFailureDoesNotAdvanceVersion(t *testing.T) {
	rawDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening raw db: %v", err)
	}
	defer rawDB.Close()

	migrations := []migration{
		{1, `CREATE TABLE t1 (id INTEGER PRIMARY KEY)`},
		// Genuine failure: ALTER on a table that doesn't exist. SQLite's
		// error here ("no such table: t_missing") is neither "duplicate
		// column name" nor "already exists", so this must abort rather than
		// being logged-and-skipped.
		{2, `ALTER TABLE t_missing ADD COLUMN x TEXT`},
		{3, `CREATE TABLE t3 (id INTEGER PRIMARY KEY)`},
	}

	version, err := applyMigrations(rawDB, migrations, 0)
	if err == nil {
		t.Fatal("expected an error from the genuinely failing migration statement")
	}
	if version != 1 {
		t.Errorf("expected the returned version to stay at 1 (the last migration that actually completed), got %d", version)
	}

	var t1Count int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='t1'`).Scan(&t1Count); err != nil {
		t.Fatalf("checking t1 existence: %v", err)
	}
	if t1Count == 0 {
		t.Error("expected migration 1 to have run before the failure")
	}
	var t3Count int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='t3'`).Scan(&t3Count); err != nil {
		t.Fatalf("checking t3 existence: %v", err)
	}
	if t3Count != 0 {
		t.Error("expected migration 3 to NOT have run after an earlier genuine failure")
	}
}

// TestIsBenignMigrationError pins the exact classification
// isBenignMigrationError relies on: SQLite's "duplicate column name" and
// "already exists" mean the change a migration statement makes is already
// present (safe to log-and-continue), while anything else is a genuine
// failure that must abort the migration.
func TestIsBenignMigrationError(t *testing.T) {
	cases := []struct {
		name   string
		errMsg string
		want   bool
	}{
		{"duplicate column", "duplicate column name: expires_at", true},
		{"table already exists", "table ticket_index already exists", true},
		{"index already exists", "index idx_invest_thread already exists", true},
		{"no such table", "no such table: t_missing", false},
		{"syntax error", `near "FROM": syntax error`, false},
		{"database locked", "database is locked", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBenignMigrationError(fmt.Errorf("%s", tc.errMsg)); got != tc.want {
				t.Errorf("isBenignMigrationError(%q) = %v, want %v", tc.errMsg, got, tc.want)
			}
		})
	}
}

// TestDB_Migrate_SecondCallOnFreshDBIsNoOp confirms migrate() can be safely
// re-invoked on a DB that OpenDBAt already fully migrated (e.g. a daemon
// restart) without erroring. Because a fresh DB's base schema already
// contains everything through v12 (see the v11/v12 migration comments), this
// exercises the schema_version gate short-circuiting every migration —
// it does NOT exercise the v11/v12 ALTER statements themselves on a genuine
// pre-v11 schema; TestDB_MigrationV11_ForcesTokenRotation and
// TestDB_MigrationV12_AddsMetricsAndDuration cover those.
func TestDB_Migrate_SecondCallOnFreshDBIsNoOp(t *testing.T) {
	db := openTestDB(t) // already migrated to v12 by OpenDBAt

	if err := migrate(db.db); err != nil {
		t.Fatalf("second migrate call should be a no-op, got error: %v", err)
	}

	version, err := db.GetSetting("schema_version")
	if err != nil {
		t.Fatalf("GetSetting(schema_version): %v", err)
	}
	if version != "12" {
		t.Errorf("schema_version after second migrate: got %q, want %q", version, "12")
	}
}

func TestDB_MigrationIdempotent_ReopenFileBackedDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db1, err := OpenDBAt(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := db1.UpsertTicketIndex(&TicketIndexEntry{
		ExternalKey: "thread:C123:1722500000.000100",
		IssueID:     "SCL-1482",
		IssueURL:    "https://linear.app/scl/issue/SCL-1482",
		Source:      "auto",
		CreatedAt:   time.Now(),
		LastSeenAt:  time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTicketIndex: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("closing first handle: %v", err)
	}

	// Reopening the same file-backed DB must not error and must not
	// re-run migrations destructively — the row inserted above must survive.
	db2, err := OpenDBAt(dbPath)
	if err != nil {
		t.Fatalf("second open (idempotency): %v", err)
	}
	defer db2.Close()

	version, err := db2.GetSetting("schema_version")
	if err != nil {
		t.Fatalf("GetSetting(schema_version): %v", err)
	}
	if version != "12" {
		t.Errorf("schema_version after reopen: got %q, want %q", version, "12")
	}

	entry, err := db2.GetTicketIndex("thread:C123:1722500000.000100")
	if err != nil {
		t.Fatalf("GetTicketIndex after reopen: %v", err)
	}
	if entry == nil {
		t.Fatal("expected ticket_index row to survive reopen")
	}
	if entry.IssueID != "SCL-1482" {
		t.Errorf("IssueID after reopen: got %q, want %q", entry.IssueID, "SCL-1482")
	}
}

func TestDB_UpsertTicketIndex_UpdatesLastSeenOnConflict(t *testing.T) {
	db := openTestDB(t)

	created := time.Now().Add(-time.Hour).Truncate(time.Second)
	firstSeen := created
	if err := db.UpsertTicketIndex(&TicketIndexEntry{
		ExternalKey: "sentry:BILLING-2291",
		IssueID:     "SCL-100",
		IssueURL:    "https://linear.app/scl/issue/SCL-100",
		Source:      "auto",
		CreatedAt:   created,
		LastSeenAt:  firstSeen,
	}); err != nil {
		t.Fatalf("first UpsertTicketIndex: %v", err)
	}

	secondSeen := time.Now().Add(time.Minute).Truncate(time.Second)
	if err := db.UpsertTicketIndex(&TicketIndexEntry{
		ExternalKey: "sentry:BILLING-2291",
		IssueID:     "SCL-100",
		IssueURL:    "https://linear.app/scl/issue/SCL-100",
		Source:      "auto",
		CreatedAt:   created,
		LastSeenAt:  secondSeen,
	}); err != nil {
		t.Fatalf("second UpsertTicketIndex (conflict): %v", err)
	}

	// Exactly one row must remain.
	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM ticket_index WHERE external_key = ?", "sentry:BILLING-2291").Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after upsert conflict, got %d", count)
	}

	entry, err := db.GetTicketIndex("sentry:BILLING-2291")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected entry to exist")
	}
	if !entry.LastSeenAt.Equal(secondSeen) {
		t.Errorf("LastSeenAt: got %v, want %v (bumped)", entry.LastSeenAt, secondSeen)
	}
	if !entry.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt should be unchanged: got %v, want %v", entry.CreatedAt, created)
	}
}

// TestDB_UpsertTicketIndex_PreservesStatusAndInvestigationOnReObservation
// guards against the natural calling pattern — build a fresh
// TicketIndexEntry per incoming event, call Upsert just to bump
// last_seen_at — silently wiping out last_status/status_checked_at/
// investigation_id that were set by an earlier, separate call (e.g. via
// UpdateTicketStatus or a prior Upsert that linked an investigation).
func TestDB_UpsertTicketIndex_PreservesStatusAndInvestigationOnReObservation(t *testing.T) {
	db := openTestDB(t)

	created := time.Now().Add(-time.Hour).Truncate(time.Second)
	statusCheckedAt := time.Now().Add(-30 * time.Minute).Truncate(time.Second)

	// First observation: links an investigation and records a status.
	if err := db.UpsertTicketIndex(&TicketIndexEntry{
		ExternalKey:     "sentry:BILLING-2291",
		IssueID:         "SCL-100",
		IssueURL:        "https://linear.app/scl/issue/SCL-100",
		Source:          "auto",
		InvestigationID: "inv-1",
		LastStatus:      "in_progress",
		StatusCheckedAt: statusCheckedAt,
		CreatedAt:       created,
		LastSeenAt:      created,
	}); err != nil {
		t.Fatalf("first UpsertTicketIndex: %v", err)
	}

	// Second observation: a duplicate Sentry alert / repeat thread mention.
	// Callers build a fresh entry per event, so LastStatus, InvestigationID,
	// and StatusCheckedAt are all zero-valued here — this must NOT erase
	// what the first call recorded.
	secondSeen := time.Now().Truncate(time.Second)
	if err := db.UpsertTicketIndex(&TicketIndexEntry{
		ExternalKey: "sentry:BILLING-2291",
		IssueID:     "SCL-100",
		IssueURL:    "https://linear.app/scl/issue/SCL-100",
		Source:      "auto",
		CreatedAt:   created,
		LastSeenAt:  secondSeen,
		// LastStatus, StatusCheckedAt, InvestigationID intentionally left zero.
	}); err != nil {
		t.Fatalf("second UpsertTicketIndex (re-observation): %v", err)
	}

	entry, err := db.GetTicketIndex("sentry:BILLING-2291")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected entry to exist")
	}
	if !entry.LastSeenAt.Equal(secondSeen) {
		t.Errorf("LastSeenAt: got %v, want %v (should still bump)", entry.LastSeenAt, secondSeen)
	}
	if entry.InvestigationID != "inv-1" {
		t.Errorf("InvestigationID clobbered by re-observation: got %q, want %q", entry.InvestigationID, "inv-1")
	}
	if entry.LastStatus != "in_progress" {
		t.Errorf("LastStatus clobbered by re-observation: got %q, want %q", entry.LastStatus, "in_progress")
	}
	if !entry.StatusCheckedAt.Equal(statusCheckedAt) {
		t.Errorf("StatusCheckedAt clobbered by re-observation: got %v, want %v", entry.StatusCheckedAt, statusCheckedAt)
	}

	// Sanity: FindInvestigationByTicket link must also survive.
	if err := db.SaveInvestigation(&InvestigationRecord{
		ID:           "inv-1",
		ThreadTS:     "1722500000.000100",
		Channel:      "C123",
		Repo:         "toad",
		FindingsJSON: `{"summary":"billing crash"}`,
		CreatedAt:    created,
	}); err != nil {
		t.Fatalf("SaveInvestigation: %v", err)
	}
	found, err := db.FindInvestigationByTicket("SCL-100")
	if err != nil {
		t.Fatalf("FindInvestigationByTicket: %v", err)
	}
	if found == nil || found.ID != "inv-1" {
		t.Errorf("expected investigation link to survive re-observation, got %v", found)
	}

	// A subsequent Upsert that DOES carry a new, non-empty value must still
	// be able to update the field — only zero values are "leave alone".
	if err := db.UpsertTicketIndex(&TicketIndexEntry{
		ExternalKey: "sentry:BILLING-2291",
		IssueID:     "SCL-100",
		IssueURL:    "https://linear.app/scl/issue/SCL-100",
		Source:      "auto",
		LastStatus:  "resolved",
		CreatedAt:   created,
		LastSeenAt:  time.Now().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("third UpsertTicketIndex (explicit status update): %v", err)
	}
	entry, err = db.GetTicketIndex("sentry:BILLING-2291")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry.LastStatus != "resolved" {
		t.Errorf("explicit non-empty LastStatus should still apply: got %q, want %q", entry.LastStatus, "resolved")
	}
	if entry.InvestigationID != "inv-1" {
		t.Errorf("InvestigationID should still survive when not explicitly changed: got %q", entry.InvestigationID)
	}
}

func TestDB_GetTicketIndex_NotFound(t *testing.T) {
	db := openTestDB(t)

	entry, err := db.GetTicketIndex("nonexistent-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entry != nil {
		t.Errorf("expected nil, got %v", entry)
	}
}

func TestDB_RecentTicketIndex(t *testing.T) {
	db := openTestDB(t)

	now := time.Now()
	for i := 0; i < 3; i++ {
		if err := db.UpsertTicketIndex(&TicketIndexEntry{
			ExternalKey: fmt.Sprintf("sentry:KEY-%d", i),
			IssueID:     fmt.Sprintf("SCL-%d", i),
			Source:      "auto",
			CreatedAt:   now.Add(time.Duration(i) * time.Second),
			LastSeenAt:  now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("UpsertTicketIndex %d: %v", i, err)
		}
	}

	entries, err := db.RecentTicketIndex(10)
	if err != nil {
		t.Fatalf("RecentTicketIndex: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	// Newest (highest last_seen_at) first.
	if entries[0].IssueID != "SCL-2" {
		t.Errorf("expected newest first (SCL-2), got %q", entries[0].IssueID)
	}

	limited, err := db.RecentTicketIndex(1)
	if err != nil {
		t.Fatalf("RecentTicketIndex(1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("expected 1 entry with limit=1, got %d", len(limited))
	}
}

func TestDB_UpdateTicketStatus(t *testing.T) {
	db := openTestDB(t)

	if err := db.UpsertTicketIndex(&TicketIndexEntry{
		ExternalKey: "sentry:BILLING-2291",
		IssueID:     "SCL-100",
		Source:      "auto",
		CreatedAt:   time.Now(),
		LastSeenAt:  time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTicketIndex: %v", err)
	}

	if err := db.UpdateTicketStatus("sentry:BILLING-2291", "in_progress", "started"); err != nil {
		t.Fatalf("UpdateTicketStatus: %v", err)
	}

	entry, err := db.GetTicketIndex("sentry:BILLING-2291")
	if err != nil {
		t.Fatalf("GetTicketIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected entry")
	}
	if entry.LastStatus != "in_progress" {
		t.Errorf("LastStatus: got %q, want %q", entry.LastStatus, "in_progress")
	}
	if entry.LastStateType != "started" {
		t.Errorf("LastStateType: got %q, want %q", entry.LastStateType, "started")
	}
	if entry.StatusCheckedAt.IsZero() {
		t.Error("expected StatusCheckedAt to be set")
	}
}

func TestDB_SaveInvestigation_GetByThread(t *testing.T) {
	db := openTestDB(t)

	older := &InvestigationRecord{
		ID:           "inv-1",
		ThreadTS:     "1722500000.000100",
		Channel:      "C123",
		Repo:         "toad",
		FindingsJSON: `{"summary":"first pass"}`,
		CreatedAt:    time.Now().Add(-time.Hour),
	}
	if err := db.SaveInvestigation(older); err != nil {
		t.Fatalf("SaveInvestigation (older): %v", err)
	}

	newer := &InvestigationRecord{
		ID:           "inv-2",
		ThreadTS:     "1722500000.000100",
		Channel:      "C123",
		Repo:         "toad",
		FindingsJSON: `{"summary":"second pass"}`,
		CreatedAt:    time.Now(),
	}
	if err := db.SaveInvestigation(newer); err != nil {
		t.Fatalf("SaveInvestigation (newer): %v", err)
	}

	got, err := db.GetInvestigationByThread("1722500000.000100")
	if err != nil {
		t.Fatalf("GetInvestigationByThread: %v", err)
	}
	if got == nil {
		t.Fatal("expected investigation")
	}
	if got.ID != "inv-2" {
		t.Errorf("expected newest investigation (inv-2), got %q", got.ID)
	}
	if got.FindingsJSON != `{"summary":"second pass"}` {
		t.Errorf("FindingsJSON: got %q", got.FindingsJSON)
	}
}

func TestDB_GetInvestigationByThread_NotFound(t *testing.T) {
	db := openTestDB(t)

	got, err := db.GetInvestigationByThread("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestDB_FindInvestigationByTicket(t *testing.T) {
	db := openTestDB(t)

	rec := &InvestigationRecord{
		ID:           "inv-1",
		ThreadTS:     "1722500000.000100",
		Channel:      "C123",
		Repo:         "toad",
		FindingsJSON: `{"summary":"nil pointer crash"}`,
		CreatedAt:    time.Now(),
	}
	if err := db.SaveInvestigation(rec); err != nil {
		t.Fatalf("SaveInvestigation: %v", err)
	}

	if err := db.UpsertTicketIndex(&TicketIndexEntry{
		ExternalKey:     "thread:C123:1722500000.000100",
		IssueID:         "SCL-1482",
		IssueURL:        "https://linear.app/scl/issue/SCL-1482",
		Source:          "auto",
		InvestigationID: "inv-1",
		CreatedAt:       time.Now(),
		LastSeenAt:      time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTicketIndex: %v", err)
	}

	got, err := db.FindInvestigationByTicket("SCL-1482")
	if err != nil {
		t.Fatalf("FindInvestigationByTicket: %v", err)
	}
	if got == nil {
		t.Fatal("expected investigation resolved via ticket_index")
	}
	if got.ID != "inv-1" {
		t.Errorf("ID: got %q, want %q", got.ID, "inv-1")
	}
	if got.FindingsJSON != `{"summary":"nil pointer crash"}` {
		t.Errorf("FindingsJSON: got %q", got.FindingsJSON)
	}
}

func TestDB_FindInvestigationByTicket_NotFound(t *testing.T) {
	db := openTestDB(t)

	got, err := db.FindInvestigationByTicket("SCL-9999")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestDB_TicketForInvestigation(t *testing.T) {
	db := openTestDB(t)

	rec := &InvestigationRecord{
		ID:           "inv-1",
		ThreadTS:     "1722500000.000100",
		Channel:      "C123",
		Repo:         "toad",
		FindingsJSON: `{"summary":"nil pointer crash"}`,
		CreatedAt:    time.Now(),
	}
	if err := db.SaveInvestigation(rec); err != nil {
		t.Fatalf("SaveInvestigation: %v", err)
	}
	if err := db.UpsertTicketIndex(&TicketIndexEntry{
		ExternalKey:     "thread:C123:1722500000.000100",
		IssueID:         "SCL-1482",
		IssueURL:        "https://linear.app/scl/issue/SCL-1482",
		Source:          "auto",
		InvestigationID: "inv-1",
		CreatedAt:       time.Now(),
		LastSeenAt:      time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTicketIndex: %v", err)
	}

	got, err := db.TicketForInvestigation("inv-1")
	if err != nil {
		t.Fatalf("TicketForInvestigation: %v", err)
	}
	if got == nil {
		t.Fatal("expected ticket resolved via investigation_id")
	}
	if got.IssueID != "SCL-1482" {
		t.Errorf("IssueID: got %q, want %q", got.IssueID, "SCL-1482")
	}
}

func TestDB_TicketForInvestigation_NotFound(t *testing.T) {
	db := openTestDB(t)

	got, err := db.TicketForInvestigation("inv-nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}

	got, err = db.TicketForInvestigation("")
	if err != nil {
		t.Fatalf("unexpected error for empty id: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty investigation id, got %v", got)
	}
}

func TestDB_RecentInvestigations(t *testing.T) {
	db := openTestDB(t)

	for i := 0; i < 3; i++ {
		rec := &InvestigationRecord{
			ID:           fmt.Sprintf("inv-%d", i),
			ThreadTS:     fmt.Sprintf("ts-%d", i),
			Channel:      "C123",
			Repo:         "toad",
			FindingsJSON: `{"title":"x"}`,
			DurationMs:   int64(1000 * (i + 1)),
			CreatedAt:    time.Now().Add(time.Duration(i) * time.Minute),
		}
		if err := db.SaveInvestigation(rec); err != nil {
			t.Fatalf("SaveInvestigation: %v", err)
		}
	}

	recs, err := db.RecentInvestigations(2)
	if err != nil {
		t.Fatalf("RecentInvestigations: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	// Newest first.
	if recs[0].ID != "inv-2" {
		t.Errorf("expected newest first (inv-2), got %q", recs[0].ID)
	}
	if recs[0].DurationMs != 3000 {
		t.Errorf("DurationMs: got %d, want 3000", recs[0].DurationMs)
	}
}

func TestDB_IncrementMetric_And_MetricSeries(t *testing.T) {
	db := openTestDB(t)

	now := time.Date(2026, 8, 2, 14, 30, 0, 0, time.UTC)
	if err := db.IncrementMetric("intake", now); err != nil {
		t.Fatalf("IncrementMetric: %v", err)
	}
	if err := db.IncrementMetric("intake", now); err != nil {
		t.Fatalf("IncrementMetric: %v", err)
	}
	if err := db.IncrementMetric("intake", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("IncrementMetric (2h ago): %v", err)
	}

	series := db.MetricSeries("intake", 4, now)
	if len(series) != 4 {
		t.Fatalf("expected series length 4, got %d", len(series))
	}
	// [now-3h, now-2h, now-1h, now] -> [0, 1, 0, 2]
	want := []int{0, 1, 0, 2}
	for i := range want {
		if series[i] != want[i] {
			t.Errorf("series[%d]: got %d, want %d (series=%v)", i, series[i], want[i], series)
		}
	}

	// A different metric name has its own independent series.
	other := db.MetricSeries("qa", 4, now)
	for i, v := range other {
		if v != 0 {
			t.Errorf("unrelated metric series[%d]: got %d, want 0", i, v)
		}
	}
}

func TestDB_MetricSeries_EmptyTableDegradesGracefully(t *testing.T) {
	db := openTestDB(t)
	series := db.MetricSeries("nonexistent", 5, time.Now())
	if len(series) != 5 {
		t.Fatalf("expected zero-filled length-5 series, got %v", series)
	}
	for i, v := range series {
		if v != 0 {
			t.Errorf("series[%d]: got %d, want 0", i, v)
		}
	}
}

func TestDB_MetricSeriesDaily(t *testing.T) {
	db := openTestDB(t)

	now := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	// Two events today, in different hours.
	if err := db.IncrementMetric("filed", now); err != nil {
		t.Fatal(err)
	}
	if err := db.IncrementMetric("filed", now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// One event yesterday.
	if err := db.IncrementMetric("filed", now.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}

	series := db.MetricSeriesDaily("filed", 3, now)
	if len(series) != 3 {
		t.Fatalf("expected series length 3, got %d", len(series))
	}
	// [2 days ago, yesterday, today] -> [0, 1, 2]
	want := []int{0, 1, 2}
	for i := range want {
		if series[i] != want[i] {
			t.Errorf("series[%d]: got %d, want %d (series=%v)", i, series[i], want[i], series)
		}
	}
}
