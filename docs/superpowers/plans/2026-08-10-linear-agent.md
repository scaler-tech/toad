# Toad as a Linear Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Toad authenticates to Linear as an installed workspace app (its own identity, not the operator's personal key) and answers @-mentions/delegations on Linear tickets with codebase-backed investigations, via polling.

**Architecture:** Phase A adds `internal/linearauth` (OAuth token store + refresh + `AuthSource`) and threads it into `issuetracker.LinearTracker`'s GraphQL auth, plus a `toad linear connect` command. Phase B adds `internal/linearagent` (GraphQL client for agent sessions, work detection, session processor) driven by a poller started from `cmd/root.go`, with dedup state in a new `agent_sessions` table (schema v13).

**Tech Stack:** Go, stdlib `net/http` + `httptest`, `modernc.org/sqlite` in-memory for tests, Cobra for the CLI command. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-10-linear-agent-design.md`

**Two deliberate deviations from the spec** (both simplifications, flag to the human if they object):
1. No `linear_agent_cursor` setting. The poller diffs Linear's session list against the `agent_sessions` table each tick; `last_handled_activity_at` is written only AFTER a response posts, so crash recovery is automatic (unhandled work is re-detected on the next poll) and `RecoverOnStartup` needs no new logic.
2. The session response is composed by a small `composeResponse` in `linearagent` (reasoning-led, evidence bullets) rather than reusing `ticket.ComposeBody`, which renders a full ticket description (Problem/Scope/Acceptance sections) — wrong shape for a comment-like reply. STE style comes from the findings prose itself (the investigation prompt already carries `ProseStyleRules`).

## Verified Linear API facts (introspected 2026-08-10; trust these over guesses)

- Auth: personal API keys are sent as `Authorization: <token>` (raw, current behavior). OAuth tokens are sent as `Authorization: Bearer <token>`.
- OAuth: authorize at `https://linear.app/oauth/authorize` with `response_type=code`, `actor=app`, scopes `read,write,app:assignable,app:mentionable`; token exchange at `https://api.linear.app/oauth/token` (form-encoded POST: `grant_type=authorization_code`, `code`, `redirect_uri`, `client_id`, `client_secret`); refresh with `grant_type=refresh_token`, `refresh_token`. Response JSON: `access_token`, `token_type`, optional `expires_in`, optional `refresh_token`, `scope`.
- `Query.agentSessions(first, after, orderBy)` — plain connection, NO filter argument: fetch and filter client-side. Statuses (enum `AgentSessionStatus`): `pending`, `active`, `complete`, `awaitingInput`, `error`, `stale`.
- `AgentSession` fields used: `id`, `status`, `createdAt`, `updatedAt`, `issue { id identifier title }`, `sourceComment { body user { name } }`, `activities(first: 50) { nodes { id createdAt content { __typename ... } } }`.
- `AgentActivity.content` is a union: `AgentActivityPromptContent` (a USER message — `body`), `AgentActivityThoughtContent`, `AgentActivityActionContent`, `AgentActivityResponseContent`, `AgentActivityErrorContent`, `AgentActivityElicitationContent` (agent-authored). Discriminate by `__typename`.
- `mutation agentActivityCreate(input: AgentActivityCreateInput!)` with `input = {agentSessionId: <id>, content: {"type": "thought"|"response"|"error", "body": <markdown>}}` → `{ agentActivityCreate { success } }`.

## Global Constraints

- **NEVER run `git commit`, `git add`, or `git push`** — this repo's CLAUDE.md routes all commits through the `/release` skill. Plan tasks have NO commit steps; leave the working tree dirty.
- Run `gofmt -l .` after each task; fix any output with `gofmt -w <file>`.
- Settings keys (exact): `linear_oauth_token`, `linear_oauth_refresh_token`.
- OAuth callback: `http://localhost:9482/callback`. Env vars: `TOAD_LINEAR_CLIENT_ID`, `TOAD_LINEAR_CLIENT_SECRET`.
- Config block: `linear_agent:` with `enabled` (default `true`) and `poll_seconds` (default `15`, min 5).
- Claim scope string for session processing: `linear-agent`.
- Sessions never mutate issues (no status/assignee changes) and never create tickets; investigations stay `PermissionReadOnly`.
- Full gate after the last task: `go build ./... && go test ./... && go vet ./... && gofmt -l .`

---

### Task 1: `internal/linearauth` — token store, OAuth exchange/refresh, AuthSource

**Files:**
- Create: `internal/linearauth/linearauth.go`
- Create: `internal/linearauth/linearauth_test.go`

**Interfaces:**
- Consumes: `state.DB` settings API: `GetSetting(key string) (string, error)`, `SetSetting(key, value string) error` (internal/state/db.go:955,967).
- Produces (later tasks rely on these exact names):
  - `func NewStore(db *state.DB) *Store` with methods `Token() string`, `RefreshToken() string`, `SaveTokens(access, refresh string) error`, `Connected() bool`
  - `func BuildAuthorizeURL(clientID, redirectURI, state string) string`
  - `func Exchange(ctx context.Context, hc *http.Client, tokenURL, clientID, clientSecret, code, redirectURI string) (*TokenResponse, error)`
  - `func Refresh(ctx context.Context, hc *http.Client, tokenURL, clientID, clientSecret, refreshToken string) (*TokenResponse, error)`
  - `type TokenResponse struct { AccessToken string; RefreshToken string }`
  - `func NewSource(store *Store, clientID, clientSecret, fallbackAPIKey string) *Source` implementing:
    - `AuthHeader() string` — `"Bearer <oauth>"` if a token is stored, else the raw API key, else `""`
    - `HandleUnauthorized(ctx context.Context) (string, bool)` — refresh-once-then-fallback (see Step 3)
  - `const DefaultTokenURL = "https://api.linear.app/oauth/token"`

- [ ] **Step 1: Write the failing tests**

Create `internal/linearauth/linearauth_test.go`:

```go
package linearauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/scaler-tech/toad/internal/state"
)

func testDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestStore_SaveAndReadTokens(t *testing.T) {
	s := NewStore(testDB(t))
	if s.Connected() {
		t.Fatal("fresh store should not be connected")
	}
	if err := s.SaveTokens("acc-1", "ref-1"); err != nil {
		t.Fatalf("SaveTokens: %v", err)
	}
	if s.Token() != "acc-1" || s.RefreshToken() != "ref-1" {
		t.Errorf("got token=%q refresh=%q", s.Token(), s.RefreshToken())
	}
	if !s.Connected() {
		t.Error("store with token should be connected")
	}
}

func TestBuildAuthorizeURL(t *testing.T) {
	u := BuildAuthorizeURL("cid", "http://localhost:9482/callback", "st4te")
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := parsed.Query()
	for k, want := range map[string]string{
		"client_id":     "cid",
		"redirect_uri":  "http://localhost:9482/callback",
		"response_type": "code",
		"actor":         "app",
		"state":         "st4te",
		"scope":         "read,write,app:assignable,app:mentionable",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if !strings.HasPrefix(u, "https://linear.app/oauth/authorize?") {
		t.Errorf("unexpected base: %s", u)
	}
}

func TestExchange_PostsFormAndParses(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok","refresh_token":"ref","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	tr, err := Exchange(context.Background(), srv.Client(), srv.URL, "cid", "sec", "the-code", "http://localhost:9482/callback")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tr.AccessToken != "tok" || tr.RefreshToken != "ref" {
		t.Errorf("got %+v", tr)
	}
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"code":          "the-code",
		"client_id":     "cid",
		"client_secret": "sec",
		"redirect_uri":  "http://localhost:9482/callback",
	} {
		if gotForm.Get(k) != want {
			t.Errorf("form %s = %q, want %q", k, gotForm.Get(k), want)
		}
	}
}

func TestExchange_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	if _, err := Exchange(context.Background(), srv.Client(), srv.URL, "c", "s", "bad", "r"); err == nil {
		t.Fatal("expected error on 400")
	}
}

func TestSource_AuthHeaderPrefersOAuth(t *testing.T) {
	store := NewStore(testDB(t))
	src := NewSource(store, "cid", "sec", "personal-key")
	if got := src.AuthHeader(); got != "personal-key" {
		t.Errorf("no oauth token: header = %q, want raw API key", got)
	}
	store.SaveTokens("acc", "")
	src.Invalidate() // drop the memo so the new token is visible
	if got := src.AuthHeader(); got != "Bearer acc" {
		t.Errorf("with oauth token: header = %q, want Bearer acc", got)
	}
}

func TestSource_HandleUnauthorized_RefreshesOnce(t *testing.T) {
	store := NewStore(testDB(t))
	store.SaveTokens("stale", "ref-1")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "ref-1" {
			t.Errorf("unexpected refresh form: %v", r.PostForm)
		}
		w.Write([]byte(`{"access_token":"fresh","refresh_token":"ref-2"}`))
	}))
	defer srv.Close()

	src := NewSource(store, "cid", "sec", "personal-key")
	src.tokenURL = srv.URL
	src.httpClient = srv.Client()

	header, retry := src.HandleUnauthorized(context.Background())
	if !retry || header != "Bearer fresh" {
		t.Errorf("got header=%q retry=%v", header, retry)
	}
	if store.Token() != "fresh" || store.RefreshToken() != "ref-2" {
		t.Error("refreshed tokens not persisted")
	}
	if calls != 1 {
		t.Errorf("refresh called %d times, want 1", calls)
	}
}

func TestSource_HandleUnauthorized_FallsBackToAPIKey(t *testing.T) {
	store := NewStore(testDB(t))
	store.SaveTokens("stale", "ref-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()

	src := NewSource(store, "cid", "sec", "personal-key")
	src.tokenURL = srv.URL
	src.httpClient = srv.Client()

	header, retry := src.HandleUnauthorized(context.Background())
	if !retry || header != "personal-key" {
		t.Errorf("got header=%q retry=%v, want raw API key fallback", header, retry)
	}
}

func TestSource_HandleUnauthorized_NoOAuthNoRetry(t *testing.T) {
	src := NewSource(NewStore(testDB(t)), "cid", "sec", "personal-key")
	if _, retry := src.HandleUnauthorized(context.Background()); retry {
		t.Error("API-key-only auth has nothing to refresh; retry must be false")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/linearauth/ -v`
