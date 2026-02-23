package tadpole

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// WorktreeResult holds paths created by worktree setup.
type WorktreeResult struct {
	Path   string // ~/.toad/worktrees/<slug>-<id>
	Branch string // toad/<slug>
}

// CreateWorktree creates a git worktree for an isolated tadpole run.
// It creates the worktree from the existing local ref, then fetches inside the
// worktree to get a fresh base — this avoids holding locks on the main repo
// that would block concurrent git operations (ribbit prefetch, other tadpoles).
func CreateWorktree(ctx context.Context, repoPath, slug, defaultBranch string) (*WorktreeResult, error) {
	slug = Slugify(slug)

	id, err := randomHex(8)
	if err != nil {
		return nil, fmt.Errorf("generating random id: %w", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("getting home dir: %w", err)
	}

	wtDir := filepath.Join(homeDir, ".toad", "worktrees")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating worktree directory: %w", err)
	}

	wtPath := filepath.Join(wtDir, slug+"-"+id)
	branch := "toad/" + slug

	// Create worktree from existing local ref (fast, no network, minimal locking)
	slog.Info("creating worktree", "path", wtPath, "branch", branch)
	if err := gitRunCtx(ctx, repoPath, "worktree", "add", "-b", branch, wtPath, "origin/"+defaultBranch); err != nil {
		return nil, fmt.Errorf("creating worktree: %w", err)
	}

	// Fetch latest inside the worktree so we don't lock the main repo
	slog.Debug("fetching origin in worktree", "branch", defaultBranch)
	if err := gitRunCtx(ctx, wtPath, "fetch", "origin", defaultBranch); err != nil {
		slog.Warn("fetch in worktree failed, continuing with existing ref", "error", err)
	} else {
		// Reset to the freshly fetched ref
		if err := gitRunCtx(ctx, wtPath, "reset", "--hard", "origin/"+defaultBranch); err != nil {
			slog.Warn("reset to fetched ref failed", "error", err)
		}
	}

	return &WorktreeResult{Path: wtPath, Branch: branch}, nil
}

// CheckoutWorktree creates a worktree from an existing remote branch (for review fixes).
func CheckoutWorktree(ctx context.Context, repoPath, branch string) (*WorktreeResult, error) {
	id, err := randomHex(8)
	if err != nil {
		return nil, fmt.Errorf("generating random id: %w", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("getting home dir: %w", err)
	}

	wtDir := filepath.Join(homeDir, ".toad", "worktrees")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating worktree directory: %w", err)
	}

	slug := Slugify(branch)
	wtPath := filepath.Join(wtDir, slug+"-fix-"+id)

	// Fetch latest for the branch
	slog.Info("fetching branch for review fix", "branch", branch)
	if err := gitRunCtx(ctx, repoPath, "fetch", "origin", branch); err != nil {
		return nil, fmt.Errorf("fetching branch: %w", err)
	}

	// Create worktree from the fetched branch
	if err := gitRunCtx(ctx, repoPath, "worktree", "add", wtPath, "origin/"+branch); err != nil {
		return nil, fmt.Errorf("creating worktree from existing branch: %w", err)
	}

	// Checkout the branch (not detached HEAD)
	if err := gitRunCtx(ctx, wtPath, "checkout", branch); err != nil {
		// If local branch doesn't exist, create it tracking the remote
		if err2 := gitRunCtx(ctx, wtPath, "checkout", "-b", branch, "origin/"+branch); err2 != nil {
			return nil, fmt.Errorf("checking out branch: %w", err2)
		}
	}

	return &WorktreeResult{Path: wtPath, Branch: branch}, nil
}

// pushBranch pushes the current branch to origin without creating a PR.
func pushBranch(ctx context.Context, worktreePath, branch string) error {
	return gitRunCtx(ctx, worktreePath, "push", "origin", branch)
}

// RemoveWorktree force-removes a worktree and prunes. Best-effort cleanup.
// Uses a 30-second timeout to avoid hanging during shutdown.
func RemoveWorktree(repoPath, wtPath string) error {
	slog.Info("removing worktree", "path", wtPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := gitRunCtx(ctx, repoPath, "worktree", "remove", "--force", wtPath); err != nil {
		slog.Warn("worktree remove failed, trying manual cleanup", "error", err)
		_ = os.RemoveAll(wtPath)
	}

	// Prune stale worktree references
	if err := gitRunCtx(ctx, repoPath, "worktree", "prune"); err != nil {
		slog.Warn("worktree prune failed", "error", err)
	}

	return nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a task description to a URL-safe slug.
// "fix nil pointer in handler" → "fix-nil-pointer-in-handler"
func Slugify(task string) string {
	s := strings.ToLower(task)
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
		s = strings.TrimRight(s, "-")
	}
	if s == "" {
		s = "tadpole"
	}
	return s
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b)[:n], nil
}

func gitRunCtx(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
