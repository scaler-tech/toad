package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GitLabProvider implements Provider using the glab CLI.
type GitLabProvider struct {
	Host         string   // optional, for self-hosted (sets GITLAB_HOST env)
	BotUsernames []string // usernames to treat as bots (fallback for older GitLab)
}

func (g *GitLabProvider) Check() error {
	_, err := exec.LookPath("glab")
	if err != nil {
		return fmt.Errorf("glab CLI not found in PATH — install it first: https://gitlab.com/gitlab-org/cli")
	}
	return nil
}

// glabCmd builds an exec.CommandContext for the glab CLI, setting the working
// directory and injecting GITLAB_HOST for self-hosted instances.
func (g *GitLabProvider) glabCmd(ctx context.Context, repoPath string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "glab", args...)
	cmd.Dir = repoPath
	if g.Host != "" {
		cmd.Env = append(os.Environ(), "GITLAB_HOST="+g.Host)
	}
	return cmd
}

func (g *GitLabProvider) CreatePR(ctx context.Context, opts CreatePROpts) (string, error) {
	// No --target-branch: glab defaults to the repo's default branch,
	// which matches tadpole behaviour (worktrees branch off default_branch).
	args := []string{"mr", "create",
		"--title", opts.Title,
		"--description", opts.Body,
		"--source-branch", opts.Branch,
		"--yes",
	}
	for _, label := range opts.Labels {
		args = append(args, "--label", label)
	}

	cmd := g.glabCmd(ctx, opts.RepoPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("glab mr create: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	// glab mr create prints the MR URL on the last non-empty line
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.Contains(line, "/merge_requests/") {
			return line, nil
		}
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (g *GitLabProvider) EnableAutoMerge(ctx context.Context, repoPath, branch string) error {
	cmd := g.glabCmd(ctx, repoPath,
		"mr", "merge", branch,
		"--when-pipeline-succeeds",
		"--squash",
		"--remove-source-branch",
		"--yes",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("glab mr merge --when-pipeline-succeeds: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (g *GitLabProvider) GetPRState(ctx context.Context, prNumber int, repoPath string) (string, error) {
	cmd := g.glabCmd(ctx, repoPath,
		"mr", "view", strconv.Itoa(prNumber), "--output", "json",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var mr struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &mr); err != nil {
		return "", fmt.Errorf("parsing MR state: %w", err)
	}
	return mapGitLabState(mr.State), nil
}

// mapGitLabState maps GitLab MR states to the canonical states used by the Provider interface.
func mapGitLabState(state string) string {
	switch strings.ToLower(state) {
	case "opened":
		return "OPEN"
	case "closed":
		return "CLOSED"
	case "merged":
		return "MERGED"
	case "locked":
		return "CLOSED"
	default:
		return strings.ToUpper(state)
	}
}

// glabPipeline represents a pipeline from the GitLab API.
type glabPipeline struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
}

// glabJob represents a CI job from the GitLab API.
type glabJob struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (g *GitLabProvider) GetCIStatus(ctx context.Context, prNumber int, repoPath string) (*CIStatus, error) {
	// Step 1: Get the head pipeline for this MR
	mrCmd := g.glabCmd(ctx, repoPath,
		"api", fmt.Sprintf("projects/:fullpath/merge_requests/%d", prNumber),
		"--jq", ".head_pipeline",
	)

	var mrStdout, mrStderr bytes.Buffer
	mrCmd.Stdout = &mrStdout
	mrCmd.Stderr = &mrStderr

	if err := mrCmd.Run(); err != nil {
		return nil, fmt.Errorf("fetching MR pipeline: %w: %s", err, strings.TrimSpace(mrStderr.String()))
	}

	var pipeline glabPipeline
	if err := json.Unmarshal(mrStdout.Bytes(), &pipeline); err != nil {
		// No pipeline or null — treat as success (no CI configured)
		return &CIStatus{State: "success"}, nil
	}
	if pipeline.ID == 0 {
		return &CIStatus{State: "success"}, nil
	}

	// Step 2: Get jobs for this pipeline
	jobsCmd := g.glabCmd(ctx, repoPath,
		"api", fmt.Sprintf("projects/:fullpath/pipelines/%d/jobs", pipeline.ID),
	)

	var jobsStdout, jobsStderr bytes.Buffer
	jobsCmd.Stdout = &jobsStdout
	jobsCmd.Stderr = &jobsStderr

	if err := jobsCmd.Run(); err != nil {
		return nil, fmt.Errorf("fetching pipeline jobs: %w: %s", err, strings.TrimSpace(jobsStderr.String()))
	}

	var jobs []glabJob
	if err := json.Unmarshal(jobsStdout.Bytes(), &jobs); err != nil {
		return nil, fmt.Errorf("parsing pipeline jobs: %w", err)
	}

	if len(jobs) == 0 {
		return &CIStatus{State: "success"}, nil
	}

	hasPending := false
	hasFailure := false
	var failedIDs []string

	for _, j := range jobs {
		switch strings.ToLower(j.Status) {
		case "pending", "created", "waiting_for_resource", "preparing", "running":
			hasPending = true
		case "failed":
			hasFailure = true
			failedIDs = append(failedIDs, strconv.Itoa(j.ID))
		}
	}

	result := &CIStatus{State: "success"}
	if hasPending {
		result.State = "pending"
	}
	if hasFailure {
		result.State = "failure"
		sort.Strings(failedIDs)
		result.FailedIDs = failedIDs
	}
	return result, nil
}

func (g *GitLabProvider) GetCIFailureLogs(ctx context.Context, failedRunIDs []string, repoPath string) string {
	const maxPerRun = 8192
	const maxTotal = 15360

	var allLogs strings.Builder
	for _, jobID := range failedRunIDs {
		cmd := g.glabCmd(ctx, repoPath, "ci", "trace", jobID)

		output, err := cmd.Output()
		if err != nil {
			slog.Warn("failed to fetch CI logs", "job_id", jobID, "error", err)
			continue
		}

		logText := string(output)
		if len(logText) > maxPerRun {
			logText = logText[len(logText)-maxPerRun:]
		}

		fmt.Fprintf(&allLogs, "=== Job %s (failed) ===\n%s\n\n", jobID, logText)

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

// glabNote represents a note (comment) from the GitLab API.
type glabNote struct {
	ID        int    `json:"id"`
	Body      string `json:"body"`
	System    bool   `json:"system"`
	Author    struct {
		Username string `json:"username"`
		Bot      bool   `json:"bot"`      // GitLab 16.8+
	} `json:"author"`
	Position *struct {
		NewPath string `json:"new_path"`
	} `json:"position"`
	CreatedAt string `json:"created_at"`
}

func (g *GitLabProvider) GetPRComments(ctx context.Context, prNumber int, repoPath string) ([]PRComment, error) {
	cmd := g.glabCmd(ctx, repoPath,
		"api", fmt.Sprintf("projects/:fullpath/merge_requests/%d/notes", prNumber),
		"--paginate",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("fetching MR notes: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var notes []glabNote
	if err := json.Unmarshal(stdout.Bytes(), &notes); err != nil {
		return nil, fmt.Errorf("parsing MR notes: %w", err)
	}

	var comments []PRComment
	for _, n := range notes {
		if n.System {
			continue
		}

		path := ""
		if n.Position != nil {
			path = n.Position.NewPath
		}

		userType := "User"
		if n.Author.Bot || g.isBotUsername(n.Author.Username) {
			userType = "Bot"
		}

		t, _ := time.Parse(time.RFC3339, n.CreatedAt)
		comments = append(comments, PRComment{
			ID:        n.ID,
			Body:      n.Body,
			Path:      path,
			UserLogin: n.Author.Username,
			UserType:  userType,
			CreatedAt: t,
		})
	}
	return comments, nil
}

// isBotUsername checks if a username matches any of the configured bot usernames.
func (g *GitLabProvider) isBotUsername(username string) bool {
	for _, bot := range g.BotUsernames {
		if strings.EqualFold(bot, username) {
			return true
		}
	}
	return false
}

// glabMR represents a merge request from the GitLab API.
type glabMR struct {
	IID    int `json:"iid"`
	Author struct {
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"author"`
}

func (g *GitLabProvider) ListBotPRs(ctx context.Context, branch, repoPath string) ([]int, error) {
	cmd := g.glabCmd(ctx, repoPath,
		"api", fmt.Sprintf("projects/:fullpath/merge_requests?state=opened&target_branch=%s", url.QueryEscape(branch)),
		"--paginate",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("listing MRs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var mrs []glabMR
	if err := json.Unmarshal(stdout.Bytes(), &mrs); err != nil {
		return nil, fmt.Errorf("parsing MR list: %w", err)
	}

	var result []int
	for _, mr := range mrs {
		if mr.Author.Bot || g.isBotUsername(mr.Author.Username) {
			result = append(result, mr.IID)
		}
	}
	return result, nil
}

func (g *GitLabProvider) MergePR(ctx context.Context, prNumber int, repoPath string) error {
	cmd := g.glabCmd(ctx, repoPath,
		"mr", "merge", strconv.Itoa(prNumber),
		"--squash",
		"--remove-source-branch",
		"--yes",
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("glab mr merge: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (g *GitLabProvider) ExtractPRNumber(prURL string) (int, error) {
	parts := strings.Split(strings.TrimRight(prURL, "/"), "/")
	// Find "merge_requests" in the path and take the next segment
	for i, part := range parts {
		if part == "merge_requests" && i+1 < len(parts) {
			return strconv.Atoi(parts[i+1])
		}
	}
	return 0, fmt.Errorf("not a merge request URL: %s", prURL)
}

func (g *GitLabProvider) PRNoun() string { return "MR" }

func (g *GitLabProvider) ExtractRunID(detailsURL string) string {
	const marker = "/-/jobs/"
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
