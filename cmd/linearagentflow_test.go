package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/linearagent"
	"github.com/scaler-tech/toad/internal/responder"
	"github.com/scaler-tech/toad/internal/state"
)

func TestSessionMessages_TranscriptRolesAndPromptLast(t *testing.T) {
	w := linearagent.Work{
		Session: linearagent.Session{
			SourceComment: "@toad why slow?",
			Activities: []linearagent.Activity{
				{Type: "thought", Body: "Reading.", CreatedAt: time.Now().Add(-3 * time.Minute)},
				{Type: "response", Body: "The cap was removed.", CreatedAt: time.Now().Add(-2 * time.Minute)},
				{Type: "prompt", Body: "update the ticket", CreatedAt: time.Now().Add(-time.Minute)},
			},
		},
		Prompt: "update the ticket",
	}
	msgs := sessionMessages(w)
	if len(msgs) < 3 {
		t.Fatalf("msgs = %+v", msgs)
	}
	if msgs[0].Role != "user" || !strings.Contains(msgs[0].Text, "why slow") {
		t.Errorf("first message should be the source comment, got %+v", msgs[0])
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" || last.Text != "update the ticket" {
		t.Errorf("last message must be the current prompt, got %+v", last)
	}
	// toad's response activity appears with role toad; thoughts are omitted.
	for _, m := range msgs {
		if strings.Contains(m.Text, "Reading.") {
			t.Error("thought activities are noise — omit them")
		}
	}
}

func TestSessionMessages_DuplicateEarlierPromptKept(t *testing.T) {
	w := linearagent.Work{
		Session: linearagent.Session{
			Activities: []linearagent.Activity{
				{Type: "prompt", Body: "what's the status?", CreatedAt: time.Now().Add(-10 * time.Minute)},
				{Type: "response", Body: "Still investigating.", CreatedAt: time.Now().Add(-9 * time.Minute)},
				{Type: "prompt", Body: "what's the status?", CreatedAt: time.Now().Add(-time.Minute)},
			},
		},
		Prompt: "what's the status?",
	}
	msgs := sessionMessages(w)

	// Both the earlier duplicate prompt and the trailing current-prompt
	// append should survive — only the LAST matching activity (the one that
	// IS the current prompt) is skipped from the transcript loop.
	count := 0
	for _, m := range msgs {
		if m.Role == "user" && m.Text == "what's the status?" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("got %d occurrences of the repeated prompt, want 2 (earlier genuine turn + trailing current prompt): %+v", count, msgs)
	}
	if !strings.Contains(msgs[len(msgs)-1].Text, "what's the status?") || msgs[len(msgs)-1].Role != "user" {
		t.Errorf("last message must still be the current prompt, got %+v", msgs[len(msgs)-1])
	}
}

func TestRenderPriorFindings_HandlesBothShapes(t *testing.T) {
	f := investigation.Findings{Feasible: true, Reasoning: "the cap is gone", Repo: "mono"}
	fj, _ := json.Marshal(f)
	got := renderPriorFindings(&state.InvestigationRecord{FindingsJSON: string(fj), CreatedAt: time.Now().Add(-2 * time.Hour)})
	if !strings.Contains(got, "the cap is gone") || !strings.Contains(got, "ago") {
		t.Errorf("findings shape: %q", got)
	}

	env := responder.Envelope{Reply: "r", DidInvestigate: true, FindingsSummary: "loop is unbounded"}
	ej, _ := json.Marshal(env)
	got = renderPriorFindings(&state.InvestigationRecord{FindingsJSON: string(ej), CreatedAt: time.Now().Add(-30 * time.Minute)})
	if !strings.Contains(got, "loop is unbounded") {
		t.Errorf("envelope shape: %q", got)
	}

	if renderPriorFindings(nil) != "" {
		t.Error("nil record renders empty")
	}
}
