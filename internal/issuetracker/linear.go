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
	auth           AuthSource
	graphqlURL     string

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

	// resolvedUsers extends the same success-only cache policy to
	// assignee/delegate resolution (resolveUser), also guarded by
	// teamIDMu. Keyed by lower-cased name/email.
	resolvedUsers map[string]linearUserResolution
}

// linearUserResolution is a cached (or freshly resolved) Linear user: its
// UUID and whether it's an app/agent user (e.g. Biome, a Linear OAuth-app
// user) rather than a human — CreateIssue routes an agent to the issue's
// delegateId, a human to assigneeId. See resolveAgentKind for how IsAgent
// is decided.
type linearUserResolution struct {
	ID      string
	IsAgent bool
}

// NewLinearTracker creates a Linear tracker from config.
func NewLinearTracker(cfg config.IssueTrackerConfig) *LinearTracker {
	return NewLinearTrackerWithAuth(cfg, nil)
}

// NewLinearTrackerWithAuth creates a Linear tracker from config with an optional AuthSource.
func NewLinearTrackerWithAuth(cfg config.IssueTrackerConfig, auth AuthSource) *LinearTracker {
	return &LinearTracker{
		apiToken:       cfg.APIToken,
		teamID:         cfg.TeamID,
		bugLabelID:     cfg.BugLabelID,
		featureLabelID: cfg.FeatureLabelID,
		createIssues:   cfg.CreateIssues,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		auth:           auth,
		graphqlURL:     "https://api.linear.app/graphql",
	}
}

