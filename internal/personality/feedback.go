package personality

import (
	"fmt"
	"log/slog"
)

type OutcomeSignal struct {
	Type         string
	PRURL        string
	ReviewRounds int
	Metadata     map[string]string
}

type emojiMapping struct {
	traits     []string
	directions []float64
}

func defaultEmojiMappings() map[string]emojiMapping {
	return map[string]emojiMapping{
		"turtle":      {traits: []string{"thoroughness", "speed_vs_polish"}, directions: []float64{-0.03, 0.03}},
		"rabbit":      {traits: []string{"thoroughness", "context_hunger"}, directions: []float64{0.03, 0.03}},
		"mute":        {traits: []string{"verbosity", "explanation_depth"}, directions: []float64{-0.03, -0.03}},
		"loudspeaker": {traits: []string{"verbosity", "explanation_depth"}, directions: []float64{0.03, 0.03}},
		"ocean":       {traits: []string{"scope_appetite"}, directions: []float64{-0.03}},
		"test_tube":   {traits: []string{"test_affinity"}, directions: []float64{0.03}},
		"bulb":        {traits: []string{"creativity"}, directions: []float64{0.03}},
	}
}

func (m *Manager) ProcessEmoji(emoji, context string) error {
	if !m.LearningEnabled() {
		return nil
	}

	if emoji == "dart" {
		return m.processReinforce([]string{"scope_appetite", "risk_tolerance"}, context)
	}

	mappings := defaultEmojiMappings()
	mapping, ok := mappings[emoji]
	if !ok {
		return nil // unknown emoji is a no-op, not an error
	}

	for i, trait := range mapping.traits {
		detail := fmt.Sprintf(":%s: on %s", emoji, truncate(context, 50))
		if err := m.applyAdjustment(trait, mapping.directions[i], "emoji", detail, ""); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) processReinforce(traits []string, context string) error {
	if m.store == nil {
		return nil
	}

	recent, err := m.store.Recent(50)
	if err != nil {
		return err
	}

	for _, trait := range traits {
		for _, adj := range recent {
			if adj.Trait == trait && adj.Delta < 0 {
				correction := -adj.Delta * 0.5
				detail := fmt.Sprintf(":dart: reinforced %s (context: %s)", trait, truncate(context, 40))
				if err := m.applyAdjustment(trait, correction, "emoji", detail, "reinforcement"); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func (m *Manager) ProcessOutcome(signal OutcomeSignal) error {
	if !m.LearningEnabled() {
		return nil
	}

	detail := fmt.Sprintf("%s: %s", signal.Type, signal.PRURL)

	apply := func(trait string, delta float64, reasoning string) {
		if err := m.applyAdjustment(trait, delta, "outcome", detail, reasoning); err != nil {
			slog.Error("personality outcome adjustment failed",
				"trait", trait, "delta", delta, "error", err)
		}
	}

	switch signal.Type {
	case "pr_merged":
		apply("risk_tolerance", 0.02, "PR merged cleanly")
		apply("scope_appetite", 0.01, "PR merged cleanly")
		apply("test_affinity", 0.01, "PR merged cleanly")
		apply("strictness", 0.01, "PR merged cleanly")
	case "pr_closed":
		apply("risk_tolerance", -0.03, "PR closed without merge")
		apply("confidence_threshold", 0.02, "PR closed without merge")
		apply("scope_sensitivity", 0.02, "PR closed without merge")
	case "pr_review_rounds":
		if signal.ReviewRounds > 1 {
			reason := fmt.Sprintf("PR needed %d review rounds", signal.ReviewRounds)
			apply("strictness", 0.02, reason)
			apply("pattern_conformity", 0.01, reason)
			apply("collaboration", 0.01, reason)
		}
	case "ribbit_followup":
		apply("thoroughness", 0.01, "user followed up")
		apply("explanation_depth", 0.01, "user followed up")
	case "digest_dismissed":
		apply("confidence_threshold", 0.02, "digest opportunity dismissed")
	case "digest_approved":
		apply("confidence_threshold", -0.01, "digest opportunity approved")
		apply("autonomy", 0.01, "digest opportunity approved")
	}

	return nil
}

func truncate(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "..."
}
