package vcs

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

func (g *GitHubProvider) CreatePR(ctx context.Context, opts CreatePROpts) (string, error) {
	args := []string{"pr", "create",
		"--title", opts.Title,
		"--body", opts.Body,
		"--head", opts.Branch,
	}
	for _, label := range opts.Labels {
		args = append(args, "--label", label)
	}

	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = opts.RepoPath
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (g *GitHubProvider) EnableAutoMerge(ctx context.Context, repoPath, branch string) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "merge", "--auto", "--squash", branch)
	cmd.Dir = repoPath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr merge --auto: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (g *GitHubProvider) GetPRState(ctx context.Context, prNumber int, repoPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view",
		strconv.Itoa(prNumber),
		"--json", "state",
	)
	cmd.Dir = repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var pr struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &pr); err != nil {
		return "", fmt.Errorf("parsing PR state: %w", err)
	}
	return pr.State, nil
}

// ghCheck represents a single check run from `gh pr checks --json`.
type ghCheck struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Link  string `json:"link"`
}

func (g *GitHubProvider) GetCIStatus(ctx context.Context, prNumber int, repoPath string) (*CIStatus, error) {
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
		return &CIStatus{State: "success"}, nil
	}

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
			if id := g.ExtractRunID(c.Link); id != "" {
				failedIDs[id] = true
			}
		}
	}

	result := &CIStatus{State: "success"}
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

func (g *GitHubProvider) GetCIFailureLogs(ctx context.Context, failedRunIDs []string, repoPath string) string {
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

// ghComment represents a comment from the GitHub API.
type ghComment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
	Path string `json:"path"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	CreatedAt string `json:"created_at"`
}

func (g *GitHubProvider) GetPRComments(ctx context.Context, prNumber int, repoPath string) ([]PRComment, error) {
	// Fetch inline review comments
	reviewComments, reviewErr := g.fetchComments(ctx,
		fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", prNumber), repoPath)
	if reviewErr != nil {
		slog.Warn("failed to get review comments", "pr", prNumber, "error", reviewErr)
	}

	// Fetch conversation comments
	issueComments, issueErr := g.fetchComments(ctx,
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", prNumber), repoPath)
	if issueErr != nil {
		slog.Warn("failed to get issue comments", "pr", prNumber, "error", issueErr)
	}

	// If both fetches failed, return error so caller knows we got nothing
	if reviewErr != nil && issueErr != nil {
		return nil, fmt.Errorf("failed to fetch PR comments: reviews: %w; issues: %v", reviewErr, issueErr)
	}

	var all []PRComment
	for _, c := range reviewComments {
		all = append(all, ghCommentToPRComment(c))
	}
	for _, c := range issueComments {
		all = append(all, ghCommentToPRComment(c))
	}
	return all, nil
}

func (g *GitHubProvider) fetchComments(ctx context.Context, endpoint, repoPath string) ([]ghComment, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", endpoint, "--jq", ".")
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

func ghCommentToPRComment(c ghComment) PRComment {
	t, _ := time.Parse(time.RFC3339, c.CreatedAt)
	return PRComment{
		ID:        c.ID,
		Body:      c.Body,
		Path:      c.Path,
		UserLogin: c.User.Login,
		UserType:  c.User.Type,
		CreatedAt: t,
	}
}

// ghBotPR represents a PR from `gh pr list --json`.
type ghBotPR struct {
	Number int `json:"number"`
	Author struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"author"`
}

func (g *GitHubProvider) ListBotPRs(ctx context.Context, branch, repoPath string) ([]int, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--base", branch,
		"--state", "open",
		"--json", "number,author",
	)
	cmd.Dir = repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var prs []ghBotPR
	if err := json.Unmarshal(stdout.Bytes(), &prs); err != nil {
		return nil, fmt.Errorf("parsing PR list: %w", err)
	}

	var result []int
	for _, pr := range prs {
		if strings.EqualFold(pr.Author.Type, "Bot") {
			result = append(result, pr.Number)
		}
	}
	return result, nil
}

func (g *GitHubProvider) MergePR(ctx context.Context, prNumber int, repoPath string) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "merge",
		strconv.Itoa(prNumber), "--squash", "--delete-branch")
	cmd.Dir = repoPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr merge: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (g *GitHubProvider) ExtractPRNumber(prURL string) (int, error) {
	parts := strings.Split(strings.TrimRight(prURL, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "pull" {
		return 0, fmt.Errorf("not a PR URL: %s", prURL)
	}
	return strconv.Atoi(parts[len(parts)-1])
}

func (g *GitHubProvider) ExtractRunID(detailsURL string) string {
	const marker = "/actions/runs/"
	idx := strings.Index(detailsURL, marker)
	if idx < 0 {
		return ""
	}
	rest := detailsURL[idx+len(marker):]
	if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
		rest = rest[:slashIdx]
	}
	if _, err := strconv.ParseInt(rest, 10, 64); err != nil {
		return ""
	}
	return rest
}
