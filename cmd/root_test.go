package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/scaler-tech/toad/internal/issuetracker"
)

// mockTracker implements issuetracker.Tracker for testing enrichWithIssueDetails.
type mockTracker struct {
	issuetracker.NoopTracker
	refs    []*issuetracker.IssueRef
	details map[string]*issuetracker.IssueDetails
}

func (m *mockTracker) ExtractAllIssueRefs(text string) []*issuetracker.IssueRef {
	return m.refs
}

func (m *mockTracker) GetIssueDetails(_ context.Context, ref *issuetracker.IssueRef) (*issuetracker.IssueDetails, error) {
	if d, ok := m.details[ref.ID]; ok {
		return d, nil
	}
	return nil, nil
}

func TestEnrichWithIssueDetails_NoTracker(t *testing.T) {
	ctx := context.Background()
	tc := []string{"some context"}
	result, details := enrichWithIssueDetails(ctx, issuetracker.NoopTracker{}, "hello", tc)
	if len(result) != 1 || result[0] != "some context" {
		t.Errorf("expected unchanged context, got %v", result)
	}
	if details != nil {
		t.Errorf("expected no fetched details, got %+v", details)
	}
}

func TestEnrichWithIssueDetails_ResolvesTickets(t *testing.T) {
	ctx := context.Background()
	tracker := &mockTracker{
		refs: []*issuetracker.IssueRef{
			{Provider: "linear", ID: "DAT-3199"},
			{Provider: "linear", ID: "DAT-4000"},
		},
		details: map[string]*issuetracker.IssueDetails{
			"DAT-3199": {ID: "DAT-3199", Title: "Vague error messages", Description: "Template errors are too vague"},
			"DAT-4000": {ID: "DAT-4000", Title: "Login issue", Description: "Users can't log in"},
		},
	}
	tc := []string{"original context"}
	result, details := enrichWithIssueDetails(ctx, tracker, "check DAT-3199", tc)
	if len(result) != 3 {
		t.Fatalf("expected 3 context entries, got %d", len(result))
	}
	if result[0] != "original context" {
		t.Errorf("first entry should be original, got %q", result[0])
	}
	if !searchString(result[1], "DAT-3199") || !searchString(result[1], "Vague error messages") {
		t.Errorf("expected DAT-3199 details in result[1], got %q", result[1])
	}
	if !searchString(result[2], "DAT-4000") {
		t.Errorf("expected DAT-4000 in result[2], got %q", result[2])
	}
	if len(details) != 2 {
		t.Fatalf("expected 2 fetched details, got %d", len(details))
	}
	if details[0].ID != "DAT-3199" || details[1].ID != "DAT-4000" {
		t.Errorf("expected fetched details in ref order, got %+v", details)
	}
}

func TestEnrichWithIssueDetails_CapsAt3(t *testing.T) {
	ctx := context.Background()
	tracker := &mockTracker{
		refs: []*issuetracker.IssueRef{
			{ID: "A-1"}, {ID: "A-2"}, {ID: "A-3"}, {ID: "A-4"},
		},
		details: map[string]*issuetracker.IssueDetails{
			"A-1": {ID: "A-1", Title: "One"},
			"A-2": {ID: "A-2", Title: "Two"},
			"A-3": {ID: "A-3", Title: "Three"},
			"A-4": {ID: "A-4", Title: "Four"},
		},
	}
	result, details := enrichWithIssueDetails(ctx, tracker, "text", nil)
	// 0 original + 3 enriched (capped)
	if len(result) != 3 {
		t.Fatalf("expected 3 enriched entries (cap), got %d", len(result))
	}
	if len(details) != 3 {
		t.Fatalf("expected 3 fetched details (cap), got %d", len(details))
	}
}

func TestEnrichWithIssueDetails_TruncatesLongDescription(t *testing.T) {
	ctx := context.Background()
	longDesc := strings.Repeat("x", 600)
	tracker := &mockTracker{
		refs: []*issuetracker.IssueRef{{ID: "B-1"}},
		details: map[string]*issuetracker.IssueDetails{
			"B-1": {ID: "B-1", Title: "Long", Description: longDesc},
		},
	}
	result, details := enrichWithIssueDetails(ctx, tracker, "text", nil)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if len(details) != 1 || details[0].Description != longDesc {
		t.Fatalf("expected 1 fetched detail carrying the untruncated description, got %+v", details)
	}
	// 500 chars + "..." = 503, plus the "[B-1] Long\n" prefix
	if len(result[0]) > 520 {
		t.Errorf("description should be truncated, got len=%d", len(result[0]))
	}
	if !searchString(result[0], "...") {
		t.Error("expected truncation marker")
	}
}

// helpers

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
