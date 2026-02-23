package slack

import (
	"strings"
	"testing"

	goslack "github.com/slack-go/slack"
)

func TestAppendUnique_NewText(t *testing.T) {
	parts := []string{"hello"}
	result := appendUnique(parts, "world")
	if len(result) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(result))
	}
	if result[1] != "world" {
		t.Errorf("expected 'world', got %q", result[1])
	}
}

func TestAppendUnique_ExactDuplicate(t *testing.T) {
	parts := []string{"hello", "world"}
	result := appendUnique(parts, "hello")
	if len(result) != 2 {
		t.Errorf("expected 2 parts (no dup), got %d", len(result))
	}
}

func TestAppendUnique_SubstringMatch(t *testing.T) {
	parts := []string{"hello world"}
	result := appendUnique(parts, "hello")
	if len(result) != 1 {
		t.Errorf("expected 1 part (substring match), got %d", len(result))
	}
}

func TestAppendUnique_EmptySlice(t *testing.T) {
	result := appendUnique(nil, "hello")
	if len(result) != 1 {
		t.Fatalf("expected 1 part, got %d", len(result))
	}
}

func TestAppendUnique_NotSubstring(t *testing.T) {
	parts := []string{"hello"}
	result := appendUnique(parts, "hello world")
	if len(result) != 2 {
		t.Errorf("expected 2 parts (not a substring of existing), got %d", len(result))
	}
}

func TestExtractFullText_PlainText(t *testing.T) {
	msg := goslack.Msg{Text: "simple message"}
	result := extractFullText(msg)
	if result != "simple message" {
		t.Errorf("expected 'simple message', got %q", result)
	}
}

func TestExtractFullText_Empty(t *testing.T) {
	msg := goslack.Msg{}
	result := extractFullText(msg)
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}
}

func TestExtractFullText_WithAttachments(t *testing.T) {
	msg := goslack.Msg{
		Text: "alert",
		Attachments: []goslack.Attachment{
			{
				Title: "ErrorException",
				Text:  "Undefined array key 2026",
			},
		},
	}
	result := extractFullText(msg)
	if !strings.Contains(result, "alert") {
		t.Error("missing plain text")
	}
	if !strings.Contains(result, "ErrorException") {
		t.Error("missing attachment title")
	}
	if !strings.Contains(result, "Undefined array key 2026") {
		t.Error("missing attachment text")
	}
}

func TestExtractFullText_AttachmentFields(t *testing.T) {
	msg := goslack.Msg{
		Attachments: []goslack.Attachment{
			{
				Fields: []goslack.AttachmentField{
					{Title: "environment", Value: "production"},
					{Title: "server", Value: "web-01"},
				},
			},
		},
	}
	result := extractFullText(msg)
	if !strings.Contains(result, "environment: production") {
		t.Error("missing field 'environment: production'")
	}
	if !strings.Contains(result, "server: web-01") {
		t.Error("missing field 'server: web-01'")
	}
}

func TestExtractFullText_AttachmentPretext(t *testing.T) {
	msg := goslack.Msg{
		Attachments: []goslack.Attachment{
			{Pretext: "New alert from Sentry", Text: "details here"},
		},
	}
	result := extractFullText(msg)
	if !strings.Contains(result, "New alert from Sentry") {
		t.Error("missing pretext")
	}
	if !strings.Contains(result, "details here") {
		t.Error("missing text")
	}
}

func TestExtractFullText_DeduplicatesTextAndAttachment(t *testing.T) {
	// When attachment text is the same as the main text, should not duplicate
	msg := goslack.Msg{
		Text: "the error message",
		Attachments: []goslack.Attachment{
			{Text: "the error message"},
		},
	}
	result := extractFullText(msg)
	count := strings.Count(result, "the error message")
	if count != 1 {
		t.Errorf("expected text once (deduped), appeared %d times", count)
	}
}

func TestExtractFullText_FallbackOnly(t *testing.T) {
	msg := goslack.Msg{
		Attachments: []goslack.Attachment{
			{Fallback: "fallback text"},
		},
	}
	result := extractFullText(msg)
	if !strings.Contains(result, "fallback text") {
		t.Error("missing fallback text when no other content")
	}
}

func TestExtractFullText_FallbackIgnoredWhenContentExists(t *testing.T) {
	msg := goslack.Msg{
		Text: "main text",
		Attachments: []goslack.Attachment{
			{Fallback: "fallback text"},
		},
	}
	result := extractFullText(msg)
	if strings.Contains(result, "fallback text") {
		t.Error("fallback should be ignored when other content exists")
	}
}

func TestHasKeywordTrigger_Match(t *testing.T) {
	if !hasKeywordTrigger("hey toad fix this", []string{"toad fix"}) {
		t.Error("expected match")
	}
}

func TestHasKeywordTrigger_NoMatch(t *testing.T) {
	if hasKeywordTrigger("hello world", []string{"toad fix", "toad help"}) {
		t.Error("expected no match")
	}
}

func TestHasKeywordTrigger_CaseInsensitive(t *testing.T) {
	if !hasKeywordTrigger("TOAD FIX please", []string{"toad fix"}) {
		t.Error("expected case-insensitive match")
	}
}

func TestHasKeywordTrigger_EmptyKeywords(t *testing.T) {
	if hasKeywordTrigger("toad fix", nil) {
		t.Error("expected no match with empty keywords")
	}
}

func TestHasKeywordTrigger_EmptyText(t *testing.T) {
	if hasKeywordTrigger("", []string{"toad fix"}) {
		t.Error("expected no match with empty text")
	}
}

func TestHasKeywordTrigger_MultipleKeywords(t *testing.T) {
	if !hasKeywordTrigger("can you toad help me?", []string{"toad fix", "toad help"}) {
		t.Error("expected match on second keyword")
	}
}

func TestHasKeywordTrigger_PartialNoMatch(t *testing.T) {
	if hasKeywordTrigger("toad", []string{"toad fix"}) {
		t.Error("partial keyword should not match")
	}
}
