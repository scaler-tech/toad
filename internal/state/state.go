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
	threads     map[string]string // slackThreadTS → runID
	history     []*Run
	historySize int
}

// NewManager creates an in-memory-only manager (for tests and CLI).
func NewManager() *Manager {
	return &Manager{
		runs:    make(map[string]*Run),
		threads: make(map[string]string),
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
		threads:     make(map[string]string),
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
			m.threads[run.SlackThreadTS] = run.ID
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

// Claim atomically checks if a thread is already tracked and registers it if not.
// Returns true if the claim succeeded (thread was not tracked), false if already taken.
func (m *Manager) Claim(threadTS string) bool {
	if threadTS == "" {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.threads[threadTS]; exists {
		return false
	}
	// Reserve the thread with a placeholder — Track will fill in the real run
	m.threads[threadTS] = ""
	return true
}

// Track registers a new run.
func (m *Manager) Track(run *Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[run.ID] = run
	if run.SlackThreadTS != "" {
		m.threads[run.SlackThreadTS] = run.ID
	}
	if m.db != nil {
		if err := m.db.SaveRun(run); err != nil {
			slog.Error("failed to persist run", "id", run.ID, "error", err)
		}
	}
}

// Unclaim removes a thread claim without registering a run (for error cleanup).
func (m *Manager) Unclaim(threadTS string) {
	if threadTS == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Only unclaim if it's still a placeholder (empty runID)
	if runID, exists := m.threads[threadTS]; exists && runID == "" {
		delete(m.threads, threadTS)
	}
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
		delete(m.threads, run.SlackThreadTS)
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

// GetByThread looks up a run by its Slack thread timestamp.
func (m *Manager) GetByThread(threadTS string) *Run {
	m.mu.RLock()
	defer m.mu.RUnlock()
	runID, ok := m.threads[threadTS]
	if !ok {
		return nil
	}
	return m.runs[runID]
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
