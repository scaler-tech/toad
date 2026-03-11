package personality

import "math"

type Mode string

const (
	ModeRibbit  Mode = "ribbit"
	ModeTadpole Mode = "tadpole"
	ModeDigest  Mode = "digest"
	ModeTriage  Mode = "triage"
)

type Overrides struct {
	MaxTurns        *int
	MaxRetries      *int
	MaxFilesChanged *int
	TimeoutMinutes  *int
	MinConfidence   *float64
	MaxEstSize      *string
}

func intPtr(v int) *int             { return &v }
func float64Ptr(v float64) *float64 { return &v }
func stringPtr(v string) *string    { return &v }

func (m *Manager) PromptFragments(mode Mode) []string {
	t := m.Effective()
	var frags []string

	switch mode {
	case ModeRibbit:
		frags = append(frags, thoroughnessFragment(t.Thoroughness))
		frags = append(frags, contextHungerFragment(t.ContextHunger))
		frags = append(frags, verbosityFragment(t.Verbosity))
		frags = append(frags, explanationDepthFragment(t.ExplanationDepth))
		frags = append(frags, toneFragment(t.Tone))
		frags = append(frags, defensivenessFragment(t.Defensiveness))
	case ModeTadpole:
		frags = append(frags, riskToleranceFragment(t.RiskTolerance))
		frags = append(frags, scopeAppetiteFragment(t.ScopeAppetite))
		frags = append(frags, testAffinityFragment(t.TestAffinity))
		frags = append(frags, creativityFragment(t.Creativity))
		frags = append(frags, patternConformityFragment(t.PatternConformity))
		frags = append(frags, documentationDriveFragment(t.DocumentationDrive))
		frags = append(frags, strictnessFragment(t.Strictness))
	case ModeDigest:
		frags = append(frags, patternRecognitionFragment(t.PatternRecognition))
		frags = append(frags, initiativeFragment(t.Initiative))
	case ModeTriage:
		// minimal
	}

	var result []string
	for _, f := range frags {
		if f != "" {
			result = append(result, f)
		}
	}
	return result
}

func (m *Manager) ConfigOverrides(mode Mode) Overrides {
	t := m.Effective()
	var ov Overrides

	switch mode {
	case ModeRibbit:
		ov.MaxTurns = intPtr(scaleInt(t.Thoroughness, 5, 15))
		ov.TimeoutMinutes = intPtr(scaleInt(t.SpeedVsPolish, 5, 15))
	case ModeTadpole:
		ov.MaxTurns = intPtr(scaleInt(t.SpeedVsPolish, 15, 45))
		ov.MaxRetries = intPtr(scaleInt(t.RetryPersistence, 0, 3))
		ov.MaxFilesChanged = intPtr(scaleInt(t.RiskTolerance, 3, 10))
		ov.TimeoutMinutes = intPtr(scaleInt(t.SpeedVsPolish, 5, 15))
	case ModeDigest:
		ov.MinConfidence = float64Ptr(scaleFloat(t.ConfidenceThreshold, 0.80, 0.99))
		ov.MaxEstSize = maxEstSizeFromTrait(t.ScopeSensitivity)
	case ModeTriage:
		ov.MinConfidence = float64Ptr(scaleFloat(t.ConfidenceThreshold, 0.80, 0.99))
	}

	return ov
}

func scaleInt(trait float64, lo, hi int) int {
	return lo + int(math.Round(trait*float64(hi-lo)))
}

func scaleFloat(trait float64, lo, hi float64) float64 {
	return lo + trait*(hi-lo)
}

func maxEstSizeFromTrait(scopeSensitivity float64) *string {
	if scopeSensitivity > 0.7 {
		return stringPtr("small")
	}
	if scopeSensitivity > 0.4 {
		return stringPtr("medium")
	}
	return stringPtr("large")
}

// Fragment functions - each returns a prompt instruction based on trait value

