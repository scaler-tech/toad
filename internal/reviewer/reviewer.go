// Package reviewer watches PRs for review comments and spawns fix tadpoles.
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

	"github.com/hergen/toad/internal/config"
	islack "github.com/hergen/toad/internal/slack"
	"github.com/hergen/toad/internal/state"
	"github.com/hergen/toad/internal/tadpole"
)

// SpawnFunc spawns a tadpole task. Typically wraps tadpole.Pool.Spawn.
type SpawnFunc func(ctx context.Context, task tadpole.Task) error

// Watcher polls toad PRs for new review comments and spawns fix tadpoles.
type Watcher struct {
	db              *state.DB
	repos           []config.RepoConfig
	spawn           SpawnFunc
	slack           *islack.Client
	interval        time.Duration
	maxReviewRounds int
	maxCIFixRounds  int
	triageModel     string
}

// NewWatcher creates a PR review watcher.
func NewWatcher(db *state.DB, repos []config.RepoConfig, spawn SpawnFunc, slack *islack.Client, maxReviewRounds, maxCIFixRounds int, triageModel string) *Watcher {
	if maxReviewRounds <= 0 {
		maxReviewRounds = 3
	}
	if maxCIFixRounds <= 0 {
		maxCIFixRounds = 2
	}
	return &Watcher{
		db:              db,
		repos:           repos,
		spawn:           spawn,
		slack:           slack,
		interval:        2 * time.Minute,
		maxReviewRounds: maxReviewRounds,
		maxCIFixRounds:  maxCIFixRounds,
		triageModel:     triageModel,
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
func (w *Watcher) TrackPR(prNumber int, prURL, branch, runID, channel, thread, repoPath string) {
	if err := w.db.SavePRWatch(prNumber, prURL, branch, runID, channel, thread, repoPath); err != nil {
		slog.Error("failed to track PR for review", "pr", prNumber, "error", err)
	} else {
		slog.Info("tracking PR for review comments", "pr", prNumber, "branch", branch, "repo", repoPath)
	}
}

func (w *Watcher) poll(ctx context.Context) {
	watches, err := w.db.OpenPRWatches(w.maxReviewRounds, w.maxCIFixRounds)
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
	// 0. Skip if a tadpole is already running for this thread — avoids branch collision
	// when the previous fix is still in progress and the worktree holds the branch.
	if watch.SlackThread != "" {
		if run, err := w.db.GetByThread(watch.SlackThread); err == nil && run != nil {
			slog.Debug("tadpole already running for thread, skipping PR check",
				"pr", watch.PRNumber,
				"run_id", run.ID,
				"status", run.Status,
			)
			return nil
		}
	}

	// 1. Check PR state — close the watch if merged/closed
	prState, err := w.getPRState(ctx, watch.PRNumber, watch.RepoPath)
	if err != nil {
		return fmt.Errorf("getting PR state: %w", err)
	}
	if !strings.EqualFold(prState.State, "open") {
		slog.Info("PR closed/merged, stopping watch", "pr", watch.PRNumber, "state", prState.State)
		return w.db.ClosePRWatch(watch.PRNumber)
	}

	// 2. Check review comments (takes priority over CI)
	commentFixSpawned, err := w.checkReviewComments(ctx, watch)
	if err != nil {
		return err
	}

	// 3. If no comment fix was spawned, check CI status
	// Skip CI check if a comment fix was just spawned — the push will restart CI anyway.
	if !commentFixSpawned {
		if _, err := w.checkCI(ctx, watch); err != nil {
			return err
		}
	}

	return nil
}

// checkReviewComments fetches and triages new review comments, spawning a fix tadpole if actionable.
// Returns true if a fix tadpole was spawned.
func (w *Watcher) checkReviewComments(ctx context.Context, watch *state.PRWatch) (bool, error) {
	if watch.FixCount >= w.maxReviewRounds {
		return false, nil
	}

	// Fetch both inline review comments AND conversation comments.
	// GitHub has two separate APIs: pulls/{n}/comments for inline code review
	// comments, and issues/{n}/comments for conversation-tab comments.
	reviewComments, err := w.getReviewComments(ctx, watch.PRNumber, watch.RepoPath)
	if err != nil {
		slog.Warn("failed to get review comments, continuing", "pr", watch.PRNumber, "error", err)
	}
	issueComments, err := w.getIssueComments(ctx, watch.PRNumber, watch.RepoPath)
	if err != nil {
		slog.Warn("failed to get issue comments, continuing", "pr", watch.PRNumber, "error", err)
	}

	// Merge both comment types — IDs are globally unique across GitHub
	var allComments []ghComment
	allComments = append(allComments, reviewComments...)
	allComments = append(allComments, issueComments...)

	// Filter to new comments from humans (not bots)
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

	slog.Debug("PR review check",
		"pr", watch.PRNumber,
		"review_comments", len(reviewComments),
		"issue_comments", len(issueComments),
		"new_human_comments", len(newComments),
	)

	if len(newComments) == 0 {
		return false, nil
	}

	// Update last seen comment ID BEFORE triage to prevent duplicate processing
	// if the next poll fires while triage or the tadpole is running.
	maxID := newComments[len(newComments)-1].ID
	if err := w.db.UpdatePRWatchLastComment(watch.PRNumber, maxID); err != nil {
		return false, fmt.Errorf("updating last comment ID: %w", err)
	}

	// Use Haiku to triage which comments are actionable code feedback
	triage, err := w.triageComments(ctx, watch.PRNumber, newComments)
	if err != nil {
		slog.Warn("failed to triage review comments", "pr", watch.PRNumber, "error", err)
		return false, nil
	}

	if !triage.Actionable {
		slog.Info("review comments triaged as non-actionable",
			"pr", watch.PRNumber,
			"count", len(newComments),
			"reason", triage.Reason,
		)
		return false, nil
	}

	slog.Info("actionable review comments found",
		"pr", watch.PRNumber,
		"count", len(newComments),
		"fix_count", watch.FixCount,
		"summary", triage.Summary,
	)

	task := tadpole.Task{
		Description:    triage.TaskDescription,
		Summary:        fmt.Sprintf("fix review comments on PR #%d", watch.PRNumber),
		Category:       "bug",
		EstSize:        "small",
		SlackChannel:   watch.SlackChannel,
		SlackThreadTS:  watch.SlackThread,
		ExistingBranch: watch.Branch,
		Repo:           config.RepoByPath(w.repos, watch.RepoPath),
	}

	// Notify in Slack
	if w.slack != nil && watch.SlackChannel != "" && watch.SlackThread != "" {
		w.slack.ReplyInThread(watch.SlackChannel, watch.SlackThread,
			fmt.Sprintf(":mag: Review feedback on PR #%d — spawning fix tadpole...\n> %s",
				watch.PRNumber, triage.Summary))
	}

	// Spawn fix tadpole
	if err := w.spawn(ctx, task); err != nil {
		slog.Error("failed to spawn review fix tadpole", "pr", watch.PRNumber, "error", err)
		if w.slack != nil && watch.SlackChannel != "" {
			w.slack.ReplyInThread(watch.SlackChannel, watch.SlackThread,
				fmt.Sprintf(":x: Failed to spawn fix tadpole for PR #%d: %s", watch.PRNumber, err))
		}
		return false, err
	}

	return true, nil
}

// ciStatus represents the aggregate CI status for a PR.
type ciStatus struct {
	State     string   // "pending", "success", "failure"
	FailedIDs []string // GitHub Actions run IDs that failed
}

// ghCheck represents a single check run from `gh pr checks --json`.
type ghCheck struct {
	Name  string `json:"name"`
	State string `json:"state"` // "PENDING", "SUCCESS", "FAILURE", "ERROR", etc.
	Link  string `json:"link"`  // details URL, e.g. GitHub Actions run link
}

// getCIStatus checks the CI status for a PR using `gh pr checks`.
func (w *Watcher) getCIStatus(ctx context.Context, prNumber int, repoPath string) (*ciStatus, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "checks",
		strconv.Itoa(prNumber),
		"--json", "name,state,link",
	)
	cmd.Dir = repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var checks []ghCheck
	if err := json.Unmarshal(stdout.Bytes(), &checks); err != nil {
		return nil, fmt.Errorf("parsing checks: %w", err)
	}

	if len(checks) == 0 {
		return &ciStatus{State: "success"}, nil
	}

	// Determine aggregate state
	hasPending := false
	hasFailure := false
	failedIDs := make(map[string]bool)

	for _, c := range checks {
		state := strings.ToUpper(c.State)

		switch {
		case state == "PENDING" || state == "QUEUED" || state == "IN_PROGRESS":
			hasPending = true
		case state == "FAILURE" || state == "ERROR":
			hasFailure = true
			if id := extractRunID(c.Link); id != "" {
				failedIDs[id] = true
			}
		}
	}

	result := &ciStatus{State: "success"}
	if hasPending {
		result.State = "pending"
	}
	if hasFailure {
		result.State = "failure"
		for id := range failedIDs {
			result.FailedIDs = append(result.FailedIDs, id)
		}
	}
	return result, nil
}

// extractRunID parses a GitHub Actions run ID from a details URL.
// e.g. "https://github.com/owner/repo/actions/runs/12345/job/67890" → "12345"
func extractRunID(detailsURL string) string {
	const marker = "/actions/runs/"
	idx := strings.Index(detailsURL, marker)
	if idx < 0 {
		return ""
	}
	rest := detailsURL[idx+len(marker):]
	// Take everything up to the next "/" or end of string
	if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
		rest = rest[:slashIdx]
	}
	// Validate it looks like a number
	if _, err := strconv.ParseInt(rest, 10, 64); err != nil {
		return ""
	}
	return rest
}

