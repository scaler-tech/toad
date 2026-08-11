package issuetracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scaler-tech/toad/internal/config"
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

func TestExtractAllIssueRefs(t *testing.T) {
	lt := &LinearTracker{}

	tests := []struct {
		name    string
		text    string
		wantIDs []string
	}{
		{
			name:    "no refs",
			text:    "just some text",
			wantIDs: nil,
		},
		{
			name:    "single bare ID",
			text:    "fix PLF-3198",
			wantIDs: []string{"PLF-3198"},
		},
		{
			name:    "multiple bare IDs",
			text:    "REP-1577 is about reporting, PLF-3198 is about platform, ENV-42 is about environment",
			wantIDs: []string{"REP-1577", "PLF-3198", "ENV-42"},
		},
		{
			name:    "URL and bare IDs",
			text:    "see https://linear.app/team/issue/PLF-3198/slug and also REP-1577",
			wantIDs: []string{"PLF-3198", "REP-1577"},
		},
		{
			name:    "dedup URL and bare same ID",
			text:    "PLF-3198 see https://linear.app/team/issue/PLF-3198",
			wantIDs: []string{"PLF-3198"},
		},
		{
			name:    "filters acronyms",
			text:    "HTTP-200 PLF-42 JSON-5",
			wantIDs: []string{"PLF-42"},
		},
		{
			name:    "multiple URLs",
			text:    "https://linear.app/t/issue/PLF-1/a and https://linear.app/t/issue/PLF-2/b",
			wantIDs: []string{"PLF-1", "PLF-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs := lt.ExtractAllIssueRefs(tt.text)
			var gotIDs []string
			for _, r := range refs {
				gotIDs = append(gotIDs, r.ID)
			}
			if len(gotIDs) != len(tt.wantIDs) {
				t.Fatalf("got %v, want %v", gotIDs, tt.wantIDs)
			}
			for i := range gotIDs {
				if gotIDs[i] != tt.wantIDs[i] {
					t.Errorf("ref[%d] = %q, want %q", i, gotIDs[i], tt.wantIDs[i])
				}
			}
		})
	}
}

func TestParseIssueIdentifier(t *testing.T) {
	tests := []struct {
		id      string
		wantKey string
		wantNum int
		wantErr bool
	}{
		{"PLF-3198", "PLF", 3198, false},
		{"REP-1", "REP", 1, false},
		{"AB-0", "AB", 0, false},
		{"NONUM", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			key, num, err := parseIssueIdentifier(tt.id)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if key != tt.wantKey || num != tt.wantNum {
				t.Errorf("got (%q, %d), want (%q, %d)", key, num, tt.wantKey, tt.wantNum)
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

func TestCreateIssue_StateIDOmittedWhenNotSet(t *testing.T) {
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
						"id":         "issue-uuid-1",
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
		apiToken:   "token",
		teamID:     "00000000-0000-0000-0000-000000000001",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := receivedVars["stateId"]; ok {
		t.Errorf("expected stateId to be omitted, got %v", receivedVars["stateId"])
	}
}

func TestCreateIssue_StateIDIncludedWhenSet(t *testing.T) {
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
						"id":         "issue-uuid-2",
						"identifier": "PLF-2",
						"url":        "https://linear.app/team/issue/PLF-2",
						"title":      "test",
					},
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

	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{
		Title:   "test",
		StateID: "state-uuid-123",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := receivedVars["stateId"]; got != "state-uuid-123" {
		t.Errorf("expected stateId 'state-uuid-123', got %v", got)
	}
}

func TestCreateIssue_ExtraLabelsMergedWithCategoryLabel(t *testing.T) {
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
						"id":         "issue-uuid-3",
						"identifier": "PLF-3",
						"url":        "https://linear.app/team/issue/PLF-3",
						"title":      "test",
					},
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		teamID:     "00000000-0000-0000-0000-000000000001",
		bugLabelID: "bug-label-id",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{
		Title:    "test",
		Category: "bug",
		Labels:   []string{"extra-1", "extra-2"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	labels, ok := receivedVars["labelIds"].([]any)
	if !ok || len(labels) != 3 {
		t.Fatalf("expected 3 merged labels, got %v", receivedVars["labelIds"])
	}
	want := []string{"bug-label-id", "extra-1", "extra-2"}
	for i, w := range want {
		if labels[i] != w {
			t.Errorf("label[%d] = %v, want %q", i, labels[i], w)
		}
	}
}

func TestCreateIssue_ExtraLabelsWithoutCategory(t *testing.T) {
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
						"id":         "issue-uuid-4",
						"identifier": "PLF-4",
						"url":        "https://linear.app/team/issue/PLF-4",
						"title":      "test",
					},
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

	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{
		Title:  "test",
		Labels: []string{"extra-only"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	labels, ok := receivedVars["labelIds"].([]any)
	if !ok || len(labels) != 1 || labels[0] != "extra-only" {
		t.Errorf("expected labelIds=[extra-only], got %v", receivedVars["labelIds"])
	}
}

func TestCreateIssue_InternalIDPopulated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueCreate": map[string]any{
					"success": true,
					"issue": map[string]any{
						"id":         "issue-internal-uuid",
						"identifier": "PLF-5",
						"url":        "https://linear.app/team/issue/PLF-5",
						"title":      "test title",
					},
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

	ref, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.InternalID != "issue-internal-uuid" {
		t.Errorf("expected InternalID 'issue-internal-uuid', got %q", ref.InternalID)
	}
	if ref.ID != "PLF-5" {
		t.Errorf("expected ID 'PLF-5', got %q", ref.ID)
	}
}

func TestResolveTeamID_ConcurrentSingleFlight(t *testing.T) {
	var teamsCallCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&payload)

		if strings.Contains(payload.Query, "teams {") {
			atomic.AddInt32(&teamsCallCount, 1)
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"teams": map[string]any{
						"nodes": []map[string]any{
							{"id": "00000000-0000-0000-0000-000000000abc", "key": "PLF"},
						},
					},
				},
			})
			return
		}

		// issueCreate mutation
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueCreate": map[string]any{
					"success": true,
					"issue": map[string]any{
						"id":         "issue-uuid",
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
		apiToken:   "token",
		teamID:     "PLF",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	const n = 10
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "concurrent"})
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Errorf("unexpected error from concurrent CreateIssue: %v", err)
		}
	}

	if got := atomic.LoadInt32(&teamsCallCount); got != 1 {
		t.Errorf("expected exactly 1 teams resolution query, got %d", got)
	}
}

