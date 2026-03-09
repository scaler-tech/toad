package state

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/scaler-tech/toad/internal/toadpath"
)

// RecoverResult summarizes what was cleaned up on startup.
type RecoverResult struct {
	StaleRuns       int
	OrphanWorktrees int
}

// RecoverOnStartup finds runs left in active states from a previous crash,
// marks them failed, and cleans up any orphaned worktrees.
func RecoverOnStartup(db *DB) (*RecoverResult, error) {
	result := &RecoverResult{}

	// 1. Find active runs (starting/running/validating/shipping) and mark them failed
	active, err := db.ActiveRuns()
	if err != nil {
		return nil, err
	}

	for _, run := range active {
		slog.Warn("recovering stale run",
			"id", run.ID,
			"status", run.Status,
			"branch", run.Branch,
			"worktree", run.WorktreePath,
		)

		if err := db.CompleteRun(run.ID, &RunResult{
			Success: false,
			Error:   "toad crashed during execution",
		}); err != nil {
			slog.Error("failed to mark stale run as failed", "id", run.ID, "error", err)
			continue
		}
		result.StaleRuns++

		// Clean up the worktree if it still exists
		if run.WorktreePath != "" {
			removeWorktreeDir(run.WorktreePath)
		}
	}

	// 2. Scan for orphaned worktree directories not tracked in the DB
	home, err := toadpath.Home()
	if err != nil {
		slog.Warn("cannot check for orphan worktrees", "error", err)
		return result, nil
	}

	wtDir := filepath.Join(home, "worktrees")
	entries, err := os.ReadDir(wtDir)
	if err != nil {
		// No worktrees dir is fine — nothing to clean
		if os.IsNotExist(err) {
			return result, nil
		}
		slog.Warn("cannot read worktrees directory", "error", err)
		return result, nil
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		wtPath := filepath.Join(wtDir, entry.Name())
		if db.HasWorktree(wtPath) {
			continue // tracked by an active run (shouldn't happen after step 1, but be safe)
		}
		slog.Warn("removing orphaned worktree", "path", wtPath)
		removeWorktreeDir(wtPath)
		result.OrphanWorktrees++
	}

	if result.StaleRuns > 0 || result.OrphanWorktrees > 0 {
		slog.Info("recovery complete",
			"stale_runs", result.StaleRuns,
			"orphan_worktrees", result.OrphanWorktrees,
		)
	}

	return result, nil
}

func removeWorktreeDir(path string) {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("failed to remove worktree directory", "path", path, "error", err)
	}
}