Expected: FAIL to build — package does not exist yet.

- [ ] **Step 3: Implement `internal/linearauth/linearauth.go`**

```go
// Package linearauth manages toad's Linear OAuth app credentials: a
// settings-backed token store, the authorization-code exchange and refresh
// calls, and an AuthSource that issuetracker uses to pick the right
// Authorization header (OAuth app token preferred, personal API key as the
// fallback).
package linearauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/scaler-tech/toad/internal/state"
)

const (
	settingToken        = "linear_oauth_token"
	settingRefreshToken = "linear_oauth_refresh_token"

	// DefaultTokenURL is Linear's OAuth token endpoint.
	DefaultTokenURL = "https://api.linear.app/oauth/token"

	authorizeURL = "https://linear.app/oauth/authorize"

	// Scopes requested at authorization: ticket CRUD plus the two agent
	// capabilities (assignable + mentionable).
	oauthScopes = "read,write,app:assignable,app:mentionable"
)

// Store reads and writes the OAuth tokens in the state DB settings table.
type Store struct {
	db *state.DB
}

func NewStore(db *state.DB) *Store { return &Store{db: db} }

func (s *Store) Token() string {
	v, err := s.db.GetSetting(settingToken)
	if err != nil {
		slog.Warn("reading linear oauth token", "error", err)
	}
	return v
}

func (s *Store) RefreshToken() string {
	v, err := s.db.GetSetting(settingRefreshToken)
	if err != nil {
		slog.Warn("reading linear oauth refresh token", "error", err)
	}
	return v
}

func (s *Store) SaveTokens(access, refresh string) error {
	if err := s.db.SetSetting(settingToken, access); err != nil {
		return err
	}
	if refresh != "" {
		return s.db.SetSetting(settingRefreshToken, refresh)
	}
	return nil
}

// Connected reports whether an app token is stored.
func (s *Store) Connected() bool { return s.Token() != "" }

// BuildAuthorizeURL renders the browser URL for the actor=app OAuth flow.
func BuildAuthorizeURL(clientID, redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", oauthScopes)
	q.Set("state", state)
	q.Set("actor", "app")
	return authorizeURL + "?" + q.Encode()
}

// TokenResponse is the subset of Linear's token endpoint response toad uses.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// Exchange trades an authorization code for tokens.
func Exchange(ctx context.Context, hc *http.Client, tokenURL, clientID, clientSecret, code, redirectURI string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	return postTokenForm(ctx, hc, tokenURL, form)
}

// Refresh trades a refresh token for a new access token.
func Refresh(ctx context.Context, hc *http.Client, tokenURL, clientID, clientSecret, refreshToken string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	return postTokenForm(ctx, hc, tokenURL, form)
}

func postTokenForm(ctx context.Context, hc *http.Client, tokenURL string, form url.Values) (*TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linear token endpoint returned %d: %s", resp.StatusCode, string(body))
	}
	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}
	return &tr, nil
}

// Source picks the Authorization header for Linear GraphQL calls: the stored
// OAuth app token (as "Bearer <token>") when connected, else the personal
// API key raw (Linear's personal-key convention). On a 401 it refreshes the
// OAuth token once; if that fails it falls back to the API key for the
// retry so filing keeps working.
type Source struct {
	store          *Store
	clientID       string
	clientSecret   string
	fallbackAPIKey string

	tokenURL   string
	httpClient *http.Client

	mu     sync.Mutex
	cached string // memoized header; invalidated on 401 or Invalidate()
}

func NewSource(store *Store, clientID, clientSecret, fallbackAPIKey string) *Source {
	return &Source{
		store:          store,
		clientID:       clientID,
		clientSecret:   clientSecret,
		fallbackAPIKey: fallbackAPIKey,
		tokenURL:       DefaultTokenURL,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}
}

// AuthHeader returns the current best Authorization header value ("" if no
// credential is configured at all).
func (s *Source) AuthHeader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != "" {
		return s.cached
	}
	if tok := s.store.Token(); tok != "" {
		s.cached = "Bearer " + tok
	} else {
		s.cached = s.fallbackAPIKey
	}
	return s.cached
}

// Invalidate drops the memoized header so the next AuthHeader re-reads the
// store (used after connect, and by tests).
func (s *Source) Invalidate() {
	s.mu.Lock()
	s.cached = ""
	s.mu.Unlock()
}

// HandleUnauthorized reacts to a 401: refresh the OAuth token once and
// return the new header; on refresh failure return the API-key fallback.
// Returns retry=false when there is no OAuth token in play (a bad personal
// key cannot be repaired here).
func (s *Source) HandleUnauthorized(ctx context.Context) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached = ""
	if s.store.Token() == "" {
		return "", false
	}
	if rt := s.store.RefreshToken(); rt != "" {
		tr, err := Refresh(ctx, s.httpClient, s.tokenURL, s.clientID, s.clientSecret, rt)
		if err == nil {
			if err := s.store.SaveTokens(tr.AccessToken, tr.RefreshToken); err != nil {
				slog.Warn("persisting refreshed linear token", "error", err)
			}
			s.cached = "Bearer " + tr.AccessToken
			return s.cached, true
		}
		slog.Error("linear oauth refresh failed; falling back to personal API key for this call", "error", err)
	} else {
		slog.Error("linear oauth token rejected and no refresh token stored; falling back to personal API key for this call")
	}
	if s.fallbackAPIKey == "" {
		return "", false
	}
	s.cached = s.fallbackAPIKey
	return s.cached, true
}
```

Note: `state.OpenDBAt(":memory:")` is the existing test convention (see `internal/state` tests). If the exported name differs, check `internal/state/db.go:80` (`OpenDBAt`) — do not invent a new constructor.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/linearauth/ -v`
Expected: PASS (all 8 tests).

- [ ] **Step 5: Format check**

Run: `gofmt -l internal/linearauth/` — fix with `gofmt -w` if needed. Do NOT commit.

---

### Task 2: AuthSource seam in `issuetracker.LinearTracker` (401 retry-once)

**Files:**
- Modify: `internal/issuetracker/tracker.go` (add the `AuthSource` interface near the `Tracker` interface)
- Modify: `internal/issuetracker/linear.go` (`NewLinearTracker`, `doGraphQL`; add `NewLinearTrackerWithAuth`)
- Modify: `internal/issuetracker/tracker.go` or wherever `NewTracker` lives (add `NewTrackerWithAuth`)
- Test: `internal/issuetracker/linear_test.go` (append)

**Interfaces:**
- Consumes: nothing from Task 1 directly — `AuthSource` is defined HERE (small interface, consumer-side) and `linearauth.Source` already satisfies it structurally.
- Produces:
  - `type AuthSource interface { AuthHeader() string; HandleUnauthorized(ctx context.Context) (string, bool) }` in `internal/issuetracker`
  - `func NewTrackerWithAuth(cfg config.IssueTrackerConfig, auth AuthSource) Tracker`
  - `func NewLinearTrackerWithAuth(cfg config.IssueTrackerConfig, auth AuthSource) *LinearTracker`
  - Behavior: with a non-nil `auth`, every GraphQL call uses `auth.AuthHeader()`; on HTTP 401 it calls `auth.HandleUnauthorized(ctx)` and retries the request exactly once with the returned header when retry=true. With nil auth, behavior is exactly today's (`lt.apiToken` raw).

- [ ] **Step 1: Read the existing test patterns**

Read `internal/issuetracker/linear_test.go` (first ~100 lines) to see how `LinearTracker` is constructed with a test HTTP client/transport today. Reuse that pattern — the tests below assume a helper exists or can be trivially written to point the tracker's `httpClient` at an `httptest.Server` and its GraphQL URL; `doGraphQL` posts to a fixed URL const/field. If the URL is hard-coded (`https://api.linear.app/graphql` at linear.go:282), add an unexported field `graphqlURL string` defaulted to that const so tests can override it — follow whatever the existing tests already do first.

- [ ] **Step 2: Write the failing tests**

Append to `internal/issuetracker/linear_test.go`:

