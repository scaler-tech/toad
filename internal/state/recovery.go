package state

import (
	"log/slog"
)

// RecoverResult summarizes what was cleaned up on startup.
type RecoverResult struct {
	StaleRuns           int
	StaleInvestigations int
	StaleOpportunities  []*DigestOpportunity
}

// RecoverOnStartup finds runs left in active states from a previous crash and
// marks them failed, and finds any investigations stuck mid-flight so callers
// can resume them.
func RecoverOnStartup(db *DB) (*RecoverResult, error) {
	result := &RecoverResult{}

	// 1. Find active runs (starting/investigating) and mark them failed
	active, err := db.ActiveRuns()
	if err != nil {
		return nil, err
	}

	for _, run := range active {
		slog.Warn("recovering stale run",
			"id", run.ID,
			"status", run.Status,
			"branch", run.Branch,
		)

		if err := db.CompleteRun(run.ID, &RunResult{
			Success: false,
			Error:   "toad crashed during execution",
		}); err != nil {
			slog.Error("failed to mark stale run as failed", "id", run.ID, "error", err)
			continue
		}
		result.StaleRuns++
	}

	// 2. Find stuck investigations (rows stay in DB until resume completes)
	staleOpps, err := db.StaleInvestigations()
	if err != nil {
		slog.Warn("failed to query stale investigations", "error", err)
	} else {
		result.StaleInvestigations = len(staleOpps)
		result.StaleOpportunities = staleOpps
	}

	if result.StaleRuns > 0 || result.StaleInvestigations > 0 {
		slog.Info("recovery complete",
			"stale_runs", result.StaleRuns,
			"stale_investigations", result.StaleInvestigations,
		)
	}

	return result, nil
}
