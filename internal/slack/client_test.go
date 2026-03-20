package slack

import (
	"strings"
	"testing"
	"time"

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

func TestBuildPathScrubber_ReplacesPath(t *testing.T) {
	scrub := buildPathScrubber(map[string]string{
		"/Users/hergen/Documents/scaler/scaler-mono": "scaler-mono",
	})
	input := "The file is at /Users/hergen/Documents/scaler/scaler-mono/src/main.go"
	result := scrub(input)
	expected := "The file is at <scaler-mono>/src/main.go"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestBuildPathScrubber_LongestFirst(t *testing.T) {
	scrub := buildPathScrubber(map[string]string{
		"/Users/hergen/Documents":                    "docs",
		"/Users/hergen/Documents/scaler/scaler-mono": "scaler-mono",
	})
	input := "path: /Users/hergen/Documents/scaler/scaler-mono/file.go"
	result := scrub(input)
	// Longest path should match first, not the shorter prefix
	if strings.Contains(result, "<docs>") {
		t.Errorf("shorter path matched before longer — got %q", result)
	}
	expected := "path: <scaler-mono>/file.go"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestBuildPathScrubber_MultiplePaths(t *testing.T) {
	scrub := buildPathScrubber(map[string]string{
		"/Users/hergen/Documents/scaler/scaler-mono": "scaler-mono",
		"/Users/hergen/Documents/scaler/audits-php":  "audits-php",
	})
	input := "repos: /Users/hergen/Documents/scaler/scaler-mono and /Users/hergen/Documents/scaler/audits-php"
	result := scrub(input)
	if strings.Contains(result, "/Users/hergen") {
		t.Errorf("absolute path not scrubbed: %q", result)
	}
	if !strings.Contains(result, "<scaler-mono>") || !strings.Contains(result, "<audits-php>") {
		t.Errorf("expected both repo names, got %q", result)
	}
}

func TestFixThisBlocks(t *testing.T) {
	text := "Found a bug in utils/time.go"
	threadTS := "1234567890.123456"
	blocks := FixThisBlocks(text, threadTS)

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	section, ok := blocks[0].(*goslack.SectionBlock)
	if !ok {
		t.Fatalf("expected SectionBlock, got %T", blocks[0])
	}
	if section.Text == nil || section.Text.Text != text {
		t.Errorf("expected text %q, got %q", text, section.Text.Text)
	}

	actions, ok := blocks[1].(*goslack.ActionBlock)
	if !ok {
		t.Fatalf("expected ActionBlock, got %T", blocks[1])
	}
	if len(actions.Elements.ElementSet) != 1 {
		t.Fatalf("expected 1 element, got %d", len(actions.Elements.ElementSet))
	}
	btn, ok := actions.Elements.ElementSet[0].(*goslack.ButtonBlockElement)
	if !ok {
		t.Fatalf("expected ButtonBlockElement, got %T", actions.Elements.ElementSet[0])
	}
	if btn.ActionID != "toad_fix" {
		t.Errorf("expected action_id 'toad_fix', got %q", btn.ActionID)
	}
	if btn.Value != threadTS {
		t.Errorf("expected value %q, got %q", threadTS, btn.Value)
	}
	if btn.Style != goslack.StylePrimary {
		t.Errorf("expected primary style, got %q", btn.Style)
	}
}

func TestSpawnedByBlocks(t *testing.T) {
	text := "Found a bug in utils/time.go"
	// Build original blocks as FixThisBlocks would
	origBlocks := FixThisBlocks(text, "1234567890.123456")
	orig := goslack.Blocks{BlockSet: origBlocks}

	blocks := SpawnedByBlocks(orig, "jamie")

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (section + context), got %d", len(blocks))
	}

	// First block: original section preserved
	section, ok := blocks[0].(*goslack.SectionBlock)
	if !ok {
		t.Fatalf("expected SectionBlock, got %T", blocks[0])
	}
	if section.Text.Text != text {
		t.Errorf("expected text %q, got %q", text, section.Text.Text)
	}

	// Second block: action block replaced with context
	ctx, ok := blocks[1].(*goslack.ContextBlock)
	if !ok {
		t.Fatalf("expected ContextBlock, got %T", blocks[1])
	}
	if len(ctx.ContextElements.Elements) != 1 {
		t.Fatalf("expected 1 context element, got %d", len(ctx.ContextElements.Elements))
	}
	ctxText, ok := ctx.ContextElements.Elements[0].(*goslack.TextBlockObject)
	if !ok {
		t.Fatalf("expected TextBlockObject, got %T", ctx.ContextElements.Elements[0])
	}
	if ctxText.Text != ":hatching_chick: Tadpole spawned by jamie" {
		t.Errorf("unexpected context text: %q", ctxText.Text)
	}
}

func TestSpawnedByBlocks_ProcessingState(t *testing.T) {
	text := "Found a bug in utils/time.go"
	origBlocks := FixThisBlocks(text, "1234567890.123456")
	orig := goslack.Blocks{BlockSet: origBlocks}

	blocks := SpawnedByBlocks(orig, "")

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (section + context), got %d", len(blocks))
	}

	ctx, ok := blocks[1].(*goslack.ContextBlock)
	if !ok {
		t.Fatalf("expected ContextBlock, got %T", blocks[1])
	}
	ctxText, ok := ctx.ContextElements.Elements[0].(*goslack.TextBlockObject)
	if !ok {
		t.Fatalf("expected TextBlockObject, got %T", ctx.ContextElements.Elements[0])
	}
	if ctxText.Text != ":hourglass_flowing_sand: Spawning tadpole..." {
		t.Errorf("unexpected processing text: %q", ctxText.Text)
	}
}

func TestParseInteraction_FixButton(t *testing.T) {
	cb := &goslack.InteractionCallback{
		Type: goslack.InteractionTypeBlockActions,
		Channel: goslack.Channel{
			GroupConversation: goslack.GroupConversation{
				Conversation: goslack.Conversation{ID: "C123"},
			},
		},
		User:      goslack.User{ID: "U456", Name: "jamie"},
		MessageTs: "111.222",
		ActionCallback: goslack.ActionCallbacks{
			BlockActions: []*goslack.BlockAction{
				{
					ActionID: "toad_fix",
					Value:    "999.888",
					BlockID:  "toad_fix_actions",
				},
			},
		},
	}

	action, threadTS, channel, userID := parseFixAction(cb)
	if !action {
		t.Fatal("expected action=true")
	}
	if threadTS != "999.888" {
		t.Errorf("expected threadTS '999.888', got %q", threadTS)
	}
	if channel != "C123" {
		t.Errorf("expected channel 'C123', got %q", channel)
	}
	if userID != "U456" {
		t.Errorf("expected userID 'U456', got %q", userID)
	}
}

func TestParseInteraction_WrongAction(t *testing.T) {
	cb := &goslack.InteractionCallback{
		Type: goslack.InteractionTypeBlockActions,
		ActionCallback: goslack.ActionCallbacks{
			BlockActions: []*goslack.BlockAction{
				{ActionID: "something_else", Value: "999.888"},
			},
		},
	}

	action, _, _, _ := parseFixAction(cb)
	if action {
		t.Fatal("expected action=false for non-toad action")
	}
}

func TestSetPathScrubber_EmptyMap(t *testing.T) {
	c := &Client{seen: make(map[string]time.Time), replies: make(map[string]time.Time)}
	c.SetPathScrubber(map[string]string{})
	if c.pathScrubber != nil {
		t.Error("expected nil scrubber for empty map")
	}
}

func TestSetStatus_NilAPI(t *testing.T) {
	c := &Client{}
	c.SetStatus("C123", "1234.5678", "thinking...")
}

func TestClearStatus_NilAPI(t *testing.T) {
	c := &Client{}
	c.ClearStatus("C123", "1234.5678")
}

func TestSetStatus_WithLoadingMessages(t *testing.T) {
	c := &Client{}
	c.SetStatus("C123", "1234.5678", "thinking...", "loading 1", "loading 2")
}