```go
type fakeAuth struct {
	header      string
	retryHeader string
	retry       bool
	unauthCalls int
}

func (f *fakeAuth) AuthHeader() string { return f.header }
func (f *fakeAuth) HandleUnauthorized(ctx context.Context) (string, bool) {
	f.unauthCalls++
	return f.retryHeader, f.retry
}

func TestDoGraphQL_UsesAuthSourceHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	lt := NewLinearTrackerWithAuth(config.IssueTrackerConfig{Enabled: true, Provider: "linear"}, &fakeAuth{header: "Bearer app-token"})
	lt.httpClient = srv.Client()
	lt.graphqlURL = srv.URL

	if _, err := lt.doGraphQL(context.Background(), `query { viewer { id } }`, nil); err != nil {
		t.Fatalf("doGraphQL: %v", err)
	}
	if gotAuth != "Bearer app-token" {
		t.Errorf("Authorization = %q, want Bearer app-token", gotAuth)
	}
}

func TestDoGraphQL_401RetriesOnceWithNewHeader(t *testing.T) {
	var headers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Get("Authorization"))
		if len(headers) == 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	auth := &fakeAuth{header: "Bearer stale", retryHeader: "Bearer fresh", retry: true}
	lt := NewLinearTrackerWithAuth(config.IssueTrackerConfig{Enabled: true, Provider: "linear"}, auth)
	lt.httpClient = srv.Client()
	lt.graphqlURL = srv.URL

	if _, err := lt.doGraphQL(context.Background(), `query { viewer { id } }`, nil); err != nil {
		t.Fatalf("doGraphQL after retry: %v", err)
	}
	if len(headers) != 2 || headers[0] != "Bearer stale" || headers[1] != "Bearer fresh" {
		t.Errorf("headers = %v", headers)
	}
	if auth.unauthCalls != 1 {
		t.Errorf("HandleUnauthorized called %d times, want 1", auth.unauthCalls)
	}
}

func TestDoGraphQL_401NoRetryWhenSourceDeclines(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	lt := NewLinearTrackerWithAuth(config.IssueTrackerConfig{Enabled: true, Provider: "linear"}, &fakeAuth{header: "key", retry: false})
	lt.httpClient = srv.Client()
	lt.graphqlURL = srv.URL

	if _, err := lt.doGraphQL(context.Background(), `query { viewer { id } }`, nil); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("server called %d times, want 1 (no retry)", calls)
	}
}

func TestDoGraphQL_NilAuthKeepsLegacyHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	lt := NewLinearTracker(config.IssueTrackerConfig{Enabled: true, Provider: "linear", APIToken: "personal-key"})
	lt.httpClient = srv.Client()
	lt.graphqlURL = srv.URL

	if _, err := lt.doGraphQL(context.Background(), `query { viewer { id } }`, nil); err != nil {
		t.Fatalf("doGraphQL: %v", err)
	}
	if gotAuth != "personal-key" {
		t.Errorf("Authorization = %q, want raw personal-key", gotAuth)
	}
}
```

Adjust imports (`net/http`, `net/http/httptest`, `context`, `config`) to match the file's existing import block. If `linear_test.go` already has an httptest helper for pointing the tracker at a server, use it instead of raw construction.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/issuetracker/ -run 'TestDoGraphQL' -v`
Expected: FAIL to build — `NewLinearTrackerWithAuth`, `graphqlURL`, `AuthSource` don't exist.

- [ ] **Step 4: Implement**

In `internal/issuetracker/tracker.go`, next to the `Tracker` interface:

```go
// AuthSource supplies the Authorization header for Linear API calls and
// reacts to a 401. Implemented by linearauth.Source; defined here
// (consumer-side) so issuetracker does not import linearauth.
type AuthSource interface {
	// AuthHeader returns the current Authorization header value.
	AuthHeader() string
	// HandleUnauthorized is called after a 401. It returns a replacement
	// header and whether the request should be retried (exactly once).
	HandleUnauthorized(ctx context.Context) (string, bool)
}
```

In `internal/issuetracker/linear.go`:
- Add fields to `LinearTracker`: `auth AuthSource` and `graphqlURL string` (defaulted to `"https://api.linear.app/graphql"` in the constructors; replace the hard-coded URL at linear.go:282 with `lt.graphqlURL`).
- `func NewLinearTrackerWithAuth(cfg config.IssueTrackerConfig, auth AuthSource) *LinearTracker` — same body as `NewLinearTracker` plus `auth`; have `NewLinearTracker(cfg)` delegate to it with `nil`.
- In `doGraphQL`, replace the single `req.Header.Set("Authorization", lt.apiToken)` + do with:

```go
	header := lt.apiToken
	if lt.auth != nil {
		header = lt.auth.AuthHeader()
	}

	resp, err := lt.sendGraphQL(ctx, body, header)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && lt.auth != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		newHeader, retry := lt.auth.HandleUnauthorized(ctx)
		if !retry {
			return nil, fmt.Errorf("linear API returned 401 and auth source could not recover")
		}
		resp, err = lt.sendGraphQL(ctx, body, newHeader)
		if err != nil {
			return nil, err
		}
	}
```

with a small extracted helper (the request construction currently inline at linear.go:282-291):

```go
// sendGraphQL builds and sends one GraphQL POST with the given auth header.
// The caller owns the response body.
func (lt *LinearTracker) sendGraphQL(ctx context.Context, body []byte, authHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", lt.graphqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader)
	return lt.httpClient.Do(req)
}
```

The rest of `doGraphQL` (status check, GraphQL-error check, body read) continues on `resp` unchanged — make sure the existing `defer resp.Body.Close()` still applies to the FINAL response (restructure to close explicitly if needed).

- Add near `NewTracker` (find it — it returns `NoopTracker` when disabled):

```go
// NewTrackerWithAuth is NewTracker with an AuthSource for app-identity
// authentication. A nil auth behaves exactly like NewTracker.
func NewTrackerWithAuth(cfg config.IssueTrackerConfig, auth AuthSource) Tracker {
	// mirror NewTracker's enabled/provider switch, but construct the
	// linear tracker via NewLinearTrackerWithAuth(cfg, auth)
}
```

(Copy the actual body of `NewTracker` and thread `auth` through; keep `NewTracker` delegating to `NewTrackerWithAuth(cfg, nil)` so there is one switch, not two.)

- [ ] **Step 5: Run the package tests**

Run: `go test ./internal/issuetracker/ -v`
Expected: PASS — new tests and all 1893 lines of existing ones (auth==nil path unchanged).

- [ ] **Step 6: Format check**

Run: `gofmt -l internal/issuetracker/` — fix if needed. Do NOT commit.

---

### Task 3: `toad linear connect` command

**Files:**
- Create: `cmd/linear.go`
- Create: `cmd/linear_test.go`

