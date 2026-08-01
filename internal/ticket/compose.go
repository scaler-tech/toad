package ticket

import (
	"strings"

	"github.com/scaler-tech/toad/internal/investigation"
)

// ComposeBody renders the Linear issue description for an investigation's
// Findings: the problem statement, a root-cause hypothesis backed by
// evidence, scope, non-goals, acceptance criteria, and a footer that links
// the ticket back to its Slack thread, any corroborating Sentry issues, and
// the investigation record that produced it.
//
// Sections with no content (e.g. no non-goals) are omitted entirely rather
// than rendered with an empty body under a bare header.
func ComposeBody(f investigation.Findings, slackPermalink, investigationID string) string {
	var b strings.Builder

	writeTextSection(&b, "Problem", f.Problem)
	writeTextSection(&b, "Root cause (hypothesis)", composeRootCause(f))
	writeListSection(&b, "Scope", f.Scope, "- ")
	writeListSection(&b, "Non-goals", f.NonGoals, "- ")
	writeListSection(&b, "Acceptance criteria", f.AcceptanceCriteria, "- [ ] ")

	if footer := composeFooter(f, slackPermalink, investigationID); footer != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("---\n")
		b.WriteString(footer)
	}

	return strings.TrimRight(b.String(), "\n")
}

// composeRootCause combines the free-text hypothesis with its supporting
// evidence, rendered as one bullet per item.
func composeRootCause(f investigation.Findings) string {
	var b strings.Builder
	if strings.TrimSpace(f.RootCause) != "" {
		b.WriteString(f.RootCause)
	}
	if len(f.Evidence) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		for i, e := range f.Evidence {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(evidenceBullet(e))
		}
	}
	return b.String()
}

// evidenceBullet renders a single Evidence entry as a Markdown bullet,
// prefixing the ref with its kind when the kind isn't already obvious from
// the ref's own shape (a "file" ref like "path/to.go:118" self-describes;
// a bare commit hash or Sentry ID does not).
func evidenceBullet(e investigation.Evidence) string {
	ref := e.Ref
	if e.Kind != "" && e.Kind != "file" {
		ref = e.Kind + ":" + ref
	}
	if e.Note == "" {
		return "- `" + ref + "`"
	}
	return "- `" + ref + "` — " + e.Note
}

// composeFooter builds the ticket's provenance footer: the Slack permalink
// (when available), one "sentry:<id>" line per corroborating Sentry issue,
// and the "toad:investigation <id>" backlink. Blank/whitespace-only Sentry
// IDs are skipped (see nonBlankSentryIDs) so they never render as a bare,
// meaningless "sentry:" line.
func composeFooter(f investigation.Findings, slackPermalink, investigationID string) string {
	var lines []string
	if slackPermalink != "" {
		lines = append(lines, slackPermalink)
	}
	for _, id := range nonBlankSentryIDs(f) {
		lines = append(lines, "sentry:"+id)
	}
	if investigationID != "" {
		lines = append(lines, "toad:investigation "+investigationID)
	}
	return strings.Join(lines, "\n")
}

func writeTextSection(b *strings.Builder, header, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString("## " + header + "\n\n")
	b.WriteString(body)
	b.WriteString("\n")
}

func writeListSection(b *strings.Builder, header string, items []string, bulletPrefix string) {
	if len(items) == 0 {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString("## " + header + "\n\n")
	for _, item := range items {
		b.WriteString(bulletPrefix + item + "\n")
	}
}
