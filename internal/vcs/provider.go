// Package vcs provides a generic interface for version control platform integrations.
//
// In toad v2, tadpole coding/shipping was dropped in favor of an investigation
// agent that files Linear tickets; VCS involvement is now limited to read-only
// signals that inform triage and investigation, not PR/MR lifecycle management.
package vcs

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Provider abstracts VCS platform operations (GitHub, GitLab, etc.).
type Provider interface {
	// Check verifies the VCS CLI tool is available.
	Check() error

	// GetSuggestedReviewers returns up to max login handles of recent committers
	// to the given files, excluding bot accounts.
	GetSuggestedReviewers(ctx context.Context, repoPath string, files []string, botNames map[string]bool, max int) []string
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
