package issuetracker

import (
	"context"
	"fmt"
	"log/slog"
)

// GateResult describes the outcome of checking whether a spawn should be gated
// because the referenced ticket is actively assigned to someone, already
// done, or delegated to an agent.
type GateResult struct {
	Gated     bool         // true if the spawn should be blocked
	Done      bool         // true if the ticket is in a terminal state (Done, Canceled, etc.)
	Delegated bool         // true if the ticket is delegated to an agent (Linear's native delegate field) — toad stays hands-off
	Status    *IssueStatus // the fetched status (non-nil only when Gated)
}

// GateOpts holds the parameters for CheckAssigneeGate.
type GateOpts struct {
	IssueRef       *IssueRef
	StaleDays      int    // assignments older than this are ignored
	Findings       string // investigation reasoning / task spec
	SlackPermalink string // link back to the Slack thread
}

// CheckAssigneeGate checks if an issue is actively assigned and, if so,
// posts a comment with findings to the ticket.
//
// Fails open: if the issue ref is nil, the status fetch fails, or the comment
// post fails, the spawn proceeds normally. This ensures a Linear API outage
// never blocks Toad from working.
func CheckAssigneeGate(ctx context.Context, tracker Tracker, opts GateOpts) *GateResult {
	if opts.IssueRef == nil {
		return &GateResult{Gated: false}
	}

	status, err := tracker.GetIssueStatus(ctx, opts.IssueRef)
	if err != nil {
		slog.Warn("issue status check failed, proceeding with spawn",
			"issue", opts.IssueRef.ID, "error", err)
		return &GateResult{Gated: false}
	}
	if status == nil {
		return &GateResult{Gated: false}
	}

	// Terminal state: ticket is already Done/Canceled — silently skip.
	// No comment, no spawn. Only a direct @toad invocation should override.
	if status.IsDone() {
		slog.Info("ticket is in terminal state, skipping silently",
			"issue", opts.IssueRef.ID, "state", status.State)
		return &GateResult{Gated: true, Done: true, Status: status}
	}

	// Delegated to an agent (e.g. Biome): the work has moved past toad's
	// area of influence — silently skip, same as the terminal-state case
	// above. No comment, no spawn.
	if status.IsDelegated() {
		slog.Info("ticket delegated to an agent, staying hands-off",
			"issue", opts.IssueRef.ID, "delegate", status.DelegateName)
		return &GateResult{Gated: true, Delegated: true, Status: status}
	}

	if !status.IsActivelyAssigned(opts.StaleDays) {
		slog.Debug("issue not actively assigned, proceeding with spawn",
			"issue", opts.IssueRef.ID, "assignee", status.AssigneeName,
			"state", status.State)
		return &GateResult{Gated: false}
	}

	// Build and post the comment to the ticket
	body := fmt.Sprintf(
		"## Toad investigation findings\n\n"+
			"%s\n\n"+
			"---\n"+
			"_Toad detected this issue in Slack but deferred to you since you're assigned._\n"+
			"_Say `@toad fix this` in the Slack thread to have Toad investigate anyway._",
		opts.Findings,
	)
	if opts.SlackPermalink != "" {
		body += fmt.Sprintf("\n_Slack thread: %s_", opts.SlackPermalink)
	}

	// Pass the already-resolved internal ID to avoid a redundant GetIssueStatus call.
	ref := *opts.IssueRef
	ref.InternalID = status.InternalID
	if err := tracker.PostComment(ctx, &ref, body); err != nil {
		slog.Warn("failed to post findings comment to ticket, proceeding with spawn",
			"issue", opts.IssueRef.ID, "error", err)
		return &GateResult{Gated: false}
	}

	slog.Info("ticket assignee gate: deferred to assignee",
		"issue", opts.IssueRef.ID, "assignee", status.AssigneeName,
		"state", status.State)

	return &GateResult{
		Gated:  true,
		Status: status,
	}
}
