package digest

import (
	"time"
)

// minConfidence returns the active confidence floor: 0.95 by default,
// overridden by cfg.MinConfidence when set. A nil cfg just falls back to the
// 0.95 default (each caller's own nil-cfg handling for anything beyond
// confidence — e.g. passesGuardrails' fail-closed category/size checks — is
// unaffected).
func (e *Engine) minConfidence() float64 {
	minConf := 0.95
	if e.cfg != nil && e.cfg.MinConfidence > 0 {
		minConf = e.cfg.MinConfidence
	}
	return minConf
}

func (e *Engine) passesGuardrails(opp Opportunity) bool {
	if opp.Confidence < e.minConfidence() {
		return false
	}

	// A nil cfg means the engine was never configured with guardrails at
	// all — fail closed rather than dereference a nil pointer for the
	// category/size checks below.
	if e.cfg == nil {
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
	// A nil cfg has no configured hourly cap — conservative behavior is to
	// refuse to spawn rather than dereference a nil pointer.
	if e.cfg == nil {
		return false
	}

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
