// Package preflight runs startup checks to fail fast on misconfiguration.
package preflight

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/scaler-tech/toad/internal/config"
)

// Result holds the outcome of a single check.
type Result struct {
	Name   string
	OK     bool
	Detail string
}

// Run executes all preflight checks and returns errors for failures.
func Run(cfg *config.Config) []Result {
	var results []Result
	results = append(results, checkVCS(cfg)...)
	results = append(results, checkRepos(cfg.Repos.List)...)
	return results
}

// checkVCS runs gh/glab CLI auth checks only for platforms actually in use
// across the global VCS config and any per-repo overrides.
func checkVCS(cfg *config.Config) []Result {
	var results []Result

	needsGitHub, needsGitLab := false, false
	switch strings.ToLower(config.ResolvedVCS(nil, cfg.VCS).Platform) {
	case "gitlab":
		needsGitLab = true
	default:
		needsGitHub = true
	}
	for _, r := range cfg.Repos.List {
		switch strings.ToLower(config.ResolvedVCS(&r, cfg.VCS).Platform) {
		case "gitlab":
			needsGitLab = true
		default:
			needsGitHub = true
		}
	}

	if needsGitHub {
		results = append(results, checkGH()...)
	}
	if needsGitLab {
		results = append(results, checkGlab()...)
	}

	return results
}

func checkGH() []Result {
	var results []Result

	// Check gh is installed
	out, err := exec.Command("gh", "auth", "status").CombinedOutput()
	if err != nil {
		results = append(results, Result{
			Name:   "gh auth",
			OK:     false,
			Detail: fmt.Sprintf("gh auth status failed: %s", strings.TrimSpace(string(out))),
		})
	} else {
		results = append(results, Result{Name: "gh auth", OK: true})
	}

	return results
}

func checkGlab() []Result {
	var results []Result

	// Check glab is installed and authenticated
	out, err := exec.Command("glab", "auth", "status").CombinedOutput()
	if err != nil {
		results = append(results, Result{
			Name:   "glab auth",
			OK:     false,
			Detail: fmt.Sprintf("glab auth status failed: %s", strings.TrimSpace(string(out))),
		})
	} else {
		results = append(results, Result{Name: "glab auth", OK: true})
	}

	return results
}

func checkRepos(repos []config.RepoConfig) []Result {
	var results []Result
	for _, r := range repos {
		info, err := os.Stat(r.Path)
		if err != nil {
			results = append(results, Result{
				Name:   fmt.Sprintf("repo %s", r.Name),
				OK:     false,
				Detail: fmt.Sprintf("path does not exist: %s", r.Path),
			})
			continue
		}
		if !info.IsDir() {
			results = append(results, Result{
				Name:   fmt.Sprintf("repo %s", r.Name),
				OK:     false,
				Detail: fmt.Sprintf("path is not a directory: %s", r.Path),
			})
			continue
		}
		// Check it's a git repo
		gitDir := r.Path + "/.git"
		if _, err := os.Stat(gitDir); err != nil {
			results = append(results, Result{
				Name:   fmt.Sprintf("repo %s", r.Name),
				OK:     false,
				Detail: fmt.Sprintf("not a git repository: %s", r.Path),
			})
			continue
		}
		results = append(results, Result{Name: fmt.Sprintf("repo %s", r.Name), OK: true})
	}
	return results
}

// Errors returns only the failed checks.
func Errors(results []Result) []Result {
	var errs []Result
	for _, r := range results {
		if !r.OK {
			errs = append(errs, r)
		}
	}
	return errs
}

// FormatErrors formats failed checks into a readable error message.
func FormatErrors(errs []Result) string {
	var b strings.Builder
	b.WriteString("preflight checks failed:\n")
	for _, e := range errs {
		b.WriteString(fmt.Sprintf("  ✗ %s: %s\n", e.Name, e.Detail))
	}
	return b.String()
}
