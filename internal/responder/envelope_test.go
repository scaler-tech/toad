package responder

import (
	"strings"
	"testing"
)

func TestParseEnvelope_CleanJSON(t *testing.T) {
	e := ParseEnvelope(`{"reply":"The cap was removed in 5f17415.","ticket_update":{"issue":"DAT-5107","comment":"Root cause: unbounded query loop."},"did_investigate":true,"findings_summary":"facetsForScope has no page cap."}`)
	if e.Reply != "The cap was removed in 5f17415." {
		t.Errorf("reply = %q", e.Reply)
	}
	if e.TicketUpdate == nil || e.TicketUpdate.Issue != "DAT-5107" || e.TicketUpdate.Comment == "" {
		t.Errorf("ticket_update = %+v", e.TicketUpdate)
	}
	if !e.DidInvestigate || e.FindingsSummary == "" {
		t.Errorf("investigate flags = %v %q", e.DidInvestigate, e.FindingsSummary)
	}
}

func TestParseEnvelope_FencedAndProseWrapped(t *testing.T) {
	raw := "Here is my answer:\n```json\n{\"reply\":\"Done.\",\"did_investigate\":false}\n```\nHope that helps."
	e := ParseEnvelope(raw)
	if e.Reply != "Done." {
		t.Errorf("reply = %q", e.Reply)
	}
	if e.TicketUpdate != nil {
		t.Errorf("ticket_update should be nil, got %+v", e.TicketUpdate)
	}
}

func TestParseEnvelope_GarbageFallsBackToReply(t *testing.T) {
	raw := "I looked at the code and the answer is 42. No JSON here."
	e := ParseEnvelope(raw)
	if e.Reply != raw {
		t.Errorf("fallback reply = %q, want the whole text", e.Reply)
	}
	if e.TicketUpdate != nil {
		t.Error("fallback must NEVER carry a ticket update")
	}
	if e.DidInvestigate || e.FindingsSummary != "" {
		t.Error("fallback must not claim an investigation")
	}
}

func TestParseEnvelope_EmptyReplyFallsBackToText(t *testing.T) {
	// A JSON object without a reply is useless — treat the raw text as the reply.
	raw := `{"did_investigate":false}`
	e := ParseEnvelope(raw)
	if !strings.Contains(e.Reply, "did_investigate") {
		t.Errorf("reply = %q, want raw text fallback", e.Reply)
	}
}

func TestTicketUpdate_IsZero(t *testing.T) {
	if !(&TicketUpdate{}).IsZero() {
		t.Error("empty update should be zero")
	}
	if (&TicketUpdate{Comment: "x"}).IsZero() {
		t.Error("update with a comment is not zero")
	}
	if !(&TicketUpdate{Issue: "DAT-1"}).IsZero() {
		t.Error("an issue name alone (no title/description/comment) IS zero — nothing to apply")
	}
}