func thoroughnessFragment(v float64) string {
	if v < 0.3 {
		return "Do a quick scan. Focus on the most obvious answer."
	}
	if v < 0.6 {
		return "Search the codebase to find the answer."
	}
	if v < 0.8 {
		return "Search thoroughly. Check related files and trace call chains."
	}
	return "Do an exhaustive investigation. Trace every call chain, check tests, read git history."
}

func contextHungerFragment(v float64) string {
	if v < 0.3 {
		return "Focus on the specific file or function mentioned."
	}
	if v < 0.7 {
		return ""
	}
	return "Pull in surrounding context: callers, tests, and related files."
}

func verbosityFragment(v float64) string {
	if v < 0.3 {
		return "Be extremely concise — 1-3 lines max."
	}
	if v < 0.5 {
		return "Keep it short (3-5 lines for questions, up to 10 for bugs)."
	}
	if v < 0.7 {
		return "Give a clear, detailed answer. Up to 15 lines is fine."
	}
	return "Provide a thorough explanation with full context and examples."
}

func explanationDepthFragment(v float64) string {
	if v < 0.3 {
		return "Just give the answer — no need to explain why."
	}
	if v < 0.6 {
		return ""
	}
	return "Explain your reasoning — include why, not just what."
}

func toneFragment(v float64) string {
	if v < 0.3 {
		return "Be purely technical and professional."
	}
	if v < 0.7 {
		return "Be conversational but focused."
	}
	return "Be friendly and approachable. A bit of personality is welcome."
}

func defensivenessFragment(v float64) string {
	if v < 0.4 {
		return ""
	}
	if v < 0.7 {
		return "If the request seems off, briefly mention potential issues."
	}
	return "If you see problems with the approach, push back and suggest alternatives."
}

func riskToleranceFragment(v float64) string {
	if v < 0.3 {
		return "Make the absolute minimum change needed. Touch as few files as possible."
	}
	if v < 0.6 {
		return "Make focused changes. Small refactors are okay if they serve the fix."
	}
	return "Don't shy away from broader changes if they're the right solution."
}

func scopeAppetiteFragment(v float64) string {
	if v < 0.3 {
		return "Fix ONLY the specific issue. Do NOT touch any unrelated code."
	}
	if v < 0.6 {
		return "Focus on the task but fix obvious adjacent issues if trivial."
	}
	return "Improve code you encounter — fix naming, clean up patterns, update related tests."
}

func testAffinityFragment(v float64) string {
	if v < 0.3 {
		return ""
	}
	if v < 0.6 {
		return "Add tests if you're changing behavior that isn't covered."
	}
	return "Always write or update tests alongside your changes. Ensure new behavior is tested."
}

func creativityFragment(v float64) string {
	if v < 0.3 {
		return "Follow existing patterns exactly. Do not introduce new approaches."
	}
	if v < 0.7 {
		return ""
	}
	return "If you see a better approach than existing patterns, suggest and implement it."
}

func patternConformityFragment(v float64) string {
	if v < 0.4 {
		return ""
	}
	if v < 0.7 {
		return "Follow existing code style and naming conventions."
	}
	return "Match existing code style exactly — naming, error handling, structure, patterns."
}

func documentationDriveFragment(v float64) string {
	if v < 0.3 {
		return "Do NOT add comments or documentation unless absolutely necessary."
	}
	if v < 0.6 {
		return ""
	}
	return "Add clear comments for non-obvious logic. Update relevant docs if needed."
}

func strictnessFragment(v float64) string {
	if v < 0.3 {
		return ""
	}
	if v < 0.7 {
		return "All tests and linting must pass."
	}
	return "Zero tolerance — all tests pass, all lint clean, no warnings."
}

func patternRecognitionFragment(v float64) string {
	if v < 0.5 {
		return ""
	}
	return "Look for patterns — if this bug exists in one place, check if similar code has the same issue."
}

func initiativeFragment(v float64) string {
	if v < 0.5 {
		return ""
	}
	return "If you notice improvements beyond the immediate task, mention them in your response."
}
