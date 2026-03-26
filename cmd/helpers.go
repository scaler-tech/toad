package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/vcs"
)

// enrichWithIssueDetails scans the primary message and thread context for
// issue tracker references (e.g. Linear URLs, bare ticket IDs). For each
// unique reference found, it fetches the full issue title and description
// and appends them as additional context lines. This lets triage and ribbit
// see what a linked ticket actually says rather than just its ID.
func enrichWithIssueDetails(ctx context.Context, tracker issuetracker.Tracker, text string, threadContext []string) []string {
	// Gather all text to scan for references
	allText := text
	for _, tc := range threadContext {
		allText += "\n" + tc
	}

	refs := tracker.ExtractAllIssueRefs(allText)
	if len(refs) == 0 {
		return threadContext
	}

	// Resolve each unique ref (cap at 3 to avoid slow lookups)
	limit := 3
	if len(refs) < limit {
		limit = len(refs)
	}
	var enriched []string
	for _, ref := range refs[:limit] {
		details, err := tracker.GetIssueDetails(ctx, ref)
		if err != nil {
			slog.Warn("failed to fetch issue details for enrichment", "issue", ref.ID, "error", err)
			continue
		}
		if details == nil {
			continue
		}
		entry := fmt.Sprintf("[%s] %s", details.ID, details.Title)
		if details.Description != "" {
			// Truncate long descriptions to keep the prompt reasonable
			desc := details.Description
			if len(desc) > 500 {
				desc = desc[:500] + "..."
			}
			entry += "\n" + desc
		}
		enriched = append(enriched, entry)
		slog.Debug("enriched thread context with issue details", "issue", details.ID)
	}

	if len(enriched) == 0 {
		return threadContext
	}

	return append(threadContext, enriched...)
}

// isRetryIntent checks if a message text indicates the user wants to retry a previous attempt.
func isRetryIntent(text string) bool {
	lower := strings.ToLower(text)
	retryPhrases := []string{
		"try again",
		"retry",
		"redo",
		"re-do",
		"one more time",
		"rerun",
		"re-run",
	}
	for _, phrase := range retryPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// hasFailedTadpole checks thread context for evidence of a previous toad failure.
func hasFailedTadpole(threadContext []string) bool {
	for _, msg := range threadContext {
		if strings.Contains(msg, ":x: Tadpole failed") {
			return true
		}
	}
	return false
}

// truncate returns the first n runes of s, appending "..." if truncated.
func truncate(s string, n int) string {
	if n <= 3 {
		return "..."
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-3]) + "..."
}

// stripCodeFences removes markdown code fences (```json ... ``` or ``` ... ```)
// from text, returning the inner content. If no fences are found, returns the
// original text unchanged.
func stripCodeFences(text string) string {
	// Find opening fence
	fenceStart := strings.Index(text, "```")
	if fenceStart < 0 {
		return text
	}
	// Skip past the opening fence line (```json, ```, etc.)
	inner := text[fenceStart+3:]
	if nl := strings.Index(inner, "\n"); nl >= 0 {
		inner = inner[nl+1:]
	}
	// Find closing fence
	if fenceEnd := strings.Index(inner, "```"); fenceEnd >= 0 {
		inner = inner[:fenceEnd]
	}
	return inner
}

// findMatchingBrace finds the index of the '}' that matches the '{' at pos,
// accounting for nested braces and JSON strings.
func findMatchingBrace(s string, pos int) int {
	depth := 0
	inString := false
	escaped := false
	for i := pos; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		ch := s[i]
		if inString {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// syncRepos periodically fetches and fast-forward pulls all configured repos.
// This keeps the local checkout fresh for ribbit (read-only Q&A) and digest
// investigations, which operate on the working tree without fetching.
func syncRepos(ctx context.Context, repos []config.RepoConfig, interval time.Duration) {
	slog.Info("repo sync started", "interval", interval, "repos", len(repos))

	// Run immediately on startup, then on ticker.
	syncAll(repos)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			syncAll(repos)
		case <-ctx.Done():
			return
		}
	}
}

func syncAll(repos []config.RepoConfig) {
	for _, repo := range repos {
		fetchCmd := exec.Command("git", "fetch", "origin")
		fetchCmd.Dir = repo.Path
		if out, err := fetchCmd.CombinedOutput(); err != nil {
			slog.Warn("repo sync fetch failed", "repo", repo.Name, "error", err, "output", strings.TrimSpace(string(out)))
			continue
		}

		// Fast-forward pull if on the default branch (no-op if detached or on another branch).
		// Falls back to hard reset when branches have diverged — these are toad's
		// working copies with no local changes to preserve.
		branchCmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
		branchCmd.Dir = repo.Path
		branchOut, err := branchCmd.Output()
		if err != nil {
			continue
		}
		currentBranch := strings.TrimSpace(string(branchOut))
		if currentBranch == repo.DefaultBranch {
			pullCmd := exec.Command("git", "pull", "--ff-only")
			pullCmd.Dir = repo.Path
			if _, err := pullCmd.CombinedOutput(); err != nil {
				// Diverged branch — reset to match origin (no local work to lose).
				resetCmd := exec.Command("git", "reset", "--hard", "origin/"+repo.DefaultBranch)
				resetCmd.Dir = repo.Path
				if out, resetErr := resetCmd.CombinedOutput(); resetErr != nil {
					slog.Warn("repo sync reset failed", "repo", repo.Name, "error", resetErr, "output", strings.TrimSpace(string(out)))
				} else {
					slog.Info("repo sync reset to origin", "repo", repo.Name, "branch", repo.DefaultBranch)
				}
			}
		}

		slog.Debug("repo synced", "repo", repo.Name, "branch", currentBranch)
	}
}

// buildVCSResolver constructs a VCS Resolver from config, merging per-repo
// overrides with the global VCS settings. Each unique provider is Check()-ed
// during construction.
func buildVCSResolver(cfg *config.Config) (vcs.Resolver, error) {
	repoVCS := make(map[string]vcs.ProviderConfig, len(cfg.Repos.List))
	for _, r := range cfg.Repos.List {
		resolved := config.ResolvedVCS(&r, cfg.VCS)
		repoVCS[r.Path] = vcs.ProviderConfig{
			Platform:     resolved.Platform,
			Host:         resolved.Host,
			BotUsernames: resolved.BotUsernames,
		}
	}
	primary := config.PrimaryRepo(cfg.Repos.List)
	fallbackVCS := config.ResolvedVCS(primary, cfg.VCS)
	fallbackCfg := vcs.ProviderConfig{
		Platform:     fallbackVCS.Platform,
		Host:         fallbackVCS.Host,
		BotUsernames: fallbackVCS.BotUsernames,
	}
	return vcs.NewResolver(repoVCS, fallbackCfg)
}
