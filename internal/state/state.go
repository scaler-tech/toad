// Package state manages in-memory and SQLite-persisted run state.
package state

import (
	"log/slog"
	"sync"
	"time"
)

// Run represents an active or completed tadpole run.
type Run struct {
	ID            string
	Status        string // "starting", "running", "validating", "shipping", "done", "failed"
	SlackChannel  string
	SlackThreadTS string
	Branch        string
	WorktreePath  string
	Task          string
	RepoName      string
	ClaimScope    string
	StartedAt     time.Time
	Result        *RunResult
}

type RunResult struct {
	Success      bool
	PRUrl        string
	Error        string
	FilesChanged int
	Duration     time.Duration
	Cost         float64
}

// Manager tracks tadpole runs and maps Slack threads to runs.
type Manager struct {
	mu          sync.RWMutex
	db          *DB // nil for in-memory only (tests, CLI)
	runs        map[string]*Run
	threads     map[string]map[string]string // slackThreadTS → scope → runID
	history     []*Run
	historySize int
}

// NewManager creates an in-memory-only manager (for tests and CLI).
func NewManager() *Manager {
	return &Manager{
		runs:    make(map[string]*Run),
		threads: make(map[string]map[string]string),
	}
}

// NewPersistentManager creates a manager backed by SQLite.
// It hydrates in-memory state from the database on creation.
// historySize controls how many completed runs to keep (0 = use default of 50).
func NewPersistentManager(db *DB, historySize int) (*Manager, error) {
	if historySize <= 0 {
		historySize = 50
	}
	m := &Manager{
		db:          db,
		runs:        make(map[string]*Run),
		threads:     make(map[string]map[string]string),
		historySize: historySize,
	}

	// Hydrate active runs from DB
	active, err := db.ActiveRuns()
	if err != nil {
		return nil, err
	}
	for _, run := range active {
		m.runs[run.ID] = run
		if run.SlackThreadTS != "" {
			if m.threads[run.SlackThreadTS] == nil {
				m.threads[run.SlackThreadTS] = make(map[string]string)
			}
			m.threads[run.SlackThreadTS][run.ClaimScope] = run.ID
		}
	}

	// Hydrate history from DB
	history, err := db.History(historySize)
	if err != nil {
		return nil, err
	}
	m.history = history

	slog.Info("state hydrated from db", "active", len(active), "history", len(history))
	return m, nil
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

// Track registers a new run.
func (m *Manager) Track(run *Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[run.ID] = run
	if run.SlackThreadTS != "" {
		if m.threads[run.SlackThreadTS] == nil {
			m.threads[run.SlackThreadTS] = make(map[string]string)
		}
		m.threads[run.SlackThreadTS][run.ClaimScope] = run.ID
	}
	if m.db != nil {
		if err := m.db.SaveRun(run); err != nil {
			slog.Error("failed to persist run", "id", run.ID, "error", err)
		}
	}
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

// Update updates the status of an existing run.
func (m *Manager) Update(runID, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if run, ok := m.runs[runID]; ok {
		run.Status = status
		if m.db != nil {
			if err := m.db.UpdateStatus(runID, status); err != nil {
				slog.Error("failed to persist status update", "id", runID, "error", err)
			}
		}
	}
}

// Complete marks a run as done and moves it to history.
func (m *Manager) Complete(runID string, result *RunResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return
	}
	if result.Success {
		run.Status = "done"
	} else {
		run.Status = "failed"
	}
	run.Result = result
	delete(m.runs, runID)
	if run.SlackThreadTS != "" {
		if inner, exists := m.threads[run.SlackThreadTS]; exists {
			delete(inner, run.ClaimScope)
			if len(inner) == 0 {
				delete(m.threads, run.SlackThreadTS)
			}
		}
	}
	m.history = append(m.history, run)
	cap := m.historySize
	if cap <= 0 {
		cap = 50
	}
	if len(m.history) > cap {
		m.history = m.history[len(m.history)-cap:]
	}
	if m.db != nil {
		if err := m.db.CompleteRun(runID, result); err != nil {
			slog.Error("failed to persist run completion", "id", runID, "error", err)
		}
	}
}

// GetByThread looks up all active runs for a Slack thread timestamp.
func (m *Manager) GetByThread(threadTS string) []*Run {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inner, ok := m.threads[threadTS]
	if !ok {
		return nil
	}
	var runs []*Run
	for _, runID := range inner {
		if run, exists := m.runs[runID]; exists {
			runs = append(runs, run)
		}
	}
	return runs
}

// Active returns all currently running tadpoles.
func (m *Manager) Active() []*Run {
	m.mu.RLock()
	defer m.mu.RUnlock()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		cp := *r
		runs = append(runs, &cp)
	}
	return runs
}

// History returns completed runs.
func (m *Manager) History() []*Run {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Run, len(m.history))
	for i, r := range m.history {
		cp := *r
		out[i] = &cp
	}
	return out
}

// SetWorktreeInfo updates the branch and worktree path for a tracked run.
func (m *Manager) SetWorktreeInfo(runID, branch, wtPath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if run, ok := m.runs[runID]; ok {
		run.Branch = branch
		run.WorktreePath = wtPath
		if m.db != nil {
			if err := m.db.SaveRun(run); err != nil {
				slog.Error("failed to persist worktree info", "id", runID, "error", err)
			}
		}
	}
}
