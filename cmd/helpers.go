package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/vcs"
)

// enrichWithIssueDetails scans the primary message and thread context for
// issue tracker references (e.g. Linear URLs, bare ticket IDs). For each
// unique reference found, it fetches the full issue title and description
// and appends them as additional context lines. This lets triage and ribbit
// see what a linked ticket actually says rather than just its ID.
//
// It also returns the raw fetched issuetracker.IssueDetails alongside the
// formatted context lines. Callers (cmd/handlers.go, cmd/ticketflow.go)
// thread this slice through to buildTicketContextBlock instead of letting
// it re-derive and re-fetch the same references from the now-enriched
// text — GetIssueDetails is a network call per ref, and both functions were
// independently extracting refs from and fetching details for the same
// underlying message/thread (up to 2 GetIssueDetails calls per ticket per
// flow) before this was threaded through.
func enrichWithIssueDetails(ctx context.Context, tracker issuetracker.Tracker, text string, threadContext []string) ([]string, []issuetracker.IssueDetails) {
	// Gather all text to scan for references
	allText := text
	for _, tc := range threadContext {
		allText += "\n" + tc
	}

	refs := tracker.ExtractAllIssueRefs(allText)
	if len(refs) == 0 {
		return threadContext, nil
	}

	// Resolve each unique ref (cap at 3 to avoid slow lookups)
	limit := 3
	if len(refs) < limit {
		limit = len(refs)
	}
	var enriched []string
	var fetched []issuetracker.IssueDetails
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
		fetched = append(fetched, *details)
		slog.Debug("enriched thread context with issue details", "issue", details.ID)
	}

	if len(enriched) == 0 {
		return threadContext, nil
	}

	return append(threadContext, enriched...), fetched
}

// repoSyncer matches SyncRepoNow's signature (and investigation.RepoSyncer's)
// without importing internal/investigation here just for the type.
type repoSyncer func(ctx context.Context, repo config.RepoConfig) error

// syncRepos periodically fetches and fast-forward pulls all configured repos.
// This keeps the local checkout fresh for ribbit (read-only Q&A) and digest
// investigations, which operate on the working tree without fetching. sync is
// normally a repoSyncTracker-wrapped SyncRepoNow (see root.go) so every
// attempt's outcome is recorded for the dashboard.
func syncRepos(ctx context.Context, repos []config.RepoConfig, interval time.Duration, sync repoSyncer) {
	slog.Info("repo sync started", "interval", interval, "repos", len(repos))

	// Run immediately on startup, then on ticker.
	syncAll(ctx, repos, sync)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			syncAll(ctx, repos, sync)
		case <-ctx.Done():
			return
		}
	}
}

func syncAll(ctx context.Context, repos []config.RepoConfig, sync repoSyncer) {
	for _, repo := range repos {
		if err := sync(ctx, repo); err != nil {
			slog.Warn("repo sync failed", "repo", repo.Name, "error", err)
		}
	}
}

// repoSyncTracker records the outcome of every repo sync attempt (periodic
// background sync and the on-demand pre-investigation sync alike, since both
// ultimately call SyncRepoNow) keyed by repo name, and snapshots into
// state.DaemonStats.RepoSync every 10s (root.go's stats ticker) for the
// dashboard's per-repo freshness display and sync-warning alert. Safe for
// concurrent use — sync attempts and the stats ticker run on different
// goroutines.
type repoSyncTracker struct {
	mu     sync.Mutex
	status map[string]state.RepoSyncStatus
}

func newRepoSyncTracker() *repoSyncTracker {
	return &repoSyncTracker{status: make(map[string]state.RepoSyncStatus)}
}

// record updates the tracked status for repo after one sync attempt. err nil
// means success (refreshes LastSyncAt, clears LastError); non-nil means
// failure (LastError set, LastSyncAt left at its previous value).
func (t *repoSyncTracker) record(repo string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.status[repo]
	st.CheckedAt = time.Now()
	if err != nil {
		st.LastError = err.Error()
	} else {
		st.LastSyncAt = st.CheckedAt
		st.LastError = ""
	}
	t.status[repo] = st
}

