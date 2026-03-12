package state

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestTrackAndGetByThread(t *testing.T) {
	m := NewManager()
	run := &Run{
		ID:            "run-1",
		Status:        "running",
		SlackThreadTS: "1234.5678",
		Task:          "fix bug",
		StartedAt:     time.Now(),
	}
	m.Track(run)

	got := m.GetByThread("1234.5678")
	if len(got) == 0 {
		t.Fatal("expected to find run by thread")
	}
	if got[0].ID != "run-1" {
		t.Errorf("got ID %q, want %q", got[0].ID, "run-1")
	}
}

func TestGetByThread_NotFound(t *testing.T) {
	m := NewManager()
	if got := m.GetByThread("nonexistent"); len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

func TestClaim(t *testing.T) {
	m := NewManager()
	if !m.Claim("thread-1") {
		t.Fatal("first claim should succeed")
	}
	if m.Claim("thread-1") {
		t.Fatal("second claim on same thread should fail")
	}
}

func TestClaim_EmptyThread(t *testing.T) {
	m := NewManager()
	// Empty thread always succeeds (CLI mode)
	if !m.Claim("") {
		t.Fatal("empty thread claim should succeed")
	}
	if !m.Claim("") {
		t.Fatal("empty thread claim should always succeed")
	}
}

func TestUnclaim(t *testing.T) {
	m := NewManager()
	m.Claim("thread-1")
	m.Unclaim("thread-1")

	// Should be able to claim again after unclaim
	if !m.Claim("thread-1") {
		t.Fatal("claim after unclaim should succeed")
	}
}

func TestUnclaim_DoesNotRemoveTrackedRun(t *testing.T) {
	m := NewManager()
	m.Track(&Run{
		ID:            "run-1",
		SlackThreadTS: "thread-1",
		StartedAt:     time.Now(),
	})

	// Unclaim should NOT remove a thread that has a real run tracked
	m.Unclaim("thread-1")

	got := m.GetByThread("thread-1")
	if len(got) == 0 {
		t.Fatal("unclaim should not remove a tracked run's thread mapping")
	}
}

func TestUpdate(t *testing.T) {
	m := NewManager()
	m.Track(&Run{ID: "run-1", Status: "starting", StartedAt: time.Now()})
	m.Update("run-1", "running")

	runs := m.Active()
	if len(runs) != 1 || runs[0].Status != "running" {
		t.Errorf("expected status 'running', got %v", runs)
	}
}

func TestComplete_Success(t *testing.T) {
	m := NewManager()
	m.Track(&Run{ID: "run-1", Status: "running", StartedAt: time.Now()})

	m.Complete("run-1", &RunResult{Success: true, PRUrl: "https://github.com/pr/1"})

	if len(m.Active()) != 0 {
		t.Error("completed run should not be in active list")
	}
	history := m.History()
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history))
	}
	if history[0].Status != "done" {
		t.Errorf("expected status 'done', got %q", history[0].Status)
	}
}

func TestComplete_Failure(t *testing.T) {
	m := NewManager()
	m.Track(&Run{ID: "run-1", Status: "running", StartedAt: time.Now()})

	m.Complete("run-1", &RunResult{Success: false, Error: "tests failed"})

	history := m.History()
	if len(history) != 1 || history[0].Status != "failed" {
		t.Errorf("expected failed status in history")
	}
}

func TestHistoryCap(t *testing.T) {
	m := NewManager()
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("run-%d", i)
		m.Track(&Run{ID: id, Status: "running", StartedAt: time.Now()})
		m.Complete(id, &RunResult{Success: true})
	}

	history := m.History()
	if len(history) != 50 {
		t.Errorf("history should be capped at 50, got %d", len(history))
	}
	// Oldest should be run-10 (0-9 evicted)
	if history[0].ID != "run-10" {
		t.Errorf("oldest entry should be run-10, got %s", history[0].ID)
	}
}

func TestActive(t *testing.T) {
	m := NewManager()
	m.Track(&Run{ID: "run-1", Status: "running", StartedAt: time.Now()})
	m.Track(&Run{ID: "run-2", Status: "starting", StartedAt: time.Now()})

	active := m.Active()
	if len(active) != 2 {
		t.Errorf("expected 2 active runs, got %d", len(active))
	}
}

func TestConcurrentAccess(t *testing.T) {
	m := NewManager()
	var wg sync.WaitGroup

	// Hammer the manager from multiple goroutines
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("run-%d", i)
			ts := fmt.Sprintf("thread-%d", i)
			m.Claim(ts)
			m.Track(&Run{ID: id, Status: "running", SlackThreadTS: ts, StartedAt: time.Now()})
			m.Update(id, "validating")
			m.GetByThread(ts)
			m.Active()
			m.Complete(id, &RunResult{Success: true})
			m.History()
		}(i)
	}
	wg.Wait()

	if len(m.Active()) != 0 {
		t.Errorf("all runs should be complete, got %d active", len(m.Active()))
	}
}

func TestClaimReleasedAfterComplete(t *testing.T) {
	m := NewManager()

	// Claim and track a run
	if !m.Claim("thread-1") {
		t.Fatal("initial claim should succeed")
	}
	m.Track(&Run{ID: "run-1", Status: "running", SlackThreadTS: "thread-1", StartedAt: time.Now()})

	// While running, claim should fail
	if m.Claim("thread-1") {
		t.Error("claim should fail while run is active")
	}

	// Complete the run
	m.Complete("run-1", &RunResult{Success: true})

	// Thread should now be reclaimable
	if !m.Claim("thread-1") {
		t.Error("claim should succeed after run completes")
	}
}

