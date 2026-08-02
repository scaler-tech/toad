package cmd

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/scaler-tech/toad/internal/config"
)

// repoDefaults holds auto-detected values for a repository.
type repoDefaults struct {
	Stack         string
	Module        string
	Description   string
	DefaultBranch string
}

// detectRepoDefaults runs all auto-detection for a repository path.
func detectRepoDefaults(repoPath string) repoDefaults {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}

	d := repoDefaults{DefaultBranch: defaultBranchFallback}
	d.Stack, d.Module = config.DetectStack(abs)
	d.Description = config.ReadREADMEFirstParagraph(abs)
	d.DefaultBranch = detectDefaultBranch(abs)
	return d
}

// detectDefaultBranch tries git to find the default branch.
func detectDefaultBranch(repoPath string) string {
	// Try origin HEAD ref
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		parts := strings.Split(ref, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}

	// Try current branch
	cmd = exec.Command("git", "branch", "--show-current")
	cmd.Dir = repoPath
	out, err = cmd.Output()
	if err == nil {
		branch := strings.TrimSpace(string(out))
		if branch != "" {
			return branch
		}
	}

	return defaultBranchFallback
}

const defaultBranchFallback = "main"
