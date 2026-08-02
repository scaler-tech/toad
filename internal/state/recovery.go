package state

import (
	"log/slog"
)

// RecoverResult summarizes what was cleaned up on startup.
type RecoverResult struct {
	StaleInvestigations int
	StaleOpportunities  []*DigestOpportunity
}

// RecoverOnStartup finds any investigations left stuck mid-flight by a
// previous crash so callers can resume them. It previously also marked
// stale entries in the old runs pipeline as failed; that phase was removed
// along with the runs pipeline itself (see state.go).
func RecoverOnStartup(db *DB) (*RecoverResult, error) {
	result := &RecoverResult{}

	// Find stuck investigations (rows stay in DB until resume completes)
	staleOpps, err := db.StaleInvestigations()
	if err != nil {
		slog.Warn("failed to query stale investigations", "error", err)
	} else {
		result.StaleInvestigations = len(staleOpps)
		result.StaleOpportunities = staleOpps
	}

	if result.StaleInvestigations > 0 {
		slog.Info("recovery complete",
			"stale_investigations", result.StaleInvestigations,
		)
	}

	return result, nil
}
