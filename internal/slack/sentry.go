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
//
// The host is anchored: (?:[\w-]+\.)?sentry\.io requires the URL authority to
// BE sentry.io or a single-level subdomain of it, immediately after the
// scheme. This rejects lookalike hosts like "notsentry.io" (no dot boundary
// before "sentry") and URLs where "sentry.io" only appears in the path, e.g.
// "https://evil.com/sentry.io/issues/999999" (the authority there is
// evil.com, not sentry.io).
var sentryRefRe = regexp.MustCompile(
	`<(https?://(?:[\w-]+\.)?sentry\.io(?:/[^|<>]*)?)\|([^<>]+)>|(https?://(?:[\w-]+\.)?sentry\.io(?:/[^\s<>]*)?)`,
)

// sentryIssueIDRe matches a full Sentry issue URL (org subdomain or
// sentry.io/organizations/<org>/ form), host-anchored the same way as
// sentryRefRe, and captures the trailing issue id. It's applied to URL
// strings already isolated by sentryRefRe, but keeps its own host anchor so
// it's safe to use standalone too.
var sentryIssueIDRe = regexp.MustCompile(
	`^https?://(?:[\w-]+\.)?sentry\.io/(?:organizations/[^/\s]+/)?issues/([A-Za-z0-9]+)`,
)

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