func TestResolveTeamID_RetriesAfterFailure(t *testing.T) {
	var teamsCallCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&payload)

		if strings.Contains(payload.Query, "teams {") {
			n := atomic.AddInt32(&teamsCallCount, 1)
			if n == 1 {
				// First resolution attempt fails transiently.
				json.NewEncoder(w).Encode(map[string]any{
					"errors": []map[string]any{
						{"message": "temporary failure"},
					},
				})
				return
			}
			// Second attempt succeeds.
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"teams": map[string]any{
						"nodes": []map[string]any{
							{"id": "00000000-0000-0000-0000-000000000abc", "key": "PLF"},
						},
					},
				},
			})
			return
		}

		// issueCreate mutation
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueCreate": map[string]any{
					"success": true,
					"issue": map[string]any{
						"id":         "issue-uuid",
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
		apiToken:   "token",
		teamID:     "PLF",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	// First call: team resolution fails, CreateIssue should surface the error
	// rather than caching it permanently.
	_, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "first"})
	if err == nil {
		t.Fatal("expected first CreateIssue to fail (team resolution error)")
	}

	// Second call: resolution is retried (not stuck on the cached failure)
	// and succeeds.
	_, err = lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "second"})
	if err != nil {
		t.Fatalf("expected second CreateIssue to succeed after retry, got %v", err)
	}

	if got := atomic.LoadInt32(&teamsCallCount); got != 2 {
		t.Errorf("expected exactly 2 teams resolution queries (fail then succeed), got %d", got)
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

// --- GetIssueStatus tests ---

func TestGetIssueStatus_Assigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{
						{
							"id":        "uuid-123",
							"state":     map[string]any{"name": "In Progress"},
							"assignee":  map[string]any{"displayName": "Jane Doe"},
							"updatedAt": "2026-03-01T12:00:00Z",
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	status, err := lt.GetIssueStatus(context.Background(), &IssueRef{ID: "PLF-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status == nil {
		t.Fatal("expected non-nil status")
	}
	if status.State != "In Progress" {
		t.Errorf("expected state 'In Progress', got %q", status.State)
	}
	if status.AssigneeName != "Jane Doe" {
		t.Errorf("expected assignee 'Jane Doe', got %q", status.AssigneeName)
	}
	if status.InternalID != "uuid-123" {
		t.Errorf("expected internal ID 'uuid-123', got %q", status.InternalID)
	}
}

// TestGetIssueStatus_WithStateType confirms the GraphQL response's
// state.type field (Linear's stable workflow-state-type enum) is parsed
// into IssueStatus.StateType alongside the existing state.name.
func TestGetIssueStatus_WithStateType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{
						{
							"id":        "uuid-789",
							"state":     map[string]any{"name": "In Progress", "type": "started"},
							"assignee":  map[string]any{"displayName": "Jane Doe"},
							"updatedAt": "2026-03-01T12:00:00Z",
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	status, err := lt.GetIssueStatus(context.Background(), &IssueRef{ID: "PLF-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status == nil {
		t.Fatal("expected non-nil status")
	}
	if status.State != "In Progress" {
		t.Errorf("expected state 'In Progress', got %q", status.State)
	}
	if status.StateType != "started" {
		t.Errorf("expected state type 'started', got %q", status.StateType)
	}
}

func TestGetIssueStatus_Unassigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{
						{
							"id":        "uuid-456",
							"state":     map[string]any{"name": "Todo"},
							"assignee":  nil,
							"updatedAt": "2026-03-01T12:00:00Z",
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	status, err := lt.GetIssueStatus(context.Background(), &IssueRef{ID: "PLF-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status == nil {
		t.Fatal("expected non-nil status")
	}
	if status.AssigneeName != "" {
		t.Errorf("expected empty assignee, got %q", status.AssigneeName)
	}
}

// TestGetIssueStatus_Delegated confirms the GraphQL response's delegate
// field is parsed into IssueStatus.DelegateName.
func TestGetIssueStatus_Delegated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{
						{
							"id":        "uuid-delegated",
							"state":     map[string]any{"name": "In Progress", "type": "started"},
							"assignee":  nil,
							"delegate":  map[string]any{"id": "agent-uuid", "name": "Biome", "displayName": "Biome"},
							"updatedAt": "2026-03-01T12:00:00Z",
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	status, err := lt.GetIssueStatus(context.Background(), &IssueRef{ID: "PLF-4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status == nil {
		t.Fatal("expected non-nil status")
	}
	if status.DelegateName != "Biome" {
		t.Errorf("expected delegate 'Biome', got %q", status.DelegateName)
	}
	if !status.IsDelegated() {
		t.Error("expected IsDelegated() to be true")
	}
}

// TestGetIssueStatus_DelegateNull confirms a null delegate (the common case
// for most issues) parses to an empty DelegateName rather than erroring.
func TestGetIssueStatus_DelegateNull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{
						{
							"id":        "uuid-no-delegate",
							"state":     map[string]any{"name": "Todo"},
							"assignee":  nil,
							"delegate":  nil,
							"updatedAt": "2026-03-01T12:00:00Z",
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	status, err := lt.GetIssueStatus(context.Background(), &IssueRef{ID: "PLF-5"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status == nil {
		t.Fatal("expected non-nil status")
	}
	if status.DelegateName != "" {
		t.Errorf("expected empty delegate, got %q", status.DelegateName)
	}
	if status.IsDelegated() {
		t.Error("expected IsDelegated() to be false for a null delegate")
	}
}

func TestGetIssueStatus_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{},
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	status, err := lt.GetIssueStatus(context.Background(), &IssueRef{ID: "PLF-999"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != nil {
		t.Error("expected nil status for non-existent issue")
	}
}

func TestGetIssueStatus_NoToken(t *testing.T) {
	lt := &LinearTracker{}
	status, err := lt.GetIssueStatus(context.Background(), &IssueRef{ID: "PLF-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != nil {
		t.Error("expected nil status when no token configured")
	}
}

// --- PostComment tests ---

func TestPostComment_Success(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var payload struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&payload)

		if strings.Contains(payload.Query, "issues(") {
			// GetIssueStatus call (to resolve internal ID)
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issues": map[string]any{
						"nodes": []map[string]any{
							{
								"id":        "uuid-resolved",
								"state":     map[string]any{"name": "Todo"},
								"assignee":  nil,
								"updatedAt": "2026-03-01T12:00:00Z",
							},
						},
					},
				},
			})
		} else if strings.Contains(payload.Query, "commentCreate") {
			// PostComment call
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"commentCreate": map[string]any{
						"success": true,
					},
				},
			})
		}
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	err := lt.PostComment(context.Background(), &IssueRef{ID: "PLF-1"}, "Test comment")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 API calls (status + comment), got %d", callCount)
	}
}

func TestPostComment_IssueNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{},
				},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	err := lt.PostComment(context.Background(), &IssueRef{ID: "PLF-999"}, "Test")
	if err == nil {
		t.Fatal("expected error when issue not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got %v", err)
	}
}

func TestPostComment_PreResolvedInternalID(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var payload struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&payload)

		if strings.Contains(payload.Query, "issues(") {
			t.Error("unexpected GetIssueStatus call — should have used pre-resolved InternalID")
		}
		// Only the commentCreate call should happen
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"commentCreate": map[string]any{"success": true},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	err := lt.PostComment(context.Background(), &IssueRef{ID: "PLF-1", InternalID: "uuid-pre-resolved"}, "Test comment")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 API call (comment only), got %d", callCount)
	}
}

func TestPostComment_NoToken(t *testing.T) {
	lt := &LinearTracker{}
	err := lt.PostComment(context.Background(), &IssueRef{ID: "PLF-1"}, "Test")
	if err == nil {
		t.Fatal("expected error when no token configured")
	}
}

// --- doGraphQL tests ---

func TestDoGraphQL_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-token" {
			t.Errorf("expected auth header, got %q", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"hello": "world"},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "test-token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	data, err := lt.doGraphQL(context.Background(), "{ hello }", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(data), "world") {
		t.Errorf("expected data to contain 'world', got %s", string(data))
	}
}

