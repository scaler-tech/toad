package digest

import (
	"time"
)

func (e *Engine) passesGuardrails(opp Opportunity) bool {
	// Confidence check
	minConf := 0.95
	if e.cfg != nil && e.cfg.MinConfidence > 0 {
		minConf = e.cfg.MinConfidence
	}
	// In comment mode (dry-run + comment investigation), lower the floor —
	// posting investigation findings has no downside so we can be more inclusive.
	if e.cfg != nil && e.cfg.DryRun && e.cfg.CommentInvestigation && minConf > 0.85 {
		minConf = 0.85
	}
	if opp.Confidence < minConf {
		return false
	}

	// Category check
	allowed := false
	for _, cat := range e.cfg.AllowedCategories {
		if opp.Category == cat {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}

	// Size check
	maxSize := e.cfg.MaxEstSize
	if maxSize == "tiny" && opp.EstSize != "tiny" {
		return false
	}
	if maxSize == "small" && opp.EstSize != "tiny" && opp.EstSize != "small" {
		return false
	}
	if maxSize == "medium" && opp.EstSize != "tiny" && opp.EstSize != "small" && opp.EstSize != "medium" {
		return false
	}

	return true
}

// trySpawn checks and increments the hourly spawn counter.
// Returns true if under the limit, false if at capacity.
func (e *Engine) trySpawn() bool {
	e.spawnMu.Lock()
	defer e.spawnMu.Unlock()

	currentHour := time.Now().Hour()
	if currentHour != e.spawnHour {
		e.spawnCount = 0
		e.spawnHour = currentHour
	}

	if e.spawnCount >= e.cfg.MaxAutoSpawnHour {
		return false
	}
	e.spawnCount++
	return true
}
