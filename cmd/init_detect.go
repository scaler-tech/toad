package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hergen/toad/internal/config"
)

// repoDefaults holds auto-detected values for a repository.
type repoDefaults struct {
	Stack         string
	Module        string
	Description   string
	TestCommand   string
	LintCommand   string
	DefaultBranch string
}

// detectRepoDefaults runs all auto-detection for a repository path.
func detectRepoDefaults(repoPath string) repoDefaults {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}

	d := repoDefaults{DefaultBranch: "main"}
	d.Stack, d.Module = config.DetectStack(abs)
	d.Description = config.ReadREADMEFirstParagraph(abs)
	d.TestCommand, d.LintCommand = suggestCommands(d.Stack, abs)
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

	return "main"
}

// suggestCommands returns suggested test and lint commands based on detected stack.
func suggestCommands(stack, repoPath string) (testCmd, lintCmd string) {
	switch stack {
	case "Go":
		testCmd = "go test ./..."
		for _, f := range []string{".golangci.yml", ".golangci.yaml", ".golangci.toml"} {
			if _, err := os.Stat(filepath.Join(repoPath, f)); err == nil {
				lintCmd = "golangci-lint run"
				return testCmd, lintCmd
			}
		}
		lintCmd = "go vet ./..."

	case "TypeScript", "TypeScript/React", "JavaScript", "JavaScript/React":
		runner := "npm"
		if _, err := os.Stat(filepath.Join(repoPath, "yarn.lock")); err == nil {
			runner = "yarn"
		} else if _, err := os.Stat(filepath.Join(repoPath, "pnpm-lock.yaml")); err == nil {
			runner = "pnpm"
		} else if _, err := os.Stat(filepath.Join(repoPath, "bun.lock")); err == nil {
			runner = "bun"
		} else if _, err := os.Stat(filepath.Join(repoPath, "bun.lockb")); err == nil {
			runner = "bun"
		}
		testCmd = runner + " test"
		lintCmd = runner + " run lint"
		if runner == "yarn" {
			lintCmd = "yarn lint"
		}

	case "Python":
		testCmd = "pytest"
		lintCmd = "ruff check ."

	case "Rust":
		testCmd = "cargo test"
		lintCmd = "cargo clippy"

	case "PHP":
		if _, err := os.Stat(filepath.Join(repoPath, "Makefile")); err == nil {
			testCmd = "make test"
			lintCmd = "make stan && make cs"
		} else {
			testCmd = "./vendor/bin/phpunit"
			lintCmd = "./vendor/bin/phpstan analyse" //nolint:misspell // phpstan's actual command name
		}
	}
	return testCmd, lintCmd
}