// hasCredentials reports whether any Linear credential is available: the
// personal API key, or an auth source that can produce an Authorization
// header (a connected OAuth app token, or its API-key fallback). Guards
// that skip API calls must use this, not a bare apiToken check — an
// OAuth-only setup has an empty apiToken but a fully working credential.
func (lt *LinearTracker) hasCredentials() bool {
	if lt.apiToken != "" {
		return true
	}
	return lt.auth != nil && lt.auth.AuthHeader() != ""
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
	if !lt.hasCredentials() {
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

// sendGraphQL builds and sends one GraphQL POST with the given auth header.
// The caller owns the response body.
func (lt *LinearTracker) sendGraphQL(ctx context.Context, body []byte, authHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", lt.graphqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)
	resp, err := lt.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("linear API request: %w", err)
	}
	return resp, nil
}

// doGraphQL sends a GraphQL request to the Linear API and returns the raw
// response body. When an AuthSource is present and the server returns 401, it
// calls HandleUnauthorized for a new header and retries exactly once. With nil
// auth, no 401 retry is attempted (legacy behavior). Checks status codes and
// extracts GraphQL-level errors.
func (lt *LinearTracker) doGraphQL(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	payload := map[string]any{"query": query}
	if variables != nil {
		payload["variables"] = variables
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	header := lt.apiToken
	if lt.auth != nil {
		header = lt.auth.AuthHeader()
	}

	resp, err := lt.sendGraphQL(ctx, body, header)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized && lt.auth != nil {
		newHeader, retry := lt.auth.HandleUnauthorized(ctx)
		if !retry {
			return nil, fmt.Errorf("linear API returned 401 and auth source could not recover")
		}
		resp, err = lt.sendGraphQL(ctx, body, newHeader)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		respBody, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}
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

// resolveAgentKind decides whether a matched Linear user is an app/agent
// user (e.g. Biome, provisioned as a Linear OAuth-app user) rather than a
// human — CreateIssue needs this to know whether to route a resolved name
// to the issue's assigneeId (human) or delegateId (agent).
//
// Two signals, in priority order:
//  1. Linear's `app` boolean on the User type, when the response actually
//     carries it (non-nil) — the authoritative signal straight from Linear.
//  2. A fallback heuristic: an email ending in "@oauthapp.linear.app" is
//     how Linear provisions its own OAuth-app/agent users (Biome included),
//     used whenever `app` is nil — either because queryUsersWithAgentSignal
//     had to retry without requesting it (this API version's schema doesn't
//     expose it) or because the field parsed but came back null for this
//     particular node.
func resolveAgentKind(app *bool, email string) bool {
	if app != nil {
		return *app
	}
	return strings.HasSuffix(strings.ToLower(email), "@oauthapp.linear.app")
}

// queryUsersWithAgentSignal runs queryWithApp (which requests an `app`
// field on User, the primary agent-detection signal) and, if the whole
// GraphQL request errors — most likely because this Linear API version
// doesn't expose `app` on the User type and rejects the query outright —
// retries with queryWithoutApp instead of failing user resolution entirely.
// This is the "best-effort app-field probe, tolerate its absence"
// defensiveness resolveAgentKind's email-suffix fallback exists for.
func (lt *LinearTracker) queryUsersWithAgentSignal(ctx context.Context, queryWithApp, queryWithoutApp string, variables map[string]any) (json.RawMessage, error) {
	data, err := lt.doGraphQL(ctx, queryWithApp, variables)
	if err == nil {
		return data, nil
	}
	slog.Debug("Linear users query with 'app' field failed, retrying without it", "error", err)
	return lt.doGraphQL(ctx, queryWithoutApp, variables)
}

// resolveUser resolves a display name, real name, or email to a Linear user
// ID plus whether it's an app/agent user (see resolveAgentKind). A value
// containing "@" is treated as an email and looked up via the users email
// filter; otherwise it's matched case-insensitively against name OR
// displayName — an exact match wins, and an unambiguous case-insensitive
// substring match is accepted otherwise (mirrors resolveProjectID's
// partial-match rules). Successful resolutions are cached under teamIDMu
// for the tracker's lifetime; failures are not.
func (lt *LinearTracker) resolveUser(ctx context.Context, nameOrEmail string) (id string, isAgent bool, err error) {
	if uuidRe.MatchString(nameOrEmail) {
		// A bare UUID carries no agent signal of its own; treat as human
		// (existing assigneeId behavior) since that's the overwhelmingly
		// common case for a UUID passed directly.
		return nameOrEmail, false, nil
	}

	cacheKey := "user\x00" + strings.ToLower(nameOrEmail)
	lt.teamIDMu.Lock()
	defer lt.teamIDMu.Unlock()
	if res, ok := lt.resolvedUsers[cacheKey]; ok {
		return res.ID, res.IsAgent, nil
	}

	if strings.Contains(nameOrEmail, "@") {
		data, qerr := lt.queryUsersWithAgentSignal(ctx,
			`query($email: String!) { users(filter: { email: { eq: $email } }, first: 1) { nodes { id email app } } }`,
			`query($email: String!) { users(filter: { email: { eq: $email } }, first: 1) { nodes { id email } } }`,
			map[string]any{"email": nameOrEmail},
		)
		if qerr != nil {
			return "", false, fmt.Errorf("fetching user by email: %w", qerr)
		}
		var result struct {
			Users struct {
				Nodes []struct {
					ID    string `json:"id"`
					Email string `json:"email"`
					App   *bool  `json:"app"`
				} `json:"nodes"`
			} `json:"users"`
		}
		if uerr := json.Unmarshal(data, &result); uerr != nil {
			return "", false, fmt.Errorf("parsing users response: %w", uerr)
		}
		if len(result.Users.Nodes) == 0 {
			return "", false, fmt.Errorf("linear user with email %q not found", nameOrEmail)
		}
		node := result.Users.Nodes[0]
		res := linearUserResolution{ID: node.ID, IsAgent: resolveAgentKind(node.App, node.Email)}
		lt.cacheUserLocked(cacheKey, res)
		return res.ID, res.IsAgent, nil
	}

	data, qerr := lt.queryUsersWithAgentSignal(ctx,
		`{ users(first: 250) { nodes { id name displayName email app } } }`,
		`{ users(first: 250) { nodes { id name displayName email } } }`,
		nil,
	)
	if qerr != nil {
		return "", false, fmt.Errorf("fetching users: %w", qerr)
	}
	var result struct {
		Users struct {
			Nodes []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
				Email       string `json:"email"`
				App         *bool  `json:"app"`
			} `json:"nodes"`
		} `json:"users"`
	}
	if uerr := json.Unmarshal(data, &result); uerr != nil {
		return "", false, fmt.Errorf("parsing users response: %w", uerr)
	}

	var partial []linearUserResolution
	lower := strings.ToLower(nameOrEmail)
	for _, u := range result.Users.Nodes {
		if strings.EqualFold(u.Name, nameOrEmail) || strings.EqualFold(u.DisplayName, nameOrEmail) {
			res := linearUserResolution{ID: u.ID, IsAgent: resolveAgentKind(u.App, u.Email)}
			lt.cacheUserLocked(cacheKey, res)
			return res.ID, res.IsAgent, nil
		}
		if strings.Contains(strings.ToLower(u.Name), lower) || strings.Contains(strings.ToLower(u.DisplayName), lower) {
			partial = append(partial, linearUserResolution{ID: u.ID, IsAgent: resolveAgentKind(u.App, u.Email)})
		}
	}
	if len(partial) == 1 {
		lt.cacheUserLocked(cacheKey, partial[0])
		return partial[0].ID, partial[0].IsAgent, nil
	}
	if len(partial) > 1 {
		return "", false, fmt.Errorf("linear user %q is ambiguous (%d partial matches)", nameOrEmail, len(partial))
	}
	return "", false, fmt.Errorf("linear user %q not found", nameOrEmail)
}

// cacheUserLocked records a successful user resolution. Callers must already
// hold teamIDMu.
func (lt *LinearTracker) cacheUserLocked(key string, res linearUserResolution) {
	if lt.resolvedUsers == nil {
		lt.resolvedUsers = make(map[string]linearUserResolution)
	}
	lt.resolvedUsers[key] = res
}

// resolveAssignees walks names (CreateIssueOpts.Assignees, already
// requester-substituted by cmd/ticketflow.go) in order, resolving each to a
// Linear user and routing it to the issue's single assignee slot (first
// HUMAN match) or its single delegate slot (first AGENT match) — Linear
// supports exactly one of each. A name that resolves fine but would fill an
// already-filled slot is logged at Info and skipped (it's not "unresolved",
// there's just nowhere left to put it — Linear has one assignee and one
// delegate). A name that doesn't resolve to any Linear user at all is
// warned and returned in unresolved, so the caller (composeFiledReply, via
// IssueRef) can surface it rather than silently dropping the request.
// Neither case ever blocks issue creation.
func (lt *LinearTracker) resolveAssignees(ctx context.Context, names []string) (
	assigneeID, assigneeName, delegateID, delegateName string, unresolved []string) {
	for _, name := range names {
		id, isAgent, err := lt.resolveUser(ctx, name)
		if err != nil {
			slog.Warn("requested Linear assignee/delegate not resolved, skipping", "name", name, "error", err)
			unresolved = append(unresolved, name)
			continue
		}
		if isAgent {
			if delegateID != "" {
				slog.Info("dropping extra Linear delegate candidate (only one delegate slot)", "kept", delegateName, "dropped", name)
				continue
			}
			delegateID, delegateName = id, name
			continue
		}
		if assigneeID != "" {
			slog.Info("dropping extra Linear assignee candidate (only one assignee slot)", "kept", assigneeName, "dropped", name)
			continue
		}
		assigneeID, assigneeName = id, name
	}
	return assigneeID, assigneeName, delegateID, delegateName, unresolved
}

// CreateIssue creates a new Linear issue via the GraphQL API.
func (lt *LinearTracker) CreateIssue(ctx context.Context, opts CreateIssueOpts) (*IssueRef, error) {
	if !lt.hasCredentials() {
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

	// Build label IDs based on category, then merge in any explicit labels.
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

	// Resolve requested assignees/delegates in order — first HUMAN match
	// fills assigneeId, first AGENT match fills delegateId; unresolved names
	// are warned and returned so the caller can surface them, but never
	// block creation. See resolveAssignees' doc comment.
	var assigneeName, delegateName string
	var unresolvedAssignees []string
	if len(opts.Assignees) > 0 {
		var assigneeID, delegateID string
		assigneeID, assigneeName, delegateID, delegateName, unresolvedAssignees = lt.resolveAssignees(ctx, opts.Assignees)
		if assigneeID != "" {
			variables["assigneeId"] = assigneeID
		}
		if delegateID != "" {
			variables["delegateId"] = delegateID
		}
	}

	query := `mutation IssueCreate($title: String!, $description: String, $teamId: String!, $labelIds: [String!], $stateId: String, $projectId: String, $assigneeId: String, $delegateId: String) {
		issueCreate(input: { title: $title, description: $description, teamId: $teamId, labelIds: $labelIds, stateId: $stateId, projectId: $projectId, assigneeId: $assigneeId, delegateId: $delegateId }) {
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
		Provider:            "linear",
		ID:                  issue.Identifier,
		URL:                 issue.URL,
		Title:               issue.Title,
		InternalID:          issue.ID,
		AssignedTo:          assigneeName,
		DelegatedTo:         delegateName,
		UnresolvedAssignees: unresolvedAssignees,
	}, nil
}

// GetIssueStatus fetches the current status and assignment info for a Linear issue.
// Uses the issue's updatedAt as a proxy for assignment recency (Linear has no
// first-class assignedAt field).
func (lt *LinearTracker) GetIssueStatus(ctx context.Context, ref *IssueRef) (*IssueStatus, error) {
	if !lt.hasCredentials() {
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
				delegate { id name displayName }
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
				Delegate *struct {
					ID          string `json:"id"`
					Name        string `json:"name"`
					DisplayName string `json:"displayName"`
				} `json:"delegate"`
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
	if node.Delegate != nil {
		status.DelegateName = node.Delegate.DisplayName
		if status.DelegateName == "" {
			status.DelegateName = node.Delegate.Name
		}
	}
	return status, nil
}

// PostComment posts a comment on a Linear issue.
// If ref.InternalID is set, the status lookup is skipped.
func (lt *LinearTracker) PostComment(ctx context.Context, ref *IssueRef, body string) error {
	if !lt.hasCredentials() {
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
