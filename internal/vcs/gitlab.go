package vcs

import (
	"context"
	"fmt"
	"os/exec"
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

// GetSuggestedReviewers is not implemented for GitLab and always returns nil.
func (g *GitLabProvider) GetSuggestedReviewers(_ context.Context, _ string, _ []string, _ map[string]bool, _ int) []string {
	return nil
}
