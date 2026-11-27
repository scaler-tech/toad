package slack

import "testing"

func TestToMrkdwn(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bold stars", "a **b** c", "a *b* c"},
		{"bold underscores", "a __b c__ d", "a *b c* d"},
		{"dunder identifier kept", "call __init__.py now", "call __init__.py now"},
		{"already mrkdwn", "*b* _i_ :crown: <https://x.io|x>", "*b* _i_ :crown: <https://x.io|x>"},
		{"link", "see [docs](https://x.io/a) now", "see <https://x.io/a|docs> now"},
		{"heading", "## The cause\nbody", "*The cause*\nbody"},
		{"bullets", "- one\n  * two\n*bold* line", "• one\n  • two\n*bold* line"},
		{"fenced code untouched", "```\n**x**\n- y\nfoo.ts:1\n```\n**z**", "```\n**x**\n- y\nfoo.ts:1\n```\n*z*"},
		{"one-line fence closes itself", "```x```\n**z**", "```x```\n*z*"},
		{"inline code untouched", "`**not bold**` and **bold**", "`**not bold**` and *bold*"},
		{"bare path line", "see DraftMetersTab.vue:14-15 and app/Foo/Bar.php:41.", "see `DraftMetersTab.vue:14-15` and `app/Foo/Bar.php:41`."},
		{"paren path", "store (draftMetersStore.ts:206)", "store (`draftMetersStore.ts:206`)"},
		{"comma list", "foo.ts:206, 1040-1041", "`foo.ts:206`, 1040-1041"},
		{"path in code not doubled", "`foo.ts:12`", "`foo.ts:12`"},
		{"path in url untouched", "https://github.com/o/r/blob/main/foo.ts:12 and <https://x/foo.ts:3|foo.ts:3>", "https://github.com/o/r/blob/main/foo.ts:12 and <https://x/foo.ts:3|foo.ts:3>"},
		{"host port untouched", "listening on example.com:443", "listening on example.com:443"},
		{"bold path", "**foo.ts:9**", "*`foo.ts:9`*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToMrkdwn(tc.in)
			if got != tc.want {
				t.Fatalf("ToMrkdwn(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			if again := ToMrkdwn(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestToMrkdwnRealisticReasoning(t *testing.T) {
	in := "The Utility type filter is the only Discovered Meters filter stuck in the Filters drawer. The data path already works, so this is a frontend-only fix.\n\nThe store already holds activeSubcategoryFilter (draftMetersStore.ts:206) and sends filters[subcategory_id] (draftMetersStore.ts:425). Promoting it to a toolbar pill is a near-copy of `DraftMetersProviderFilter.vue`.\n\n**Open question: should the filter cover provider meters, scaler meters, or both?** The list mixes both kinds.\n\n- one\n- two"
	want := "The Utility type filter is the only Discovered Meters filter stuck in the Filters drawer. The data path already works, so this is a frontend-only fix.\n\nThe store already holds activeSubcategoryFilter (`draftMetersStore.ts:206`) and sends filters[subcategory_id] (`draftMetersStore.ts:425`). Promoting it to a toolbar pill is a near-copy of `DraftMetersProviderFilter.vue`.\n\n*Open question: should the filter cover provider meters, scaler meters, or both?* The list mixes both kinds.\n\n• one\n• two"
	got := ToMrkdwn(in)
	t.Log("\n" + got)
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
}