func TestClaimReleasedAfterFailure(t *testing.T) {
	m := NewManager()

	if !m.Claim("thread-2") {
		t.Fatal("initial claim should succeed")
	}
	m.Track(&Run{ID: "run-2", Status: "running", SlackThreadTS: "thread-2", StartedAt: time.Now()})
	m.Complete("run-2", &RunResult{Success: false, Error: "test failure"})

	// Thread should be reclaimable after failure too
	if !m.Claim("thread-2") {
		t.Error("claim should succeed after failed run")
	}
}

// --- Scoped claim tests ---

func TestClaimScoped_MultipleScopesSameThread(t *testing.T) {
	m := NewManager()
	if !m.ClaimScoped("thread-1", "DAT-100") {
		t.Fatal("first scoped claim should succeed")
	}
	if !m.ClaimScoped("thread-1", "DAT-200") {
		t.Fatal("second scoped claim with different scope should succeed")
	}
}

func TestClaimScoped_SameScopeFails(t *testing.T) {
	m := NewManager()
	if !m.ClaimScoped("thread-1", "DAT-100") {
		t.Fatal("first scoped claim should succeed")
	}
	if m.ClaimScoped("thread-1", "DAT-100") {
		t.Fatal("second claim with same scope should fail")
	}
}

func TestClaimScoped_ExclusiveBlocksScoped(t *testing.T) {
	m := NewManager()
	if !m.Claim("thread-1") {
		t.Fatal("exclusive claim should succeed")
	}
	if m.ClaimScoped("thread-1", "DAT-100") {
		t.Fatal("scoped claim should fail when exclusive claim exists")
	}
}

func TestClaimScoped_ScopedBlocksExclusive(t *testing.T) {
	m := NewManager()
	if !m.ClaimScoped("thread-1", "DAT-100") {
		t.Fatal("scoped claim should succeed")
	}
	if m.Claim("thread-1") {
		t.Fatal("exclusive claim should fail when scoped claim exists")
	}
}

func TestUnclaimScoped_OnlyRemovesPlaceholder(t *testing.T) {
	m := NewManager()
	// Claim and track a scoped run
	m.ClaimScoped("thread-1", "DAT-100")
	m.Track(&Run{
		ID:            "run-1",
		SlackThreadTS: "thread-1",
		ClaimScope:    "DAT-100",
		StartedAt:     time.Now(),
	})

	// UnclaimScoped should NOT remove because it has a real runID
	m.UnclaimScoped("thread-1", "DAT-100")

	got := m.GetByThread("thread-1")
	if len(got) == 0 {
		t.Fatal("unclaim should not remove a tracked run's thread mapping")
	}

	// But a placeholder should be removable
	m.ClaimScoped("thread-1", "DAT-200")
	m.UnclaimScoped("thread-1", "DAT-200")

	// DAT-200 should be gone, DAT-100 should remain
	got = m.GetByThread("thread-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 run after unclaim of placeholder, got %d", len(got))
	}
	if got[0].ID != "run-1" {
		t.Errorf("expected run-1, got %s", got[0].ID)
	}
}

func TestComplete_ScopedReleasesOnlyItsScope(t *testing.T) {
	m := NewManager()
	m.ClaimScoped("thread-1", "DAT-100")
	m.Track(&Run{
		ID:            "run-1",
		Status:        "running",
		SlackThreadTS: "thread-1",
		ClaimScope:    "DAT-100",
		StartedAt:     time.Now(),
	})
	m.ClaimScoped("thread-1", "DAT-200")
	m.Track(&Run{
		ID:            "run-2",
		Status:        "running",
		SlackThreadTS: "thread-1",
		ClaimScope:    "DAT-200",
		StartedAt:     time.Now(),
	})

	// Complete run-1 (DAT-100)
	m.Complete("run-1", &RunResult{Success: true})

	// DAT-200 should still be active
	got := m.GetByThread("thread-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 remaining run, got %d", len(got))
	}
	if got[0].ID != "run-2" {
		t.Errorf("expected run-2 to survive, got %s", got[0].ID)
	}

	// DAT-100 should be reclaimable
	if !m.ClaimScoped("thread-1", "DAT-100") {
		t.Error("DAT-100 should be reclaimable after completion")
	}
}

func TestGetByThread_ReturnsMultiple(t *testing.T) {
	m := NewManager()
	m.ClaimScoped("thread-1", "DAT-100")
	m.Track(&Run{
		ID:            "run-1",
		Status:        "running",
		SlackThreadTS: "thread-1",
		ClaimScope:    "DAT-100",
		StartedAt:     time.Now(),
	})
	m.ClaimScoped("thread-1", "DAT-200")
	m.Track(&Run{
		ID:            "run-2",
		Status:        "running",
		SlackThreadTS: "thread-1",
		ClaimScope:    "DAT-200",
		StartedAt:     time.Now(),
	})

	got := m.GetByThread("thread-1")
	if len(got) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(got))
	}

	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if !ids["run-1"] || !ids["run-2"] {
		t.Errorf("expected run-1 and run-2, got %v", ids)
	}
}
