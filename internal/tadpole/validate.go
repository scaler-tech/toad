package tadpole

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hergen/toad/internal/config"
)

// ValidateConfig configures what validation checks to run.
type ValidateConfig struct {
	TestCommand     string
	LintCommand     string
	MaxFilesChanged int
	DefaultBranch   string
	Services        []config.ServiceConfig
	BaseCommit      string // if set, diff against this commit instead of origin/DefaultBranch (review fixes)
}

// ValidationResult holds the outcome of validation checks.
type ValidationResult struct {
	Passed       bool
	TestOutput   string
	LintOutput   string
	TestPassed   bool
	LintPassed   bool
	FilesChanged int
	FailReason   string
}

// Validate runs test/lint commands and checks file change count in a worktree.
// If services are configured, it detects which services were affected and runs
// per-service lint/test commands from the service directory.
func Validate(ctx context.Context, worktreePath string, cfg ValidateConfig) (*ValidationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	result := &ValidationResult{
		Passed:     true,
		TestPassed: true,
		LintPassed: true,
	}

	// Get list of changed files — compare against base commit for review fixes,
	// or against origin/defaultBranch for new tadpoles.
	// If BaseCommit is set but no longer an ancestor of HEAD (i.e. Claude rebased),
	// fall back to origin/defaultBranch to avoid an inflated diff.
	var changedFiles []string
	var err error
	rebased := false
	if cfg.BaseCommit != "" && isAncestor(ctx, worktreePath, cfg.BaseCommit) {
		changedFiles, err = getChangedFilesVsCommit(ctx, worktreePath, cfg.BaseCommit)
	} else {
		if cfg.BaseCommit != "" {
			slog.Info("validation: base commit no longer ancestor of HEAD (rebase detected), diffing against default branch",
				"base_commit", cfg.BaseCommit, "default_branch", cfg.DefaultBranch)
			rebased = true
		}
		changedFiles, err = getChangedFiles(ctx, worktreePath, cfg.DefaultBranch)
	}
	if err != nil {
		return nil, fmt.Errorf("listing changed files: %w", err)
	}
	result.FilesChanged = len(changedFiles)

	slog.Info("validation: files changed", "count", result.FilesChanged, "max", cfg.MaxFilesChanged)

	if result.FilesChanged == 0 {
		result.Passed = false
		result.FailReason = "no files were changed — Claude didn't make any modifications"
		return result, nil
	}

	// Skip max-files check when a rebase was detected on a review fix — the diff
	// against origin/defaultBranch includes the entire PR, not just Claude's changes.
	if cfg.MaxFilesChanged > 0 && result.FilesChanged > cfg.MaxFilesChanged && !rebased {
		result.Passed = false
		result.FailReason = fmt.Sprintf("too many files changed (%d > max %d)", result.FilesChanged, cfg.MaxFilesChanged)
		return result, nil
	}

	// Determine which services are affected and which commands to run
	checks := resolveChecks(changedFiles, cfg)

	for _, check := range checks {
		runDir := filepath.Join(worktreePath, check.path)

		// Run test command
		if check.testCommand != "" {
			label := check.path
			if label == "" {
				label = "root"
			}
			slog.Info("validation: running tests", "service", label, "command", check.testCommand)
			output, err := shellRun(ctx, runDir, check.testCommand)
			result.TestOutput += formatOutput(label, "test", output)
			if err != nil {
				result.TestPassed = false
				result.Passed = false
				result.FailReason = fmt.Sprintf("tests failed (%s)", label)
				slog.Warn("validation: tests failed", "service", label, "error", err)
			} else {
				slog.Info("validation: tests passed", "service", label)
			}
		}

		// Run lint command
		if check.lintCommand != "" {
			label := check.path
			if label == "" {
				label = "root"
			}
			slog.Info("validation: running lint", "service", label, "command", check.lintCommand)
			output, err := shellRun(ctx, runDir, check.lintCommand)
			result.LintOutput += formatOutput(label, "lint", output)
			if err != nil {
				result.LintPassed = false
				result.Passed = false
				if !result.TestPassed {
					result.FailReason = fmt.Sprintf("tests and lint failed (%s)", label)
				} else {
					result.FailReason = fmt.Sprintf("lint failed (%s)", label)
				}
				slog.Warn("validation: lint failed", "service", label, "error", err)
			} else {
				slog.Info("validation: lint passed", "service", label)
			}
		}
	}

	return result, nil
}

