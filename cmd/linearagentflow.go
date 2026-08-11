package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/linearagent"
)

// linearAgentInvestigate bridges a Linear agent session to the standard
// read-only investigation: fetch the issue (title, description, up to 20
// comments), resolve the repo, run the investigation with the session
// prompt as the request text.
func linearAgentInvestigate(deps flowDeps) func(ctx context.Context, w linearagent.Work) (*investigation.Findings, error) {
	return func(ctx context.Context, w linearagent.Work) (*investigation.Findings, error) {
		var threadContext []string
		if w.FollowUp {
			// A follow-up prompt on a session toad already answered once —
			// carry the prior investigation's conclusion forward so the
			// second pass isn't starting cold. Best-effort: a nil DB (some
			// tests construct flowDeps without a state manager) or a lookup
			// miss/error just means no prior context, not a failure.
			if db := deps.stateManager.DB(); db != nil {
				if rec, err := db.GetInvestigationByThread("linear-session:" + w.Session.ID); err == nil && rec != nil {
					var prior investigation.Findings
					if err := json.Unmarshal([]byte(rec.FindingsJSON), &prior); err == nil {
						threadContext = append(threadContext, "Toad's previous investigation of this ticket concluded: "+prior.Reasoning)
					}
				}
			}
		}
		ticketContext := ""
		if w.Session.IssueIdentifier != "" {
			ref := &issuetracker.IssueRef{Provider: "linear", ID: w.Session.IssueIdentifier}
			if details, err := deps.tracker.GetIssueDetails(ctx, ref); err == nil && details != nil {
				var comments []string
				for _, c := range details.Comments {
					comments = append(comments, fmt.Sprintf("Comment (%s): %s", c.Author, c.Body))
				}
				ticketContext = renderTicketContextBlock([]ticketItem{{
					ID:          details.ID,
					Title:       details.Title,
					Description: details.Description,
					Comments:    comments,
				}})
			}
		}

		repo := deps.resolver.Resolve("", nil)
		req := investigation.Request{
			Text:          w.Prompt,
			ThreadContext: threadContext,
			Summary:       w.Session.IssueTitle,
			TicketContext: ticketContext,
			Repo:          repo,
			Timeout:       deps.investigateTimeout,
		}
		return runInvestigation(ctx, deps.investRunner, deps.investigateSem, req)
	}
}
