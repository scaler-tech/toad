package slack

import (
	"regexp"
	"strings"
)

// protectedRe matches the spans ToMrkdwn must not rewrite as plain text:
// inline code, Slack <...> tokens, Markdown links (converted, group 3/4) and
// bare URLs.
var protectedRe = regexp.MustCompile("(`[^`]*`)|(<[^<>]*>)|\\[([^\\]]+)\\]\\((https?://[^)\\s]+)\\)|(https?://[^\\s<>`]+)")

var (
	headingRe  = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*$`)
	bulletRe   = regexp.MustCompile(`^(\s*)[-*]\s+`)
	boldStarRe = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	// Needs a space inside so identifiers like __init__ survive.
	boldUndRe = regexp.MustCompile(`(^|[^\w])__([^_\s][^_\n]*?\s[^_\n]*?)__([^\w]|$)`)
	// An allowlist of extensions keeps "example.com:443" from being read as a
	// source reference.
	pathLineRe = regexp.MustCompile(`(^|[\s(\[,;*])((?:[\w.\-]+/)*[\w\-]+\.(?:go|php|vue|tsx?|jsx?|mjs|cjs|py|rb|rs|java|kt|swift|sql|md|json|ya?ml|toml|css|scss|html|sh|c|h|cpp|cs)):(\d+(?:-\d+)?)\b`)
)

// ToMrkdwn converts the Markdown a model writes into Slack mrkdwn. It is
// idempotent, so text that is already mrkdwn passes through unchanged.
func ToMrkdwn(s string) string {
	lines := strings.Split(s, "\n")
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			// A one-line ```code``` opens and closes on the same line.
			if strings.Count(line, "```")%2 == 1 {
				inFence = !inFence
			}
			continue
		}
		if inFence {
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			text := strings.ReplaceAll(convertInline(m[1]), "*", "")
			lines[i] = "*" + text + "*"
			continue
		}
		prefix := ""
		if m := bulletRe.FindStringSubmatch(line); m != nil {
			prefix = m[1] + "• "
			line = line[len(m[0]):]
		}
		lines[i] = prefix + convertInline(line)
	}
	return strings.Join(lines, "\n")
}

func convertInline(line string) string {
	var b strings.Builder
	last := 0
	for _, m := range protectedRe.FindAllStringSubmatchIndex(line, -1) {
		b.WriteString(convertPlain(line[last:m[0]]))
		if m[6] >= 0 { // [text](url)
			b.WriteString("<" + line[m[8]:m[9]] + "|" + line[m[6]:m[7]] + ">")
		} else {
			b.WriteString(line[m[0]:m[1]])
		}
		last = m[1]
	}
	b.WriteString(convertPlain(line[last:]))
	return b.String()
}

func convertPlain(s string) string {
	s = boldStarRe.ReplaceAllString(s, "*$1*")
	s = boldUndRe.ReplaceAllString(s, "$1*$2*$3")
	return pathLineRe.ReplaceAllString(s, "$1`$2:$3`")
}
