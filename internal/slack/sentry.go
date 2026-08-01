package slack

import (
	"regexp"
)

// sentryRefRe matches either Slack's link-wrapped mrkdwn form —
// <https://acme.sentry.io/issues/5566778899|BILLING-2291> — capturing the URL
// and pipe label in groups 1 and 2, or a plain (unwrapped) Sentry URL in
// group 3. Matching both shapes in one pass keeps results in first-seen
// (left-to-right) order and avoids re-matching a wrapped URL's inner text as
// a second, plain match.
var sentryRefRe = regexp.MustCompile(
	`<(https?://[^|<>]*sentry\.io[^|<>]*)\|([^<>]+)>|(https?://[^\s<>]*sentry\.io[^\s<>]*)`,
)

// sentryIssueIDRe matches a Sentry issue URL (org subdomain or
// sentry.io/organizations/<org>/ form) and captures the trailing issue id.
var sentryIssueIDRe = regexp.MustCompile(`sentry\.io/(?:organizations/[^/\s]+/)?issues/([A-Za-z0-9]+)`)

// sentryShortIDRe matches a Sentry short-id label like "BILLING-2291".
var sentryShortIDRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9A-Z]+$`)

// ExtractSentryRefs scans text for Sentry issue references and returns the
// trailing issue identifier for each one found — either the numeric/URL id
// (e.g. "5566778899") or, for Slack-link-wrapped references with a pipe label
// that looks like a Sentry short-id (e.g. "BILLING-2291"), the label itself.
// Results are deduped, preserving first-seen order.
func ExtractSentryRefs(fullText string) []string {
	var refs []string
	seen := make(map[string]bool)
	add := func(ref string) {
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		refs = append(refs, ref)
	}

	for _, m := range sentryRefRe.FindAllStringSubmatch(fullText, -1) {
		if m[1] != "" {
			// Slack-wrapped link: prefer the label if it looks like a Sentry
			// short-id, else fall back to the id embedded in the URL.
			url, label := m[1], m[2]
			if sentryShortIDRe.MatchString(label) {
				add(label)
			} else if idm := sentryIssueIDRe.FindStringSubmatch(url); idm != nil {
				add(idm[1])
			}
			continue
		}
		if idm := sentryIssueIDRe.FindStringSubmatch(m[3]); idm != nil {
			add(idm[1])
		}
	}

	return refs
}
