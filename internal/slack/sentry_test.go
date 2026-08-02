package slack

import (
	"reflect"
	"testing"
)

func TestExtractSentryRefs_WrappedLinkWithShortIDLabel(t *testing.T) {
	// Realistic Sentry Slack attachment text: pretext + title with a
	// link-wrapped URL whose pipe label is the human-readable short-id.
	text := "New alert from Sentry\n" +
		"<https://acme.sentry.io/issues/5566778899|BILLING-2291> NullPointerException in checkout\n" +
		"1 event in the last hour"

	got := ExtractSentryRefs(text)
	want := []string{"BILLING-2291"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractSentryRefs() = %#v, want %#v", got, want)
	}
}

func TestExtractSentryRefs_WrappedLinkWithNonShortIDLabel(t *testing.T) {
	// Pipe label present but not a Sentry short-id shape — fall back to the
	// id embedded in the URL itself.
	text := "<https://acme.sentry.io/issues/5566778899|View Issue>"

	got := ExtractSentryRefs(text)
	want := []string{"5566778899"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractSentryRefs() = %#v, want %#v", got, want)
	}
}

func TestExtractSentryRefs_PlainOrgSubdomainURL(t *testing.T) {
	text := "Check this out: https://acme.sentry.io/issues/5566778899 thanks"

	got := ExtractSentryRefs(text)
	want := []string{"5566778899"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractSentryRefs() = %#v, want %#v", got, want)
	}
}

func TestExtractSentryRefs_OrganizationsPathURL(t *testing.T) {
	text := "https://sentry.io/organizations/acme/issues/1234567890/"

	got := ExtractSentryRefs(text)
	want := []string{"1234567890"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractSentryRefs() = %#v, want %#v", got, want)
	}
}

func TestExtractSentryRefs_Dedupes(t *testing.T) {
	text := "https://acme.sentry.io/issues/5566778899 and again " +
		"<https://acme.sentry.io/issues/5566778899|BILLING-2291>"

	got := ExtractSentryRefs(text)
	want := []string{"5566778899", "BILLING-2291"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractSentryRefs() = %#v, want %#v (expected both distinct refs, first-seen order, no dupes)", got, want)
	}
}

func TestExtractSentryRefs_PlainTextNoSentryLink(t *testing.T) {
	text := "The build failed on CI, can someone take a look? See logs at https://github.com/acme/repo/actions/runs/123"

	got := ExtractSentryRefs(text)
	if len(got) != 0 {
		t.Errorf("expected no refs for non-Sentry text, got %#v", got)
	}
}

// --- Host-anchoring regression tests ---
// A lookalike host ("notsentry.io") or a URL that merely contains the string
// "sentry.io" somewhere in its path (but on a different authority) must never
// be treated as a real Sentry reference — that would let an attacker-crafted
// URL spoof a Sentry issue ref that later automation (ticket-linking,
// auto-file corroboration) trusts.

func TestExtractSentryRefs_RejectsLookalikeHost(t *testing.T) {
	text := "https://notsentry.io/issues/1234567890"

	got := ExtractSentryRefs(text)
	if len(got) != 0 {
		t.Errorf("expected no refs for lookalike host %q, got %#v", text, got)
	}
}

func TestExtractSentryRefs_RejectsSentryIOInPathNotHost(t *testing.T) {
	text := "https://evil.com/sentry.io/issues/999999"

	got := ExtractSentryRefs(text)
	if len(got) != 0 {
		t.Errorf("expected no refs when sentry.io only appears in the path, got %#v", got)
	}
}

// TestExtractSentryRefs_RejectsWrappedLinkWithoutIssuesPath is the regression
// for the wrapped-label spoof: the <url|LABEL> branch previously trusted the
// label whenever the wrapped URL's HOST was sentry.io, even with no
// /issues/ (or /organizations/.../issues/) path — so a crafted
// "<https://sentry.io|FAKE-1>" would yield "FAKE-1" as if it were a real
// Sentry short-id. The wrapped URL must now match the issues-path pattern
// before the label is trusted at all.
func TestExtractSentryRefs_RejectsWrappedLinkWithoutIssuesPath(t *testing.T) {
	text := "<https://sentry.io|FAKE-1>"

	got := ExtractSentryRefs(text)
	if len(got) != 0 {
		t.Errorf("expected no refs for wrapped link with no issues path, got %#v", got)
	}
}

func TestExtractSentryRefs_SubdomainHostStillMatches(t *testing.T) {
	text := "https://acme.sentry.io/issues/123"

	got := ExtractSentryRefs(text)
	want := []string{"123"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractSentryRefs() = %#v, want %#v", got, want)
	}
}

func TestExtractSentryRefs_BareDomainHostStillMatches(t *testing.T) {
	text := "https://sentry.io/organizations/acme/issues/123"

	got := ExtractSentryRefs(text)
	want := []string{"123"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractSentryRefs() = %#v, want %#v", got, want)
	}
}
