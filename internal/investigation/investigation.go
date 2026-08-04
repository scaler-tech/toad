// Package investigation holds the shared output contract for toad's
// investigation agent: the Findings verdict produced by an investigation run
// and the parser that extracts it from raw agent output.
package investigation

// Evidence is a single piece of corroboration for a Findings verdict.
type Evidence struct {
	Kind string `json:"kind"` // "file" | "commit" | "sentry" | "thread"
	Ref  string `json:"ref"`  // "billing/export/aggregate.py:118", "a41c9f2", "BILLING-2291"
	Note string `json:"note"`
}

// Findings is the verdict an investigation agent run produces: whether the
// problem is feasible to ticket, why, and what backs that judgment.
type Findings struct {
	Feasible           bool       `json:"feasible"`
	Title              string     `json:"title"`
	Problem            string     `json:"problem"`
	RootCause          string     `json:"root_cause"` // hypothesis-phrased
	Evidence           []Evidence `json:"evidence"`
	Scope              []string   `json:"scope"`
	NonGoals           []string   `json:"non_goals"`
	AcceptanceCriteria []string   `json:"acceptance_criteria"`
	Confidence         float64    `json:"confidence"`
	Repo               string     `json:"repo"`
	SentryIssueIDs     []string   `json:"sentry_issue_ids"`
	IssueID            string     `json:"issue_id"` // existing Linear ref, if any

	// LinearTeam and LinearProject carry a filing destination the reporter
	// EXPLICITLY named in the request ("file this in the ANA team", "create
	// a ticket in the Biome project"); both are empty otherwise. They are
	// model output and therefore untrusted — but they can only ever narrow
	// WHERE a ticket lands: the Linear client resolves them against the
	// workspace's existing teams/projects and falls back to the configured
	// defaults when resolution fails. They never influence WHETHER a ticket
	// is filed (the corroboration/CTA gates run on other fields entirely).
	LinearTeam    string `json:"linear_team"`
	LinearProject string `json:"linear_project"`

	// LinearAssignees names who the reporter EXPLICITLY asked to assign or
	// delegate the ticket to, copied verbatim from their request (e.g.
	// "dejan", "biome"). The literal token "requester" is used when the
	// reporter says "me"/"myself" — i.e. assign it to whoever made the
	// request, not a name the model has to guess. Empty otherwise. Like
	// LinearTeam/LinearProject, this is model output and therefore
	// untrusted — but it can only ever narrow WHO the ticket is assigned to
	// (or which delegate label gets applied): cmd-level code resolves it
	// against the Linear workspace's existing users and the configured
	// issue_tracker.delegates map, with warn-and-skip on any resolution
	// failure. It never influences WHETHER a ticket is filed.
	LinearAssignees []string `json:"linear_assignees"`

	FilesFound []string `json:"files_found"` // from extractFilePaths
	Reasoning  string   `json:"reasoning"`   // Slack-postable prose

	// RepoSyncFailed is set by Runner.Run itself when the pre-investigation
	// repo sync failed, so the investigation proceeded against a possibly-
	// stale checkout. It is never set from the model's JSON output (json:"-"
	// keeps ParseFindings from ever trusting agent-supplied text for this
	// field) — callers (cmd/ticketflow.go's runInvestigation) use it to
	// append a user-visible staleness caveat.
	RepoSyncFailed bool `json:"-"`

	// LinearAssignee and LinearExtraLabels are RESOLVED-input fields: unlike
	// LinearAssignees above (raw, untrusted model output), these are set by
	// cmd-level code (cmd/ticketflow.go's applyLinearAssigneeMapping)
	// immediately before a ticket is filed or re-filed — never by the model,
	// hence json:"-". They're re-derived fresh on every filing attempt
	// (never trusted from a persisted/reused investigation record) because
	// the requester making "assign to me" mean something different between
	// the original report and a later CTA click by someone else — the same
	// re-derive-every-time discipline enforceCorroboration uses for
	// SentryIssueIDs. ticket.Engine.file copies these straight into
	// issuetracker.CreateIssueOpts.Assignee/ExtraLabels.
	LinearAssignee    string   `json:"-"`
	LinearExtraLabels []string `json:"-"`
}
