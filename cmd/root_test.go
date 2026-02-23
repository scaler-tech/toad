package cmd

import "testing"

func TestBuildTaskDescription_NoContext(t *testing.T) {
	result := buildTaskDescription("fix the bug", nil)
	if result != "fix the bug" {
		t.Errorf("expected trigger text only, got %q", result)
	}
}

func TestBuildTaskDescription_EmptyContext(t *testing.T) {
	result := buildTaskDescription("fix the bug", []string{})
	if result != "fix the bug" {
		t.Errorf("expected trigger text only, got %q", result)
	}
}

func TestBuildTaskDescription_WithContext(t *testing.T) {
	result := buildTaskDescription("@toad fix!", []string{
		"Getting a nil pointer in the handler",
		"stack trace: main.go:42",
	})

	if result == "" {
		t.Fatal("expected non-empty result")
	}
	// Should include thread context
	if !contains(result, "nil pointer in the handler") {
		t.Error("missing thread context message 1")
	}
	if !contains(result, "stack trace: main.go:42") {
		t.Error("missing thread context message 2")
	}
	// Trigger should also be appended since it's not in the thread
	if !contains(result, "@toad fix!") {
		t.Error("missing trigger text")
	}
}

func TestBuildTaskDescription_TriggerAlreadyInThread(t *testing.T) {
	result := buildTaskDescription("fix the bug", []string{
		"fix the bug",
		"more context here",
	})

	// Trigger should NOT be duplicated
	count := 0
	for _, line := range splitLines(result) {
		if line == "fix the bug" {
			count++
		}
	}
	if count > 1 {
		t.Errorf("trigger text duplicated %d times", count)
	}
}

func TestBuildTaskDescription_EmptyTrigger(t *testing.T) {
	result := buildTaskDescription("", []string{"context message"})
	if !contains(result, "context message") {
		t.Error("missing context message")
	}
}

func TestBuildTaskDescription_SkipsBlankContext(t *testing.T) {
	result := buildTaskDescription("fix", []string{"real message", "", "  ", "another"})
	if !contains(result, "real message") {
		t.Error("missing real message")
	}
	if !contains(result, "another") {
		t.Error("missing 'another' message")
	}
}

func TestFindMatchingBrace_Simple(t *testing.T) {
	idx := findMatchingBrace(`{"key": "value"}`, 0)
	if idx != 15 {
		t.Errorf("expected 15, got %d", idx)
	}
}

func TestFindMatchingBrace_Nested(t *testing.T) {
	idx := findMatchingBrace(`{"a": {"b": "c"}}`, 0)
	if idx != 16 {
		t.Errorf("expected 16, got %d", idx)
	}
}

func TestFindMatchingBrace_InnerObject(t *testing.T) {
	s := `{"a": {"b": "c"}}`
	idx := findMatchingBrace(s, 6)
	if idx != 15 {
		t.Errorf("expected 15, got %d", idx)
	}
}

func TestFindMatchingBrace_WithEscapedQuotes(t *testing.T) {
	idx := findMatchingBrace(`{"key": "val\"ue"}`, 0)
	if idx != 17 {
		t.Errorf("expected 17, got %d", idx)
	}
}

func TestFindMatchingBrace_BracesInString(t *testing.T) {
	idx := findMatchingBrace(`{"key": "{}"}`, 0)
	if idx != 12 {
		t.Errorf("expected 12, got %d", idx)
	}
}

func TestFindMatchingBrace_NoMatch(t *testing.T) {
	idx := findMatchingBrace(`{"key": "value"`, 0)
	if idx != -1 {
		t.Errorf("expected -1, got %d", idx)
	}
}

func TestFindMatchingBrace_SurroundingText(t *testing.T) {
	s := `Here is the result: {"feasible": true} and that's it.`
	start := 20
	idx := findMatchingBrace(s, start)
	if idx != 37 {
		t.Errorf("expected 37, got %d", idx)
	}
}

func TestFindMatchingBrace_DeeplyNested(t *testing.T) {
	s := `{"a":{"b":{"c":"d"}}}`
	idx := findMatchingBrace(s, 0)
	if idx != len(s)-1 {
		t.Errorf("expected %d, got %d", len(s)-1, idx)
	}
}

// helpers

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
