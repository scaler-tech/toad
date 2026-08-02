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
	"strings"
	"sync"
	"time"

	"github.com/scaler-tech/toad/internal/config"
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

	// teamIDMu guards team-key-to-UUID resolution. It's held across the
	// resolution call itself, so concurrent first-callers serialize rather
	// than each firing their own "teams" query. Only a *successful*
	// resolution is cached (in resolvedTeamID) — a failed attempt caches
	// nothing, so the next CreateIssue call retries instead of being
	// permanently wedged by one transient error. teamID itself is never
	// mutated after construction (so it's safe to read unsynchronized,
	// e.g. for the "configured?" check in CreateIssue); resolvedTeamID is
	// only ever read/written under teamIDMu.
	teamIDMu       sync.Mutex
	resolvedTeamID string

	// resolvedTeams / resolvedProjects cache successful override
	// resolutions for the tracker's lifetime, under teamIDMu with the same
	// success-only policy as resolvedTeamID: failures are never cached, so
	// one transient error doesn't wedge later attempts. Keys: resolvedTeams
	// by lower-cased team key/name; resolvedProjects by teamID + "\x00" +
	// lower-cased project name.
	resolvedTeams    map[string]string
	resolvedProjects map[string]string
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

// ExtractIssueRef extracts the first Linear issue reference from message text.
// Tries URL match first, then falls back to bare ID.
func (lt *LinearTracker) ExtractIssueRef(text string) *IssueRef {
	refs := lt.ExtractAllIssueRefs(text)
	if len(refs) == 0 {
		return nil
	}
	return refs[0]
}

// ExtractAllIssueRefs extracts all Linear issue references from message text.
// URL matches take priority and appear first, followed by bare IDs.
func (lt *LinearTracker) ExtractAllIssueRefs(text string) []*IssueRef {
	var refs []*IssueRef
	seen := map[string]bool{}

	// URL matches first — more specific and include the full URL
	for _, m := range linearURLRe.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		refs = append(refs, &IssueRef{
			Provider: "linear",
			ID:       m[1],
			URL:      m[0],
		})
	}

	// Bare IDs, filtering out common acronyms and already-seen URL matches
	for _, match := range bareIDRe.FindAllStringSubmatch(text, -1) {
		if len(match) < 2 {
			continue
		}
		id := match[1]
		if seen[id] {
			continue
		}
		prefix := issuePrefix(id)
		if commonAcronyms[prefix] {
			continue
		}
		seen[id] = true
		refs = append(refs, &IssueRef{
			Provider: "linear",
			ID:       id,
		})
	}

	return refs
}

// issuePrefix extracts the alphabetic prefix from an issue ID (e.g. "PLF" from "PLF-3198").
func issuePrefix(id string) string {
	for i, ch := range id {
		if ch == '-' {
			return id[:i]
		}
	}
	return id
}