**Interfaces:**
- Consumes: `linearauth.BuildAuthorizeURL`, `linearauth.Exchange`, `linearauth.NewStore(db).SaveTokens`, `linearauth.DefaultTokenURL` (Task 1); `state.OpenDB()` (existing).
- Produces: the `toad linear connect` CLI command (parent `linear` + child `connect`, the repo's first nested command); `func runConnectCallback(w http.ResponseWriter, r *http.Request, wantState string, codeCh chan<- string)` — the testable callback handler.

- [ ] **Step 1: Write the failing tests**

Create `cmd/linear_test.go`:

```go
package cmd

import (
	"net/http/httptest"
	"testing"
)

func TestConnectCallback_DeliversCode(t *testing.T) {
	codeCh := make(chan string, 1)
	req := httptest.NewRequest("GET", "/callback?code=abc123&state=expected", nil)
	w := httptest.NewRecorder()
	runConnectCallback(w, req, "expected", codeCh)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	select {
	case got := <-codeCh:
		if got != "abc123" {
			t.Errorf("code = %q", got)
		}
	default:
		t.Fatal("code not delivered")
	}
}

func TestConnectCallback_RejectsStateMismatch(t *testing.T) {
	codeCh := make(chan string, 1)
	req := httptest.NewRequest("GET", "/callback?code=abc123&state=WRONG", nil)
	w := httptest.NewRecorder()
	runConnectCallback(w, req, "expected", codeCh)
	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if len(codeCh) != 0 {
		t.Error("code must not be delivered on state mismatch")
	}
}

func TestConnectCallback_RejectsMissingCode(t *testing.T) {
	codeCh := make(chan string, 1)
	req := httptest.NewRequest("GET", "/callback?state=expected", nil)
	w := httptest.NewRecorder()
	runConnectCallback(w, req, "expected", codeCh)
	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/ -run TestConnectCallback -v`
Expected: FAIL to build — `runConnectCallback` undefined.

- [ ] **Step 3: Implement `cmd/linear.go`**

```go
package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/scaler-tech/toad/internal/linearauth"
	"github.com/scaler-tech/toad/internal/state"
)

const connectCallbackAddr = "localhost:9482"

var linearCmd = &cobra.Command{
	Use:   "linear",
	Short: "Linear app-identity commands",
}

var linearConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect toad's Linear OAuth app (actor=app) so tickets come from the app, not you",
	RunE:  runLinearConnect,
}

func init() {
	linearCmd.AddCommand(linearConnectCmd)
	rootCmd.AddCommand(linearCmd)
}

func runLinearConnect(cmd *cobra.Command, args []string) error {
	clientID := os.Getenv("TOAD_LINEAR_CLIENT_ID")
	clientSecret := os.Getenv("TOAD_LINEAR_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return errors.New("set TOAD_LINEAR_CLIENT_ID and TOAD_LINEAR_CLIENT_SECRET first (create the OAuth app in Linear workspace settings, redirect URL http://localhost:9482/callback, agent capabilities enabled)")
	}

	db, err := state.OpenDB()
	if err != nil {
		return fmt.Errorf("opening state db: %w", err)
	}
	defer db.Close()

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return err
	}
	oauthState := hex.EncodeToString(stateBytes)
	redirectURI := "http://" + connectCallbackAddr + "/callback"

	codeCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		runConnectCallback(w, r, oauthState, codeCh)
	})
	srv := &http.Server{Addr: connectCallbackAddr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "callback server: %v\n", err)
		}
	}()
	defer srv.Shutdown(context.Background())

	authURL := linearauth.BuildAuthorizeURL(clientID, redirectURI, oauthState)
	fmt.Println("Opening Linear to authorize toad as a workspace app (admin required).")
	fmt.Println("If the browser does not open, visit:\n\n  " + authURL + "\n")
	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(5 * time.Minute):
		return errors.New("timed out waiting for the OAuth callback (5m)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tr, err := linearauth.Exchange(ctx, http.DefaultClient, linearauth.DefaultTokenURL, clientID, clientSecret, code, redirectURI)
	if err != nil {
		return fmt.Errorf("exchanging authorization code: %w", err)
	}
	store := linearauth.NewStore(db)
	if err := store.SaveTokens(tr.AccessToken, tr.RefreshToken); err != nil {
		return fmt.Errorf("storing tokens: %w", err)
	}

	if name := viewerName(ctx, tr.AccessToken); name != "" {
		fmt.Printf("Connected to Linear as app identity %q. Tickets and comments now come from the app.\n", name)
	} else {
		fmt.Println("Connected to Linear as the app identity. Tickets and comments now come from the app.")
	}
	return nil
}

// runConnectCallback validates the OAuth redirect and hands the code to the
// connect flow. Split out for testing.
func runConnectCallback(w http.ResponseWriter, r *http.Request, wantState string, codeCh chan<- string) {
	if r.URL.Query().Get("state") != wantState {
		http.Error(w, "state mismatch — restart toad linear connect", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	fmt.Fprintln(w, "Toad is connected to Linear. You can close this tab.")
	select {
	case codeCh <- code:
	default:
	}
}

// viewerName asks Linear who the new token authenticates as (best-effort).
func viewerName(ctx context.Context, token string) string {
	body := []byte(`{"query":"{ viewer { name } }"}`)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.linear.app/graphql", bytesReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Viewer struct {
				Name string `json:"name"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	return out.Data.Viewer.Name
}

func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "linux":
		c = exec.Command("xdg-open", url)
	default:
		return
	}
	_ = c.Start()
}
```

`bytesReader` = `bytes.NewReader` — import `bytes` and use it directly; the name above is only to keep the snippet import-free.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/ -run TestConnectCallback -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Build + format check**

Run: `go build ./... && gofmt -l cmd/` — fix if needed. Do NOT commit.

---

### Task 4: Phase A wiring — daemon auth source, credential check, status line, CLAUDE.md

**Files:**
- Modify: `cmd/root.go` (tracker construction ~line 187; credential check after `state.OpenDB()` at line 114)
- Modify: `internal/config/config.go` (soften the api_token validation error, ~line 330)
- Modify: `cmd/status.go` (add `LinearAppConnected bool` to the API response) and `cmd/web/dashboard.html` (one kv line)
- Modify: `CLAUDE.md` (document app identity + `toad linear connect`)
- Test: `internal/config/config_test.go` (validation change), existing `cmd` tests must stay green

**Interfaces:**
- Consumes: `linearauth.NewStore`, `linearauth.NewSource` (Task 1); `issuetracker.NewTrackerWithAuth` (Task 2).
- Produces: daemon behavior — OAuth-preferred tracker auth; startup failure only when create_issues is enabled and NEITHER credential exists.

- [ ] **Step 1: Write the failing config test**

In `internal/config/config_test.go`, find the existing test covering the api_token validation error and update/add:

```go
func TestValidate_IssueTrackerMissingTokenIsNotFatal(t *testing.T) {
	cfg := defaults()
	cfg.Slack.AppToken = "xapp-1"
	cfg.Slack.BotToken = "xoxb-1"
	cfg.Repos.List = []RepoConfig{{Name: "r", Path: t.TempDir()}}
	cfg.IssueTracker.Enabled = true
	cfg.IssueTracker.CreateIssues = true
	cfg.IssueTracker.TeamID = "team"
	cfg.IssueTracker.APIToken = ""
	if err := Validate(cfg); err != nil {
		t.Errorf("missing api_token must not fail Validate (OAuth may be connected; daemon checks post-DB-open): %v", err)
	}
}
```

(Adapt the required Slack/repo fields to whatever the existing passing validation tests set up — copy a working fixture from a neighboring test.)

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/config/ -run TestValidate_IssueTrackerMissingTokenIsNotFatal -v`
Expected: FAIL — Validate currently errors on empty APIToken.

- [ ] **Step 3: Implement**

1. `internal/config/config.go` (~line 330): delete the `APIToken == ""` error from `Validate` (keep the `TeamID` check). The credential check moves to the daemon where the OAuth store is visible.

2. `cmd/root.go`, after `stateDB, err := state.OpenDB()` succeeds (line ~114) and before the tracker is built (~187), insert the auth wiring + moved check:

```go
	// Linear auth: prefer the connected OAuth app identity (toad linear
	// connect), fall back to the personal API key. The api_token validation
	// moved here from config.Validate because only the daemon can see the
	// stored OAuth token.
	linearStore := linearauth.NewStore(stateDB)
	if cfg.IssueTracker.Enabled && cfg.IssueTracker.CreateIssues &&
		cfg.IssueTracker.APIToken == "" && !linearStore.Connected() {
		return fmt.Errorf("issue_tracker.create_issues is enabled but no Linear credential is configured: run 'toad linear connect' (app identity) or set TOAD_LINEAR_API_TOKEN")
	}
	linearAuth := linearauth.NewSource(linearStore,
		os.Getenv("TOAD_LINEAR_CLIENT_ID"), os.Getenv("TOAD_LINEAR_CLIENT_SECRET"),
		cfg.IssueTracker.APIToken)
```

3. Replace `tracker := issuetracker.NewTracker(cfg.IssueTracker)` (root.go:187) with:

```go
	tracker := issuetracker.NewTrackerWithAuth(cfg.IssueTracker, linearAuth)
```

4. `cmd/status.go`: add `LinearAppConnected bool \`json:"linear_app_connected"\`` to the top-level `apiResponse` struct (~line 116) and populate it in `apiDataHandler` via `linearauth.NewStore(db).Connected()`. In `cmd/web/dashboard.html`, in the daemon-metrics block (~line 838 pattern), add:

```js
reposHTML += `<div class="kv" style="margin-top:6px"><span>linear identity</span><b>${d.linear_app_connected ? 'app (connected)' : 'personal API key'}</b></div>`;
```

(Place it with the other daemon kv lines; match surrounding quoting/style.)

5. `CLAUDE.md`: in Important Details, after the `release_notes.channel` bullet, add:

```markdown
- Linear identity: `toad linear connect` runs the actor=app OAuth flow (client credentials from `TOAD_LINEAR_CLIENT_ID`/`TOAD_LINEAR_CLIENT_SECRET`, callback on `localhost:9482`) and stores the app token in the `settings` table (`linear_oauth_token`); `LinearTracker` prefers it (`Bearer`) over the personal `TOAD_LINEAR_API_TOKEN` (raw header), refreshing once on 401 with API-key fallback, so tickets/comments come from the app identity when connected
```

- [ ] **Step 4: Run the affected packages**

Run: `go test ./internal/config/ ./cmd/ ./internal/issuetracker/ -count=1`
Expected: PASS.

- [ ] **Step 5: Build + format check**

Run: `go build ./... && gofmt -l .` — fix if needed. Do NOT commit. **Phase A is complete and releasable here.**

---

### Task 5: Schema v13 — `agent_sessions` table + accessors

**Files:**
- Modify: `internal/state/db.go` (base schema block ~line 177 region, migrations slice ~line 280, new accessors near the investigations block ~line 1351)
- Test: `internal/state/db_test.go` (append)

**Interfaces:**
- Produces (Task 7/8 rely on these exact names):

```go
type AgentSessionRecord struct {
	SessionID             string
	IssueID               string // Linear internal UUID
	IssueIdentifier       string // e.g. "PLF-3125"
	Status                string
	LastHandledActivityAt time.Time
	UpdatedAt             time.Time
}
func (d *DB) GetAgentSession(sessionID string) (*AgentSessionRecord, error) // nil,nil when absent
func (d *DB) UpsertAgentSession(rec *AgentSessionRecord) error
```

