package cmd

import (
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/state"
)

func openGateTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDigestChannelGate_NilGateFailsOpen(t *testing.T) {
	var g *digestChannelGate
	if !g.enabled("C123") {
		t.Fatal("expected nil gate to fail open (enabled)")
	}
}

func TestDigestChannelGate_NilDBFailsOpen(t *testing.T) {
	g := newDigestChannelGate(nil)
	if !g.enabled("C123") {
		t.Fatal("expected nil-db gate to fail open (enabled)")
	}
}

func TestDigestChannelGate_DefaultEnabled(t *testing.T) {
	db := openGateTestDB(t)
	g := newDigestChannelGate(db)

	if !g.enabled("C123") {
		t.Fatal("expected channel with no override to be enabled")
	}
}

func TestDigestChannelGate_ReflectsDisabledChannel(t *testing.T) {
	db := openGateTestDB(t)
	if err := db.SetDigestChannelEnabled("C123", false); err != nil {
		t.Fatalf("SetDigestChannelEnabled: %v", err)
	}

	g := newDigestChannelGate(db)
	if g.enabled("C123") {
		t.Fatal("expected disabled channel to report enabled=false")
	}
	if !g.enabled("C456") {
		t.Fatal("expected untouched channel to remain enabled")
	}
}

func TestDigestChannelGate_TTLThrottlesRefresh(t *testing.T) {
	db := openGateTestDB(t)
	g := newDigestChannelGate(db)
	g.refreshInterval = time.Hour // effectively "never" for this test

	// First call populates the cache (channel starts enabled).
	if !g.enabled("C123") {
		t.Fatal("expected initially enabled")
	}

	// Disable the channel behind the gate's back — a stale cache should
	// still report enabled until the TTL elapses.
	if err := db.SetDigestChannelEnabled("C123", false); err != nil {
		t.Fatalf("SetDigestChannelEnabled: %v", err)
	}
	if !g.enabled("C123") {
		t.Fatal("expected stale cache to still report enabled before TTL elapses")
	}

	// Force the TTL to have elapsed; the next check should pick up the change.
	g.mu.Lock()
	g.lastRefresh = time.Now().Add(-2 * time.Hour)
	g.mu.Unlock()

	if g.enabled("C123") {
		t.Fatal("expected refreshed cache to report disabled after TTL elapses")
	}
}

func TestDigestChannelGate_FailOpenOnDBError(t *testing.T) {
	db := openGateTestDB(t)
	g := newDigestChannelGate(db)

	// Prime the cache with a disabled channel.
	if err := db.SetDigestChannelEnabled("C123", false); err != nil {
		t.Fatalf("SetDigestChannelEnabled: %v", err)
	}
	if g.enabled("C123") {
		t.Fatal("expected disabled channel before DB closes")
	}

	// Force the TTL to elapse, then close the DB out from under the gate so
	// the next refresh attempt errors. The gate must fail open (keep
	// reporting the channel as it was determined before the outage would
	// also be acceptable, but the documented contract is "fail open" —
	// i.e. never let a broken toggle store silently kill the digest for a
	// channel that was previously fine). Here we assert on a channel that
	// was never toggled, which must stay enabled regardless of the error.
	g.mu.Lock()
	g.lastRefresh = time.Now().Add(-2 * time.Hour)
	g.mu.Unlock()
	db.Close()

	if !g.enabled("C999") {
		t.Fatal("expected fail-open (enabled) when refresh errors")
	}
}
