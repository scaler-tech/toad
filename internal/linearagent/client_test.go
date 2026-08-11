package linearagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type staticAuth struct{ header string }

func (s staticAuth) AuthHeader() string                                    { return s.header }
func (s staticAuth) HandleUnauthorized(ctx context.Context) (string, bool) { return "", false }

const sessionsFixture = `{"data":{"agentSessions":{"nodes":[
  {"id":"sess-1","status":"pending","createdAt":"2026-08-10T10:00:00.000Z","updatedAt":"2026-08-10T10:00:00.000Z",
   "appUser":{"id":"app-user-1"},
   "issue":{"id":"uuid-1","identifier":"PLF-9","title":"Exports are slow"},
   "sourceComment":{"body":"@toad can you look at this?"},
   "activities":{"nodes":[]}},
  {"id":"sess-2","status":"active","createdAt":"2026-08-10T09:00:00.000Z","updatedAt":"2026-08-10T09:30:00.000Z",
   "appUser":{"id":"app-user-1"},
   "issue":{"id":"uuid-2","identifier":"PLF-10","title":"Login flaky"},
   "sourceComment":null,
   "activities":{"nodes":[
     {"createdAt":"2026-08-10T09:05:00.000Z","content":{"__typename":"AgentActivityThoughtContent","body":"Reading the code."}},
     {"createdAt":"2026-08-10T09:20:00.000Z","content":{"__typename":"AgentActivityPromptContent","body":"any update?"}}
   ]}}
]}}}`

func TestListSessions_ParsesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer app-tok" {
			t.Errorf("auth header = %q", got)
		}
		var req struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "viewer") {
			w.Write([]byte(viewerFixture))
			return
		}
		w.Write([]byte(sessionsFixture))
	}))
	defer srv.Close()

	c := NewClient(staticAuth{"Bearer app-tok"})
	c.httpClient = srv.Client()
	c.graphqlURL = srv.URL

	sessions, err := c.ListSessions(context.Background(), 50)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	s1 := sessions[0]
	if s1.ID != "sess-1" || s1.Status != "pending" || s1.IssueIdentifier != "PLF-9" ||
		s1.SourceComment != "@toad can you look at this?" || len(s1.Activities) != 0 {
		t.Errorf("s1 = %+v", s1)
	}
	s2 := sessions[1]
	if len(s2.Activities) != 2 {
		t.Fatalf("s2 activities = %d", len(s2.Activities))
	}
	if s2.Activities[0].Type != "thought" || s2.Activities[0].IsUser() {
		t.Errorf("activity 0 = %+v", s2.Activities[0])
	}
	if s2.Activities[1].Type != "prompt" || !s2.Activities[1].IsUser() || s2.Activities[1].Body != "any update?" {
		t.Errorf("activity 1 = %+v", s2.Activities[1])
	}
}

func TestCreateActivity_SendsInput(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"data":{"agentActivityCreate":{"success":true}}}`))
	}))
	defer srv.Close()

	c := NewClient(staticAuth{"Bearer app-tok"})
	c.httpClient = srv.Client()
	c.graphqlURL = srv.URL

	if err := c.CreateActivity(context.Background(), "sess-1", "thought", "Reading the ticket and the code."); err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}
	vars := gotBody["variables"].(map[string]any)
	input := vars["input"].(map[string]any)
	if input["agentSessionId"] != "sess-1" {
		t.Errorf("agentSessionId = %v", input["agentSessionId"])
	}
	content := input["content"].(map[string]any)
	if content["type"] != "thought" || content["body"] != "Reading the ticket and the code." {
		t.Errorf("content = %v", content)
	}
}

func TestCreateActivity_GraphQLErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errors":[{"message":"boom"}]}`))
	}))
	defer srv.Close()
	c := NewClient(staticAuth{"x"})
	c.httpClient = srv.Client()
	c.graphqlURL = srv.URL
	if err := c.CreateActivity(context.Background(), "s", "response", "b"); err == nil {
		t.Fatal("expected error from GraphQL errors array")
	}
}

const viewerFixture = `{"data":{"viewer":{"id":"app-user-1"}}}`

const mixedOwnershipFixture = `{"data":{"agentSessions":{"nodes":[
  {"id":"sess-own","status":"pending","createdAt":"2026-08-11T08:00:00.000Z","updatedAt":"2026-08-11T08:00:00.000Z",
   "appUser":{"id":"app-user-1"},
   "issue":{"id":"uuid-1","identifier":"PLF-9","title":"Ours"},
   "sourceComment":{"body":"@toad look"},
   "activities":{"nodes":[]}},
  {"id":"sess-foreign","status":"pending","createdAt":"2026-08-11T08:01:00.000Z","updatedAt":"2026-08-11T08:01:00.000Z",
   "appUser":{"id":"biome-app-user"},
   "issue":{"id":"uuid-2","identifier":"PLF-10","title":"Biome's"},
   "sourceComment":{"body":"@biome fix this"},
   "activities":{"nodes":[]}}
]}}}`

// twoPhaseServer answers the viewer-id query and the sessions query from one
// handler, dispatching on the request body.
func twoPhaseServer(t *testing.T, sessionsJSON string, viewerCalls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "viewer") {
			if viewerCalls != nil {
				*viewerCalls++
			}
			w.Write([]byte(viewerFixture))
			return
		}
		w.Write([]byte(sessionsJSON))
	}))
}

func TestListSessions_FiltersToOwnAppUser(t *testing.T) {
	viewerCalls := 0
	srv := twoPhaseServer(t, mixedOwnershipFixture, &viewerCalls)
	defer srv.Close()

	c := NewClient(staticAuth{"Bearer app-tok"})
	c.httpClient = srv.Client()
	c.graphqlURL = srv.URL

	sessions, err := c.ListSessions(context.Background(), 50)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "sess-own" {
		t.Fatalf("workspace sessions from other agents must be filtered out; got %+v", sessions)
	}

	// The viewer id is cached — a second list must not re-query it.
	if _, err := c.ListSessions(context.Background(), 50); err != nil {
		t.Fatalf("second ListSessions: %v", err)
	}
	if viewerCalls != 1 {
		t.Errorf("viewer queried %d times, want 1 (cached)", viewerCalls)
	}
}
