package cmd

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/state"
)

// runOutcomePoller is the lightweight outcome loop: toad files tickets, and
// this poller watches what happens to them so the team can see whether
// toad's tickets are landing. Visibility only — it never changes toad's
// behavior based on what it finds.
//
// Outcomes are classified by classifyOutcome, which prefers the tracker's
// state TYPE (Linear's triage/backlog/unstarted/started/completed/canceled)
// over guessing from the bare status name: completed -> done, canceled ->
// rejected, triage -> pending, backlog/unstarted/started -> accepted. Both
// the status name and its state type are now persisted on ticket_index
// (last_status, last_state_type) so this classification survives restarts.
// Trackers that don't supply a state type fall back to name matching (see
// classifyOutcome).
//
// It ticks on interval, and each tick delegates to pollOnce.
func runOutcomePoller(ctx context.Context, db *state.DB, tracker issuetracker.Tracker, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			pollOnce(ctx, db, tracker, interval)
		case <-ctx.Done():
			return
		}
	}
}

// pollOnce checks the most recently seen tracked tickets for status
// changes. An entry is polled when its status has never been checked
// (StatusCheckedAt is zero) or was last checked longer than interval ago;
// otherwise it's skipped so we don't hammer the tracker's API. Successful
// checks persist the (possibly unchanged) status via UpdateTicketStatus,
// which also bumps status_checked_at — this is what prevents an entry from
// being re-polled on every subsequent tick. A status transition is logged
// at info; unchanged statuses are not logged. Per-entry errors are logged
// at debug and skipped without retrying, so one flaky lookup can't turn
// into a retry storm. ctx cancellation is checked between entries so a
// shutdown doesn't wait for the whole batch to finish.
//
// Exported as a standalone function (rather than folded into the ticker
// loop) so tests can drive a single deterministic pass without waiting on
// a real ticker.
func pollOnce(ctx context.Context, db *state.DB, tracker issuetracker.Tracker, interval time.Duration) {
	entries, err := db.RecentTicketIndex(100)
	if err != nil {
		slog.Debug("outcome poller: failed to list ticket index", "error", err)
		return
	}

	now := time.Now()
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}

		if !e.StatusCheckedAt.IsZero() && now.Sub(e.StatusCheckedAt) < interval {
			continue
		}

		status, err := tracker.GetIssueStatus(ctx, &issuetracker.IssueRef{ID: e.IssueID})
		if err != nil {
			slog.Debug("outcome poller: status check failed", "issue", e.IssueID, "error", err)
			continue
		}
		if status == nil {
			// Provider doesn't support status checks (or issue not found).
			continue
		}

		if status.State != e.LastStatus {
			slog.Info("ticket outcome", "issue", e.IssueID, "from", e.LastStatus, "to", status.State)
		}

		if err := db.UpdateTicketStatus(e.ExternalKey, status.State, status.StateType); err != nil {
			slog.Debug("outcome poller: failed to persist ticket status", "issue", e.IssueID, "error", err)
		}
	}
}

// outcomeCounts buckets tracked tickets by a coarse classification of their
// last known status (and state type, where known), for the status
// dashboard. Visibility only — this has no bearing on toad's behavior.
func outcomeCounts(db *state.DB) (map[string]int, error) {
	entries, err := db.RecentTicketIndex(1000)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, 6)
	for _, e := range entries {
		counts[classifyOutcome(e.LastStatus, e.LastStateType)]++
	}
	return counts, nil
}

// classifyOutcome buckets a raw tracker status (and, when available, its
// Linear workflow state type) into a coarse outcome for the dashboard:
//   - "filed": no status has been polled yet (ticket was just created)
//   - "pending": state type "triage" — awaiting triage, not yet in a workflow
//   - "accepted": state type "backlog", "unstarted", or "started" — queued
//     or actively being worked
//   - "done": state type "completed" (or, on name fallback, a "done" status)
//   - "rejected": state type "canceled" (or, on name fallback, a
//     canceled/duplicate status, either spelling)
//   - "unknown": anything else, including custom workflow states when no
//     state type is available to disambiguate them
//
// When stateType is empty (older persisted rows, or a tracker that doesn't
// supply Linear-style state types) classification falls back to matching
// the bare status name, preserving the pre-state-type behavior with one
// rename: "done" used to bucket as "accepted" and now buckets as "done" to
// line up with the type-based classification above.
func classifyOutcome(status, stateType string) string {
	if status == "" {
		return "filed"
	}

	switch strings.ToLower(stateType) {
	case "completed":
		return "done"
	case "canceled", "cancelled": //nolint:misspell // Linear uses British spelling
		return "rejected"
	case "triage":
		return "pending"
	case "backlog", "unstarted", "started":
		return "accepted"
	}

	// stateType is either empty or something outside Linear's known set
	// (e.g. a non-Linear tracker, or a future Linear type this switch
	// hasn't been taught yet). Both are deliberately treated the same way:
	// fall back to name matching rather than bucketing straight to
	// "unknown". This widens the fallback rather than narrowing it — an
	// unrecognized type is no worse a signal than no type at all.
	switch strings.ToLower(status) {
	case "cancelled", "canceled", "duplicate": //nolint:misspell // Linear uses British spelling
		return "rejected"
	case "done":
		return "done"
	default:
		return "unknown"
	}
}
