// Package vcs provides a generic interface for version control platform integrations.
package vcs

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Provider abstracts VCS platform operations (GitHub, GitLab, etc.).
type Provider interface {
	// Check verifies the VCS CLI tool is available.
	Check() error

	// CreatePR creates a pull/merge request and returns the URL.
	CreatePR(ctx context.Context, opts CreatePROpts) (string, error)

	// EnableAutoMerge enables auto-merge on a PR so it merges when CI passes.
	EnableAutoMerge(ctx context.Context, repoPath, branch string) error

	// GetPRState returns the PR state (e.g. "OPEN", "CLOSED", "MERGED").
	GetPRState(ctx context.Context, prNumber int, repoPath string) (string, error)

	// GetCIStatus returns the aggregate CI status for a PR.
	GetCIStatus(ctx context.Context, prNumber int, repoPath string) (*CIStatus, error)

	// GetCIFailureLogs fetches logs from failed CI runs, truncated to a reasonable size.
	GetCIFailureLogs(ctx context.Context, failedRunIDs []string, repoPath string) string

	// GetPRComments returns all comments (review + conversation) on a PR.
	GetPRComments(ctx context.Context, prNumber int, repoPath string) ([]PRComment, error)

	// ListBotPRs returns PR numbers authored by bots targeting the given branch.
	ListBotPRs(ctx context.Context, branch, repoPath string) ([]int, error)

	// MergePR merges a PR with squash and deletes the branch.
	MergePR(ctx context.Context, prNumber int, repoPath string) error

	// ExtractPRNumber extracts a PR number from a PR URL.
	ExtractPRNumber(prURL string) (int, error)

	// ExtractRunID extracts a CI run ID from a details URL.
	ExtractRunID(detailsURL string) string
}

// CreatePROpts holds parameters for creating a pull request.
type CreatePROpts struct {
	RepoPath string
	Branch   string
	Title    string
	Body     string
	Labels   []string
}

// CIStatus represents the aggregate CI status for a PR.
type CIStatus struct {
	State     string   // "pending", "success", "failure"
	FailedIDs []string // CI run IDs that failed
}

// PRComment represents a comment on a pull request.
type PRComment struct {
	ID        int
	Body      string
	Path      string // file path for inline review comments; empty for conversation comments
	UserLogin string
	UserType  string // "User" or "Bot"
	CreatedAt time.Time
}

// NewProvider creates a Provider from a platform name.
// Returns an error for unrecognized platforms.
func NewProvider(platform string) (Provider, error) {
	switch strings.ToLower(platform) {
	case "github":
		return &GitHubProvider{}, nil
	default:
		return nil, fmt.Errorf("unsupported VCS platform %q — supported: github", platform)
	}
}
