package tadpole

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/scaler-tech/toad/internal/toadpath"
)

// CleanupStaleWorktrees periodically removes worktree directories older than maxAge.
// Runs every hour until ctx is canceled.
func CleanupStaleWorktrees(ctx context.Context, maxAge time.Duration) {
	if maxAge <= 0 {
		return
	}
	// Run once immediately on startup, then hourly
	cleanOnce(maxAge)

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cleanOnce(maxAge)
		case <-ctx.Done():
			return
		}
	}
}

func cleanOnce(maxAge time.Duration) {
	home, err := toadpath.Home()
	if err != nil {
		return
	}
	wtDir := filepath.Join(home, "worktrees")
	entries, err := os.ReadDir(wtDir)
	if err != nil {
		return // no dir = nothing to clean
	}

	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			wtPath := filepath.Join(wtDir, entry.Name())
			slog.Info("removing stale worktree", "path", wtPath, "age", time.Since(info.ModTime()).Round(time.Minute))
			if err := os.RemoveAll(wtPath); err != nil {
				slog.Warn("failed to remove stale worktree", "path", wtPath, "error", err)
			}
		}
	}
}
