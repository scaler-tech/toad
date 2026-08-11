package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/linearagent"
	"github.com/scaler-tech/toad/internal/responder"
	"github.com/scaler-tech/toad/internal/state"
)

// linearAgentRespond bridges a Linear agent session to the responder: it
// assembles the Conversation (session transcript, prior findings, ticket
// context) and runs one conversational turn. The responder decides for
// itself whether the ask needs code reading.
func linearAgentRespond(deps flowDeps, resp *responder.Engine) func(ctx context.Context, w linearagent.Work) (*responder.Envelope, error) {
	return func(ctx context.Context, w linearagent.Work) (*responder.Envelope, error) {
		conv := responder.Conversation{
			Surface:  responder.SurfaceLinear,
			Messages: sessionMessages(w),
			Repo:     deps.resolver.Resolve("", nil),
		}

		// Prior findings: the session's own records first, then anything
		// linked to the ticket by a past filing. Best-effort on nil DB.
		if db := deps.stateManager.DB(); db != nil {
			if rec, err := db.GetInvestigationByThread("linear-session:" + w.Session.ID); err == nil && rec != nil {
				conv.PriorFindings = renderPriorFindings(rec)
			}
			if conv.PriorFindings == "" && w.Session.IssueIdentifier != "" {
				if rec, err := db.FindInvestigationByTicket(w.Session.IssueIdentifier); err == nil && rec != nil {
					conv.PriorFindings = renderPriorFindings(rec)
				}
			}
		}

		if w.Session.IssueIdentifier != "" {
			ref := &issuetracker.IssueRef{Provider: "linear", ID: w.Session.IssueIdentifier}
			if details, err := deps.tracker.GetIssueDetails(ctx, ref); err == nil && details != nil {
				var comments []string
				for _, c := range details.Comments {
					comments = append(comments, fmt.Sprintf("Comment (%s): %s", c.Author, c.Body))
				}
				conv.TicketContext = renderTicketContextBlock([]ticketItem{{
					ID:          details.ID,
					Title:       details.Title,
					Description: details.Description,
					Comments:    comments,
				}})
			}
		}

		return resp.Respond(ctx, conv)
	}
}

// sessionMessages renders the session transcript as conversation turns:
// the mention comment first, then prompts (user) and responses (toad) in
// order. Thoughts and errors are agent-side noise and are omitted. The
// current prompt is always the last message.
func sessionMessages(w linearagent.Work) []responder.Message {
	var msgs []responder.Message
	if w.Session.SourceComment != "" && w.Session.SourceComment != w.Prompt {
		msgs = append(msgs, responder.Message{Role: "user", Text: w.Session.SourceComment})
	}

	// Find the LAST prompt activity whose body matches the current prompt —
	// that one IS the current prompt (already appended below) and must be
	// skipped here. An earlier activity that happens to have the same text
	// (a teammate genuinely repeating themselves) is a distinct turn and
	// must be kept, so only this one occurrence is skipped, not every match.
	skipIdx := -1
	for i, a := range w.Session.Activities {
		if a.Type == "prompt" && a.Body == w.Prompt {
			skipIdx = i
		}
	}

	for i, a := range w.Session.Activities {
		switch a.Type {
		case "prompt":
			if i == skipIdx {
				continue
			}
			msgs = append(msgs, responder.Message{Role: "user", Text: a.Body})
		case "response":
			msgs = append(msgs, responder.Message{Role: "toad", Text: a.Body})
		}
	}
	msgs = append(msgs, responder.Message{Role: "user", Text: w.Prompt})
	return msgs
}

// renderPriorFindings turns a stored investigation record into a prompt
// block with its age. Records hold either investigation.Findings (Slack/
// digest flows, pre-responder sessions) or responder.Envelope (responder
// sessions) — take the prose from whichever parses.
func renderPriorFindings(rec *state.InvestigationRecord) string {
	if rec == nil {
		return ""
	}
	age := time.Since(rec.CreatedAt).Round(time.Minute)
	var f investigation.Findings
	if err := json.Unmarshal([]byte(rec.FindingsJSON), &f); err == nil && strings.TrimSpace(f.Reasoning) != "" {
		return fmt.Sprintf("Investigated %s ago: %s", age, f.Reasoning)
	}
	var env responder.Envelope
	if err := json.Unmarshal([]byte(rec.FindingsJSON), &env); err == nil && strings.TrimSpace(env.FindingsSummary) != "" {
		return fmt.Sprintf("Investigated %s ago: %s", age, env.FindingsSummary)
	}
	return ""
}

// applyTicketUpdate is the common core of applying a safe ticket edit —
// title/description via UpdateIssue, comment via PostComment — shared by
// every caller that needs to apply a responder.TicketUpdate against a
// tracker (currently the Linear agent bridge below; Task 8's ribbit/CTA
// path reuses it too rather than re-implementing the same two calls).
func applyTicketUpdate(ctx context.Context, tracker issuetracker.Tracker, issueIdentifier string, u responder.TicketUpdate) error {
	ref := &issuetracker.IssueRef{Provider: "linear", ID: issueIdentifier}
	if u.Title != "" || u.Description != "" {
		if err := tracker.UpdateIssue(ctx, ref, issuetracker.UpdateIssueOpts{Title: u.Title, Description: u.Description}); err != nil {
			return err
		}
	}
	if u.Comment != "" {
		if err := tracker.PostComment(ctx, ref, u.Comment); err != nil {
			return err
		}
	}
	return nil
}

// linearAgentUpdateTicket applies a responder-proposed safe ticket edit via
// applyTicketUpdate, bound to this flow's tracker dependency.
func linearAgentUpdateTicket(deps flowDeps) func(ctx context.Context, issueIdentifier string, u responder.TicketUpdate) error {
	return func(ctx context.Context, issueIdentifier string, u responder.TicketUpdate) error {
		return applyTicketUpdate(ctx, deps.tracker, issueIdentifier, u)
	}
}
