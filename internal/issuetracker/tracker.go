// Package issuetracker provides a generic interface for issue tracker integrations.
package issuetracker

import (
	"context"
	"strings"
	"time"

	"github.com/hergen/toad/internal/config"
)

// IssueDetails holds the title and description of an issue, used to enrich
// investigation prompts with ticket context.
type IssueDetails struct {
	ID          string // "PLF-3198"
	InternalID  string // provider's internal UUID
	Title       string
	Description string
	URL         string
}

// IssueStatus represents the current state and assignment of an issue.
type IssueStatus struct {
	State        string    // e.g. "In Progress", "Todo", "Done"
	AssigneeName string    // display name of assignee, empty if unassigned
	AssignedAt   time.Time // when the issue was last updated (proxy for assignment recency)
	InternalID   string    // provider's internal UUID (needed for mutations)
}

// IsActivelyAssigned returns true if the issue has an assignee whose
// assignment is more recent than the given staleness threshold.
func (s *IssueStatus) IsActivelyAssigned(staleDays int) bool {
	if s.AssigneeName == "" {
		return false
	}
	if s.AssignedAt.IsZero() {
		return false
	}
	cutoff := time.Now().AddDate(0, 0, -staleDays)
	return s.AssignedAt.After(cutoff)
}

// IssueRef represents a reference to an issue in an external tracker.
type IssueRef struct {
	Provider   string // "linear", "jira"
	ID         string // "PLF-3125"
	URL        string
	Title      string
	InternalID string // provider's internal UUID, set when already resolved to skip lookups
}

// BranchPrefix returns a lowercased issue ID suitable for branch naming.
// e.g. "PLF-3125" → "plf-3125"
func (r *IssueRef) BranchPrefix() string {
	return strings.ToLower(r.ID)
}

// Tracker is the interface for issue tracker integrations.
type Tracker interface {
	// ExtractIssueRef extracts the first issue reference from message text.
	// Returns nil if no issue reference is found.
	ExtractIssueRef(text string) *IssueRef

	// ExtractAllIssueRefs extracts all issue references from message text.
	// Returns nil if no issue references are found.
	ExtractAllIssueRefs(text string) []*IssueRef

	// GetIssueDetails fetches the title and description of an issue.
	// Returns nil, nil if the provider doesn't support detail lookups.
	GetIssueDetails(ctx context.Context, ref *IssueRef) (*IssueDetails, error)

	// CreateIssue creates a new issue in the tracker.
	CreateIssue(ctx context.Context, opts CreateIssueOpts) (*IssueRef, error)

	// ShouldCreateIssues reports whether the tracker is configured to
	// auto-create issues for opportunities that lack an existing reference.
	ShouldCreateIssues() bool

	// GetIssueStatus fetches the current status and assignment info for an issue.
	// Returns nil, nil if the provider doesn't support status checks.
	GetIssueStatus(ctx context.Context, ref *IssueRef) (*IssueStatus, error)

	// PostComment posts a comment on an existing issue.
	PostComment(ctx context.Context, ref *IssueRef, body string) error
}

// CreateIssueOpts holds parameters for creating a new issue.
type CreateIssueOpts struct {
	Title       string
	Description string
	Category    string // "bug" or "feature"
}

// NoopTracker is a no-op implementation that returns nil for everything.
type NoopTracker struct{}

func (NoopTracker) ExtractIssueRef(string) *IssueRef       { return nil }
func (NoopTracker) ExtractAllIssueRefs(string) []*IssueRef { return nil }
func (NoopTracker) GetIssueDetails(context.Context, *IssueRef) (*IssueDetails, error) {
	return nil, nil
}
func (NoopTracker) CreateIssue(context.Context, CreateIssueOpts) (*IssueRef, error) { return nil, nil }
func (NoopTracker) ShouldCreateIssues() bool                                        { return false }
func (NoopTracker) GetIssueStatus(context.Context, *IssueRef) (*IssueStatus, error) { return nil, nil }
func (NoopTracker) PostComment(context.Context, *IssueRef, string) error            { return nil }

// NewTracker creates a Tracker from config. Returns NoopTracker when disabled.
func NewTracker(cfg config.IssueTrackerConfig) Tracker {
	if !cfg.Enabled {
		return NoopTracker{}
	}
	switch cfg.Provider {
	case "linear":
		return NewLinearTracker(cfg)
	default:
		return NoopTracker{}
	}
}
