package state

import (
	"fmt"
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

func TestDB_SaveAndGetByThread(t *testing.T) {
	db := openTestDB(t)
	run := &Run{
		ID:            "run-1",
		Status:        "running",
		SlackChannel:  "C123",
		SlackThreadTS: "1234.5678",
		Branch:        "fix-bug",
		Task:          "fix the bug",
		StartedAt:     time.Now(),
	}
	if err := db.SaveRun(run); err != nil {
		t.Fatalf("save run: %v", err)
	}

	got, err := db.GetByThread("1234.5678")
	if err != nil {
		t.Fatalf("get by thread: %v", err)
	}
	if got == nil {
		t.Fatal("expected to find run by thread")
	}
	if got.ID != "run-1" {
		t.Errorf("got ID %q, want %q", got.ID, "run-1")
	}
	if got.Task != "fix the bug" {
		t.Errorf("got Task %q, want %q", got.Task, "fix the bug")
	}
}

func TestDB_GetByThread_NotFound(t *testing.T) {
	db := openTestDB(t)
	got, err := db.GetByThread("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestDB_GetByThread_SkipsCompleted(t *testing.T) {
	db := openTestDB(t)
	run := &Run{
		ID:            "run-1",
		Status:        "running",
		SlackThreadTS: "1234.5678",
		StartedAt:     time.Now(),
	}
	if err := db.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteRun("run-1", &RunResult{Success: true}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetByThread("1234.5678")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("completed run should not be returned by GetByThread")
	}
}

func TestDB_UpdateStatus(t *testing.T) {
	db := openTestDB(t)
	run := &Run{ID: "run-1", Status: "starting", StartedAt: time.Now()}
	if err := db.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	if err := db.UpdateStatus("run-1", "running"); err != nil {
		t.Fatal(err)
	}

	runs, err := db.ActiveRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != "running" {
		t.Errorf("expected status 'running', got %v", runs)
	}
}

func TestDB_CompleteRun(t *testing.T) {
	db := openTestDB(t)
	run := &Run{ID: "run-1", Status: "running", StartedAt: time.Now()}
	if err := db.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	result := &RunResult{Success: true, PRUrl: "https://github.com/pr/1", FilesChanged: 3}
	if err := db.CompleteRun("run-1", result); err != nil {
		t.Fatal(err)
	}

	// Should not appear in active
	active, _ := db.ActiveRuns()
	if len(active) != 0 {
		t.Error("completed run should not be active")
	}

	// Should appear in history
	history, _ := db.History(10)
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history))
	}
	if history[0].Status != "done" {
		t.Errorf("expected 'done', got %q", history[0].Status)
	}
	if history[0].Result == nil || history[0].Result.PRUrl != "https://github.com/pr/1" {
		t.Error("result should be preserved")
	}
}

func TestDB_CompleteRun_Failure(t *testing.T) {
	db := openTestDB(t)
	run := &Run{ID: "run-1", Status: "running", StartedAt: time.Now()}
	db.SaveRun(run)

	db.CompleteRun("run-1", &RunResult{Success: false, Error: "tests failed"})

	history, _ := db.History(10)
	if len(history) != 1 || history[0].Status != "failed" {
		t.Error("failed run should have status 'failed'")
	}
}

func TestDB_ActiveRuns(t *testing.T) {
	db := openTestDB(t)
	db.SaveRun(&Run{ID: "run-1", Status: "running", StartedAt: time.Now()})
	db.SaveRun(&Run{ID: "run-2", Status: "validating", StartedAt: time.Now()})
	db.SaveRun(&Run{ID: "run-3", Status: "done", StartedAt: time.Now()})

	active, err := db.ActiveRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Errorf("expected 2 active runs, got %d", len(active))
	}
}

func TestDB_History(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 5; i++ {
		run := &Run{ID: fmt.Sprintf("run-%d", i), Status: "done", StartedAt: time.Now()}
		db.SaveRun(run)
	}

	history, err := db.History(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Errorf("expected 3 history entries, got %d", len(history))
	}
}

