package vcs

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
)

// GitHubProvider implements Provider using the gh CLI.
type GitHubProvider struct{}

func (g *GitHubProvider) Check() error {
	_, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("gh CLI not found in PATH — install it first: https://cli.github.com")
	}
	return nil
}

// GetSuggestedReviewers returns up to max GitHub login handles who have recently
// committed to the given files, resolved via the GitHub API. Bot accounts are excluded.
//
// When multiple files are provided, each file is queried independently and
// contributors are weighted by file specificity: a file with fewer unique
// authors is more "owned" and its contributors score higher. This prevents
// generic files (e.g. Handler.php) from drowning out signal from the specific
// files that actually matter for the issue.
func (g *GitHubProvider) GetSuggestedReviewers(ctx context.Context, repoPath string, files []string, botNames map[string]bool, max int) []string {
	if len(files) == 0 {
		return nil
	}

	// Collect commit SHAs per file so we can weight by file specificity.
	type fileCommits struct {
		file string
		shas []string
	}
	var perFile []fileCommits
	for _, f := range files {
		args := []string{"log", "-n", "10", "--format=%H", "--", f}
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repoPath
		out, err := cmd.Output()
		if err != nil {
			slog.Debug("GetSuggestedReviewers: git log failed", "file", f, "error", err)
			continue
		}
		seen := make(map[string]bool)
		var shas []string
		for _, sha := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			sha = strings.TrimSpace(sha)
			if sha == "" || seen[sha] {
				continue
			}
			seen[sha] = true
			shas = append(shas, sha)
		}
		if len(shas) > 0 {
			perFile = append(perFile, fileCommits{file: f, shas: shas})
		}
	}

	if len(perFile) == 0 {
		return nil
	}

	// Resolve SHAs to GitHub logins, deduplicating API calls across files.
	shaToLogin := make(map[string]string)
	for _, fc := range perFile {
		for _, sha := range fc.shas {
			if _, ok := shaToLogin[sha]; ok {
				continue
			}
			endpoint := fmt.Sprintf("repos/{owner}/{repo}/commits/%s", sha)
			apiCmd := exec.CommandContext(ctx, "gh", "api", endpoint, "--jq", ".author.login // empty")
			apiCmd.Dir = repoPath
			apiOut, apiErr := apiCmd.Output()
			if apiErr != nil {
				slog.Debug("GetSuggestedReviewers: gh api failed", "sha", sha, "error", apiErr)
				shaToLogin[sha] = "" // cache failure to avoid retrying
				continue
			}
			login := strings.TrimSpace(string(apiOut))
			lower := strings.ToLower(login)
			if login == "" || strings.Contains(lower, "bot") || botNames[lower] {
				shaToLogin[sha] = ""
				continue
			}
			shaToLogin[sha] = login
		}
	}

	// Score contributors: for each file, contributors get 1/uniqueAuthors points.
	// A file with 2 unique authors gives each contributor 0.5 points per commit,
	// while a file with 10 unique authors gives only 0.1 — naturally demoting
	// generic files touched by many people.
	scores := make(map[string]float64)
	for _, fc := range perFile {
		// Count unique authors for this file
		authors := make(map[string]bool)
		for _, sha := range fc.shas {
			if login := shaToLogin[sha]; login != "" {
				authors[login] = true
			}
		}
		if len(authors) == 0 {
			continue
		}
		weight := 1.0 / float64(len(authors))
		for _, sha := range fc.shas {
			if login := shaToLogin[sha]; login != "" {
				scores[login] += weight
			}
		}
	}

	type loginScore struct {
		login string
		score float64
	}
	sorted := make([]loginScore, 0, len(scores))
	for login, score := range scores {
		sorted = append(sorted, loginScore{login, score})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].score != sorted[j].score {
			return sorted[i].score > sorted[j].score
		}
		return sorted[i].login < sorted[j].login
	})

	result := make([]string, 0, max)
	for _, ls := range sorted {
		if len(result) >= max {
			break
		}
		result = append(result, ls.login)
	}
	return result
}
