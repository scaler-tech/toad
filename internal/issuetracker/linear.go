package issuetracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/hergen/toad/internal/config"
)

// Linear URL pattern: https://linear.app/<team>/issue/PLF-3125/optional-slug
var linearURLRe = regexp.MustCompile(`https://linear\.app/[^/]+/issue/([A-Z]+-\d+)`)

// Bare issue ID pattern: PLF-3125 (2-5 uppercase letters, dash, digits).
var bareIDRe = regexp.MustCompile(`\b([A-Z]{2,5}-\d+)\b`)

// uuidRe matches a standard UUID format.
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// commonAcronyms that match bareIDRe but are not issue IDs.
var commonAcronyms = map[string]bool{
	"HTTP": true, "HTTPS": true, "UTF": true, "SHA": true,
	"TCP": true, "UDP": true, "ISO": true, "RFC": true,
	"SSL": true, "TLS": true, "SSH": true, "DNS": true,
	"API": true, "URL": true, "URI": true, "XML": true,
	"JSON": true, "YAML": true, "HTML": true, "CSS": true,
	"AWS": true, "GCP": true, "CPU": true, "GPU": true,
}

// LinearTracker implements the Tracker interface for Linear.
type LinearTracker struct {
	apiToken       string
	teamID         string
	bugLabelID     string
	featureLabelID string
	createIssues   bool
	httpClient     *http.Client
}

// NewLinearTracker creates a Linear tracker from config.
func NewLinearTracker(cfg config.IssueTrackerConfig) *LinearTracker {
	return &LinearTracker{
		apiToken:       cfg.APIToken,
		teamID:         cfg.TeamID,
		bugLabelID:     cfg.BugLabelID,
		featureLabelID: cfg.FeatureLabelID,
		createIssues:   cfg.CreateIssues,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}
}

// ShouldCreateIssues reports whether auto-creation is enabled.
func (lt *LinearTracker) ShouldCreateIssues() bool {
	return lt.createIssues
}

// ExtractIssueRef extracts a Linear issue reference from message text.
// Tries URL match first, then falls back to bare ID.
func (lt *LinearTracker) ExtractIssueRef(text string) *IssueRef {
	// Try URL match first — more specific and includes the full URL
	if m := linearURLRe.FindStringSubmatch(text); len(m) > 1 {
		url := linearURLRe.FindString(text)
		return &IssueRef{
			Provider: "linear",
			ID:       m[1],
			URL:      url,
		}
	}

	// Fall back to bare ID, filtering out common acronyms like HTTP-200
	for _, match := range bareIDRe.FindAllStringSubmatch(text, -1) {
		if len(match) < 2 {
			continue
		}
		id := match[1]
		// Extract the alphabetic prefix (everything before the dash)
		prefix := id
		for i, ch := range id {
			if ch == '-' {
				prefix = id[:i]
				break
			}
		}
		if commonAcronyms[prefix] {
			continue
		}
		return &IssueRef{
			Provider: "linear",
			ID:       id,
		}
	}

	return nil
}

// resolveTeamID resolves a team key (e.g. "PLF") to its UUID via the Linear API.
// If teamID is already a UUID, this is a no-op.
func (lt *LinearTracker) resolveTeamID(ctx context.Context) error {
	if uuidRe.MatchString(lt.teamID) {
		return nil
	}

	slog.Info("resolving Linear team key to UUID", "key", lt.teamID)

	query := `{ teams { nodes { id key } } }`
	payload, _ := json.Marshal(map[string]any{"query": query})

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.linear.app/graphql", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", lt.apiToken)

	resp, err := lt.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("linear API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	var result struct {
		Data struct {
			Teams struct {
				Nodes []struct {
					ID  string `json:"id"`
					Key string `json:"key"`
				} `json:"nodes"`
			} `json:"teams"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("parsing teams response: %w", err)
	}

	for _, team := range result.Data.Teams.Nodes {
		if team.Key == lt.teamID {
			slog.Info("resolved Linear team", "key", lt.teamID, "uuid", team.ID)
			lt.teamID = team.ID
			return nil
		}
	}

	return fmt.Errorf("linear team key %q not found", lt.teamID)
}

// CreateIssue creates a new Linear issue via the GraphQL API.
func (lt *LinearTracker) CreateIssue(ctx context.Context, opts CreateIssueOpts) (*IssueRef, error) {
	if lt.apiToken == "" {
		return nil, fmt.Errorf("linear API token not configured")
	}
	if lt.teamID == "" {
		return nil, fmt.Errorf("linear team ID not configured")
	}

	// Resolve team key to UUID on first call (e.g. "PLF" → "4246aba1-...")
	if err := lt.resolveTeamID(ctx); err != nil {
		return nil, fmt.Errorf("resolving team ID: %w", err)
	}

	// Build label IDs based on category
	var labelIDs []string
	switch opts.Category {
	case "bug":
		if lt.bugLabelID != "" {
			labelIDs = append(labelIDs, lt.bugLabelID)
		}
	case "feature":
		if lt.featureLabelID != "" {
			labelIDs = append(labelIDs, lt.featureLabelID)
		}
	}

	// Build the GraphQL mutation
	variables := map[string]any{
		"title":       opts.Title,
		"description": opts.Description,
		"teamId":      lt.teamID,
	}
	if len(labelIDs) > 0 {
		variables["labelIds"] = labelIDs
	}

	query := `mutation IssueCreate($title: String!, $description: String, $teamId: String!, $labelIds: [String!]) {
		issueCreate(input: { title: $title, description: $description, teamId: $teamId, labelIds: $labelIds }) {
			success
			issue {
				identifier
				url
				title
			}
		}
	}`

	payload := map[string]any{
		"query":     query,
		"variables": variables,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.linear.app/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", lt.apiToken)

	resp, err := lt.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("linear API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linear API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var gqlResp struct {
		Data struct {
			IssueCreate struct {
				Success bool `json:"success"`
				Issue   struct {
					Identifier string `json:"identifier"`
					URL        string `json:"url"`
					Title      string `json:"title"`
				} `json:"issue"`
			} `json:"issueCreate"`
		} `json:"data"`
		Errors []struct {
			Message    string         `json:"message"`
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		e := gqlResp.Errors[0]
		if len(e.Extensions) > 0 {
			extJSON, _ := json.Marshal(e.Extensions)
			return nil, fmt.Errorf("linear API error: %s (details: %s)", e.Message, string(extJSON))
		}
		return nil, fmt.Errorf("linear API error: %s (response: %s)", e.Message, string(respBody))
	}

	if !gqlResp.Data.IssueCreate.Success {
		return nil, fmt.Errorf("linear issue creation failed")
	}

	issue := gqlResp.Data.IssueCreate.Issue
	return &IssueRef{
		Provider: "linear",
		ID:       issue.Identifier,
		URL:      issue.URL,
		Title:    issue.Title,
	}, nil
}
