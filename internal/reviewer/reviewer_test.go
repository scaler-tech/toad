package reviewer

import (
	"strings"
	"testing"
)

func TestExtractResultText(t *testing.T) {
	// Wrapped in --output-format json
	wrapped := `{"result":"{\"actionable\":true,\"summary\":\"fix typo\",\"reason\":\"code change requested\"}"}`
	got := extractResultText([]byte(wrapped))
	if !strings.Contains(got, "actionable") {
		t.Errorf("expected JSON content, got: %s", got)
	}

	// Plain text fallback
	plain := `{"actionable":false,"summary":"","reason":"just an approval"}`
	got = extractResultText([]byte(plain))
	if !strings.Contains(got, "approval") {
		t.Errorf("expected plain JSON, got: %s", got)
	}
}
