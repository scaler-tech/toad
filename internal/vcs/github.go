package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
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

func (g *GitHubProvider) GetMergeability(ctx context.Context, prNumber int, repoPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view",
		strconv.Itoa(prNumber),
		"--json", "mergeable",
	)
	cmd.Dir = repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var pr struct {
		Mergeable string `json:"mergeable"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &pr); err != nil {
		return "", fmt.Errorf("parsing mergeable status: %w", err)
	}

	switch strings.ToUpper(pr.Mergeable) {
	case "MERGEABLE":
		return "MERGEABLE", nil
	case "CONFLICTING":
		return "CONFLICTING", nil
	default:
		return "UNKNOWN", nil
	}
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
		if strings.Contains(strings.ToLower(c.Name), "approval gate") {
			continue
		}
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

// ghReview represents a pull request review from the GitHub API.
type ghReview struct {
	ID    int    `json:"id"`
	Body  string `json:"body"`
	State string `json:"state"`
	User  struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	SubmittedAt string `json:"submitted_at"`
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

	// Fetch PR review submissions (captures "changes requested" reviews with body text)
	prReviews, prReviewErr := g.fetchReviews(ctx, prNumber, repoPath)
	if prReviewErr != nil {
		slog.Warn("failed to get PR reviews", "pr", prNumber, "error", prReviewErr)
	}

	// If all fetches failed, return error so caller knows we got nothing
	if reviewErr != nil && issueErr != nil && prReviewErr != nil {
		return nil, fmt.Errorf("failed to fetch PR comments: reviews: %w; issues: %w; pr_reviews: %w", reviewErr, issueErr, prReviewErr)
	}

	var all []PRComment
	for _, c := range reviewComments {
		pc := ghCommentToPRComment(c)
		pc.Source = "review"
		all = append(all, pc)
	}
	for _, c := range issueComments {
		pc := ghCommentToPRComment(c)
		pc.Source = "issue"
		all = append(all, pc)
	}
	all = append(all, prReviews...)
	return all, nil
}

// fetchReviews fetches PR review submissions and returns actionable ones as PRComments.
// Only includes reviews with state CHANGES_REQUESTED or COMMENTED that have non-empty bodies.
func (g *GitHubProvider) fetchReviews(ctx context.Context, prNumber int, repoPath string) ([]PRComment, error) {
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/reviews", prNumber)
	cmd := exec.CommandContext(ctx, "gh", "api", endpoint, "--jq", ".")
	cmd.Dir = repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var reviews []ghReview
	if err := json.Unmarshal(stdout.Bytes(), &reviews); err != nil {
		return nil, fmt.Errorf("parsing reviews: %w", err)
	}

	var comments []PRComment
	for _, r := range reviews {
		body := strings.TrimSpace(r.Body)
		if body == "" {
			continue
		}
		state := strings.ToUpper(r.State)
		if state != "CHANGES_REQUESTED" && state != "COMMENTED" {
			continue
		}
		t, _ := time.Parse(time.RFC3339, r.SubmittedAt)
		comments = append(comments, PRComment{
			ID:        r.ID,
			Body:      body,
			Source:    "pr_review",
			UserLogin: r.User.Login,
			UserType:  r.User.Type,
			CreatedAt: t,
		})
	}
	return comments, nil
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

func (g *GitHubProvider) PostPRComment(ctx context.Context, prNumber int, body, repoPath string) error {
	cmd := exec.CommandContext(ctx, "gh", "api", "--method", "POST",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", prNumber),
		"-f", "body="+body)
	cmd.Dir = repoPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("posting PR comment: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (g *GitHubProvider) AddCommentReaction(ctx context.Context, prNumber, commentID int, source, reaction, repoPath string) error {
	// GitHub has separate reaction endpoints for review comments vs issue comments.
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/issues/comments/%d/reactions", commentID)
	if source == "review" {
		endpoint = fmt.Sprintf("repos/{owner}/{repo}/pulls/comments/%d/reactions", commentID)
	}

	cmd := exec.CommandContext(ctx, "gh", "api", "--method", "POST",
		endpoint, "-f", "content="+reaction, "--silent")
	cmd.Dir = repoPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
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

func (g *GitHubProvider) PRNoun() string { return "PR" }

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
