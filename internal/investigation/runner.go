package investigation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
)

// RepoSyncer refreshes a repo's local checkout before an investigation runs
// against it. The real implementation (SyncRepoNow) is wired in from cmd/ in
// a later phase; a nil syncer is valid here and simply skips the sync step.
type RepoSyncer func(ctx context.Context, repo config.RepoConfig) error

// Runner drives a single read-only investigation agent run: it optionally
// syncs the target repo, builds the merged investigation prompt, runs the
// agent read-only against the repo, and parses the result into a Findings
// verdict.
type Runner struct {
	agent           agent.Provider
	model           string
	mcpConfigPath   string
	allowedMCPTools []string
	sync            RepoSyncer
	repoPaths       map[string]string // repo path -> repo name, for cross-repo --add-dir access
}

// NewRunner constructs a Runner. repoPaths maps each configured repo's
// absolute path to its name — the same orientation internal/ribbit uses to
// build AdditionalDirs — so the agent can be granted access to every
// configured repo even though WorkDir is set to the one being investigated.
func NewRunner(p agent.Provider, model, mcpConfigPath string, allowedMCPTools []string, sync RepoSyncer, repoPaths map[string]string) *Runner {
	return &Runner{
		agent:           p,
		model:           model,
		mcpConfigPath:   mcpConfigPath,
		allowedMCPTools: allowedMCPTools,
		sync:            sync,
		repoPaths:       repoPaths,
	}
}

// Request describes a single investigation to run.
type Request struct {
	Text          string // primary request / task description
	ThreadContext []string
	Category      string
	Confidence    float64
	Summary       string
	ChannelName   string
	Keywords      []string
	FilesHint     []string
	SentryRefs    []string           // from intake detection; may be empty
	TicketContext string             // pre-formatted <linked_tickets> block or ""
	Repo          *config.RepoConfig // resolved; required
	Timeout       time.Duration      // FromMessage: 4m; FromOpportunity: 10m
}

// Run syncs the target repo (if a syncer is configured), runs the
// investigation agent read-only against it, and parses the result into a
// Findings verdict.
func (r *Runner) Run(ctx context.Context, req Request) (*Findings, error) {
	if req.Repo == nil {
		return nil, errors.New("repo required")
	}

	if r.sync != nil {
		if err := r.sync(ctx, *req.Repo); err != nil {
			slog.Warn("investigation repo sync failed, continuing with existing checkout",
				"repo", req.Repo.Name, "error", err)
		}
	} else {
		slog.Debug("no repo syncer configured, skipping sync", "repo", req.Repo.Name)
	}

	additionalDirs := make([]string, 0, len(r.repoPaths))
	for path := range r.repoPaths {
		additionalDirs = append(additionalDirs, path)
	}

	opts := agent.RunOpts{
		Prompt:          buildPrompt(req),
		Model:           r.model,
		WorkDir:         req.Repo.Path,
		Timeout:         req.Timeout,
		Permissions:     agent.PermissionReadOnly,
		AdditionalDirs:  additionalDirs,
		MCPConfigPath:   r.mcpConfigPath,
		AllowedMCPTools: r.allowedMCPTools,
	}

	slog.Debug("running investigation", "model", r.model, "repo", req.Repo.Name)

	result, err := r.agent.Run(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("investigation agent run failed: %w", err)
	}

	findings, err := ParseFindings(result.Result)
	if err != nil {
		return nil, fmt.Errorf("parse investigation findings: %w", err)
	}

	// ParseFindings only extracts file paths from the parsed JSON's own
	// problem/root_cause/reasoning fields. The full raw transcript often
	// narrates additional paths the agent found along the way, so union
	// those in too (deduped, first-seen order preserved).
	findings.FilesFound = unionDedupe(findings.FilesFound, ExtractFilePaths(result.Result))

	if findings.Repo == "" {
		findings.Repo = req.Repo.Name
	}

	return findings, nil
}

// unionDedupe merges two string slices, preserving first-seen order and
// dropping duplicates.
func unionDedupe(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
