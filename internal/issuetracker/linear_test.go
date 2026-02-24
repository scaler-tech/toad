package issuetracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hergen/toad/internal/config"
)

func TestExtractIssueRef_LinearURL(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantID  string
		wantURL string
	}{
		{
			name:    "standard linear URL",
			text:    "Check out https://linear.app/myteam/issue/PLF-3125/fix-the-thing",
			wantID:  "PLF-3125",
			wantURL: "https://linear.app/myteam/issue/PLF-3125",
		},
		{
			name:    "URL without slug",
			text:    "See https://linear.app/team/issue/ABC-42",
			wantID:  "ABC-42",
			wantURL: "https://linear.app/team/issue/ABC-42",
		},
		{
			name:    "URL in middle of text",
			text:    "This is about https://linear.app/acme/issue/PROJ-999/some-slug and more text",
			wantID:  "PROJ-999",
			wantURL: "https://linear.app/acme/issue/PROJ-999",
		},
		{
			name:    "multiple URLs picks first",
			text:    "https://linear.app/t/issue/AA-1/first https://linear.app/t/issue/BB-2/second",
			wantID:  "AA-1",
			wantURL: "https://linear.app/t/issue/AA-1",
		},
	}

	lt := &LinearTracker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := lt.ExtractIssueRef(tt.text)
			if ref == nil {
				t.Fatal("expected issue ref, got nil")
			}
			if ref.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", ref.ID, tt.wantID)
			}
			if ref.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", ref.URL, tt.wantURL)
			}
			if ref.Provider != "linear" {
				t.Errorf("Provider = %q, want %q", ref.Provider, "linear")
			}
		})
	}
}

func TestExtractIssueRef_BareID(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		wantID string
	}{
		{
			name:   "bare ID in text",
			text:   "Working on PLF-3125 right now",
			wantID: "PLF-3125",
		},
		{
			name:   "bare ID at start",
			text:   "ABC-42 is broken",
			wantID: "ABC-42",
		},
		{
			name:   "five letter prefix",
			text:   "PROJE-1 needs fixing",
			wantID: "PROJE-1",
		},
		{
			name:   "two letter prefix",
			text:   "AB-99 is done",
			wantID: "AB-99",
		},
	}

	lt := &LinearTracker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := lt.ExtractIssueRef(tt.text)
			if ref == nil {
				t.Fatal("expected issue ref, got nil")
			}
			if ref.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", ref.ID, tt.wantID)
			}
			if ref.URL != "" {
				t.Errorf("URL should be empty for bare ID, got %q", ref.URL)
			}
		})
	}
}

func TestExtractIssueRef_NoMatch(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"empty string", ""},
		{"no issue ref", "Just a regular message about code"},
		{"single letter prefix", "A-123 is too short"},
		{"six letter prefix", "TOOLON-123 is too long"},
		{"lowercase", "plf-123 lowercase doesn't match"},
		{"no digits", "PLF- missing digits"},
		{"not a word boundary", "xPLF-123 embedded"},
	}

	lt := &LinearTracker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := lt.ExtractIssueRef(tt.text)
			if ref != nil {
				t.Errorf("expected nil, got ref with ID=%q", ref.ID)
			}
		})
	}
}

func TestExtractIssueRef_CommonAcronymsFiltered(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"HTTP status", "Got HTTP-200 response"},
		{"SHA hash", "Using SHA-256 for hashing"},
		{"UTF encoding", "UTF-8 encoded file"},
		{"TCP port", "TCP-443 is open"},
		{"ISO standard", "ISO-8601 date format"},
		{"RFC reference", "See RFC-7231 for details"},
		{"SSL version", "SSL-3 is deprecated"},
		{"TLS version", "TLS-12 connection"},
		{"API version", "API-2 endpoint"},
		{"DNS record", "DNS-53 lookup"},
	}

	lt := &LinearTracker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := lt.ExtractIssueRef(tt.text)
			if ref != nil {
				t.Errorf("expected nil for common acronym, got ref with ID=%q", ref.ID)
			}
		})
	}
}