// validationCheck represents a lint/test command to run in a specific directory.
type validationCheck struct {
	path        string // relative to worktree root ("" for root)
	testCommand string
	lintCommand string
}

// resolveChecks determines which lint/test commands to run based on changed files.
// If services are configured, it matches changed files to services by path prefix.
// Falls back to root-level commands for files that don't match any service.
func resolveChecks(changedFiles []string, cfg ValidateConfig) []validationCheck {
	if len(cfg.Services) == 0 {
		// No services configured — use root commands
		return []validationCheck{{
			testCommand: cfg.TestCommand,
			lintCommand: cfg.LintCommand,
		}}
	}

	// Track which services are affected (dedup by path)
	affected := make(map[string]*config.ServiceConfig)
	hasUnmatchedFiles := false

	for _, file := range changedFiles {
		matched := false
		for i := range cfg.Services {
			svc := &cfg.Services[i]
			if strings.HasPrefix(file, svc.Path+"/") || file == svc.Path {
				affected[svc.Path] = svc
				matched = true
				break
			}
		}
		if !matched {
			hasUnmatchedFiles = true
		}
	}

	var checks []validationCheck

	// Add per-service checks
	for _, svc := range affected {
		checks = append(checks, validationCheck{
			path:        svc.Path,
			testCommand: svc.TestCommand,
			lintCommand: svc.LintCommand,
		})
	}

	// Add root-level check for unmatched files
	if hasUnmatchedFiles && (cfg.TestCommand != "" || cfg.LintCommand != "") {
		checks = append(checks, validationCheck{
			testCommand: cfg.TestCommand,
			lintCommand: cfg.LintCommand,
		})
	}

	// If nothing matched at all, fall back to root commands
	if len(checks) == 0 {
		return []validationCheck{{
			testCommand: cfg.TestCommand,
			lintCommand: cfg.LintCommand,
		}}
	}

	return checks
}

// getChangedFiles returns the list of files that differ from origin/defaultBranch.
func getChangedFiles(ctx context.Context, worktreePath, defaultBranch string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "origin/"+defaultBranch)
	cmd.Dir = worktreePath
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return nil, nil
	}
	return strings.Split(output, "\n"), nil
}

// isAncestor returns true if commit is an ancestor of HEAD in the given worktree.
// Used to detect whether Claude rebased: if the base commit is no longer an ancestor,
// diffing against it would include upstream changes pulled in by the rebase.
func isAncestor(ctx context.Context, worktreePath, commit string) bool {
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", commit, "HEAD")
	cmd.Dir = worktreePath
	return cmd.Run() == nil
}

// getChangedFilesVsCommit returns files that differ from a specific commit hash.
// Used for review fixes where we want to know what Claude changed on the branch,
// not what the branch differs from main.
func getChangedFilesVsCommit(ctx context.Context, worktreePath, commit string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", commit)
	cmd.Dir = worktreePath
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return nil, nil
	}
	return strings.Split(output, "\n"), nil
}

// formatOutput adds a service label header when running multi-service validation.
func formatOutput(service, checkType, output string) string {
	if service == "root" || service == "" {
		return output
	}
	return fmt.Sprintf("=== %s %s ===\n%s\n", service, checkType, output)
}

func shellRun(ctx context.Context, dir, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err := cmd.Run()
	return combined.String(), err
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (truncated)"
}
