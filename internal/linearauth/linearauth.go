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
	settingToken        = "linear_oauth_token"         //nolint:gosec // settings-table key name, not a credential
	settingRefreshToken = "linear_oauth_refresh_token" //nolint:gosec // settings-table key name, not a credential

	// DefaultTokenURL is Linear's OAuth token endpoint.
	DefaultTokenURL = "https://api.linear.app/oauth/token" //nolint:gosec // public endpoint URL, not a credential

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

// SaveTokens persists the OAuth access and refresh tokens to the settings table.
// Note: if a live Source is using this Store, callers must call Source.Invalidate()
// after SaveTokens to make the new token visible to AuthHeader. The toad linear
// connect flow runs in its own process, so the daemon picks the token up at next
// startup or via the 401 refresh path (which invalidates the cache before
// re-reading the store).
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
// OAuth token once; if that fails it falls back to the API key for that one
// retry only — the fallback is never cached, so the next AuthHeader() call
// re-reads the Store and retries the Bearer path rather than getting stuck
// on the personal identity until restart.
//
// Source caches the header to avoid repeated Store reads. Callers who update
// the Store via Store.SaveTokens must call Source.Invalidate() to drop the cache
// and make the new token visible to AuthHeader. The 401 path (HandleUnauthorized)
// drops the cache before re-reading the Store, and only re-populates it on a
// successful refresh — a failed-refresh fallback leaves the cache empty.
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
// return the new header; on refresh failure return the API-key fallback for
// that retry only, without caching it — the next AuthHeader() call re-reads
// the Store and retries the Bearer path, so a transient refresh failure
// (e.g. Linear's token endpoint hiccup) doesn't permanently flip toad to the
// personal identity until restart. Returns retry=false when there is no
// OAuth token in play (a bad personal key cannot be repaired here).
//
// Note: This method holds s.mu across the blocking Refresh() call (up to 30s).
// This is intentional: 401s are rare, and serializing concurrent refreshes means
// exactly one refresh happens and all waiters pick up the new cached header,
// preventing a thundering herd of concurrent token requests to Linear.
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
	// Deliberately not cached: leave s.cached empty so the next AuthHeader()
	// re-reads the Store and retries the Bearer path instead of getting
	// stuck on the personal identity until restart.
	return s.fallbackAPIKey, true
}
