// Package state manages in-memory and SQLite-persisted daemon state.
package state

import (
	"sync"
)

// Manager tracks in-flight Slack thread claims, guarding against duplicate
// concurrent investigations/tickets for the same thread. It previously also
// tracked a full runs pipeline (Track/Update/Complete/Active/History) left
// over from the v1 tadpole architecture; that pipeline had zero production
// callers once toad dropped coding in favor of the investigation-to-ticket
// flow, so it was removed. The `runs` DB table itself is left in the schema
// (harmless, unused) rather than dropped.
type Manager struct {
	mu      sync.RWMutex
	db      *DB                          // nil for in-memory only (tests, CLI)
	threads map[string]map[string]string // slackThreadTS -> scope -> runID (runID is always "" now; see Claim)
}

// NewManager creates an in-memory-only manager (for tests and CLI).
func NewManager() *Manager {
	return &Manager{
		threads: make(map[string]map[string]string),
	}
}

// NewPersistentManager creates a manager backed by SQLite.
func NewPersistentManager(db *DB) (*Manager, error) {
	return &Manager{
		db:      db,
		threads: make(map[string]map[string]string),
	}, nil
}

// DB returns the underlying database, or nil if in-memory only.
func (m *Manager) DB() *DB {
	return m.db
}

// ClaimScoped atomically checks if a thread+scope is already tracked and registers it if not.
// Scope "" is exclusive: fails if ANY claim exists, and blocks all other claims.
// Non-empty scopes coexist with each other but not with exclusive claims.
// Returns true if the claim succeeded, false if already taken.
func (m *Manager) ClaimScoped(threadTS, scope string) bool {
	if threadTS == "" {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	inner, exists := m.threads[threadTS]
	if !exists {
		// No claims on this thread yet — always succeeds.
		m.threads[threadTS] = map[string]string{scope: ""}
		return true
	}
	// If an exclusive claim ("") exists, everything fails.
	if _, hasExclusive := inner[""]; hasExclusive {
		return false
	}
	// If requesting exclusive, fail if any scoped claim exists.
	if scope == "" {
		if len(inner) > 0 {
			return false
		}
		inner[""] = ""
		return true
	}
	// Scoped claim: fail only if same scope already claimed.
	if _, taken := inner[scope]; taken {
		return false
	}
	inner[scope] = ""
	return true
}

// Claim atomically checks if a thread is already tracked and registers it if not.
// Returns true if the claim succeeded (thread was not tracked), false if already taken.
func (m *Manager) Claim(threadTS string) bool {
	return m.ClaimScoped(threadTS, "")
}

// UnclaimScoped removes a thread+scope claim without registering a run (for error cleanup).
// Only removes placeholder entries (empty runID). Cleans outer map if inner becomes empty.
func (m *Manager) UnclaimScoped(threadTS, scope string) {
	if threadTS == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	inner, exists := m.threads[threadTS]
	if !exists {
		return
	}
	// Only unclaim if it's still a placeholder (empty runID)
	if runID, ok := inner[scope]; ok && runID == "" {
		delete(inner, scope)
		if len(inner) == 0 {
			delete(m.threads, threadTS)
		}
	}
}

// Unclaim removes a thread claim without registering a run (for error cleanup).
func (m *Manager) Unclaim(threadTS string) {
	m.UnclaimScoped(threadTS, "")
}
