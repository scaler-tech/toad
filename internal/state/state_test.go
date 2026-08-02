package state

import (
	"fmt"
	"sync"
	"testing"
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

// TestUnclaim_DoesNotRemoveTrackedRun exercises Unclaim's placeholder-only
// removal semantics. The runs pipeline (which used to promote a claim's
// placeholder "" runID to a real one via Manager.Track) was removed, so this
// test writes directly to the unexported threads map — same package — to
// simulate a "real" (non-placeholder) claim.
func TestUnclaim_DoesNotRemoveTrackedRun(t *testing.T) {
	m := NewManager()
	m.Claim("thread-1")
	m.threads["thread-1"][""] = "run-1"

	// Unclaim should NOT remove a thread whose claim is no longer a placeholder.
	m.Unclaim("thread-1")

	// If the mapping had been removed, this claim would succeed; it must
	// fail instead, proving the non-placeholder claim is still held.
	if m.Claim("thread-1") {
		t.Fatal("unclaim should not remove a non-placeholder thread mapping")
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
			ts := fmt.Sprintf("thread-%d", i)
			m.Claim(ts)
			m.Unclaim(ts)
			m.Claim(ts)
		}(i)
	}
	wg.Wait()
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

// TestUnclaimScoped_OnlyRemovesPlaceholder covers the same placeholder-vs-
// real-claim distinction as TestUnclaim_DoesNotRemoveTrackedRun, but for the
// scoped variant. Direct map manipulation replaces the old Track call (see
// that test's comment).
func TestUnclaimScoped_OnlyRemovesPlaceholder(t *testing.T) {
	m := NewManager()
	// Claim a scoped run and simulate it being "real" (non-placeholder).
	m.ClaimScoped("thread-1", "DAT-100")
	m.threads["thread-1"]["DAT-100"] = "run-1"

	// UnclaimScoped should NOT remove because it has a real runID — a
	// subsequent claim of the same scope must still fail.
	m.UnclaimScoped("thread-1", "DAT-100")
	if m.ClaimScoped("thread-1", "DAT-100") {
		t.Fatal("unclaim should not remove a non-placeholder thread mapping")
	}

	// But a placeholder should be removable
	m.ClaimScoped("thread-1", "DAT-200")
	m.UnclaimScoped("thread-1", "DAT-200")

	// DAT-200's placeholder is gone, so the scope is claimable again ...
	if !m.ClaimScoped("thread-1", "DAT-200") {
		t.Fatal("expected DAT-200 to be reclaimable after its placeholder was unclaimed")
	}
	// ... while DAT-100 still can't be, since its non-placeholder claim survived.
	if m.ClaimScoped("thread-1", "DAT-100") {
		t.Error("expected DAT-100 to remain held")
	}
}