func TestDoGraphQL_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{
				{"message": "Something went wrong"},
			},
		})
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "token",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	_, err := lt.doGraphQL(context.Background(), "{ fail }", nil)
	if err == nil {
		t.Fatal("expected error for GraphQL error response")
	}
	if !strings.Contains(err.Error(), "Something went wrong") {
		t.Errorf("expected error message, got %v", err)
	}
}

func TestDoGraphQL_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("unauthorized"))
	}))
	defer srv.Close()

	lt := &LinearTracker{
		apiToken:   "bad",
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}

	_, err := lt.doGraphQL(context.Background(), "{ test }", nil)
	if err == nil {
		t.Fatal("expected error for non-200 status")
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

// projectAwareServer fakes the two GraphQL calls CreateIssue can make when a
// project/team override is present: a "projects" query for the effective
// team, a "teams" query for overrides, and the issueCreate mutation. It
// records the mutation variables and counts projects queries.
func projectAwareServer(t *testing.T, projects map[string]string, teams map[string][2]string, capturedVars *map[string]any, projectQueries *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&payload)

		switch {
		case strings.Contains(payload.Query, "projects(first:"):
			if projectQueries != nil {
				*projectQueries++
			}
			var nodes []map[string]any
			for id, name := range projects {
				nodes = append(nodes, map[string]any{"id": id, "name": name})
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"team": map[string]any{"projects": map[string]any{"nodes": nodes}}},
			})
		case strings.Contains(payload.Query, "teams { nodes"):
			var nodes []map[string]any
			for id, keyName := range teams {
				nodes = append(nodes, map[string]any{"id": id, "key": keyName[0], "name": keyName[1]})
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"teams": map[string]any{"nodes": nodes}},
			})
		default: // issueCreate
			if capturedVars != nil {
				*capturedVars = payload.Variables
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"issueCreate": map[string]any{
					"success": true,
					"issue":   map[string]any{"identifier": "PLF-99", "url": "https://linear.app/x/issue/PLF-99", "title": "t"},
				}},
			})
		}
	}))
}

