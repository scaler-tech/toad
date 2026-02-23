package triage

import (
	"testing"
)

func TestParseResult_ValidJSON(t *testing.T) {
	input := `{"actionable":true,"confidence":0.9,"summary":"nil pointer in handler","category":"bug","estimated_size":"small","keywords":["nil","handler"],"files_hint":["internal/handler.go"]}`
	result, err := parseResult([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Actionable {
		t.Error("expected actionable=true")
	}
	if result.Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", result.Confidence)
	}
	if result.Category != "bug" {
		t.Errorf("expected category 'bug', got %q", result.Category)
	}
	if result.EstSize != "small" {
		t.Errorf("expected size 'small', got %q", result.EstSize)
	}
	if len(result.Keywords) != 2 {
		t.Errorf("expected 2 keywords, got %d", len(result.Keywords))
	}
	if len(result.FilesHint) != 1 || result.FilesHint[0] != "internal/handler.go" {
		t.Errorf("unexpected files_hint: %v", result.FilesHint)
	}
}

func TestParseResult_CodeFenced(t *testing.T) {
	input := "```json\n{\"actionable\":true,\"confidence\":0.5,\"summary\":\"test\",\"category\":\"question\",\"estimated_size\":\"tiny\",\"keywords\":[],\"files_hint\":[]}\n```"
	result, err := parseResult([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Category != "question" {
		t.Errorf("expected category 'question', got %q", result.Category)
	}
}

func TestParseResult_TrailingText(t *testing.T) {
	input := `{"actionable":false,"confidence":0.3,"summary":"just chatting","category":"other","estimated_size":"tiny","keywords":[],"files_hint":[]}

I classified this as not actionable because it seems like general conversation.`
	result, err := parseResult([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Actionable {
		t.Error("expected actionable=false")
	}
	if result.Category != "other" {
		t.Errorf("expected category 'other', got %q", result.Category)
	}
}

func TestParseResult_LeadingText(t *testing.T) {
	input := `Here is my analysis:
{"actionable":true,"confidence":0.7,"summary":"add feature","category":"feature","estimated_size":"medium","keywords":["export"],"files_hint":[]}`
	result, err := parseResult([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Category != "feature" {
		t.Errorf("expected category 'feature', got %q", result.Category)
	}
}

func TestParseResult_InvalidJSON(t *testing.T) {
	_, err := parseResult([]byte("not json at all"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseResult_EmptyInput(t *testing.T) {
	_, err := parseResult([]byte(""))
	if err == nil {
		t.Error("expected error for empty input")
	}
}

func TestParseResult_EmptyKeywordsAndFiles(t *testing.T) {
	input := `{"actionable":true,"confidence":1.0,"summary":"test","category":"bug","estimated_size":"tiny","keywords":[],"files_hint":[]}`
	result, err := parseResult([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Keywords) != 0 {
		t.Errorf("expected empty keywords, got %v", result.Keywords)
	}
	if len(result.FilesHint) != 0 {
		t.Errorf("expected empty files_hint, got %v", result.FilesHint)
	}
}
