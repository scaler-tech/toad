package tadpole

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"fix nil pointer in handler", "fix-nil-pointer-in-handler"},
		{"Fix The BUG!!!", "fix-the-bug"},
		{"simple", "simple"},
		{"  spaces  everywhere  ", "spaces-everywhere"},
		{"UPPERCASE", "uppercase"},
		{"with-hyphens-already", "with-hyphens-already"},
		{"special@chars#here$123", "special-chars-here-123"},
		{"", "tadpole"},
		{"---", "tadpole"},
		{"a", "a"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := Slugify(tt.input)
			if got != tt.want {
				t.Errorf("Slugify(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSlugify_Truncation(t *testing.T) {
	long := strings.Repeat("a", 100)
	got := Slugify(long)
	if len(got) > 40 {
		t.Errorf("slug should be max 40 chars, got %d", len(got))
	}
}

func TestSlugify_TruncationNoTrailingHyphen(t *testing.T) {
	// Create input where truncation at 40 would leave a trailing hyphen
	// 39 chars of 'a' + '-' + more text = truncated at 40 should strip trailing '-'
	input := strings.Repeat("a", 39) + " bbb"
	got := Slugify(input)
	if strings.HasSuffix(got, "-") {
		t.Errorf("slug should not end with hyphen, got %q", got)
	}
	if len(got) > 40 {
		t.Errorf("slug should be max 40 chars, got %d", len(got))
	}
}

func TestSlugify_NoLeadingHyphen(t *testing.T) {
	got := Slugify("---leading")
	if strings.HasPrefix(got, "-") {
		t.Errorf("slug should not start with hyphen, got %q", got)
	}
}