const testTeamUUID = "00000000-0000-0000-0000-000000000123"

func projectTestTracker(srv *httptest.Server) *LinearTracker {
	return &LinearTracker{
		apiToken:   "test-token",
		teamID:     testTeamUUID,
		httpClient: &http.Client{Transport: &rewriteTransport{url: srv.URL}},
	}
}

func TestCreateIssue_ProjectResolvedAndAttached(t *testing.T) {
	var vars map[string]any
	srv := projectAwareServer(t, map[string]string{
		"proj-biome": "Biome",
		"proj-other": "Platform Cleanup",
	}, nil, &vars, nil)
	defer srv.Close()

	lt := projectTestTracker(srv)
	if _, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "t", Project: "biome"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vars["projectId"] != "proj-biome" {
		t.Errorf("projectId = %v, want proj-biome (case-insensitive exact match)", vars["projectId"])
	}
}

func TestCreateIssue_UnknownProjectFilesWithoutProject(t *testing.T) {
	var vars map[string]any
	srv := projectAwareServer(t, map[string]string{"proj-other": "Platform Cleanup"}, nil, &vars, nil)
	defer srv.Close()

	lt := projectTestTracker(srv)
	ref, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "t", Project: "nonexistent"})
	if err != nil {
		t.Fatalf("unexpected error: %v (resolution failure must not block creation)", err)
	}
	if ref == nil {
		t.Fatal("expected issue ref despite unresolved project")
		return
	}
	if _, ok := vars["projectId"]; ok {
		t.Errorf("projectId = %v, want absent when the project cannot be resolved", vars["projectId"])
	}
}