// getCIFailureLogs fetches failed CI logs using `gh run view --log-failed`.
func (w *Watcher) getCIFailureLogs(ctx context.Context, failedRunIDs []string, repoPath string) string {
	const maxPerRun = 8192
	const maxTotal = 15360

	var allLogs strings.Builder
	for _, runID := range failedRunIDs {
		cmd := exec.CommandContext(ctx, "gh", "run", "view", runID, "--log-failed")
		cmd.Dir = repoPath

		output, err := cmd.Output()
		if err != nil {
			slog.Warn("failed to fetch CI logs", "run_id", runID, "error", err)
			continue
		}

		logText := string(output)
		// Keep the tail of logs — that's where failures appear
		if len(logText) > maxPerRun {
			logText = logText[len(logText)-maxPerRun:]
		}

		fmt.Fprintf(&allLogs, "=== Run %s (failed) ===\n%s\n\n", runID, logText)

		if allLogs.Len() >= maxTotal {
			break
		}
	}

	result := allLogs.String()
	if len(result) > maxTotal {
		result = result[len(result)-maxTotal:]
	}
	return result
}

// checkCI checks CI status for a PR and spawns a fix tadpole if CI failed.
// Returns true if a fix tadpole was spawned.
func (w *Watcher) checkCI(ctx context.Context, watch *state.PRWatch) (bool, error) {
	if watch.CIFixCount >= w.maxCIFixRounds {
		slog.Debug("CI fix budget exhausted", "pr", watch.PRNumber, "ci_fix_count", watch.CIFixCount)
		return false, nil
	}

	ci, err := w.getCIStatus(ctx, watch.PRNumber, watch.RepoPath)
	if err != nil {
		slog.Warn("failed to get CI status", "pr", watch.PRNumber, "error", err)
		return false, nil
	}

	switch ci.State {
	case "pending":
		slog.Debug("CI still pending", "pr", watch.PRNumber)
		return false, nil
	case "success":
		slog.Debug("CI passing", "pr", watch.PRNumber)
		return false, nil
	}

	// CI failed — fetch logs and spawn fix
	slog.Info("CI failure detected",
		"pr", watch.PRNumber,
		"failed_runs", len(ci.FailedIDs),
		"ci_fix_count", watch.CIFixCount,
	)

	if err := w.db.IncrementCIFixCount(watch.PRNumber); err != nil {
		return false, fmt.Errorf("incrementing CI fix count: %w", err)
	}

	logs := w.getCIFailureLogs(ctx, ci.FailedIDs, watch.RepoPath)

	var taskDesc strings.Builder
	fmt.Fprintf(&taskDesc, "Fix CI failures on PR #%d.\n\n", watch.PRNumber)
	if logs != "" {
		taskDesc.WriteString("The CI pipeline is failing. Fix the issues shown in the logs below.\n\n")
		taskDesc.WriteString("CI failure logs:\n\n```\n")
		taskDesc.WriteString(logs)
		taskDesc.WriteString("\n```\n")
	} else {
		taskDesc.WriteString("The CI pipeline is failing but logs could not be retrieved. ")
		taskDesc.WriteString("Run the test and lint commands locally, identify what's broken, and fix it.\n")
	}

	task := tadpole.Task{
		Description:    taskDesc.String(),
		Summary:        fmt.Sprintf("fix CI failures on PR #%d", watch.PRNumber),
		Category:       "bug",
		EstSize:        "small",
		SlackChannel:   watch.SlackChannel,
		SlackThreadTS:  watch.SlackThread,
		ExistingBranch: watch.Branch,
		Repo:           config.RepoByPath(w.repos, watch.RepoPath),
	}

	// Notify in Slack
	if w.slack != nil && watch.SlackChannel != "" && watch.SlackThread != "" {
		w.slack.ReplyInThread(watch.SlackChannel, watch.SlackThread,
			fmt.Sprintf(":rotating_light: CI failing on PR #%d (attempt %d/%d) — spawning fix tadpole...",
				watch.PRNumber, watch.CIFixCount+1, w.maxCIFixRounds))
	}

	if err := w.spawn(ctx, task); err != nil {
		slog.Error("failed to spawn CI fix tadpole", "pr", watch.PRNumber, "error", err)
		if w.slack != nil && watch.SlackChannel != "" {
			w.slack.ReplyInThread(watch.SlackChannel, watch.SlackThread,
				fmt.Sprintf(":x: Failed to spawn CI fix tadpole for PR #%d: %s", watch.PRNumber, err))
		}
		return false, err
	}

	return true, nil
}