// snapshot returns a copy of the tracked statuses, safe to embed in a
// state.DaemonStats for JSON marshaling without holding t's lock. Returns nil
// (which marshals as an absent/omitted field, matching the "no sync
// attempted yet" case) when nothing has been recorded.
func (t *repoSyncTracker) snapshot() map[string]state.RepoSyncStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.status) == 0 {
		return nil
	}
	out := make(map[string]state.RepoSyncStatus, len(t.status))
	for k, v := range t.status {
		out[k] = v
	}
	return out
}

// wrap adapts a repoSyncer (normally SyncRepoNow) to record every attempt's
// outcome in t before returning it to the caller unchanged.
func (t *repoSyncTracker) wrap(sync repoSyncer) repoSyncer {
	return func(ctx context.Context, repo config.RepoConfig) error {
		err := sync(ctx, repo)
		t.record(repo.Name, err)
		return err
	}
}

// concurrencyGauge reports the current occupancy of a counting semaphore —
// a buffered channel used with the acquire-then-release (send-then-receive)
// pattern, as ribbitSem/investigateSem are throughout cmd/. slots is the
// channel's fixed capacity; inFlight is how many slots are currently held.
// Reading len/cap on a channel concurrently with other goroutines sending to
// or receiving from it is well-defined and race-free (they're simple field
// reads on the runtime's channel header), so this needs no extra locking —
// deliberately simpler than a parallel atomic-counter registry, which would
// risk drifting from the semaphore's actual occupancy and wouldn't cover the
// MCP ask tool's use of the same ribbitSem instance (internal/mcp/tools.go)
// without new cross-package plumbing.
func concurrencyGauge(sem chan struct{}) (slots, inFlight int) {
	return cap(sem), len(sem)
}

// incrementMetric bumps a dashboard trend-series counter (see
// state.DB.IncrementMetric), tolerating a nil db (in-memory
// state.NewManager(), used by tests/CLI one-shots with no persistent DB) and
// any write failure — these are best-effort telemetry and must never affect
// the main flow.
func incrementMetric(db *state.DB, name string) {
	if db == nil {
		return
	}
	if err := db.IncrementMetric(name, time.Now()); err != nil {
		slog.Debug("metric increment failed", "metric", name, "error", err)
	}
}

// SyncRepoNow fetches and fast-forward pulls a single repo's working copy.
// It is the single-repo primitive behind the periodic syncRepos loop above;
// other callers (e.g. an on-demand investigation gate) can invoke it directly
// to make sure a repo is current before reading from it.
func SyncRepoNow(ctx context.Context, repo config.RepoConfig) error {
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", "origin")
	fetchCmd.Dir = repo.Path
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	// Fast-forward pull if on the default branch (no-op if detached or on another branch).
	// Falls back to hard reset when branches have diverged — these are toad's
	// working copies with no local changes to preserve.
	branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = repo.Path
	branchOut, err := branchCmd.Output()
	if err != nil {
		// Detached HEAD or unreadable — nothing more we can safely do.
		return nil //nolint:nilerr // deliberate: skip sync rather than fail the investigation
	}
	currentBranch := strings.TrimSpace(string(branchOut))
	if currentBranch == repo.DefaultBranch {
		pullCmd := exec.CommandContext(ctx, "git", "pull", "--ff-only")
		pullCmd.Dir = repo.Path
		if _, err := pullCmd.CombinedOutput(); err != nil {
			// Diverged branch — reset to match origin (no local work to lose).
			resetCmd := exec.CommandContext(ctx, "git", "reset", "--hard", "origin/"+repo.DefaultBranch)
			resetCmd.Dir = repo.Path
			if out, resetErr := resetCmd.CombinedOutput(); resetErr != nil {
				return fmt.Errorf("reset failed: %w (output: %s)", resetErr, strings.TrimSpace(string(out)))
			}
			slog.Info("repo sync reset to origin", "repo", repo.Name, "branch", repo.DefaultBranch)
		}
	}

	slog.Debug("repo synced", "repo", repo.Name, "branch", currentBranch)
	return nil
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