func TestCreateIssue_AmbiguousPartialProjectFilesWithoutProject(t *testing.T) {
	var vars map[string]any
	srv := projectAwareServer(t, map[string]string{
		"proj-a": "Billing Revamp",
		"proj-b": "Billing Cleanup",
	}, nil, &vars, nil)
	defer srv.Close()

	lt := projectTestTracker(srv)
	if _, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "t", Project: "Billing"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := vars["projectId"]; ok {
		t.Errorf("projectId = %v, want absent for an ambiguous partial match", vars["projectId"])
	}
}

func TestCreateIssue_TeamOverrideByKeyAndName(t *testing.T) {
	teams := map[string][2]string{
		"team-ana": {"ANA", "Analytics"},
		"team-plf": {"PLF", "Growth Platform"},
	}
	for _, override := range []string{"ana", "Analytics"} {
		var vars map[string]any
		srv := projectAwareServer(t, nil, teams, &vars, nil)
		lt := projectTestTracker(srv)
		if _, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "t", Team: override}); err != nil {
			srv.Close()
			t.Fatalf("override %q: unexpected error: %v", override, err)
		}
		if vars["teamId"] != "team-ana" {
			t.Errorf("override %q: teamId = %v, want team-ana", override, vars["teamId"])
		}
		srv.Close()
	}
}

func TestCreateIssue_UnknownTeamOverrideFallsBackToDefault(t *testing.T) {
	var vars map[string]any
	srv := projectAwareServer(t, nil, map[string][2]string{"team-plf": {"PLF", "Growth Platform"}}, &vars, nil)
	defer srv.Close()

	lt := projectTestTracker(srv)
	if _, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "t", Team: "NOPE"}); err != nil {
		t.Fatalf("unexpected error: %v (unknown override must fall back, not fail)", err)
	}
	if vars["teamId"] != testTeamUUID {
		t.Errorf("teamId = %v, want the configured default team", vars["teamId"])
	}
}

func TestCreateIssue_ProjectResolutionCached(t *testing.T) {
	var vars map[string]any
	queries := 0
	srv := projectAwareServer(t, map[string]string{"proj-biome": "Biome"}, nil, &vars, &queries)
	defer srv.Close()

	lt := projectTestTracker(srv)
	for i := 0; i < 2; i++ {
		if _, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "t", Project: "Biome"}); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	if queries != 1 {
		t.Errorf("projects queries = %d, want 1 (second call must hit the cache)", queries)
	}
}

// fakeUser is a users(...) fixture: id/name/displayName/email, plus App —
// Linear's native agent/app-user signal. App == nil simulates a user node
// where the `app` field either wasn't requested (rejectAppField below) or
// came back null; App != nil simulates the field being present and
// authoritative, overriding the email-suffix fallback either way (see
// resolveAgentKind).
type fakeUser struct {
	ID          string
	Name        string
	DisplayName string
	Email       string
	App         *bool
}

func boolPtr(b bool) *bool { return &b }

// assigneeAwareServer fakes every GraphQL call CreateIssue's assignee/
// delegate resolution can make: a "users" query (both the email-filtered
// lookup and the full-list lookup), and the issueCreate mutation. It
// records the mutation variables.
//
// rejectAppField simulates a Linear API version whose schema doesn't expose
// `app` on User: any users query that requests the "app" field gets a
// GraphQL validation error instead of data, forcing
// queryUsersWithAgentSignal's defensive retry-without-app path — the
// retried (app-less) query still succeeds normally.
func assigneeAwareServer(t *testing.T, users []fakeUser, rejectAppField bool, capturedVars *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&payload)

		requestsApp := strings.Contains(payload.Query, " app") || strings.Contains(payload.Query, "\tapp")

		switch {
		case strings.Contains(payload.Query, "email: { eq: $email }"):
			if rejectAppField && requestsApp {
				json.NewEncoder(w).Encode(map[string]any{
					"errors": []map[string]any{{"message": `Cannot query field "app" on type "User".`}},
				})
				return
			}
			email, _ := payload.Variables["email"].(string)
			var nodes []map[string]any
			for _, u := range users {
				if strings.EqualFold(u.Email, email) {
					n := map[string]any{"id": u.ID, "email": u.Email}
					if requestsApp {
						n["app"] = boolToJSON(u.App)
					}
					nodes = append(nodes, n)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"users": map[string]any{"nodes": nodes}},
			})
		case strings.Contains(payload.Query, "users(first: 250)"):
			if rejectAppField && requestsApp {
				json.NewEncoder(w).Encode(map[string]any{
					"errors": []map[string]any{{"message": `Cannot query field "app" on type "User".`}},
				})
				return
			}
			var nodes []map[string]any
			for _, u := range users {
				n := map[string]any{"id": u.ID, "name": u.Name, "displayName": u.DisplayName, "email": u.Email}
				if requestsApp {
					n["app"] = boolToJSON(u.App)
				}
				nodes = append(nodes, n)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"users": map[string]any{"nodes": nodes}},
			})
		default: // issueCreate
			if capturedVars != nil {
				*capturedVars = payload.Variables
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"issueCreate": map[string]any{
					"success": true,
					"issue":   map[string]any{"identifier": "PLF-99", "url": "https://linear.app/x/issue/PLF-99", "title": "t"},
				}},
			})
		}
	}))
}

