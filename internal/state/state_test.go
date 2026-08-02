package state

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

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

	// If the mapping had been removed, this claim would succeed; it must
	// fail instead, proving the tracked run's exclusive claim is still held.
	if m.Claim("thread-1") {
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

	// UnclaimScoped should NOT remove because it has a real runID — a
	// subsequent claim of the same scope must still fail.
	m.UnclaimScoped("thread-1", "DAT-100")
	if m.ClaimScoped("thread-1", "DAT-100") {
		t.Fatal("unclaim should not remove a tracked run's thread mapping")
	}

	// But a placeholder should be removable
	m.ClaimScoped("thread-1", "DAT-200")
	m.UnclaimScoped("thread-1", "DAT-200")

	// DAT-200's placeholder is gone, so the scope is claimable again ...
	if !m.ClaimScoped("thread-1", "DAT-200") {
		t.Fatal("expected DAT-200 to be reclaimable after its placeholder was unclaimed")
	}
	// ... while DAT-100 still can't be, since run-1's tracked claim survived.
	if m.ClaimScoped("thread-1", "DAT-100") {
		t.Error("expected DAT-100 to remain held by the tracked run")
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

	// DAT-200 should still be active — its scope must still be held, i.e.
	// unclaimable, since run-2 is untouched by completing run-1.
	if m.ClaimScoped("thread-1", "DAT-200") {
		t.Error("expected DAT-200 to still be held by run-2 after completing only run-1")
	}

	// DAT-100 should be reclaimable
	if !m.ClaimScoped("thread-1", "DAT-100") {
		t.Error("DAT-100 should be reclaimable after completion")
	}
}
