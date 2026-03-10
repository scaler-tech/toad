package slack

import (
	"log/slog"
	"strings"

	goslack "github.com/slack-go/slack"

	"github.com/scaler-tech/toad/internal/state"
)

// slackUser is a minimal representation for fuzzy matching.
type slackUser struct {
	ID          string
	DisplayName string
	RealName    string
}

// fuzzyMatchSlackUser tries to match a GitHub login to a Slack user.
// Returns the Slack user ID if exactly one match is found, empty string otherwise.
func fuzzyMatchSlackUser(githubLogin string, users []slackUser) string {
	login := strings.ToLower(githubLogin)
	var matches []string

	for _, u := range users {
		display := strings.ToLower(u.DisplayName)
		real := strings.ToLower(u.RealName)

		// Exact match on display name or real name
		if display == login || real == login {
			matches = append(matches, u.ID)
			continue
		}

		// Name component match: concatenate first+last parts and compare
		parts := strings.Fields(real)
		if len(parts) >= 2 {
			concat := strings.Join(parts, "")
			if concat == login {
				matches = append(matches, u.ID)
				continue
			}
			// Also check if login matches first or last name exactly
			for _, part := range parts {
				if part == login {
					matches = append(matches, u.ID)
					break
				}
			}
		}
	}

	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// ResolveGitHubToSlack resolves a list of GitHub logins to Slack user IDs.
// Uses DB mappings first, then fuzzy matching against workspace users.
// Returns a map of github_login -> slack_user_id for resolved users only.
func ResolveGitHubToSlack(db *state.DB, api *goslack.Client, logins []string) map[string]string {
	result := make(map[string]string)
	var unresolved []string

	// Phase 1: DB lookup
	for _, login := range logins {
		slackID, err := db.LookupSlackByGitHub(login)
		if err != nil {
			slog.Debug("github-slack lookup failed", "login", login, "error", err)
			continue
		}
		if slackID != "" {
			result[login] = slackID
		} else {
			unresolved = append(unresolved, login)
		}
	}

	if len(unresolved) == 0 {
		return result
	}

	// Phase 2: Fuzzy match against workspace users
	slackUsers, err := api.GetUsers()
	if err != nil {
		slog.Warn("failed to fetch Slack users for fuzzy match", "error", err)
		return result
	}

	users := make([]slackUser, 0, len(slackUsers))
	for _, u := range slackUsers {
		if u.Deleted || u.IsBot {
			continue
		}
		users = append(users, slackUser{
			ID:          u.ID,
			DisplayName: u.Profile.DisplayName,
			RealName:    u.RealName,
		})
	}

	for _, login := range unresolved {
		if slackID := fuzzyMatchSlackUser(login, users); slackID != "" {
			result[login] = slackID
		}
	}

	return result
}