// commentTriage is the result of Haiku evaluating review comments.
type commentTriage struct {
	Actionable      bool   `json:"actionable"`
	Summary         string `json:"summary"`
	Reason          string `json:"reason"`
	TaskDescription string `json:"-"` // built after triage
}

const commentTriagePrompt = `You are triaging PR review comments to decide if they require code changes.

The comments below are from PR #%d. Evaluate whether ANY of them contain actionable code feedback — requests for changes, bug reports, suggestions for improvement, or specific issues to fix.

<comments>
%s
</comments>

Your response MUST be ONLY a JSON object — no prose, no markdown fences:
{"actionable": true, "summary": "one-line summary of what needs to change", "reason": "why this is/isn't actionable"}

Rules:
- actionable=true if ANY comment requests a code change, points out a bug, or suggests an improvement
- actionable=false for approvals (LGTM, looks good), acknowledgments (thanks, nice), merge notices, questions that don't require code changes, or general discussion
- The summary should describe what code changes are needed (only if actionable)
- Be conservative — when in doubt, it's actionable`

func (w *Watcher) triageComments(ctx context.Context, prNumber int, comments []ghComment) (*commentTriage, error) {
	// Format comments for the prompt
	var sb strings.Builder
	for i, c := range comments {
		if c.Path != "" {
			fmt.Fprintf(&sb, "[%d] File: %s\n", i, c.Path)
		} else {
			fmt.Fprintf(&sb, "[%d] (general comment)\n", i)
		}
		fmt.Fprintf(&sb, "    @%s: %s\n\n", c.User.Login, c.Body)
	}

	prompt := fmt.Sprintf(commentTriagePrompt, prNumber, sb.String())

	triageCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{
		"--print",
		"--max-turns", "1",
		"--output-format", "json",
		"--model", w.triageModel,
		"-p", prompt,
	}

	cmd := exec.CommandContext(triageCtx, "claude", args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("claude triage call failed: %w", err)
	}

	// Parse the JSON response — handle --output-format json wrapper
	var result commentTriage
	text := extractResultText(output)
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return nil, fmt.Errorf("parsing triage response: %w (raw: %s)", err, string(output))
	}

	// Build the task description from the original comments if actionable
	if result.Actionable {
		var task strings.Builder
		fmt.Fprintf(&task, "Fix review comments on PR #%d.\n\n", prNumber)
		task.WriteString("Review comments to address:\n\n")
		for _, c := range comments {
			if c.Path != "" {
				fmt.Fprintf(&task, "File: %s\n", c.Path)
			}
			fmt.Fprintf(&task, "@%s: %s\n\n", c.User.Login, c.Body)
		}
		result.TaskDescription = task.String()
	}

	return &result, nil
}

// extractResultText extracts the text content from claude --output-format json response.
func extractResultText(output []byte) string {
	var resp struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(output, &resp); err == nil && resp.Result != "" {
		return resp.Result
	}
	return strings.TrimSpace(string(output))
}

func (w *Watcher) getReviewComments(ctx context.Context, prNumber int, repoPath string) ([]ghComment, error) {
	cmd := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", prNumber),
		"--jq", ".",
	)
	cmd.Dir = repoPath

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


func (w *Watcher) getIssueComments(ctx context.Context, prNumber int, repoPath string) ([]ghComment, error) {
	cmd := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", prNumber),
		"--jq", ".",
	)
	cmd.Dir = repoPath

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

func (w *Watcher) getPRState(ctx context.Context, prNumber int, repoPath string) (*ghPR, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view",
		strconv.Itoa(prNumber),
		"--json", "state",
	)
	cmd.Dir = repoPath

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
