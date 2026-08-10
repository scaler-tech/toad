package investigation

import (
	"strings"
	"testing"

	"github.com/scaler-tech/toad/internal/agent"
)

func TestParseFindings_CleanJSON(t *testing.T) {
	input := `{
  "feasible": true,
  "title": "Billing export truncates rows over 10k",
  "problem": "Nightly export in billing/export/aggregate.py silently drops rows once the result set exceeds 10k.",
  "root_cause": "aggregate.py:118 caps the query with a LIMIT 10000 that was added for a debug run and never removed.",
  "evidence": [
    {"kind": "file", "ref": "billing/export/aggregate.py:118", "note": "hardcoded LIMIT 10000"},
    {"kind": "commit", "ref": "a41c9f2", "note": "introduced the debug limit"}
  ],
  "scope": ["Remove the hardcoded LIMIT", "Add a regression test for >10k rows"],
  "non_goals": ["Rewriting the aggregation query"],
  "acceptance_criteria": ["Export includes all rows regardless of count", "Test covers >10k row export"],
  "confidence": 0.82,
  "repo": "billing-service",
  "sentry_issue_ids": ["BILLING-2291"],
  "issue_id": "ENG-4821",
  "reasoning": "Found a hardcoded LIMIT 10000 in aggregate.py that truncates large exports; confirmed via commit a41c9f2."
}`

	f, err := ParseFindings(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !f.Feasible {
		t.Error("expected Feasible=true")
	}
	if f.Title != "Billing export truncates rows over 10k" {
		t.Errorf("unexpected Title: %q", f.Title)
	}
	if f.Problem != "Nightly export in billing/export/aggregate.py silently drops rows once the result set exceeds 10k." {
		t.Errorf("unexpected Problem: %q", f.Problem)
	}
	if f.RootCause != "aggregate.py:118 caps the query with a LIMIT 10000 that was added for a debug run and never removed." {
		t.Errorf("unexpected RootCause: %q", f.RootCause)
	}
	if len(f.Evidence) != 2 {
		t.Fatalf("expected 2 evidence entries, got %d", len(f.Evidence))
	}
	if f.Evidence[0] != (Evidence{Kind: "file", Ref: "billing/export/aggregate.py:118", Note: "hardcoded LIMIT 10000"}) {
		t.Errorf("unexpected Evidence[0]: %+v", f.Evidence[0])
	}
	if f.Evidence[1] != (Evidence{Kind: "commit", Ref: "a41c9f2", Note: "introduced the debug limit"}) {
		t.Errorf("unexpected Evidence[1]: %+v", f.Evidence[1])
	}
	if len(f.Scope) != 2 || f.Scope[0] != "Remove the hardcoded LIMIT" || f.Scope[1] != "Add a regression test for >10k rows" {
		t.Errorf("unexpected Scope: %v", f.Scope)
	}
	if len(f.NonGoals) != 1 || f.NonGoals[0] != "Rewriting the aggregation query" {
		t.Errorf("unexpected NonGoals: %v", f.NonGoals)
	}
	if len(f.AcceptanceCriteria) != 2 {
		t.Errorf("unexpected AcceptanceCriteria: %v", f.AcceptanceCriteria)
	}
	if f.Confidence != 0.82 {
		t.Errorf("expected Confidence=0.82, got %v", f.Confidence)
	}
	if f.Repo != "billing-service" {
		t.Errorf("unexpected Repo: %q", f.Repo)
	}
	if len(f.SentryIssueIDs) != 1 || f.SentryIssueIDs[0] != "BILLING-2291" {
		t.Errorf("unexpected SentryIssueIDs: %v", f.SentryIssueIDs)
	}
	if f.IssueID != "ENG-4821" {
		t.Errorf("unexpected IssueID: %q", f.IssueID)
	}
	if len(f.FilesFound) != 1 || f.FilesFound[0] != "billing/export/aggregate.py" {
		t.Errorf("expected FilesFound=[billing/export/aggregate.py], got %v", f.FilesFound)
	}
	if !strings.Contains(f.Reasoning, "hardcoded LIMIT 10000") {
		t.Errorf("unexpected Reasoning: %q", f.Reasoning)
	}
}

func TestParseFindings_CodeFencedJSON(t *testing.T) {
	fence := "```json"
	jsonBody := `{
  "feasible": false,
  "title": "Search relevance request too vague",
  "problem": "User asked to make search better with no specifics.",
  "root_cause": "No reproducible bug; request lacks acceptance criteria.",
  "evidence": [],
  "scope": [],
  "non_goals": [],
  "acceptance_criteria": [],
  "repo": "web-app",
  "sentry_issue_ids": [],
  "issue_id": "",
  "reasoning": "Not enough detail to scope a ticket; asked for clarification in thread."
}`
	input := "Here's my investigation summary:\n\n" + fence + "\n" + jsonBody + "\n```\n\nLet me know if you need more detail."

	f, err := ParseFindings(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if f.Feasible {
		t.Error("expected Feasible=false")
	}
	if f.Title != "Search relevance request too vague" {
		t.Errorf("unexpected Title: %q", f.Title)
	}
	if f.Repo != "web-app" {
		t.Errorf("unexpected Repo: %q", f.Repo)
	}
	// confidence was omitted from the JSON entirely — must default to zero value.
	if f.Confidence != 0 {
		t.Errorf("expected Confidence=0 when field is missing, got %v", f.Confidence)
	}
	if len(f.FilesFound) != 0 {
		t.Errorf("expected no files found, got %v", f.FilesFound)
	}
}

func TestParseFindings_EmbeddedAfterProse(t *testing.T) {
	input := `Investigated the reporting pipeline and confirmed the crash. Verdict: {"feasible":true,"title":"Nil pointer in report handler","problem":"internal/reports/handler.go panics when the query returns zero rows","root_cause":"missing nil check on the aggregate result before dereferencing","evidence":[],"scope":["Add nil guard before dereferencing the aggregate result"],"non_goals":[],"acceptance_criteria":["Handler returns an empty report instead of panicking"],"confidence":0.91,"repo":"reports-service","sentry_issue_ids":["RPT-118"],"issue_id":"","reasoning":"Traced the panic to internal/reports/handler.go missing a nil check."}`

	f, err := ParseFindings(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !f.Feasible {
		t.Error("expected Feasible=true")
	}
	if f.Confidence != 0.91 {
		t.Errorf("expected Confidence=0.91, got %v", f.Confidence)
	}
	if len(f.SentryIssueIDs) != 1 || f.SentryIssueIDs[0] != "RPT-118" {
		t.Errorf("unexpected SentryIssueIDs: %v", f.SentryIssueIDs)
	}
	found := false
	for _, p := range f.FilesFound {
		if p == "internal/reports/handler.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected FilesFound to contain internal/reports/handler.go, got %v", f.FilesFound)
	}
}

// TestParseFindings_Strategy3BackscanOnly constructs input that only parses
// via ParseFindings' third strategy (backscan from the last `"feasible"`
// occurrence): prose appears both before and after the real JSON object, and
// an earlier decoy `{` (with no matching closing brace of its own) sits in
// the leading prose.
//
//   - Strategy 1 fails because the literal substring `{"feasible"` never
//     appears — the real JSON is pretty-printed with a newline/indentation
//     between its opening brace and the "feasible" key.
//   - Strategy 2 fails because it takes the FIRST `{` in the text (the
//     decoy) and brace-matches from there: since the decoy never closes,
//     depth only returns to 1 (not 0) at the real JSON's own closing brace,
//     so findMatchingBrace never finds a match and strategy 2 gives up.
//   - Strategy 3 succeeds because it starts its backscan from the LAST
//     `"feasible"` occurrence and finds the real JSON's own opening brace —
//     the nearest `{` behind it — independent of the earlier decoy.
func TestParseFindings_Strategy3BackscanOnly(t *testing.T) {
	input := `Investigated the pipeline. Configuration snapshot looked odd: { "note": true
Actually, here's the real verdict after some digging.
{
  "feasible": true,
  "title": "Nil pointer in refund handler",
  "problem": "internal/billing/refund.go panics on empty batch",
  "root_cause": "missing len check before indexing batch[0]",
  "evidence": [],
  "scope": ["Add empty-batch guard"],
  "non_goals": [],
  "acceptance_criteria": ["Handler returns early on empty batch"],
  "confidence": 0.87,
  "repo": "billing-service",
  "sentry_issue_ids": ["BILL-77"],
  "issue_id": "",
  "reasoning": "Traced the panic to internal/billing/refund.go missing a length check."
}
Let me know if you'd like the fix applied.`

	// Sanity-check the premises this test depends on, so a future edit to
	// ParseFindings' strategy order (or to this fixture) fails loudly here
	// instead of silently exercising a different strategy than intended.
	if strings.Contains(input, `{"feasible"`) {
		t.Fatal("test fixture invalid: literal `{\"feasible\"` present, would satisfy strategy 1")
	}
	if strings.Contains(input, "```") {
		t.Fatal("test fixture invalid: contains a code fence, changes strategy 2's behavior")
	}

	f, err := ParseFindings(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !f.Feasible {
		t.Error("expected Feasible=true")
	}
	if f.Title != "Nil pointer in refund handler" {
		t.Errorf("unexpected Title: %q", f.Title)
	}
	if f.Confidence != 0.87 {
		t.Errorf("expected Confidence=0.87, got %v", f.Confidence)
	}
	if f.Repo != "billing-service" {
		t.Errorf("unexpected Repo: %q", f.Repo)
	}
	if len(f.SentryIssueIDs) != 1 || f.SentryIssueIDs[0] != "BILL-77" {
		t.Errorf("unexpected SentryIssueIDs: %v", f.SentryIssueIDs)
	}
	found := false
	for _, p := range f.FilesFound {
		if p == "internal/billing/refund.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected FilesFound to contain internal/billing/refund.go, got %v", f.FilesFound)
	}
}

func TestParseFindings_MalformedInput(t *testing.T) {
	input := "The agent crashed before producing a verdict. No JSON here at all."

	f, err := ParseFindings(input)
	if err == nil {
		t.Fatal("expected an error for malformed input, got nil")
	}
	if !strings.Contains(err.Error(), "no valid JSON") {
		t.Errorf("expected error to contain %q, got %q", "no valid JSON", err.Error())
	}
	if f != nil {
		t.Errorf("expected nil Findings on error, got %+v", f)
	}
}

func TestExtractFilePaths(t *testing.T) {
	text := "The bug is in internal/reports/handler.go and also touches billing/export/aggregate.py:118. " +
		"See https://example.com/foo.go for context, and note Handler.php isn't a path."

	paths := ExtractFilePaths(text)

	want := map[string]bool{
		"internal/reports/handler.go": true,
	}
	got := map[string]bool{}
	for _, p := range paths {
		got[p] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("expected ExtractFilePaths to include %q, got %v", w, paths)
		}
	}
	if got["billing/export/aggregate.py:118"] {
		t.Errorf("did not expect a path with a trailing :line suffix to match, got %v", paths)
	}
	if len(got) != len(want) {
		t.Errorf("expected exactly %d matched paths, got %v", len(want), paths)
	}
}

func TestParseFindings_LinearDestinationFields(t *testing.T) {
	raw := `{"feasible": true, "title": "t", "problem": "p", "root_cause": "r", "evidence": [], "scope": [], "non_goals": [], "acceptance_criteria": [], "confidence": 0.5, "repo": "biome", "sentry_issue_ids": [], "issue_id": "", "linear_team": "ANA", "linear_project": "Biome", "files_found": [], "reasoning": "x"}`
	f, err := ParseFindings(raw)
	if err != nil {
		t.Fatalf("ParseFindings() error = %v", err)
	}
	if f.LinearTeam != "ANA" {
		t.Errorf("LinearTeam = %q, want ANA", f.LinearTeam)
	}
	if f.LinearProject != "Biome" {
		t.Errorf("LinearProject = %q, want Biome", f.LinearProject)
	}
}

func TestPrompt_InstructsLinearDestinationExtraction(t *testing.T) {
	p := buildPrompt(Request{Text: "create a ticket in the Biome project"})
	for _, want := range []string{`"linear_team"`, `"linear_project"`, "explicitly names a Linear team or project"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestPrompt_IncludesProseStyleRules(t *testing.T) {
	p := buildPrompt(Request{Text: "the export job double-counts refunds"})
	if !strings.Contains(p, agent.ProseStyleRules) {
		t.Error("prompt missing the shared prose style rules")
	}
}

func TestParseFindings_LinearAssigneesField(t *testing.T) {
	raw := `{"feasible": true, "title": "t", "problem": "p", "root_cause": "r", "evidence": [], "scope": [], "non_goals": [], "acceptance_criteria": [], "confidence": 0.5, "repo": "biome", "sentry_issue_ids": [], "issue_id": "", "linear_team": "", "linear_project": "", "linear_assignees": ["requester", "biome"], "files_found": [], "reasoning": "x"}`
	f, err := ParseFindings(raw)
	if err != nil {
		t.Fatalf("ParseFindings() error = %v", err)
	}
	want := []string{"requester", "biome"}
	if len(f.LinearAssignees) != len(want) {
		t.Fatalf("LinearAssignees = %v, want %v", f.LinearAssignees, want)
	}
	for i, w := range want {
		if f.LinearAssignees[i] != w {
			t.Errorf("LinearAssignees[%d] = %q, want %q", i, f.LinearAssignees[i], w)
		}
	}
}

func TestParseFindings_LinearAssigneesEmptyByDefault(t *testing.T) {
	raw := `{"feasible": true, "title": "t", "problem": "p", "root_cause": "r", "evidence": [], "scope": [], "non_goals": [], "acceptance_criteria": [], "confidence": 0.5, "repo": "biome", "sentry_issue_ids": [], "issue_id": "", "linear_team": "", "linear_project": "", "files_found": [], "reasoning": "x"}`
	f, err := ParseFindings(raw)
	if err != nil {
		t.Fatalf("ParseFindings() error = %v", err)
	}
	if len(f.LinearAssignees) != 0 {
		t.Errorf("LinearAssignees = %v, want empty", f.LinearAssignees)
	}
}

func TestPrompt_InstructsLinearAssigneeExtraction(t *testing.T) {
	p := buildPrompt(Request{Text: "assign this to me and biome"})
	for _, want := range []string{`"linear_assignees"`, `"requester"`, "explicitly asks to assign or hand off the ticket"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestPrompt_ReasoningInstruction(t *testing.T) {
	p := buildPrompt(Request{Text: "investigate this"})
	for _, want := range []string{
		"Lead with the conclusion",
		"Describe the code, not your search",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing reasoning instruction %q", want)
		}
	}
	for _, gone := range []string{"*Verdict.*", "Explanation format", "*Not checked.*"} {
		if strings.Contains(p, gone) {
			t.Errorf("prompt still contains dropped template element %q", gone)
		}
	}
}
