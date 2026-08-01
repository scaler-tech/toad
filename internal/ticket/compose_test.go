package ticket

import (
	"strings"
	"testing"

	"github.com/scaler-tech/toad/internal/investigation"
)

func TestComposeBody_Golden(t *testing.T) {
	f := investigation.Findings{
		Feasible:  true,
		Title:     "Export aggregation crashes on empty account",
		Problem:   "Billing export fails for accounts with zero line items.",
		RootCause: "The aggregator divides by the line-item count without a zero guard.",
		Evidence: []investigation.Evidence{
			{Kind: "file", Ref: "billing/export/aggregate.py:118", Note: "division with no zero-check"},
			{Kind: "commit", Ref: "a41c9f2", Note: "introduced the aggregation path"},
		},
		Scope:              []string{"Guard the aggregator against zero line items", "Add a regression test"},
		AcceptanceCriteria: []string{"Empty-account export no longer 500s", "Regression test covers the zero case"},
		Confidence:         0.92,
		SentryIssueIDs:     []string{"BILLING-2291"},
		Repo:               "billing-api",
	}

	got := ComposeBody(f, "https://slack.example.com/archives/C123/p172250000", "inv-42")

	want := "**Repo:** billing-api\n\n" +
		"## Problem\n\n" +
		"Billing export fails for accounts with zero line items.\n\n" +
		"## Root cause (hypothesis)\n\n" +
		"The aggregator divides by the line-item count without a zero guard.\n\n" +
		"- `billing/export/aggregate.py:118` — division with no zero-check\n" +
		"- `commit:a41c9f2` — introduced the aggregation path\n\n" +
		"## Scope\n\n" +
		"- Guard the aggregator against zero line items\n" +
		"- Add a regression test\n\n" +
		"## Acceptance criteria\n\n" +
		"- [ ] Empty-account export no longer 500s\n" +
		"- [ ] Regression test covers the zero case\n\n" +
		"---\n" +
		"https://slack.example.com/archives/C123/p172250000\n" +
		"sentry:BILLING-2291\n" +
		"toad:investigation inv-42"

	if got != want {
		t.Errorf("ComposeBody() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestComposeBody_OmitsEmptySections(t *testing.T) {
	f := investigation.Findings{
		Problem:   "Something broke.",
		RootCause: "Unknown yet.",
	}

	got := ComposeBody(f, "", "inv-1")

	want := "## Problem\n\n" +
		"Something broke.\n\n" +
		"## Root cause (hypothesis)\n\n" +
		"Unknown yet.\n\n" +
		"---\n" +
		"toad:investigation inv-1"

	if got != want {
		t.Errorf("ComposeBody() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	for _, unwanted := range []string{"## Scope", "## Non-goals", "## Acceptance criteria", "**Repo:**"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("expected %q section to be omitted, got:\n%s", unwanted, got)
		}
	}
}
