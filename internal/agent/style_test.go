package agent

import (
	"fmt"
	"strings"
	"testing"
)

// TestProseStyleRules_NoStrayPercent guards the sites that fold these
// constants into fmt.Sprintf format strings (rather than injecting them as a
// %s argument): every '%' must be part of an escaped '%%' pair, or a
// one-off literal % here would silently corrupt those templates (extra
// "%!x(MISSING)" noise or a shifted verb).
func TestProseStyleRules_NoStrayPercent(t *testing.T) {
	for name, s := range map[string]string{
		"ProseStyleRules":     ProseStyleRules,
		"ProseStyleRulesSlim": ProseStyleRulesSlim,
	} {
		for i := 0; i < len(s); i++ {
			if s[i] != '%' {
				continue
			}
			if i+1 >= len(s) || s[i+1] != '%' {
				t.Fatalf("%s: unescaped %% at byte %d (context: %q)", name, i, s[max(0, i-10):min(len(s), i+10)])
			}
			i++ // skip the escaped pair
		}
	}
}

// TestProseStyleRules_SprintfSafe belt-and-suspenders: injecting either
// constant as a %s argument into a surrounding format string (the mechanism
// every current call site uses) must carry the content through unchanged,
// regardless of what it contains.
func TestProseStyleRules_SprintfSafe(t *testing.T) {
	for name, s := range map[string]string{
		"ProseStyleRules":     ProseStyleRules,
		"ProseStyleRulesSlim": ProseStyleRulesSlim,
	} {
		got := fmt.Sprintf("Writing style:\n%s\n", s)
		want := "Writing style:\n" + s + "\n"
		if got != want {
			t.Errorf("%s: fmt.Sprintf injection mismatch", name)
		}
		if strings.Contains(got, "%!") {
			t.Errorf("%s: Sprintf output contains a formatting-error marker: %q", name, got)
		}
	}
}
