// Package vcs provides a generic interface for version control platform integrations.
package vcs

import (
	"context"
	"fmt"
	"sort"
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

	// GetMergeability returns whether a PR can be merged.
	// Returns "MERGEABLE", "CONFLICTING", or "UNKNOWN".
	GetMergeability(ctx context.Context, prNumber int, repoPath string) (string, error)

	// GetCIStatus returns the aggregate CI status for a PR.
	GetCIStatus(ctx context.Context, prNumber int, repoPath string) (*CIStatus, error)

	// GetCIFailureLogs fetches logs from failed CI runs, truncated to a reasonable size.
	GetCIFailureLogs(ctx context.Context, failedRunIDs []string, repoPath string) string

	// GetPRComments returns all comments (review + conversation) on a PR.
	GetPRComments(ctx context.Context, prNumber int, repoPath string) ([]PRComment, error)

	// AddCommentReaction adds an emoji reaction to a PR comment.
	// source is the comment type ("review" or "issue") for GitHub API routing.
	AddCommentReaction(ctx context.Context, prNumber, commentID int, source, reaction, repoPath string) error

	// PostPRComment posts a text comment on a PR (issue comment for GitHub, note for GitLab).
	PostPRComment(ctx context.Context, prNumber int, body, repoPath string) error

	// ListBotPRs returns PR numbers authored by bots targeting the given branch.
	ListBotPRs(ctx context.Context, branch, repoPath string) ([]int, error)

	// MergePR merges a PR with squash and deletes the branch.
	MergePR(ctx context.Context, prNumber int, repoPath string) error

	// ExtractPRNumber extracts a PR number from a PR URL.
	ExtractPRNumber(prURL string) (int, error)

	// ExtractRunID extracts a CI run ID from a details URL.
	ExtractRunID(detailsURL string) string

	// PRNoun returns the platform-specific noun for a pull/merge request
	// (e.g. "PR" for GitHub, "MR" for GitLab).
	PRNoun() string

	// GetSuggestedReviewers returns up to max login handles of recent committers
	// to the given files, excluding bot accounts.
	GetSuggestedReviewers(ctx context.Context, repoPath string, files []string, botNames map[string]bool, max int) []string
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
	Source    string // comment type: "review", "issue", "pr_review" (GitHub), "note" (GitLab)
	UserLogin string
	UserType  string // "User" or "Bot"
	CreatedAt time.Time
}

// PRCommentRef is a lightweight reference to a PR comment for reaction tracking.
type PRCommentRef struct {
	ID     int
	Source string // "review" or "issue"
}

// ProviderConfig holds parameters for creating a VCS provider.
type ProviderConfig struct {
	Platform     string
	Host         string   // for self-hosted GitLab
	BotUsernames []string // usernames to treat as bots
}

// NewProvider creates a Provider from configuration.
// Returns an error for unrecognized platforms.
func NewProvider(cfg ProviderConfig) (Provider, error) {
	switch strings.ToLower(cfg.Platform) {
	case "github":
		return &GitHubProvider{}, nil
	case "gitlab":
		return &GitLabProvider{Host: cfg.Host, BotUsernames: cfg.BotUsernames}, nil
	default:
		return nil, fmt.Errorf("unsupported VCS platform %q — supported: github, gitlab", cfg.Platform)
	}
}

// Resolver maps a repo path to its VCS Provider.
type Resolver func(repoPath string) Provider

// NewResolver builds a Resolver from per-repo provider configs.
// repoVCS maps repo paths to their ProviderConfig.
// fallback is the provider config used for unknown/empty repo paths.
// Repos with identical configs share a single Provider instance.
// Each unique provider is Check()-ed once during construction.
func NewResolver(repoVCS map[string]ProviderConfig, fallback ProviderConfig) (Resolver, error) {
	cache := make(map[string]Provider)

	getOrCreate := func(c ProviderConfig) (Provider, error) {
		key := configKey(c)
		if p, ok := cache[key]; ok {
			return p, nil
		}
		p, err := NewProvider(c)
		if err != nil {
			return nil, err
		}
		if err := p.Check(); err != nil {
			return nil, err
		}
		cache[key] = p
		return p, nil
	}

	// Pre-build provider for each repo path.
	repoProviders := make(map[string]Provider, len(repoVCS))
	for path, cfg := range repoVCS {
		p, err := getOrCreate(cfg)
		if err != nil {
			return nil, fmt.Errorf("repo %s: %w", path, err)
		}
		repoProviders[path] = p
	}

	// Build fallback provider.
	fb, err := getOrCreate(fallback)
	if err != nil {
		return nil, fmt.Errorf("fallback VCS: %w", err)
	}

	return func(repoPath string) Provider {
		if p, ok := repoProviders[repoPath]; ok {
			return p
		}
		return fb
	}, nil
}

// configKey returns a canonical string for deduplicating ProviderConfigs.
// Uses \x00 as separator to avoid collisions from values containing | or ,.
func configKey(c ProviderConfig) string {
	sorted := make([]string, len(c.BotUsernames))
	copy(sorted, c.BotUsernames)
	sort.Strings(sorted)
	return strings.ToLower(c.Platform) + "\x00" + strings.ToLower(c.Host) + "\x00" + strings.Join(sorted, "\x00")
}
