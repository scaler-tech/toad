package config

import (
	"os"
	"path/filepath"
)

// Resolver maps triage output to a specific repo using file-existence verification.
type Resolver struct {
	profiles []RepoProfile
	repos    []RepoConfig
}

// NewResolver creates a repo resolver.
func NewResolver(profiles []RepoProfile, repos []RepoConfig) *Resolver {
	return &Resolver{profiles: profiles, repos: repos}
}

// Resolve takes triage output and returns the best-matching repo config.
// Priority: file verification > triage hint > primary fallback > nil.
func (r *Resolver) Resolve(triageRepo string, fileHints []string) *RepoConfig {
	// Single repo: always return it.
	if len(r.repos) == 1 {
		return &r.repos[0]
	}

	// Stat-check file hints against all repos.
	fileCounts := r.verifyFiles(fileHints)

	// Find the repo with most file matches.
	bestFileRepo := ""
	bestCount := 0
	for name, count := range fileCounts {
		if count > bestCount {
			bestCount = count
			bestFileRepo = name
		}
	}

	// If file hints match exactly one repo, use it (highest confidence).
	if bestCount > 0 && countNonZero(fileCounts) == 1 {
		return RepoByName(r.repos, bestFileRepo)
	}

	// If file hints confirm triage's suggestion, use it.
	if triageRepo != "" && fileCounts[triageRepo] > 0 {
		return RepoByName(r.repos, triageRepo)
	}

	// If file hints contradict triage (exist in a different repo), use file-matched repo.
	if bestCount > 0 && bestFileRepo != triageRepo {
		return RepoByName(r.repos, bestFileRepo)
	}

	// No file matches — trust triage's repo suggestion.
	if triageRepo != "" {
		if repo := RepoByName(r.repos, triageRepo); repo != nil {
			return repo
		}
	}

	// No triage repo — fall back to primary.
	return PrimaryRepo(r.repos)
}

// verifyFiles checks which repos contain the hinted files.
func (r *Resolver) verifyFiles(fileHints []string) map[string]int {
	counts := make(map[string]int)
	if len(fileHints) == 0 {
		return counts
	}

	for _, hint := range fileHints {
		for _, p := range r.profiles {
			candidate := filepath.Join(p.Path, hint)
			if _, err := os.Stat(candidate); err == nil {
				counts[p.Name]++
			}
		}
	}
	return counts
}

func countNonZero(m map[string]int) int {
	n := 0
	for _, v := range m {
		if v > 0 {
			n++
		}
	}
	return n
}
