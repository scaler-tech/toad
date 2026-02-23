package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	islack "github.com/hergen/toad/internal/slack"
	"github.com/hergen/toad/internal/state"
	"github.com/hergen/toad/internal/tadpole"
)

// SpawnFunc spawns a tadpole task. Typically wraps tadpole.Pool.Spawn.
type SpawnFunc func(ctx context.Context, task tadpole.Task) error

// Watcher polls toad PRs for new review comments and spawns fix tadpoles.
type Watcher struct {
	db       *state.DB
	repoPath string
	spawn    SpawnFunc
	slack    *islack.Client
	interval time.Duration
}

// NewWatcher creates a PR review watcher.
func NewWatcher(db *state.DB, repoPath string, spawn SpawnFunc, slack *islack.Client) *Watcher {
	return &Watcher{
		db:       db,
		repoPath: repoPath,
		spawn:    spawn,
		slack:    slack,
		interval: 2 * time.Minute,
	}
}

// Run starts the polling loop. Blocks until ctx is canceled.
func (w *Watcher) Run(ctx context.Context) {
	slog.Info("PR review watcher started", "interval", w.interval)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.poll(ctx)
		case <-ctx.Done():
			slog.Info("PR review watcher stopped")
			return
		}
	}
}

// TrackPR registers a PR for review comment monitoring.
func (w *Watcher) TrackPR(prNumber int, prURL, branch, runID, channel, thread string) {
	if err := w.db.SavePRWatch(prNumber, prURL, branch, runID, channel, thread); err != nil {
		slog.Error("failed to track PR for review", "pr", prNumber, "error", err)
	} else {
		slog.Info("tracking PR for review comments", "pr", prNumber, "branch", branch)
	}
}

func (w *Watcher) poll(ctx context.Context) {
	watches, err := w.db.OpenPRWatches()
	if err != nil {
		slog.Error("failed to get open PR watches", "error", err)
		return
	}

	slog.Debug("PR watcher polling", "open_watches", len(watches))

	for _, watch := range watches {
		if err := w.checkPR(ctx, watch); err != nil {
			slog.Warn("failed to check PR", "pr", watch.PRNumber, "error", err)
		}
	}
}

// ghComment represents a review comment from the GitHub API.
type ghComment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
	Path string `json:"path"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"` // "User" or "Bot"
	} `json:"user"`
	CreatedAt string `json:"created_at"`
}

// ghPR represents PR state from the GitHub CLI.
type ghPR struct {
	State string `json:"state"` // "OPEN", "CLOSED", "MERGED"
}