- [ ] **Step 1: Write the failing tests**

Append to `internal/state/db_test.go` (match the file's existing in-memory-DB test helper):

```go
func TestAgentSessions_UpsertAndGet(t *testing.T) {
	db := testDB(t) // reuse the file's existing helper name for an in-memory DB
	got, err := db.GetAgentSession("sess-1")
	if err != nil || got != nil {
		t.Fatalf("empty get = %v, %v; want nil, nil", got, err)
	}
	rec := &AgentSessionRecord{
		SessionID: "sess-1", IssueID: "uuid-1", IssueIdentifier: "PLF-1",
		Status: "active", LastHandledActivityAt: time.Now().UTC().Truncate(time.Second),
		UpdatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := db.UpsertAgentSession(rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err = db.GetAgentSession("sess-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v, %v", got, err)
	}
	if got.IssueIdentifier != "PLF-1" || got.Status != "active" {
		t.Errorf("got %+v", got)
	}
	// Upsert overwrites.
	rec.Status = "complete"
	if err := db.UpsertAgentSession(rec); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, _ = db.GetAgentSession("sess-1")
	if got.Status != "complete" {
		t.Errorf("status = %q after re-upsert", got.Status)
	}
}
```

If the existing helper for an in-memory DB has a different name than `testDB`, use that name — check the top of `db_test.go` first.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/state/ -run TestAgentSessions -v`
Expected: FAIL to build.

- [ ] **Step 3: Implement**

1. Base schema block (next to the `investigations` CREATE TABLE, db.go ~177): add

```sql
CREATE TABLE IF NOT EXISTS agent_sessions (
	session_id               TEXT PRIMARY KEY,
	issue_id                 TEXT,
	issue_identifier         TEXT,
	status                   TEXT,
	last_handled_activity_at DATETIME,
	updated_at               DATETIME
);
```

2. Migrations slice (after the v12 entry, db.go ~line 290), same dual-path convention as v10/v12:

```go
// v13: linear agent sessions — dedup/progress record for the polled
// agent-session flow (internal/linearagent). New table, so CREATE TABLE
// IF NOT EXISTS is safe to replay; mirrored into the base schema block
// for fresh installs.
{13, `CREATE TABLE IF NOT EXISTS agent_sessions (
	  session_id               TEXT PRIMARY KEY,
	  issue_id                 TEXT,
	  issue_identifier         TEXT,
	  status                   TEXT,
	  last_handled_activity_at DATETIME,
	  updated_at               DATETIME)`},
```

3. Accessors near the investigations block (db.go ~1351), following its scan/error style:

```go
// AgentSessionRecord is toad's handled-state for one Linear agent session.
// LastHandledActivityAt is written only after a response posts, so a crash
// mid-processing leaves the session detectable as unhandled on the next poll.
type AgentSessionRecord struct {
	SessionID             string
	IssueID               string
	IssueIdentifier       string
	Status                string
	LastHandledActivityAt time.Time
	UpdatedAt             time.Time
}

func (d *DB) UpsertAgentSession(rec *AgentSessionRecord) error {
	return d.dbRetry(func() error {
		_, err := d.db.Exec(`INSERT OR REPLACE INTO agent_sessions
			(session_id, issue_id, issue_identifier, status, last_handled_activity_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			rec.SessionID, rec.IssueID, rec.IssueIdentifier, rec.Status,
			rec.LastHandledActivityAt, rec.UpdatedAt)
		return err
	})
}

func (d *DB) GetAgentSession(sessionID string) (*AgentSessionRecord, error) {
	var rec AgentSessionRecord
	err := d.db.QueryRow(`SELECT session_id, issue_id, issue_identifier, status,
			COALESCE(last_handled_activity_at, '0001-01-01 00:00:00'), updated_at
		FROM agent_sessions WHERE session_id = ?`, sessionID).
		Scan(&rec.SessionID, &rec.IssueID, &rec.IssueIdentifier, &rec.Status,
			&rec.LastHandledActivityAt, &rec.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}
```

Match the exact `dbRetry` invocation style used by `SaveInvestigation` (db.go:1362) — if it wraps differently (e.g. `dbRetry(...)` free function), copy that shape.

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/state/ -count=1`
Expected: PASS, including existing migration tests (version now lands at 13).
If a test asserts the schema version equals 12, update it to 13 — that assertion's job is to move with the schema.

- [ ] **Step 5: Format check**

Run: `gofmt -l internal/state/` — fix if needed. Do NOT commit.

---

### Task 6: `internal/linearagent` — GraphQL client (sessions list + activity create)

**Files:**
- Create: `internal/linearagent/client.go`
- Create: `internal/linearagent/client_test.go`

**Interfaces:**
- Consumes: `issuetracker.AuthSource` (Task 2's interface — `linearauth.Source` satisfies it).
- Produces (Tasks 7/8 rely on these exact names):

```go
type Session struct {
	ID              string
	Status          string // pending|active|complete|awaitingInput|error|stale
	CreatedAt       time.Time
	UpdatedAt       time.Time
	IssueID         string // Linear internal UUID
	IssueIdentifier string // "PLF-3125"
	IssueTitle      string
	SourceComment   string // body of the mention/delegation comment ("" if none)
	Activities      []Activity
}
type Activity struct {
	CreatedAt time.Time
	Type      string // "prompt"|"thought"|"action"|"response"|"error"|"elicitation"
	Body      string
}
func (a Activity) IsUser() bool // Type == "prompt"

type Client struct { /* auth issuetracker.AuthSource; httpClient *http.Client; graphqlURL string */ }
func NewClient(auth issuetracker.AuthSource) *Client
func (c *Client) ListSessions(ctx context.Context, first int) ([]Session, error)
func (c *Client) CreateActivity(ctx context.Context, sessionID, activityType, body string) error
```

- [ ] **Step 1: Write the failing tests**

Create `internal/linearagent/client_test.go`:

```go
package linearagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type staticAuth struct{ header string }

func (s staticAuth) AuthHeader() string { return s.header }
func (s staticAuth) HandleUnauthorized(ctx context.Context) (string, bool) { return "", false }

const sessionsFixture = `{"data":{"agentSessions":{"nodes":[
  {"id":"sess-1","status":"pending","createdAt":"2026-08-10T10:00:00.000Z","updatedAt":"2026-08-10T10:00:00.000Z",
   "issue":{"id":"uuid-1","identifier":"PLF-9","title":"Exports are slow"},
   "sourceComment":{"body":"@toad can you look at this?"},
   "activities":{"nodes":[]}},
  {"id":"sess-2","status":"active","createdAt":"2026-08-10T09:00:00.000Z","updatedAt":"2026-08-10T09:30:00.000Z",
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/linearagent/ -v`
Expected: FAIL to build — package doesn't exist.

- [ ] **Step 3: Implement `internal/linearagent/client.go`**

```go
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
}

func NewClient(auth issuetracker.AuthSource) *Client {
	return &Client{
		auth:       auth,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		graphqlURL: "https://api.linear.app/graphql",
	}
}

const listSessionsQuery = `query ToadAgentSessions($first: Int!) {
  agentSessions(first: $first, orderBy: updatedAt) {
    nodes {
      id status createdAt updatedAt
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

// ListSessions fetches the app's most recently updated agent sessions.
func (c *Client) ListSessions(ctx context.Context, first int) ([]Session, error) {
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
				Issue     *struct {
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/linearagent/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Format check**

Run: `gofmt -l internal/linearagent/` — fix if needed. Do NOT commit.

---

### Task 7: `internal/linearagent` — work detection + poller

**Files:**
- Create: `internal/linearagent/poller.go`
- Create: `internal/linearagent/poller_test.go`

**Interfaces:**
- Consumes: `Session`/`Activity` (Task 6), `state.AgentSessionRecord`/`GetAgentSession` (Task 5).
- Produces (Task 8/9 rely on these):

```go
type Work struct {
	Session     Session
	Prompt      string    // text toad should answer: latest unhandled prompt, or SourceComment for a new session
	FollowUp    bool      // true when toad has answered this session before
	TriggeredAt time.Time // timestamp of the user event being answered (becomes LastHandledActivityAt)
}
// DetectWork returns nil when the session needs nothing from toad.
func DetectWork(s Session, rec *state.AgentSessionRecord) *Work

type SessionLister interface {
	ListSessions(ctx context.Context, first int) ([]Session, error)
}
type Poller struct { /* lister SessionLister; db *state.DB; interval time.Duration; handle func(context.Context, Work) */ }
func NewPoller(lister SessionLister, db *state.DB, interval time.Duration, handle func(context.Context, Work)) *Poller
func (p *Poller) Run(ctx context.Context) // ticker loop; returns on ctx.Done()
```

- [ ] **Step 1: Write the failing tests**

Create `internal/linearagent/poller_test.go`:

```go
package linearagent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/state"
)

func ts(min int) time.Time {
	return time.Date(2026, 8, 10, 10, min, 0, 0, time.UTC)
}

func TestDetectWork_NewSessionNoActivities(t *testing.T) {
	s := Session{ID: "s1", Status: "pending", CreatedAt: ts(0), SourceComment: "@toad look at PLF-9"}
	w := DetectWork(s, nil)
	if w == nil {
		t.Fatal("new session must be work")
	}
	if w.FollowUp || w.Prompt != "@toad look at PLF-9" || !w.TriggeredAt.Equal(ts(0)) {
		t.Errorf("work = %+v", w)
	}
}

func TestDetectWork_TerminalStatusesSkipped(t *testing.T) {
	for _, status := range []string{"complete", "error", "stale"} {
		s := Session{ID: "s1", Status: status, CreatedAt: ts(0), SourceComment: "hi"}
		if DetectWork(s, nil) != nil {
			t.Errorf("status %q must not be work", status)
		}
	}
}

func TestDetectWork_HandledSessionNoNewPrompt(t *testing.T) {
	s := Session{ID: "s1", Status: "active", CreatedAt: ts(0), SourceComment: "hi",
		Activities: []Activity{
			{CreatedAt: ts(1), Type: "thought", Body: "Reading."},
			{CreatedAt: ts(5), Type: "response", Body: "Done."},
		}}
	rec := &state.AgentSessionRecord{SessionID: "s1", LastHandledActivityAt: ts(0)}
	if w := DetectWork(s, rec); w != nil {
		t.Errorf("handled session with no new prompt must not be work, got %+v", w)
	}
}

func TestDetectWork_FollowUpPrompt(t *testing.T) {
	s := Session{ID: "s1", Status: "active", CreatedAt: ts(0), SourceComment: "hi",
		Activities: []Activity{
			{CreatedAt: ts(5), Type: "response", Body: "Done."},
			{CreatedAt: ts(9), Type: "prompt", Body: "what about the retry path?"},
		}}
	rec := &state.AgentSessionRecord{SessionID: "s1", LastHandledActivityAt: ts(0)}
	w := DetectWork(s, rec)
	if w == nil {
		t.Fatal("new prompt after handled point must be work")
	}
	if !w.FollowUp || w.Prompt != "what about the retry path?" || !w.TriggeredAt.Equal(ts(9)) {
		t.Errorf("work = %+v", w)
	}
}

func TestDetectWork_UnhandledNewSessionWithOwnAckOnly(t *testing.T) {
	// Crash case: toad acked (thought) but never responded and never wrote
	// the record — the session must still be detected as work.
	s := Session{ID: "s1", Status: "active", CreatedAt: ts(0), SourceComment: "hi",
		Activities: []Activity{{CreatedAt: ts(1), Type: "thought", Body: "Reading."}}}
	if w := DetectWork(s, nil); w == nil {
		t.Fatal("session without a stored record must be re-detected as work")
	}
}

func TestPoller_DispatchesDetectedWork(t *testing.T) {
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer db.Close()

	lister := &fakeLister{sessions: []Session{
		{ID: "s1", Status: "pending", CreatedAt: ts(0), SourceComment: "check this"},
	}}
	var mu sync.Mutex
	var handled []Work
	p := NewPoller(lister, db, 10*time.Millisecond, func(ctx context.Context, w Work) {
		mu.Lock()
		handled = append(handled, w)
		mu.Unlock()
		// Simulate the processor completing: write the handled record.
		db.UpsertAgentSession(&state.AgentSessionRecord{
			SessionID: w.Session.ID, Status: w.Session.Status,
			LastHandledActivityAt: w.TriggeredAt, UpdatedAt: time.Now().UTC(),
		})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 1 {
		t.Fatalf("handled %d times, want exactly 1 (dedup after record write)", len(handled))
	}
	if handled[0].Session.ID != "s1" {
		t.Errorf("handled = %+v", handled[0])
	}
}

type fakeLister struct{ sessions []Session }

func (f *fakeLister) ListSessions(ctx context.Context, first int) ([]Session, error) {
	return f.sessions, nil
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/linearagent/ -run 'TestDetectWork|TestPoller' -v`
Expected: FAIL to build.

- [ ] **Step 3: Implement `internal/linearagent/poller.go`**

```go
package linearagent

import (
	"context"
	"log/slog"
	"time"

	"github.com/scaler-tech/toad/internal/state"
)

// Work is one unhandled user request found in a session.
type Work struct {
	Session     Session
	Prompt      string
	FollowUp    bool
	TriggeredAt time.Time
}

// DetectWork compares a live session snapshot against toad's handled-state
// record. It returns nil when the session needs nothing. The record's
// LastHandledActivityAt is written only after a response posts, so any
// crash before that leaves the work re-detectable here.
func DetectWork(s Session, rec *state.AgentSessionRecord) *Work {
	switch s.Status {
	case "complete", "error", "stale":
		return nil
	}

	// The latest user event: the newest prompt activity, or session
	// creation itself (mention/delegation) when no prompt exists yet.
	latestPromptAt := time.Time{}
	prompt := ""
	for _, a := range s.Activities {
		if a.IsUser() && a.CreatedAt.After(latestPromptAt) {
			latestPromptAt = a.CreatedAt
			prompt = a.Body
		}
	}
	triggeredAt := s.CreatedAt
	if !latestPromptAt.IsZero() {
		triggeredAt = latestPromptAt
	}
	if prompt == "" {
		prompt = s.SourceComment
	}

	if rec != nil && !rec.LastHandledActivityAt.Before(triggeredAt) {
		return nil // already answered this user event
	}
	return &Work{
		Session:     s,
		Prompt:      prompt,
		FollowUp:    rec != nil,
		TriggeredAt: triggeredAt,
	}
}

// SessionLister is the slice of Client the poller needs (test seam, and the
// spot a webhook-fed intake would replace).
type SessionLister interface {
	ListSessions(ctx context.Context, first int) ([]Session, error)
}

// Poller periodically lists sessions and dispatches detected work. It is
// toad's polling-based Intake: no inbound HTTP, at the cost of up to one
// interval of latency (Linear may briefly show the agent as unresponsive).
type Poller struct {
	lister   SessionLister
	db       *state.DB
	interval time.Duration
	handle   func(context.Context, Work)

	inFlight map[string]bool // session IDs currently being processed
}

func NewPoller(lister SessionLister, db *state.DB, interval time.Duration, handle func(context.Context, Work)) *Poller {
	return &Poller{lister: lister, db: db, interval: interval, handle: handle, inFlight: make(map[string]bool)}
}

// Run polls until ctx is done. Work is handled synchronously per tick, one
// session at a time — the handler itself fans out to the investigation
// semaphore, so the poller stays simple and cannot double-dispatch.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.pollOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	sessions, err := p.lister.ListSessions(ctx, 50)
	if err != nil {
		slog.Warn("linear agent poll failed", "error", err)
		return
	}
	for _, s := range sessions {
		if p.inFlight[s.ID] {
			continue
		}
		rec, err := p.db.GetAgentSession(s.ID)
		if err != nil {
			slog.Warn("reading agent session record", "session", s.ID, "error", err)
			continue
		}
		w := DetectWork(s, rec)
		if w == nil {
			continue
		}
		p.inFlight[s.ID] = true
		p.handle(ctx, *w)
		delete(p.inFlight, s.ID)
	}
}
```

Note for the reviewer: `inFlight` is single-goroutine state (the poller loop is the only writer, and `handle` is called synchronously) — no mutex needed. If Task 8's processor is wired to run asynchronously instead, the poller must NOT mark/unmark synchronously; keep the synchronous contract.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/linearagent/ -count=1`
Expected: PASS (client + poller tests).

- [ ] **Step 5: Format check**

Run: `gofmt -l internal/linearagent/` — fix if needed. Do NOT commit.

---

### Task 8: `internal/linearagent` — session processor + response composition

**Files:**
- Create: `internal/linearagent/processor.go`
- Create: `internal/linearagent/processor_test.go`

**Interfaces:**
- Consumes: `Work` (Task 7), `Client.CreateActivity` semantics (Task 6), `state.DB` (`FindInvestigationByTicket` db.go:1390, `SaveInvestigation` db.go:1362, `UpsertAgentSession` Task 5), `investigation.Findings`.
- Produces (Task 9 wires this):

```go
type ActivityPoster interface {
	CreateActivity(ctx context.Context, sessionID, activityType, body string) error
}
type ProcessorOpts struct {
	Poster      ActivityPoster
	DB          *state.DB
	Claim       func(key, scope string) bool  // stateManager.ClaimScoped
	Unclaim     func(key, scope string)       // stateManager.UnclaimScoped
	Investigate func(ctx context.Context, w Work) (*investigation.Findings, error)
	Timeout     time.Duration
}
func NewProcessor(opts ProcessorOpts) *Processor
func (p *Processor) Handle(ctx context.Context, w Work) // the poller's handle func
func composeResponse(f *investigation.Findings) string
```

Behavior contract (the tests below pin it):
1. Post `thought` ack FIRST, before any slow work.
2. `Claim(issueIdentifier, "linear-agent")`; if it fails, post a `response` saying an investigation is already running and stop (do NOT mark handled — the next prompt will retrigger). Release via defer.
3. Reuse: `DB.FindInvestigationByTicket(issueIdentifier)`; if found, feasible, and `CreatedAt.After(w.TriggeredAt.Add(-24 * time.Hour))` AND not a follow-up, answer from stored findings without re-investigating. Follow-ups always re-investigate (the user asked something new).
4. Fresh run: `Investigate(ctx, w)`; save via `SaveInvestigation` with `ThreadTS: "linear-session:" + w.Session.ID`, `Channel: "linear"`.
5. Post `composeResponse(findings)` as a `response` activity; then `UpsertAgentSession` with `LastHandledActivityAt: w.TriggeredAt` — ONLY after the response posts.
6. Any error → post an `error` activity with a one-line reason; do NOT write the handled record.

- [ ] **Step 1: Write the failing tests**

Create `internal/linearagent/processor_test.go`:

```go
package linearagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/state"
)

type postedActivity struct{ Type, Body string }

type fakePoster struct{ posted []postedActivity }

func (f *fakePoster) CreateActivity(ctx context.Context, sessionID, activityType, body string) error {
	f.posted = append(f.posted, postedActivity{activityType, body})
	return nil
}

func testFindings() *investigation.Findings {
	return &investigation.Findings{
		Feasible: true, Title: "Export double-counts refunds",
		Problem:   "Totals are 2x for partial refunds.",
		RootCause: "aggregate() sums superseded rows (billing/export/aggregate.py:118).",
		Evidence: []investigation.Evidence{
			{Kind: "file", Ref: "billing/export/aggregate.py:118", Note: "no supersede filter"},
		},
		Confidence: 0.8, Repo: "billing",
		Reasoning: "The export double-counts partial refunds. The fix is one file.",
	}
}

func procDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func work() Work {
	return Work{
		Session: Session{ID: "sess-1", Status: "pending", CreatedAt: time.Now().Add(-time.Minute),
			IssueID: "uuid-1", IssueIdentifier: "PLF-9", IssueTitle: "Exports are slow"},
		Prompt:      "@toad why are exports slow?",
		TriggeredAt: time.Now().Add(-time.Minute),
	}
}

func newTestProcessor(db *state.DB, poster *fakePoster, investigate func(ctx context.Context, w Work) (*investigation.Findings, error)) *Processor {
	claims := map[string]bool{}
	return NewProcessor(ProcessorOpts{
		Poster: poster,
		DB:     db,
		Claim: func(key, scope string) bool {
			k := key + "/" + scope
			if claims[k] {
				return false
			}
			claims[k] = true
			return true
		},
		Unclaim:     func(key, scope string) { delete(claims, key+"/"+scope) },
		Investigate: investigate,
		Timeout:     time.Minute,
	})
}

func TestHandle_AckThenInvestigateThenRespond(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		return testFindings(), nil
	})
	w := work()
	p.Handle(context.Background(), w)

	if len(poster.posted) != 2 {
		t.Fatalf("posted %d activities: %+v", len(poster.posted), poster.posted)
	}
	if poster.posted[0].Type != "thought" {
		t.Errorf("first activity = %+v, want thought ack", poster.posted[0])
	}
	if poster.posted[1].Type != "response" || !strings.Contains(poster.posted[1].Body, "double-counts") {
		t.Errorf("second activity = %+v", poster.posted[1])
	}
	// Handled record written with the trigger time.
	rec, _ := db.GetAgentSession("sess-1")
	if rec == nil || !rec.LastHandledActivityAt.Equal(w.TriggeredAt) {
		t.Errorf("record = %+v", rec)
	}
	// Findings persisted for later reuse.
	inv, _ := db.GetInvestigationByThread("linear-session:sess-1")
	if inv == nil {
		t.Error("investigation not persisted")
	}
}

func TestHandle_InvestigationErrorPostsErrorActivity(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		return nil, errors.New("agent exploded")
	})
	p.Handle(context.Background(), work())

	last := poster.posted[len(poster.posted)-1]
	if last.Type != "error" {
		t.Errorf("last activity = %+v, want error", last)
	}
	if rec, _ := db.GetAgentSession("sess-1"); rec != nil {
		t.Error("failed handling must not write the handled record")
	}
}

func TestHandle_ClaimConflictStopsWithoutHandledRecord(t *testing.T) {
	db := procDB(t)
	poster := &fakePoster{}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		t.Fatal("must not investigate on claim conflict")
		return nil, nil
	})
	// Occupy the claim.
	if !p.opts.Claim("PLF-9", "linear-agent") {
		t.Fatal("setup claim failed")
	}
	p.Handle(context.Background(), work())
	if rec, _ := db.GetAgentSession("sess-1"); rec != nil {
		t.Error("claim conflict must not write the handled record")
	}
}

func TestHandle_ReusesFreshFindings(t *testing.T) {
	db := procDB(t)
	f := testFindings()
	fj, _ := json.Marshal(f)
	// A prior filing on this issue links ticket_index -> investigations.
	db.SaveInvestigation(&state.InvestigationRecord{
		ID: "inv-1", ThreadTS: "slack-thread", Channel: "C1",
		FindingsJSON: string(fj), CreatedAt: time.Now(),
	})
	db.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey: "thread:C1:slack-thread", IssueID: "PLF-9",
		InvestigationID: "inv-1", CreatedAt: time.Now(), LastSeenAt: time.Now(),
	})

	poster := &fakePoster{}
	p := newTestProcessor(db, poster, func(ctx context.Context, w Work) (*investigation.Findings, error) {
		t.Fatal("fresh findings exist; must not re-investigate")
		return nil, nil
	})
	p.Handle(context.Background(), work())

	last := poster.posted[len(poster.posted)-1]
	if last.Type != "response" || !strings.Contains(last.Body, "double-counts") {
		t.Errorf("last = %+v", last)
	}
}

func TestComposeResponse_LeadsWithReasoningAndCitesEvidence(t *testing.T) {
	body := composeResponse(testFindings())
	if !strings.HasPrefix(body, "The export double-counts partial refunds.") {
		t.Errorf("must lead with reasoning, got: %q", body)
	}
	if !strings.Contains(body, "billing/export/aggregate.py:118") {
		t.Error("must cite evidence refs")
	}
}

func TestComposeResponse_InfeasibleStatesItPlainly(t *testing.T) {
	f := testFindings()
	f.Feasible = false
	f.Reasoning = "I could not confirm the root cause. I searched the export and billing packages."
	body := composeResponse(f)
	if !strings.Contains(body, "could not confirm") {
		t.Errorf("infeasible response must carry the reasoning, got: %q", body)
	}
}
```

Check `state.TicketIndexEntry` field names against db.go:1207-1314 before writing the reuse test — the entry MUST link `IssueID: "PLF-9"` to `InvestigationID: "inv-1"` for `FindInvestigationByTicket("PLF-9")` to return `inv-1`. Adjust field names to the real struct if they differ.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/linearagent/ -run 'TestHandle|TestComposeResponse' -v`
Expected: FAIL to build.

- [ ] **Step 3: Implement `internal/linearagent/processor.go`**

```go
package linearagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/state"
)

// ActivityPoster is the slice of Client the processor needs.
type ActivityPoster interface {
	CreateActivity(ctx context.Context, sessionID, activityType, body string) error
}

// ProcessorOpts wires the processor to the daemon (callback style, like
// digest.EngineOpts).
type ProcessorOpts struct {
	Poster      ActivityPoster
	DB          *state.DB
	Claim       func(key, scope string) bool
	Unclaim     func(key, scope string)
	Investigate func(ctx context.Context, w Work) (*investigation.Findings, error)
	Timeout     time.Duration
}

// Processor answers one session's unhandled work: ack, claim, investigate
// (or reuse), respond. Sessions never file tickets and never mutate issues.
type Processor struct {
	opts ProcessorOpts
}

func NewProcessor(opts ProcessorOpts) *Processor { return &Processor{opts: opts} }

const claimScope = "linear-agent"

// Handle processes one Work item. It is the poller's handle callback.
func (p *Processor) Handle(ctx context.Context, w Work) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	// Ack immediately — Linear marks silent sessions unresponsive.
	if err := p.opts.Poster.CreateActivity(ctx, w.Session.ID, "thought", "Reading the ticket and the code."); err != nil {
		slog.Warn("posting session ack", "session", w.Session.ID, "error", err)
	}

	claimKey := w.Session.IssueIdentifier
	if claimKey == "" {
		claimKey = w.Session.ID
	}
	if !p.opts.Claim(claimKey, claimScope) {
		// Another flow is already investigating this issue. Say so; the
		// handled record stays unwritten so a later prompt retriggers.
		p.post(ctx, w.Session.ID, "response", "An investigation is already running for this issue. Ask again in a few minutes.")
		return
	}
	defer p.opts.Unclaim(claimKey, claimScope)

	findings, err := p.findFindings(ctx, w)
	if err != nil {
		p.post(ctx, w.Session.ID, "error", "The investigation failed: "+firstLine(err.Error()))
		return
	}

	if err := p.opts.Poster.CreateActivity(ctx, w.Session.ID, "response", composeResponse(findings)); err != nil {
		slog.Warn("posting session response", "session", w.Session.ID, "error", err)
		return // handled record unwritten -> retried next poll
	}

	if err := p.opts.DB.UpsertAgentSession(&state.AgentSessionRecord{
		SessionID: w.Session.ID, IssueID: w.Session.IssueID,
		IssueIdentifier: w.Session.IssueIdentifier, Status: w.Session.Status,
		LastHandledActivityAt: w.TriggeredAt, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		slog.Warn("recording handled session", "session", w.Session.ID, "error", err)
	}
}

// findFindings returns stored findings when they are fresh and the ask is
// not a follow-up; otherwise it runs a new investigation and persists it.
func (p *Processor) findFindings(ctx context.Context, w Work) (*investigation.Findings, error) {
	if !w.FollowUp && w.Session.IssueIdentifier != "" {
		if rec, err := p.opts.DB.FindInvestigationByTicket(w.Session.IssueIdentifier); err == nil && rec != nil {
			if time.Since(rec.CreatedAt) < 24*time.Hour {
				var f investigation.Findings
				if err := json.Unmarshal([]byte(rec.FindingsJSON), &f); err == nil && f.Feasible {
					slog.Info("linear session reusing stored findings", "session", w.Session.ID, "investigation", rec.ID)
					return &f, nil
				}
			}
		}
	}

	f, err := p.opts.Investigate(ctx, w)
	if err != nil {
		return nil, err
	}
	fj, _ := json.Marshal(f)
	rec := &state.InvestigationRecord{
		ID:           fmt.Sprintf("linv-%d", time.Now().UnixNano()),
		ThreadTS:     "linear-session:" + w.Session.ID,
		Channel:      "linear",
		Repo:         f.Repo,
		FindingsJSON: string(fj),
		CreatedAt:    time.Now().UTC(),
	}
	if err := p.opts.DB.SaveInvestigation(rec); err != nil {
		slog.Warn("persisting session investigation", "session", w.Session.ID, "error", err)
	}
	return f, nil
}

func (p *Processor) post(ctx context.Context, sessionID, activityType, body string) {
	if err := p.opts.Poster.CreateActivity(ctx, sessionID, activityType, body); err != nil {
		slog.Warn("posting session activity", "session", sessionID, "type", activityType, "error", err)
	}
}

// composeResponse renders findings as a session response: the reasoning
// prose first (it already follows the STE style rules — the investigation
// prompt injects them), then evidence references.
func composeResponse(f *investigation.Findings) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(f.Reasoning))
	if len(f.Evidence) > 0 {
		b.WriteString("\n\nEvidence:\n")
		for _, e := range f.Evidence {
			if e.Note != "" {
				fmt.Fprintf(&b, "- `%s` — %s\n", e.Ref, e.Note)
			} else {
				fmt.Fprintf(&b, "- `%s`\n", e.Ref)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
```

- [ ] **Step 4: Run all package tests**

Run: `go test ./internal/linearagent/ -count=1`
Expected: PASS (client, poller, processor, compose).

- [ ] **Step 5: Format check**

Run: `gofmt -l internal/linearagent/` — fix if needed. Do NOT commit.

---

### Task 9: Phase B wiring — config block, daemon startup, Investigate bridge, docs, full gate

**Files:**
- Modify: `internal/config/config.go` (add `LinearAgentConfig`, defaults)
- Modify: `cmd/root.go` (start the poller)
- Create: `cmd/linearagentflow.go` (the Investigate bridge)
- Modify: `CLAUDE.md` (document the agent flow + config)
- Test: `internal/config/config_test.go` (defaults), `cmd/` builds; full repo gate

**Interfaces:**
- Consumes: `linearagent.NewClient/NewPoller/NewProcessor/Work` (Tasks 6–8), `linearauth.Store.Connected` (Task 1), `flowDeps` fields (root.go:274-283: `stateManager`, `tracker`, `resolver`, `investRunner`, `investigateSem`, `investigateTimeout`).
- Produces: running daemon behavior.

- [ ] **Step 1: Write the failing config test**

Append to `internal/config/config_test.go`:

```go
func TestDefaults_LinearAgent(t *testing.T) {
	cfg := defaults()
	if !cfg.LinearAgent.Enabled {
		t.Error("linear_agent.enabled should default true")
	}
	if cfg.LinearAgent.PollSeconds != 15 {
		t.Errorf("poll_seconds default = %d, want 15", cfg.LinearAgent.PollSeconds)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ -run TestDefaults_LinearAgent -v`
Expected: FAIL to build (`cfg.LinearAgent` undefined).

- [ ] **Step 3: Implement config**

In `internal/config/config.go`:

```go
// LinearAgentConfig controls the polled Linear agent-session flow
// (mentions/delegations answered with investigations).
type LinearAgentConfig struct {
	Enabled     bool `yaml:"enabled"`      // default: true (needs a connected app token to start)
	PollSeconds int  `yaml:"poll_seconds"` // default: 15, min 5
}
```

Add `LinearAgent LinearAgentConfig \`yaml:"linear_agent"\`` to `Config` (after `IssueTracker`), and to `defaults()`:

```go
LinearAgent: LinearAgentConfig{
	Enabled:     true,
	PollSeconds: 15,
},
```

In `Validate` (or a normalization step in `Load` if that's where clamping lives — follow the existing pattern for other minimums): clamp `PollSeconds < 5` to 5.

- [ ] **Step 4: Implement the bridge — `cmd/linearagentflow.go`**

```go
package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/linearagent"
)

// linearAgentInvestigate bridges a Linear agent session to the standard
// read-only investigation: fetch the issue (title, description, up to 20
// comments), resolve the repo, run the investigation with the session
// prompt as the request text.
func linearAgentInvestigate(deps flowDeps) func(ctx context.Context, w linearagent.Work) (*investigation.Findings, error) {
	return func(ctx context.Context, w linearagent.Work) (*investigation.Findings, error) {
		var threadContext []string
		ticketContext := ""
		if w.Session.IssueIdentifier != "" {
			ref := &issuetracker.IssueRef{Provider: "linear", ID: w.Session.IssueIdentifier}
			if details, err := deps.tracker.GetIssueDetails(ctx, ref); err == nil && details != nil {
				var b strings.Builder
				fmt.Fprintf(&b, "<linked_tickets>\n%s: %s\n%s\n", details.ID, details.Title, details.Description)
				for _, c := range details.Comments {
					fmt.Fprintf(&b, "---\n%s: %s\n", c.Author, c.Body)
				}
				b.WriteString("</linked_tickets>")
				ticketContext = b.String()
			}
		}

		repo := deps.resolver.Resolve("", nil)
		req := investigation.Request{
			Text:          w.Prompt,
			ThreadContext: threadContext,
			Summary:       w.Session.IssueTitle,
			TicketContext: ticketContext,
			Repo:          repo,
			Timeout:       deps.investigateTimeout,
		}
		return runInvestigation(ctx, deps.investRunner, deps.investigateSem, req)
	}
}
```

Check `deps.resolver.Resolve`'s exact signature (config/resolver.go) — it takes a repo hint and file hints; pass empty values so it falls back to the primary repo. Adjust the call to the real parameter list.

- [ ] **Step 5: Wire the daemon — `cmd/root.go`**

After the digest engine startup block (~root.go:427-443), add:

```go
	// Linear agent sessions: answer @-mentions and delegations on Linear
	// tickets with codebase-backed investigations. Polling-only (no inbound
	// HTTP); starts only with a connected app identity.
	if cfg.LinearAgent.Enabled && linearStore.Connected() {
		agentClient := linearagent.NewClient(linearAuth)
		processor := linearagent.NewProcessor(linearagent.ProcessorOpts{
			Poster:      agentClient,
			DB:          stateDB,
			Claim:       stateManager.ClaimScoped,
			Unclaim:     stateManager.UnclaimScoped,
			Investigate: linearAgentInvestigate(deps),
			Timeout:     deps.investigateTimeout,
		})
		poller := linearagent.NewPoller(agentClient, stateDB,
			time.Duration(cfg.LinearAgent.PollSeconds)*time.Second, processor.Handle)
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			poller.Run(ctx)
		}()
		slog.Info("linear agent poller started", "interval_seconds", cfg.LinearAgent.PollSeconds)
	}
```

(`linearStore` and `linearAuth` exist from Task 4's wiring. Place this where `deps` is already built.)

- [ ] **Step 6: Update CLAUDE.md**

- In the architecture overview, add a flow line after the digest bullet:

```markdown
- Linear agent (polled): when connected as a Linear app (`toad linear connect`), a poller (`internal/linearagent`, `linear_agent.poll_seconds`, default 15s) picks up agent sessions created by @-mentioning or delegating to toad in Linear, acks with a `thought` activity, runs the standard read-only investigation (reusing stored findings when fresh), and replies with a `response` activity — never filing tickets or mutating issues from this path
```

- In Packages, add: `internal/linearauth` (OAuth store/exchange/refresh + AuthSource) and `internal/linearagent` (agent-session client, work detection poller, processor).
- In Important Details, update the schema line: `current schema is version 13 — ... v13 added agent_sessions (Linear agent-session dedup/handled-state)`.

- [ ] **Step 7: Full gate**

Run: `go build ./... && go test ./... && go vet ./... && gofmt -l .`
Expected: everything passes, gofmt prints nothing. Do NOT commit — hand back for `/release`.
