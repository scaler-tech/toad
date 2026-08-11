// Package linearagent makes toad an addressable Linear agent: it polls the
// app's agent sessions (mentions and delegations create them), runs
// codebase-backed investigations, and replies with agent activities.
// Delivery is polling-only by design — toad has no inbound HTTP surface.
// The Intake seam (poller today) is where a webhook listener would slot in.
package linearagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/scaler-tech/toad/internal/issuetracker"
)

// Session is one Linear agent session snapshot.
type Session struct {
	ID              string
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	IssueID         string
	IssueIdentifier string
	IssueTitle      string
	SourceComment   string
	Activities      []Activity
}

// Activity is one entry in a session's transcript.
type Activity struct {
	CreatedAt time.Time
	Type      string
	Body      string
}

// IsUser reports whether the activity is a user message (prompt) rather
// than agent output.
func (a Activity) IsUser() bool { return a.Type == "prompt" }

// Client is a minimal GraphQL client for the agent-session API. It is
// separate from issuetracker's tracker on purpose: sessions/activities are
// agent concerns, and they only ever authenticate as the app.
type Client struct {
	auth       issuetracker.AuthSource
	httpClient *http.Client
	graphqlURL string

	// mu guards appUserID, the cached id of toad's own app user (viewer).
	// agentSessions returns EVERY agent's sessions in the workspace — other
	// agent apps' included — so ListSessions must filter to sessions whose
	// appUser is toad itself; posting to a foreign session fails with
	// "Invalid agent session".
	mu        sync.Mutex
	appUserID string
}

func NewClient(auth issuetracker.AuthSource) *Client {
	return &Client{
		auth:       auth,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		graphqlURL: "https://api.linear.app/graphql",
	}
}

const viewerQuery = `query ToadViewer { viewer { id } }`

// ownAppUserID returns (and caches for the client's lifetime) the id of the
// app user this client authenticates as.
func (c *Client) ownAppUserID(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.appUserID != "" {
		return c.appUserID, nil
	}
	raw, err := c.do(ctx, viewerQuery, nil)
	if err != nil {
		return "", fmt.Errorf("resolving own app user: %w", err)
	}
	var out struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parsing viewer: %w", err)
	}
	if out.Viewer.ID == "" {
		return "", fmt.Errorf("viewer query returned no id")
	}
	c.appUserID = out.Viewer.ID
	return c.appUserID, nil
}

const listSessionsQuery = `query ToadAgentSessions($first: Int!) {
  agentSessions(first: $first, orderBy: updatedAt) {
    nodes {
      id status createdAt updatedAt
      appUser { id }
      issue { id identifier title }
      sourceComment { body }
      activities(first: 50) {
        nodes {
          createdAt
          content {
            __typename
            ... on AgentActivityPromptContent { body }
            ... on AgentActivityThoughtContent { body }
            ... on AgentActivityResponseContent { body }
            ... on AgentActivityErrorContent { body }
            ... on AgentActivityElicitationContent { body }
          }
        }
      }
    }
  }
}`

// ListSessions fetches the app's most recently updated agent sessions,
// filtered to sessions addressed to toad itself (agentSessions returns the
// whole workspace's agent sessions, other agents' included).
func (c *Client) ListSessions(ctx context.Context, first int) ([]Session, error) {
	ownID, err := c.ownAppUserID(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := c.do(ctx, listSessionsQuery, map[string]any{"first": first})
	if err != nil {
		return nil, err
	}
	var out struct {
		AgentSessions struct {
			Nodes []struct {
				ID        string    `json:"id"`
				Status    string    `json:"status"`
				CreatedAt time.Time `json:"createdAt"`
				UpdatedAt time.Time `json:"updatedAt"`
				AppUser   *struct {
					ID string `json:"id"`
				} `json:"appUser"`
				Issue *struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					Title      string `json:"title"`
				} `json:"issue"`
				SourceComment *struct {
					Body string `json:"body"`
				} `json:"sourceComment"`
				Activities struct {
					Nodes []struct {
						CreatedAt time.Time `json:"createdAt"`
						Content   *struct {
							Typename string `json:"__typename"`
							Body     string `json:"body"`
						} `json:"content"`
					} `json:"nodes"`
				} `json:"activities"`
			} `json:"nodes"`
		} `json:"agentSessions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parsing agentSessions: %w", err)
	}
	sessions := make([]Session, 0, len(out.AgentSessions.Nodes))
	for _, n := range out.AgentSessions.Nodes {
		if n.AppUser == nil || n.AppUser.ID != ownID {
			continue // another agent's session — not ours to answer
		}
		s := Session{
			ID: n.ID, Status: n.Status, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
		}
		if n.Issue != nil {
			s.IssueID, s.IssueIdentifier, s.IssueTitle = n.Issue.ID, n.Issue.Identifier, n.Issue.Title
		}
		if n.SourceComment != nil {
			s.SourceComment = n.SourceComment.Body
		}
		for _, a := range n.Activities.Nodes {
			if a.Content == nil {
				continue
			}
			s.Activities = append(s.Activities, Activity{
				CreatedAt: a.CreatedAt,
				Type:      activityType(a.Content.Typename),
				Body:      a.Content.Body,
			})
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// activityType maps the content union's __typename to a short type string.
func activityType(typename string) string {
	t := strings.TrimSuffix(strings.TrimPrefix(typename, "AgentActivity"), "Content")
	return strings.ToLower(t)
}

const createActivityMutation = `mutation ToadAgentActivityCreate($input: AgentActivityCreateInput!) {
  agentActivityCreate(input: $input) { success }
}`

// CreateActivity posts one activity (type: thought|response|error) to a session.
func (c *Client) CreateActivity(ctx context.Context, sessionID, activityType, body string) error {
	_, err := c.do(ctx, createActivityMutation, map[string]any{
		"input": map[string]any{
			"agentSessionId": sessionID,
			"content":        map[string]any{"type": activityType, "body": body},
		},
	})
	return err
}

// do runs one GraphQL request with the app credential and returns raw data.
func (c *Client) do(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.graphqlURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.auth.AuthHeader())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if newHeader, retry := c.auth.HandleUnauthorized(ctx); retry {
			req2, err := http.NewRequestWithContext(ctx, "POST", c.graphqlURL, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			req2.Header.Set("Content-Type", "application/json")
			req2.Header.Set("Authorization", newHeader)
			resp2, err := c.httpClient.Do(req2)
			if err != nil {
				return nil, err
			}
			defer resp2.Body.Close()
			respBody, err = io.ReadAll(io.LimitReader(resp2.Body, 4<<20))
			if err != nil {
				return nil, err
			}
			resp = resp2
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linear API returned %d: %s", resp.StatusCode, string(respBody))
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("parsing GraphQL response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return nil, fmt.Errorf("linear GraphQL error: %s", envelope.Errors[0].Message)
	}
	return envelope.Data, nil
}