func (w *Watcher) checkPR(ctx context.Context, watch *state.PRWatch) error {
	// Check PR state first — close the watch if merged/closed
	prState, err := w.getPRState(ctx, watch.PRNumber)
	if err != nil {
		return fmt.Errorf("getting PR state: %w", err)
	}
	if !strings.EqualFold(prState.State, "open") {
		slog.Info("PR closed/merged, stopping watch", "pr", watch.PRNumber, "state", prState.State)
		return w.db.ClosePRWatch(watch.PRNumber)
	}

	// Fetch both inline review comments AND conversation comments.
	// GitHub has two separate APIs: pulls/{n}/comments for inline code review
	// comments, and issues/{n}/comments for conversation-tab comments.
	reviewComments, err := w.getReviewComments(ctx, watch.PRNumber)
	if err != nil {
		slog.Warn("failed to get review comments, continuing", "pr", watch.PRNumber, "error", err)
	}
	issueComments, err := w.getIssueComments(ctx, watch.PRNumber)
	if err != nil {
		slog.Warn("failed to get issue comments, continuing", "pr", watch.PRNumber, "error", err)
	}

	// Merge both comment types — IDs are globally unique across GitHub
	var allComments []ghComment
	allComments = append(allComments, reviewComments...)
	allComments = append(allComments, issueComments...)

	// Filter to new comments from humans (not bots, not toad)
	var newComments []ghComment
	for _, c := range allComments {
		if c.ID <= watch.LastCommentID {
			continue
		}
		if c.User.Type == "Bot" {
			continue
		}
		newComments = append(newComments, c)
	}

	slog.Debug("PR check complete",
		"pr", watch.PRNumber,
		"review_comments", len(reviewComments),
		"issue_comments", len(issueComments),
		"new_human_comments", len(newComments),
	)

	if len(newComments) == 0 {
		return nil
	}

	slog.Info("new review comments found",
		"pr", watch.PRNumber,
		"count", len(newComments),
		"fix_count", watch.FixCount,
	)

	// Build fix task from review comments
	var sb strings.Builder
	fmt.Fprintf(&sb, "Fix review comments on PR #%d.\n\n", watch.PRNumber)
	sb.WriteString("Review comments to address:\n\n")
	for _, c := range newComments {
		if c.Path != "" {
			fmt.Fprintf(&sb, "File: %s\n", c.Path)
		}
		fmt.Fprintf(&sb, "@%s: %s\n\n", c.User.Login, c.Body)
	}

	task := tadpole.Task{
		Description:    sb.String(),
		Summary:        fmt.Sprintf("fix review comments on PR #%d", watch.PRNumber),
		Category:       "bug",
		EstSize:        "small",
		SlackChannel:   watch.SlackChannel,
		SlackThreadTS:  watch.SlackThread,
		ExistingBranch: watch.Branch,
	}

	// Update last seen comment ID BEFORE spawning to prevent duplicate spawns
	// if the spawn is slow or the next poll fires while a tadpole is running.
	maxID := newComments[len(newComments)-1].ID
	if err := w.db.UpdatePRWatchLastComment(watch.PRNumber, maxID); err != nil {
		return fmt.Errorf("updating last comment ID: %w", err)
	}

	// Notify in Slack
	if w.slack != nil && watch.SlackChannel != "" && watch.SlackThread != "" {
		w.slack.ReplyInThread(watch.SlackChannel, watch.SlackThread,
			fmt.Sprintf(":mag: %d new review comment(s) on PR #%d — spawning fix tadpole...",
				len(newComments), watch.PRNumber))
	}

	// Spawn fix tadpole
	if err := w.spawn(ctx, task); err != nil {
		slog.Error("failed to spawn review fix tadpole", "pr", watch.PRNumber, "error", err)
		if w.slack != nil && watch.SlackChannel != "" {
			w.slack.ReplyInThread(watch.SlackChannel, watch.SlackThread,
				fmt.Sprintf(":x: Failed to spawn fix tadpole for PR #%d: %s", watch.PRNumber, err))
		}
		return err
	}

	return nil
}

func (w *Watcher) getReviewComments(ctx context.Context, prNumber int) ([]ghComment, error) {
	cmd := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", prNumber),
		"--jq", ".",
	)
	cmd.Dir = w.repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var comments []ghComment
	if err := json.Unmarshal(stdout.Bytes(), &comments); err != nil {
		return nil, fmt.Errorf("parsing comments: %w", err)
	}
	return comments, nil
}

func (w *Watcher) getIssueComments(ctx context.Context, prNumber int) ([]ghComment, error) {
	cmd := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", prNumber),
		"--jq", ".",
	)
	cmd.Dir = w.repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var comments []ghComment
	if err := json.Unmarshal(stdout.Bytes(), &comments); err != nil {
		return nil, fmt.Errorf("parsing issue comments: %w", err)
	}
	return comments, nil
}

func (w *Watcher) getPRState(ctx context.Context, prNumber int) (*ghPR, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view",
		strconv.Itoa(prNumber),
		"--json", "state",
	)
	cmd.Dir = w.repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var pr ghPR
	if err := json.Unmarshal(stdout.Bytes(), &pr); err != nil {
		return nil, fmt.Errorf("parsing PR state: %w", err)
	}
	return &pr, nil
}

// ExtractPRNumber extracts a PR number from a GitHub PR URL.
// e.g., "https://github.com/owner/repo/pull/42" → 42
func ExtractPRNumber(prURL string) (int, error) {
	parts := strings.Split(strings.TrimRight(prURL, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "pull" {
		return 0, fmt.Errorf("not a PR URL: %s", prURL)
	}
	return strconv.Atoi(parts[len(parts)-1])
}