// boolToJSON returns the value a *bool fixture should encode as: nil ->
// JSON null (the "app field present but null" case), otherwise the pointed-
// to value.
func boolToJSON(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// biomeAgentUser is the fixture shape the operator's design describes for
// Biome: a Linear OAuth-app/agent user with displayName "biome" and an
// email ending in the OAuth-app suffix — resolvable by either signal.
var biomeAgentUser = fakeUser{ID: "u-biome", Name: "Biome", DisplayName: "biome", Email: "biome@oauthapp.linear.app"}

func TestResolveUser_ByEmail(t *testing.T) {
	srv := assigneeAwareServer(t, []fakeUser{{ID: "u-1", Name: "Alice", Email: "alice@example.com"}}, false, nil)
	defer srv.Close()

	lt := projectTestTracker(srv)
	id, isAgent, err := lt.resolveUser(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "u-1" {
		t.Errorf("id = %q, want u-1", id)
	}
	if isAgent {
		t.Error("isAgent = true, want false for a plain human email")
	}
}

func TestResolveUser_ByName(t *testing.T) {
	srv := assigneeAwareServer(t, []fakeUser{
		{ID: "u-1", Name: "alice", DisplayName: "Alice A"},
		{ID: "u-2", Name: "bob", DisplayName: "Bob B"},
	}, false, nil)
	defer srv.Close()

	lt := projectTestTracker(srv)
	for _, tc := range []struct{ query, want string }{
		{"Alice", "u-1"}, // case-insensitive exact match on Name
		{"Bob B", "u-2"}, // case-insensitive exact match on DisplayName
	} {
		id, _, err := lt.resolveUser(context.Background(), tc.query)
		if err != nil {
			t.Fatalf("query %q: unexpected error: %v", tc.query, err)
		}
		if id != tc.want {
			t.Errorf("query %q: id = %q, want %q", tc.query, id, tc.want)
		}
	}
}

func TestResolveUser_AmbiguousAndUnknown(t *testing.T) {
	srv := assigneeAwareServer(t, []fakeUser{
		{ID: "u-1", Name: "alice-billing", DisplayName: "Alice Billing"},
		{ID: "u-2", Name: "alice-growth", DisplayName: "Alice Growth"},
	}, false, nil)
	defer srv.Close()

	lt := projectTestTracker(srv)
	if _, _, err := lt.resolveUser(context.Background(), "alice"); err == nil {
		t.Error("expected ambiguous-match error, got nil")
	}
	if _, _, err := lt.resolveUser(context.Background(), "nonexistent"); err == nil {
		t.Error("expected not-found error, got nil")
	}
}

// TestResolveUser_AgentViaEmailSuffix covers the fallback signal: the `app`
// field is present but null for this node (App: nil in the fixture, still
// requested by the query since rejectAppField is false), so resolution must
// fall back to the "@oauthapp.linear.app" email-suffix heuristic.
func TestResolveUser_AgentViaEmailSuffix(t *testing.T) {
	srv := assigneeAwareServer(t, []fakeUser{biomeAgentUser}, false, nil)
	defer srv.Close()

	lt := projectTestTracker(srv)
	id, isAgent, err := lt.resolveUser(context.Background(), "biome")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "u-biome" || !isAgent {
		t.Errorf("id=%q isAgent=%v, want u-biome/true (email-suffix fallback)", id, isAgent)
	}
}

// TestResolveUser_AgentViaAppFieldPrimarySignal covers the primary signal:
// `app: true` is present in the response even though the email doesn't use
// the OAuth-app suffix — the app field must win.
func TestResolveUser_AgentViaAppFieldPrimarySignal(t *testing.T) {
	srv := assigneeAwareServer(t, []fakeUser{
		{ID: "u-agent", Name: "Custom Agent", DisplayName: "custom-agent", Email: "agent@example.com", App: boolPtr(true)},
	}, false, nil)
	defer srv.Close()

	lt := projectTestTracker(srv)
	id, isAgent, err := lt.resolveUser(context.Background(), "custom-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "u-agent" || !isAgent {
		t.Errorf("id=%q isAgent=%v, want u-agent/true (app field takes priority)", id, isAgent)
	}
}

// TestResolveUser_AppFieldAbsentFromSchemaFallsBackToEmailSuffix exercises
// queryUsersWithAgentSignal's defensive retry: when the whole query fails
// because this Linear API version doesn't expose `app` on User at all,
// resolution must retry without it and still succeed, falling back to the
// email-suffix heuristic.
func TestResolveUser_AppFieldAbsentFromSchemaFallsBackToEmailSuffix(t *testing.T) {
	srv := assigneeAwareServer(t, []fakeUser{biomeAgentUser}, true /* rejectAppField */, nil)
	defer srv.Close()

	lt := projectTestTracker(srv)
	id, isAgent, err := lt.resolveUser(context.Background(), "biome")
	if err != nil {
		t.Fatalf("unexpected error: %v (must tolerate the app field being absent from the schema)", err)
	}
	if id != "u-biome" || !isAgent {
		t.Errorf("id=%q isAgent=%v, want u-biome/true", id, isAgent)
	}
}

func TestCreateIssue_HumanAndAgentBothSet(t *testing.T) {
	var vars map[string]any
	srv := assigneeAwareServer(t, []fakeUser{
		{ID: "u-alice", Name: "alice", DisplayName: "Alice", Email: "alice@example.com"},
		biomeAgentUser,
	}, false, &vars)
	defer srv.Close()

	lt := projectTestTracker(srv)
	ref, err := lt.CreateIssue(context.Background(), CreateIssueOpts{
		Title:     "t",
		Assignees: []string{"alice@example.com", "biome"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vars["assigneeId"] != "u-alice" {
		t.Errorf("assigneeId = %v, want u-alice", vars["assigneeId"])
	}
	if vars["delegateId"] != "u-biome" {
		t.Errorf("delegateId = %v, want u-biome", vars["delegateId"])
	}
	if ref.AssignedTo != "alice@example.com" {
		t.Errorf("AssignedTo = %q, want alice@example.com", ref.AssignedTo)
	}
	if ref.DelegatedTo != "biome" {
		t.Errorf("DelegatedTo = %q, want biome", ref.DelegatedTo)
	}
	if len(ref.UnresolvedAssignees) != 0 {
		t.Errorf("UnresolvedAssignees = %v, want empty", ref.UnresolvedAssignees)
	}
}

func TestCreateIssue_AgentOnlySetsDelegateOnly(t *testing.T) {
	var vars map[string]any
	srv := assigneeAwareServer(t, []fakeUser{biomeAgentUser}, false, &vars)
	defer srv.Close()

	lt := projectTestTracker(srv)
	ref, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "t", Assignees: []string{"biome"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := vars["assigneeId"]; ok {
		t.Errorf("assigneeId = %v, want absent (only an agent was requested)", vars["assigneeId"])
	}
	if vars["delegateId"] != "u-biome" {
		t.Errorf("delegateId = %v, want u-biome", vars["delegateId"])
	}
	if ref.AssignedTo != "" {
		t.Errorf("AssignedTo = %q, want empty", ref.AssignedTo)
	}
}

func TestCreateIssue_UnknownNameSkipped(t *testing.T) {
	var vars map[string]any
	srv := assigneeAwareServer(t, nil, false, &vars)
	defer srv.Close()

	lt := projectTestTracker(srv)
	ref, err := lt.CreateIssue(context.Background(), CreateIssueOpts{Title: "t", Assignees: []string{"nobody"}})
	if err != nil {
		t.Fatalf("unexpected error: %v (resolution failure must not block creation)", err)
	}
	if _, ok := vars["assigneeId"]; ok {
		t.Errorf("assigneeId = %v, want absent when the name cannot be resolved", vars["assigneeId"])
	}
	if len(ref.UnresolvedAssignees) != 1 || ref.UnresolvedAssignees[0] != "nobody" {
		t.Errorf("UnresolvedAssignees = %v, want [nobody]", ref.UnresolvedAssignees)
	}
}

// TestCreateIssue_DuplicateHumansFirstWins covers resolveAssignees' single-
// assignee-slot rule: a second human candidate resolves fine but has
// nowhere to go, so it's dropped (not reported as unresolved — it DID
// resolve, there's just no second assignee slot).
func TestCreateIssue_DuplicateHumansFirstWins(t *testing.T) {
	var vars map[string]any
	srv := assigneeAwareServer(t, []fakeUser{
		{ID: "u-alice", Name: "alice", Email: "alice@example.com"},
		{ID: "u-bob", Name: "bob", Email: "bob@example.com"},
	}, false, &vars)
	defer srv.Close()

	lt := projectTestTracker(srv)
	ref, err := lt.CreateIssue(context.Background(), CreateIssueOpts{
		Title:     "t",
		Assignees: []string{"alice@example.com", "bob@example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vars["assigneeId"] != "u-alice" {
		t.Errorf("assigneeId = %v, want u-alice (first human candidate wins)", vars["assigneeId"])
	}
	if ref.AssignedTo != "alice@example.com" {
		t.Errorf("AssignedTo = %q, want alice@example.com", ref.AssignedTo)
	}
	if len(ref.UnresolvedAssignees) != 0 {
		t.Errorf("UnresolvedAssignees = %v, want empty (bob resolved fine, just lost the single assignee slot)", ref.UnresolvedAssignees)
	}
}

func TestResolveUser_ResolutionCached(t *testing.T) {
	queries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"users": map[string]any{"nodes": []map[string]any{
				{"id": "u-1", "name": "alice", "displayName": "Alice", "email": "alice@example.com", "app": nil},
			}}},
		})
	}))
	defer srv.Close()

	lt := projectTestTracker(srv)
	for i := 0; i < 2; i++ {
		if _, _, err := lt.resolveUser(context.Background(), "alice"); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	if queries != 1 {
		t.Errorf("user queries = %d, want 1 (second call must hit the cache)", queries)
	}
}

// fakeAuth implements AuthSource for testing.
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

func TestOAuthOnlyCredentials_GetIssueDetailsQueriesAPI(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"data":{"issues":{"nodes":[{"id":"uuid-1","identifier":"PLF-1","title":"T","description":"D","url":"u","comments":{"nodes":[]}}]}}}`))
	}))
	defer srv.Close()

	// OAuth-only setup: no personal API key; the auth source carries the
	// connected app token. The tracker must still perform lookups.
	lt := NewLinearTrackerWithAuth(config.IssueTrackerConfig{Enabled: true, Provider: "linear"}, &fakeAuth{header: "Bearer app-tok"})
	lt.httpClient = srv.Client()
	lt.graphqlURL = srv.URL

	details, err := lt.GetIssueDetails(context.Background(), &IssueRef{Provider: "linear", ID: "PLF-1"})
	if err != nil {
		t.Fatalf("GetIssueDetails: %v", err)
	}
	if details == nil {
		t.Fatal("OAuth-only tracker returned nil details — empty-apiToken guard ignores the connected app token")
	}
	if hits == 0 {
		t.Fatal("API was never queried")
	}
}

func TestOAuthOnlyCredentials_PostCommentSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"commentCreate":{"success":true}}}`))
	}))
	defer srv.Close()

	lt := NewLinearTrackerWithAuth(config.IssueTrackerConfig{Enabled: true, Provider: "linear"}, &fakeAuth{header: "Bearer app-tok"})
	lt.httpClient = srv.Client()
	lt.graphqlURL = srv.URL

	if err := lt.PostComment(context.Background(), &IssueRef{Provider: "linear", ID: "PLF-1", InternalID: "uuid-1"}, "hello"); err != nil {
		t.Fatalf("OAuth-only PostComment: %v", err)
	}
}

func TestNoCredentialsAtAll_GetIssueDetailsSkipsLookup(t *testing.T) {
	lt := NewLinearTrackerWithAuth(config.IssueTrackerConfig{Enabled: true, Provider: "linear"}, &fakeAuth{header: ""})
	details, err := lt.GetIssueDetails(context.Background(), &IssueRef{Provider: "linear", ID: "PLF-1"})
	if err != nil || details != nil {
		t.Fatalf("no credentials must skip lookup gracefully, got details=%v err=%v", details, err)
	}
}
