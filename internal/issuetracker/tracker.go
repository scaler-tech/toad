// Package issuetracker provides a generic interface for issue tracker integrations.
package issuetracker

import (
	"context"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/config"
)

// IssueDetails holds the title and description of an issue, used to enrich
// investigation prompts with ticket context.
type IssueDetails struct {
	ID          string // "PLF-3198"
	InternalID  string // provider's internal UUID
	Title       string
	Description string
	URL         string
	Comments    []IssueComment
}

// IssueComment holds a single comment on an issue.
type IssueComment struct {
	Author string
	Body   string
}

// IssueStatus represents the current state and assignment of an issue.
type IssueStatus struct {
	State        string    // e.g. "In Progress", "Todo", "Done"
	StateType    string    // Linear workflow state type: triage/backlog/unstarted/started/completed/canceled (empty if unknown/unsupported)
	AssigneeName string    // display name of assignee, empty if unassigned
	AssignedAt   time.Time // when the issue was last updated (proxy for assignment recency)
	InternalID   string    // provider's internal UUID (needed for mutations)
}

// terminalStates lists issue states that mean the work is finished.
// Toad should not act on these unless explicitly invoked.
// Keys are lowercase for case-insensitive matching.
var terminalStates = map[string]bool{
	"done":      true,
	"cancelled": true, //nolint:misspell // Linear uses British spelling
	"canceled":  true,
	"duplicate": true,
}

// IsDone returns true if the issue is in a terminal state (Done, Canceled, etc.).
func (s *IssueStatus) IsDone() bool {
	return terminalStates[strings.ToLower(s.State)]
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

	// AssignedTo and DelegatedTo carry the requested name/email that was
	// actually applied — CreateIssueOpts.Assignees resolved to a human user
	// (AssignedTo, -> assigneeId) and/or an app/agent user like Biome
	// (DelegatedTo, -> delegateId). Both empty when no assignee/delegate was
	// requested or none of the requested names resolved to anything usable
	// for that slot. UnresolvedAssignees lists requested names that didn't
	// resolve to any Linear user at all (as opposed to resolving fine but
	// losing out to an earlier candidate for the same slot — see
	// LinearTracker.resolveAssignees). Callers (cmd/ticketflow.go's
	// composeFiledReply) surface all three in the Slack reply so a
	// silently-dropped assignment/delegation is visible, unlike
	// LinearTeam/LinearProject's resolution failures, which fall back
	// silently.
	AssignedTo          string
	DelegatedTo         string
	UnresolvedAssignees []string
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
	Category    string   // "bug" or "feature"
	StateID     string   // optional Linear workflow state UUID
	Labels      []string // extra label IDs beyond bug/feature mapping

	// Team optionally overrides the configured default team; a key ("ANA"),
	// name ("Analytics"), or UUID. Project optionally names a project to
	// attach the issue to, resolved within the effective team. Both resolve
	// against the tracker's existing teams/projects with warn-and-fallback:
	// an unknown Team files to the default team, an unknown Project files
	// with no project — resolution failures never block issue creation.
	Team    string
	Project string

	// Assignees are display names, real names, or emails, in request order —
	// each resolved to a Linear user (and, via that resolution, whether it's
	// a human or an app/agent user like Biome). The tracker routes the
	// first resolved HUMAN to the issue's single assignee slot and the
	// first resolved AGENT to its single delegate slot (Linear supports one
	// of each); later candidates of either kind are logged and skipped
	// rather than silently dropped from visibility, and a name that doesn't
	// resolve to any Linear user at all is warned and reported back via the
	// returned IssueRef's UnresolvedAssignees. None of this ever blocks
	// issue creation — resolution failures fall back to leaving that slot
	// empty.
	Assignees []string
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