func TestDB_HasWorktree(t *testing.T) {
	db := openTestDB(t)
	db.SaveRun(&Run{ID: "run-1", Status: "running", WorktreePath: "/tmp/wt-1", StartedAt: time.Now()})

	if !db.HasWorktree("/tmp/wt-1") {
		t.Error("should find active worktree")
	}
	if db.HasWorktree("/tmp/wt-nonexistent") {
		t.Error("should not find nonexistent worktree")
	}
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

func TestDB_PRWatch(t *testing.T) {
	db := openTestDB(t)

	if err := db.SavePRWatch(42, "https://github.com/pr/42", "fix-bug", "run-1", "C123", "ts-1", "/repos/test"); err != nil {
		t.Fatal(err)
	}

	watches, err := db.OpenPRWatches(3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(watches) != 1 {
		t.Fatalf("expected 1 watch, got %d", len(watches))
	}
	if watches[0].PRNumber != 42 {
		t.Errorf("pr number: got %d, want 42", watches[0].PRNumber)
	}
	if watches[0].Branch != "fix-bug" {
		t.Errorf("branch: got %q, want %q", watches[0].Branch, "fix-bug")
	}
}

func TestDB_PRWatch_ClosedExcluded(t *testing.T) {
	db := openTestDB(t)
	db.SavePRWatch(42, "https://github.com/pr/42", "fix-bug", "run-1", "C123", "ts-1", "/repos/test")
	db.ClosePRWatch(42)

	watches, _ := db.OpenPRWatches(3, 3)
	if len(watches) != 0 {
		t.Error("closed PR should not appear in open watches")
	}
}

func TestDB_PRWatch_FixCountLimit(t *testing.T) {
	db := openTestDB(t)
	db.SavePRWatch(42, "https://github.com/pr/42", "fix-bug", "run-1", "C123", "ts-1", "/repos/test")

	// Increment review fix count 3 times
	for i := 0; i < 3; i++ {
		db.UpdatePRWatchLastComment(42, i+1)
	}

	// PR should still appear — CI fix budget is not exhausted
	watches, _ := db.OpenPRWatches(3, 3)
	if len(watches) != 1 {
		t.Error("PR at review fix limit should still appear when CI fix budget remains")
	}

	// Exhaust CI fix budget too
	for i := 0; i < 3; i++ {
		db.IncrementCIFixCount(42)
	}

	// PR should still appear — conflict fix budget is not exhausted
	watches, _ = db.OpenPRWatches(3, 3)
	if len(watches) != 1 {
		t.Error("PR at review+CI fix limits should still appear when conflict fix budget remains")
	}

	// Exhaust conflict fix budget
	for i := 0; i < 3; i++ {
		db.IncrementConflictFixCount(42)
	}

	watches, _ = db.OpenPRWatches(3, 3)
	if len(watches) != 0 {
		t.Error("PR at all fix limits should not appear in open watches")
	}
}

func TestDB_PRWatch_CIFixCount(t *testing.T) {
	db := openTestDB(t)
	db.SavePRWatch(42, "https://github.com/pr/42", "fix-bug", "run-1", "C123", "ts-1", "/repos/test")

	// Increment CI fix count
	if err := db.IncrementCIFixCount(42); err != nil {
		t.Fatal(err)
	}

	watches, _ := db.OpenPRWatches(3, 3)
	if len(watches) != 1 {
		t.Fatalf("expected 1 watch, got %d", len(watches))
	}
	if watches[0].CIFixCount != 1 {
		t.Errorf("ci_fix_count: got %d, want 1", watches[0].CIFixCount)
	}

	// Increment again
	db.IncrementCIFixCount(42)

	watches, _ = db.OpenPRWatches(3, 3)
	if len(watches) != 1 {
		t.Fatalf("expected 1 watch, got %d", len(watches))
	}
	if watches[0].CIFixCount != 2 {
		t.Errorf("ci_fix_count: got %d, want 2", watches[0].CIFixCount)
	}

	// Watch stays open when only CI fix budget is exhausted but review budget remains
	watches, _ = db.OpenPRWatches(3, 2)
	if len(watches) != 1 {
		t.Error("PR should still be open when review budget remains")
	}

	// Watch stays open when only review budget is exhausted but CI fix budget remains
	for i := 0; i < 3; i++ {
		db.UpdatePRWatchLastComment(42, i+1)
	}
	watches, _ = db.OpenPRWatches(3, 3)
	if len(watches) != 1 {
		t.Error("PR should still be open when CI fix budget remains")
	}

	// Watch still open — conflict fix budget remains
	db.IncrementCIFixCount(42) // now ci_fix_count=3
	watches, _ = db.OpenPRWatches(3, 3)
	if len(watches) != 1 {
		t.Error("PR should still be open when conflict fix budget remains")
	}

	// Watch excluded when ALL budgets are exhausted
	for i := 0; i < 3; i++ {
		db.IncrementConflictFixCount(42)
	}
	watches, _ = db.OpenPRWatches(3, 3)
	if len(watches) != 0 {
		t.Error("PR should not appear when all budgets are exhausted")
	}
}

func TestDB_PRWatch_ConflictFixCount(t *testing.T) {
	db := openTestDB(t)
	db.SavePRWatch(42, "https://github.com/pr/42", "fix-bug", "run-1", "C123", "ts-1", "/repos/test")

	if err := db.IncrementConflictFixCount(42); err != nil {
		t.Fatal(err)
	}

	watches, _ := db.OpenPRWatches(3, 3)
	if len(watches) != 1 {
		t.Fatalf("expected 1 watch, got %d", len(watches))
	}
	if watches[0].ConflictFixCount != 1 {
		t.Errorf("conflict_fix_count: got %d, want 1", watches[0].ConflictFixCount)
	}

	// Conflict fix count is independent of review fix count
	if watches[0].FixCount != 0 {
		t.Errorf("fix_count should be 0, got %d", watches[0].FixCount)
	}
}

func TestDB_PRWatch_CIExhaustedNotified(t *testing.T) {
	db := openTestDB(t)
	db.SavePRWatch(42, "https://github.com/pr/42", "fix-bug", "run-1", "C123", "ts-1", "/repos/test")

	// Initially not notified
	watches, _ := db.OpenPRWatches(3, 3)
	if len(watches) != 1 {
		t.Fatalf("expected 1 watch, got %d", len(watches))
	}
	if watches[0].CIExhaustedNotified {
		t.Error("expected CIExhaustedNotified to be false initially")
	}

	// Mark as notified
	if err := db.MarkCIExhaustedNotified(42); err != nil {
		t.Fatal(err)
	}

	watches, _ = db.OpenPRWatches(3, 3)
	if len(watches) != 1 {
		t.Fatalf("expected 1 watch, got %d", len(watches))
	}
	if !watches[0].CIExhaustedNotified {
		t.Error("expected CIExhaustedNotified to be true after marking")
	}
}

func TestDB_Stats(t *testing.T) {
	db := openTestDB(t)

	// Create some completed runs with results
	for i := 0; i < 3; i++ {
		run := &Run{
			ID:        fmt.Sprintf("done-%d", i),
			Status:    "running",
			Branch:    fmt.Sprintf("fix-%d", i),
			Task:      fmt.Sprintf("task %d", i),
			StartedAt: time.Now(),
		}
		if err := db.SaveRun(run); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteRun(run.ID, &RunResult{
			Success:  true,
			PRUrl:    fmt.Sprintf("https://github.com/pr/%d", i),
			Duration: 3 * time.Minute,
			Cost:     0.50,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Add failed runs
	for i := 0; i < 2; i++ {
		run := &Run{
			ID:        fmt.Sprintf("fail-%d", i),
			Status:    "running",
			StartedAt: time.Now(),
		}
		if err := db.SaveRun(run); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteRun(run.ID, &RunResult{
			Success:  false,
			Error:    "tests failed",
			Duration: 2 * time.Minute,
			Cost:     0.25,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Add an active run (should not count)
	db.SaveRun(&Run{ID: "active-1", Status: "running", StartedAt: time.Now()})

	// Add thread memories
	db.SaveThreadMemory("ts-1", "C1", "{}", "resp1")
	db.SaveThreadMemory("ts-2", "C1", "{}", "resp2")
	db.SaveThreadMemory("ts-3", "C2", "{}", "resp3")

	stats, err := db.Stats()
	if err != nil {
		t.Fatalf("Stats(): %v", err)
	}

	if stats.TotalRuns != 5 {
		t.Errorf("TotalRuns: got %d, want 5", stats.TotalRuns)
	}
	if stats.Succeeded != 3 {
		t.Errorf("Succeeded: got %d, want 3", stats.Succeeded)
	}
	if stats.Failed != 2 {
		t.Errorf("Failed: got %d, want 2", stats.Failed)
	}
	// 3 * 0.50 + 2 * 0.25 = 2.00
	if stats.TotalCost != 2.00 {
		t.Errorf("TotalCost: got %.2f, want 2.00", stats.TotalCost)
	}
	// (3*3m + 2*2m) / 5 = 13m/5 = 2m36s
	expectedAvg := (3*3*time.Minute + 2*2*time.Minute) / 5
	if stats.AvgDuration != expectedAvg {
		t.Errorf("AvgDuration: got %v, want %v", stats.AvgDuration, expectedAvg)
	}
	if stats.ThreadCount != 3 {
		t.Errorf("ThreadCount: got %d, want 3", stats.ThreadCount)
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

func TestDB_Stats_EmptyDB(t *testing.T) {
	db := openTestDB(t)

	stats, err := db.Stats()
	if err != nil {
		t.Fatalf("Stats(): %v", err)
	}
	if stats.TotalRuns != 0 {
		t.Errorf("TotalRuns: got %d, want 0", stats.TotalRuns)
	}
	if stats.AvgDuration != 0 {
		t.Errorf("AvgDuration: got %v, want 0", stats.AvgDuration)
	}
}

func TestDB_SaveRunWithResult(t *testing.T) {
	db := openTestDB(t)
	run := &Run{
		ID:        "run-1",
		Status:    "done",
		StartedAt: time.Now(),
		Result: &RunResult{
			Success:      true,
			PRUrl:        "https://github.com/pr/1",
			FilesChanged: 2,
		},
	}
	if err := db.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	history, _ := db.History(10)
	if len(history) != 1 {
		t.Fatal("expected 1 history entry")
	}
	if history[0].Result == nil || history[0].Result.PRUrl != "https://github.com/pr/1" {
		t.Error("result should be preserved through save/load")
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
