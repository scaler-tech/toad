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

	// The fallback must not be cached: the next AuthHeader() re-reads the
	// store and retries the Bearer path (still "stale" here since no
	// successful refresh occurred), not the personal key permanently.
	if got := src.AuthHeader(); got != "Bearer stale" {
		t.Errorf("AuthHeader() after failed-refresh fallback = %q, want re-read Bearer stale (fallback must not stick)", got)
	}
}

func TestSource_HandleUnauthorized_NoOAuthNoRetry(t *testing.T) {
	src := NewSource(NewStore(testDB(t)), "cid", "sec", "personal-key")
	if _, retry := src.HandleUnauthorized(context.Background()); retry {
		t.Error("API-key-only auth has nothing to refresh; retry must be false")
	}
}

func TestSource_HandleUnauthorized_RefreshFailsNoFallback(t *testing.T) {
	store := NewStore(testDB(t))
	store.SaveTokens("stale", "ref-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()

	src := NewSource(store, "cid", "sec", "")
	src.tokenURL = srv.URL
	src.httpClient = srv.Client()

	header, retry := src.HandleUnauthorized(context.Background())
	if retry || header != "" {
		t.Errorf("got header=%q retry=%v, want empty and no retry when refresh fails with no fallback", header, retry)
	}
}
