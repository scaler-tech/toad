// Package responder is toad's conversational core: one agent that sees the
// conversation, toad's prior findings, and ticket context, decides for
// itself whether to answer directly or read code, and returns a structured
// envelope. Surfaces (Slack ribbit, Linear agent sessions) adapt around it.
package responder

import (
	"encoding/json"
	"strings"
)

// TicketUpdate is a proposed safe ticket edit. Toad's code — never the
// agent — applies it, and only the title/description/comment subset exists.
type TicketUpdate struct {
	Issue       string `json:"issue"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Comment     string `json:"comment"`
}

// IsZero reports whether the update carries no actual change.
func (u *TicketUpdate) IsZero() bool {
	return u == nil || (u.Title == "" && u.Description == "" && u.Comment == "")
}

// Envelope is the responder agent's structured output.
type Envelope struct {
	Reply           string        `json:"reply"`
	TicketUpdate    *TicketUpdate `json:"ticket_update"`
	DidInvestigate  bool          `json:"did_investigate"`
	FindingsSummary string        `json:"findings_summary"`
}

// ParseEnvelope extracts the envelope from agent output. It tolerates prose
// and markdown fences around the JSON (same salvage idea as
// investigation.ParseFindings). It never returns an error: output with no
// usable envelope degrades to Envelope{Reply: raw} — a lost structure costs
// a pretty reply, never a guessed mutation.
func ParseEnvelope(raw string) *Envelope {
	text := strings.TrimSpace(raw)

	// Strategy 1: the literal marker `{"reply"` through the last `}`.
	if start := strings.Index(text, `{"reply"`); start >= 0 {
		if end := strings.LastIndex(text, "}"); end > start {
			if e := tryUnmarshal(text[start : end+1]); e != nil {
				return e
			}
		}
	}

	// Strategy 2: strip fences, first `{` to last `}`.
	stripped := text
	stripped = strings.TrimPrefix(stripped, "```json")
	stripped = strings.TrimPrefix(stripped, "```")
	stripped = strings.TrimSuffix(stripped, "```")
	if start := strings.Index(stripped, "{"); start >= 0 {
		if end := strings.LastIndex(stripped, "}"); end > start {
			if e := tryUnmarshal(stripped[start : end+1]); e != nil {
				return e
			}
		}
	}

	return &Envelope{Reply: text}
}

// tryUnmarshal returns a valid envelope or nil. An envelope with an empty
// reply is not valid — there is nothing to show the human.
func tryUnmarshal(s string) *Envelope {
	var e Envelope
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		return nil
	}
	if strings.TrimSpace(e.Reply) == "" {
		return nil
	}
	return &e
}