// GetIssueDetails fetches the title and description of a Linear issue.
func (lt *LinearTracker) GetIssueDetails(ctx context.Context, ref *IssueRef) (*IssueDetails, error) {
	if lt.apiToken == "" {
		return nil, nil
	}

	teamKey, number, err := parseIssueIdentifier(ref.ID)
	if err != nil {
		return nil, err
	}

	query := `query IssueDetails($filter: IssueFilter!) {
		issues(filter: $filter, first: 1) {
			nodes {
				id
				identifier
				title
				description
				url
				comments(first: 20, orderBy: createdAt) {
					nodes {
						body
						user { name }
					}
				}
			}
		}
	}`

	variables := map[string]any{
		"filter": map[string]any{
			"number": map[string]any{"eq": number},
			"team":   map[string]any{"key": map[string]any{"eq": teamKey}},
		},
	}

	data, err := lt.doGraphQL(ctx, query, variables)
	if err != nil {
		return nil, fmt.Errorf("fetching issue details: %w", err)
	}

	var result struct {
		Issues struct {
			Nodes []struct {
				ID          string `json:"id"`
				Identifier  string `json:"identifier"`
				Title       string `json:"title"`
				Description string `json:"description"`
				URL         string `json:"url"`
				Comments    struct {
					Nodes []struct {
						Body string `json:"body"`
						User struct {
							Name string `json:"name"`
						} `json:"user"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parsing issue details: %w", err)
	}

	if len(result.Issues.Nodes) == 0 {
		return nil, nil
	}

	node := result.Issues.Nodes[0]
	var comments []IssueComment
	for _, c := range node.Comments.Nodes {
		if c.Body != "" {
			comments = append(comments, IssueComment{
				Author: c.User.Name,
				Body:   c.Body,
			})
		}
	}
	return &IssueDetails{
		ID:          node.Identifier,
		InternalID:  node.ID,
		Title:       node.Title,
		Description: node.Description,
		URL:         node.URL,
		Comments:    comments,
	}, nil
}

// parseIssueIdentifier splits "PLF-3198" into ("PLF", 3198).
func parseIssueIdentifier(id string) (string, int, error) {
	prefix := issuePrefix(id)
	if prefix == id {
		return "", 0, fmt.Errorf("invalid issue identifier: %s", id)
	}
	numStr := id[len(prefix)+1:]
	var num int
	for _, ch := range numStr {
		if ch < '0' || ch > '9' {
			return "", 0, fmt.Errorf("invalid issue number in: %s", id)
		}
		num = num*10 + int(ch-'0')
	}
	return prefix, num, nil
}

// doGraphQL sends a GraphQL request to the Linear API and returns the raw
// response body. It handles auth headers, status code checks, and GraphQL
// error extraction.
func (lt *LinearTracker) doGraphQL(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	payload := map[string]any{"query": query}
	if variables != nil {
		payload["variables"] = variables
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

	// Check for GraphQL-level errors
	var gqlResp struct {
		Data   json.RawMessage `json:"data"`
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
		return nil, fmt.Errorf("linear API error: %s", e.Message)
	}

	return gqlResp.Data, nil
}

// resolveTeamID resolves a team key (e.g. "PLF") to its UUID via the Linear
// API and returns the effective team ID to use in subsequent requests. If
// teamID is already a UUID, it's returned as-is.
//
// A successful resolution is cached in resolvedTeamID for the lifetime of
// the tracker, so it runs at most once across all callers. A failed
// resolution (network error, bad response, unknown team key) is NOT
// cached — the error is returned to that caller, but the next call to
// resolveTeamID retries from scratch, so a single transient failure
// doesn't permanently disable issue creation for the process's lifetime.
// The mutex is held across the (network) resolution call itself, so
// concurrent first-callers serialize instead of each firing their own
// "teams" query; once resolvedTeamID is set, later callers take a fast
// path (lock, read, unlock) with no network call.
func (lt *LinearTracker) resolveTeamID(ctx context.Context) (string, error) {
	lt.teamIDMu.Lock()
	defer lt.teamIDMu.Unlock()

	if lt.resolvedTeamID != "" {
		return lt.resolvedTeamID, nil
	}

	if uuidRe.MatchString(lt.teamID) {
		lt.resolvedTeamID = lt.teamID
		return lt.resolvedTeamID, nil
	}

	slog.Info("resolving Linear team key to UUID", "key", lt.teamID)

	data, err := lt.doGraphQL(ctx, `{ teams { nodes { id key } } }`, nil)
	if err != nil {
		return "", fmt.Errorf("fetching teams: %w", err)
	}

	var result struct {
		Teams struct {
			Nodes []struct {
				ID  string `json:"id"`
				Key string `json:"key"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parsing teams response: %w", err)
	}

	for _, team := range result.Teams.Nodes {
		if team.Key == lt.teamID {
			slog.Info("resolved Linear team", "key", lt.teamID, "uuid", team.ID)
			lt.resolvedTeamID = team.ID
			return lt.resolvedTeamID, nil
		}
	}

	return "", fmt.Errorf("linear team key %q not found", lt.teamID)
}

// resolveTeamOverride resolves an explicitly requested team — a key
// ("ANA"), a display name ("Analytics"), or a UUID — to its UUID. Matching
// is case-insensitive on both key and name. Successful resolutions are
// cached under teamIDMu for the tracker's lifetime; failures are not.
func (lt *LinearTracker) resolveTeamOverride(ctx context.Context, team string) (string, error) {
	if uuidRe.MatchString(team) {
		return team, nil
	}

	cacheKey := strings.ToLower(team)
	lt.teamIDMu.Lock()
	defer lt.teamIDMu.Unlock()
	if id, ok := lt.resolvedTeams[cacheKey]; ok {
		return id, nil
	}

	data, err := lt.doGraphQL(ctx, `{ teams { nodes { id key name } } }`, nil)
	if err != nil {
		return "", fmt.Errorf("fetching teams: %w", err)
	}

	var result struct {
		Teams struct {
			Nodes []struct {
				ID   string `json:"id"`
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parsing teams response: %w", err)
	}

	for _, t := range result.Teams.Nodes {
		if strings.EqualFold(t.Key, team) || strings.EqualFold(t.Name, team) {
			if lt.resolvedTeams == nil {
				lt.resolvedTeams = make(map[string]string)
			}
			lt.resolvedTeams[cacheKey] = t.ID
			return t.ID, nil
		}
	}
	return "", fmt.Errorf("linear team %q not found", team)
}

// resolveProjectID resolves a project name to its UUID within the given
// team. An exact (case-insensitive) name match wins; otherwise a
// case-insensitive substring match is accepted only when it is unambiguous.
// Successful resolutions are cached under teamIDMu for the tracker's
// lifetime; failures are not.
func (lt *LinearTracker) resolveProjectID(ctx context.Context, teamID, name string) (string, error) {
	if uuidRe.MatchString(name) {
		return name, nil
	}

	cacheKey := teamID + "\x00" + strings.ToLower(name)
	lt.teamIDMu.Lock()
	defer lt.teamIDMu.Unlock()
	if id, ok := lt.resolvedProjects[cacheKey]; ok {
		return id, nil
	}

	data, err := lt.doGraphQL(ctx,
		`query($teamId: String!) { team(id: $teamId) { projects(first: 250) { nodes { id name } } } }`,
		map[string]any{"teamId": teamID},
	)
	if err != nil {
		return "", fmt.Errorf("fetching projects: %w", err)
	}

	var result struct {
		Team struct {
			Projects struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"projects"`
		} `json:"team"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parsing projects response: %w", err)
	}

	var partial []string
	lower := strings.ToLower(name)
	for _, p := range result.Team.Projects.Nodes {
		if strings.EqualFold(p.Name, name) {
			if lt.resolvedProjects == nil {
				lt.resolvedProjects = make(map[string]string)
			}
			lt.resolvedProjects[cacheKey] = p.ID
			return p.ID, nil
		}
		if strings.Contains(strings.ToLower(p.Name), lower) {
			partial = append(partial, p.ID)
		}
	}
	if len(partial) == 1 {
		if lt.resolvedProjects == nil {
			lt.resolvedProjects = make(map[string]string)
		}
		lt.resolvedProjects[cacheKey] = partial[0]
		return partial[0], nil
	}
	if len(partial) > 1 {
		return "", fmt.Errorf("linear project %q is ambiguous (%d partial matches)", name, len(partial))
	}
	return "", fmt.Errorf("linear project %q not found", name)
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
	teamID, err := lt.resolveTeamID(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving team ID: %w", err)
	}

	// An explicitly requested team overrides the configured default —
	// warn-and-fallback, never block creation on a bad hint.
	if opts.Team != "" {
		if id, terr := lt.resolveTeamOverride(ctx, opts.Team); terr != nil {
			slog.Warn("requested Linear team not resolved, filing to default team", "team", opts.Team, "error", terr)
		} else {
			teamID = id
		}
	}

	// Build label IDs based on category, then merge in any extra labels.
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
	labelIDs = append(labelIDs, opts.Labels...)

	variables := map[string]any{
		"title":       opts.Title,
		"description": opts.Description,
		"teamId":      teamID,
	}
	if len(labelIDs) > 0 {
		variables["labelIds"] = labelIDs
	}
	if opts.StateID != "" {
		variables["stateId"] = opts.StateID
	}
	if opts.Project != "" {
		if pid, perr := lt.resolveProjectID(ctx, teamID, opts.Project); perr != nil {
			slog.Warn("requested Linear project not resolved, filing without project", "project", opts.Project, "error", perr)
		} else {
			variables["projectId"] = pid
		}
	}

	query := `mutation IssueCreate($title: String!, $description: String, $teamId: String!, $labelIds: [String!], $stateId: String, $projectId: String) {
		issueCreate(input: { title: $title, description: $description, teamId: $teamId, labelIds: $labelIds, stateId: $stateId, projectId: $projectId }) {
			success
			issue {
				id
				identifier
				url
				title
			}
		}
	}`

	data, err := lt.doGraphQL(ctx, query, variables)
	if err != nil {
		return nil, err
	}

	var result struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
				URL        string `json:"url"`
				Title      string `json:"title"`
			} `json:"issue"`
		} `json:"issueCreate"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parsing issue create response: %w", err)
	}

	if !result.IssueCreate.Success {
		return nil, fmt.Errorf("linear issue creation failed")
	}

	issue := result.IssueCreate.Issue
	return &IssueRef{
		Provider:   "linear",
		ID:         issue.Identifier,
		URL:        issue.URL,
		Title:      issue.Title,
		InternalID: issue.ID,
	}, nil
}

// GetIssueStatus fetches the current status and assignment info for a Linear issue.
// Uses the issue's updatedAt as a proxy for assignment recency (Linear has no
// first-class assignedAt field).
func (lt *LinearTracker) GetIssueStatus(ctx context.Context, ref *IssueRef) (*IssueStatus, error) {
	if lt.apiToken == "" {
		return nil, nil
	}

	teamKey, number, err := parseIssueIdentifier(ref.ID)
	if err != nil {
		return nil, fmt.Errorf("parsing issue ID: %w", err)
	}

	query := `query IssueByIdentifier($filter: IssueFilter!) {
		issues(filter: $filter, first: 1) {
			nodes {
				id
				state { name type }
				assignee { displayName }
				updatedAt
			}
		}
	}`

	variables := map[string]any{
		"filter": map[string]any{
			"number": map[string]any{"eq": number},
			"team":   map[string]any{"key": map[string]any{"eq": teamKey}},
		},
	}

	data, err := lt.doGraphQL(ctx, query, variables)
	if err != nil {
		return nil, fmt.Errorf("fetching issue status: %w", err)
	}

	var result struct {
		Issues struct {
			Nodes []struct {
				ID    string `json:"id"`
				State struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"state"`
				Assignee *struct {
					DisplayName string `json:"displayName"`
				} `json:"assignee"`
				UpdatedAt time.Time `json:"updatedAt"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parsing issue status: %w", err)
	}

	if len(result.Issues.Nodes) == 0 {
		return nil, nil
	}

	node := result.Issues.Nodes[0]
	status := &IssueStatus{
		State:      node.State.Name,
		StateType:  node.State.Type,
		InternalID: node.ID,
		AssignedAt: node.UpdatedAt,
	}
	if node.Assignee != nil {
		status.AssigneeName = node.Assignee.DisplayName
	}
	return status, nil
}

// PostComment posts a comment on a Linear issue.
// If ref.InternalID is set, the status lookup is skipped.
func (lt *LinearTracker) PostComment(ctx context.Context, ref *IssueRef, body string) error {
	if lt.apiToken == "" {
		return fmt.Errorf("linear API token not configured")
	}

	issueID := ref.InternalID
	if issueID == "" {
		status, err := lt.GetIssueStatus(ctx, ref)
		if err != nil {
			return fmt.Errorf("resolving issue for comment: %w", err)
		}
		if status == nil || status.InternalID == "" {
			return fmt.Errorf("issue %s not found", ref.ID)
		}
		issueID = status.InternalID
	}

	query := `mutation CommentCreate($issueId: String!, $body: String!) {
		commentCreate(input: {issueId: $issueId, body: $body}) {
			success
		}
	}`

	variables := map[string]any{
		"issueId": issueID,
		"body":    body,
	}

	data, err := lt.doGraphQL(ctx, query, variables)
	if err != nil {
		return err
	}

	var result struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parsing comment response: %w", err)
	}

	if !result.CommentCreate.Success {
		return fmt.Errorf("linear comment creation failed")
	}

	return nil
}
