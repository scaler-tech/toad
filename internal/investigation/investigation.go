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
	IssueID            string     `json:"issue_id"`    // existing Linear ref, if any
	FilesFound         []string   `json:"files_found"` // from extractFilePaths
	Reasoning          string     `json:"reasoning"`   // Slack-postable prose
}