func TestExtractIssueRef_AcronymSkippedButIssueFound(t *testing.T) {
	// Text has both a common acronym and a real issue ID — should find the issue
	text := "HTTP-200 from PLF-3125 endpoint"
	lt := &LinearTracker{}
	ref := lt.ExtractIssueRef(text)
	if ref == nil {
		t.Fatal("expected issue ref, got nil")
	}
	if ref.ID != "PLF-3125" {
		t.Errorf("expected PLF-3125, got %q", ref.ID)
	}
}

func TestExtractIssueRef_URLPreferredOverBareID(t *testing.T) {
	text := "PLF-1 see https://linear.app/team/issue/PLF-3125/slug"
	lt := &LinearTracker{}
	ref := lt.ExtractIssueRef(text)
	if ref == nil {
		t.Fatal("expected issue ref")
	}
	if ref.ID != "PLF-3125" {
		t.Errorf("expected URL-based ID PLF-3125, got %q", ref.ID)
	}
	if ref.URL == "" {
		t.Error("expected URL to be set when extracted from URL")
	}
}

func TestBranchPrefix(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"PLF-3125", "plf-3125"},
		{"ABC-42", "abc-42"},
		{"PROJ-1", "proj-1"},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			ref := &IssueRef{ID: tt.id}
			if got := ref.BranchPrefix(); got != tt.want {
				t.Errorf("BranchPrefix() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNoopTracker(t *testing.T) {
	tracker := NoopTracker{}

	ref := tracker.ExtractIssueRef("PLF-3125 some text")
	if ref != nil {
		t.Error("NoopTracker.ExtractIssueRef should return nil")
	}

	issueRef, err := tracker.CreateIssue(context.Background(), CreateIssueOpts{Title: "test"})
	if err != nil {
		t.Errorf("NoopTracker.CreateIssue should not error, got %v", err)
	}
	if issueRef != nil {
		t.Error("NoopTracker.CreateIssue should return nil")
	}

	if tracker.ShouldCreateIssues() {
		t.Error("NoopTracker.ShouldCreateIssues should return false")
	}
}

func TestNewTracker_Disabled(t *testing.T) {
	cfg := config.IssueTrackerConfig{Enabled: false}
	tracker := NewTracker(cfg)
	if _, ok := tracker.(NoopTracker); !ok {
		t.Error("disabled config should return NoopTracker")
	}
}

func TestNewTracker_Linear(t *testing.T) {
	cfg := config.IssueTrackerConfig{Enabled: true, Provider: "linear", APIToken: "test"}
	tracker := NewTracker(cfg)
	if _, ok := tracker.(*LinearTracker); !ok {
		t.Error("linear provider should return *LinearTracker")
	}
}

func TestNewTracker_UnknownProvider(t *testing.T) {
	cfg := config.IssueTrackerConfig{Enabled: true, Provider: "jira"}
	tracker := NewTracker(cfg)
	if _, ok := tracker.(NoopTracker); !ok {
		t.Error("unknown provider should return NoopTracker")
	}
}

func TestShouldCreateIssues(t *testing.T) {
	lt := &LinearTracker{createIssues: true}
	if !lt.ShouldCreateIssues() {
		t.Error("expected true when createIssues is set")
	}
	lt2 := &LinearTracker{createIssues: false}
	if lt2.ShouldCreateIssues() {
		t.Error("expected false when createIssues is not set")
	}
}

// --- CreateIssue tests with httptest ---

func TestCreateIssue_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Header.Get("Authorization") != "test-token" {
			t.Errorf("expected Authorization header 'test-token', got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type 'application/json', got %q", r.Header.Get("Content-Type"))
		}

		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&payload)

		if payload.Variables["teamId"] != "00000000-0000-0000-0000-000000000123" {
			t.Errorf("expected teamId UUID, got %v", payload.Variables["teamId"])
		}

		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueCreate": map[string]any{
					"success": true,
					"issue": map[string]any{
						"identifier": "PLF-42",
						"url":        "https://linear.app/team/issue/PLF-42",
						"title":      "Fix the bug",
					},
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "test-token",
		teamID:     "00000000-0000-0000-0000-000000000123",
		httpClient: srv.Client(),
	}
	// Override the URL by using a custom transport
	lt.httpClient.Transport = &rewriteTransport{base: srv.Client().Transport, url: srv.URL}

	ref, err := lt.CreateIssue(context.Background(), CreateIssueOpts{
		Title:       "Fix the bug",
		Description: "It's broken",
		Category:    "bug",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.ID != "PLF-42" {
		t.Errorf("expected ID PLF-42, got %q", ref.ID)
	}
	if ref.URL != "https://linear.app/team/issue/PLF-42" {
		t.Errorf("expected URL, got %q", ref.URL)
	}
	if ref.Provider != "linear" {
		t.Errorf("expected provider 'linear', got %q", ref.Provider)
	}
}

func TestCreateIssue_WithLabels(t *testing.T) {
	var receivedVars map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&payload)
		receivedVars = payload.Variables

		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueCreate": map[string]any{
					"success": true,
					"issue": map[string]any{
						"identifier": "PLF-1",
						"url":        "https://linear.app/team/issue/PLF-1",
						"title":      "test",
					},
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:       "token",
		teamID:         "00000000-0000-0000-0000-000000000001",
		bugLabelID:     "bug-label-id",
		featureLabelID: "feat-label-id",
		httpClient:     &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	// Bug category should include bug label
	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{
		Title: "bug", Category: "bug",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	labels, ok := receivedVars["labelIds"].([]any)
	if !ok || len(labels) != 1 || labels[0] != "bug-label-id" {
		t.Errorf("expected bug label, got %v", receivedVars["labelIds"])
	}

	// Feature category should include feature label
	_, err = lt.CreateIssue(context.Background(), CreateIssueOpts{
		Title: "feat", Category: "feature",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	labels, ok = receivedVars["labelIds"].([]any)
	if !ok || len(labels) != 1 || labels[0] != "feat-label-id" {
		t.Errorf("expected feature label, got %v", receivedVars["labelIds"])
	}
}

func TestCreateIssue_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{
				{"message": "Team not found"},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		teamID:     "00000000-0000-0000-0000-000000000bad",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "test"})
	if err == nil {
		t.Fatal("expected error for GraphQL error response")
	}
	if got := err.Error(); !strings.Contains(got, "Team not found") {
		t.Errorf("expected error containing 'Team not found', got %v", got)
	}
}

func TestCreateIssue_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("unauthorized"))
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "bad-token",
		teamID:     "00000000-0000-0000-0000-000000000001",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "test"})
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
}

func TestCreateIssue_MissingToken(t *testing.T) {
	lt := &LinearTracker{teamID: "00000000-0000-0000-0000-000000000001"}
	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "test"})
	if err == nil || err.Error() != "linear API token not configured" {
		t.Errorf("expected token error, got %v", err)
	}
}

func TestCreateIssue_MissingTeamID(t *testing.T) {
	lt := &LinearTracker{apiToken: "token"}
	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "test"})
	if err == nil || err.Error() != "linear team ID not configured" {
		t.Errorf("expected team ID error, got %v", err)
	}
}

func TestCreateIssue_CreationFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueCreate": map[string]any{
					"success": false,
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		teamID:     "00000000-0000-0000-0000-000000000001",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "test"})
	if err == nil || err.Error() != "linear issue creation failed" {
		t.Errorf("expected creation failed error, got %v", err)
	}
}

// rewriteTransport redirects all requests to a test server URL.
type rewriteTransport struct {
	base http.RoundTripper
	url  string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = t.url[len("http://"):]
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
