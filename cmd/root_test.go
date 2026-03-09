package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/scaler-tech/toad/internal/digest"
)

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

// TestParseInvestigateResult_ProseWithStrayBraces reproduces the exact bug:
// Claude returns prose containing "?? {}" before the real JSON, causing the
// old parser to extract "{}" and silently default feasible to false.
func TestParseInvestigateResult_ProseWithStrayBraces(t *testing.T) {
	// This is the exact pattern from the assetListCollection incident
	resultText := `Based on my research, the method protects activeFilters[scope] with ?? {} but does not protect activeFiltersValues[scope].

{"feasible": true, "task_spec": "Add null guard in filtersStore.ts", "reasoning": "Clear one-line fix"}`

	text := resultText
	var result struct {
		Feasible  bool   `json:"feasible"`
		TaskSpec  string `json:"task_spec"`
		Reasoning string `json:"reasoning"`
	}
	parsed := false

	// Strategy 1: look for {"feasible" directly
	if idx := strings.Index(text, `{"feasible"`); idx >= 0 {
		if end := findMatchingBrace(text, idx); end >= 0 {
			if err := json.Unmarshal([]byte(text[idx:end+1]), &result); err == nil {
				parsed = true
			}
		}
	}

	if !parsed {
		t.Fatal("failed to parse — strategy 1 should have found the JSON")
	}
	if !result.Feasible {
		t.Error("expected feasible=true — parser likely matched stray {} in prose")
	}
	if result.TaskSpec == "" {
		t.Error("expected non-empty task_spec")
	}
	if result.Reasoning == "" {
		t.Error("expected non-empty reasoning")
	}
}

func TestStripCodeFences_WithJSON(t *testing.T) {
	input := "Some text\n```json\n{\"feasible\": true}\n```\nMore text"
	got := stripCodeFences(input)
	expected := "{\"feasible\": true}\n"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestStripCodeFences_NoFences(t *testing.T) {
	input := `{"feasible": true}`
	got := stripCodeFences(input)
	if got != input {
		t.Errorf("expected input unchanged, got %q", got)
	}
}

func TestStripCodeFences_PlainFences(t *testing.T) {
	input := "```\n{\"feasible\": false}\n```"
	got := stripCodeFences(input)
	expected := "{\"feasible\": false}\n"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestIsRetryIntent(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"try again", true},
		{"can you try again?", true},
		{"retry", true},
		{"please retry this", true},
		{"redo", true},
		{"re-do this please", true},
		{"one more time", true},
		{"rerun", true},
		{"re-run it", true},
		{"@toad TRY AGAIN", true},
		{"fix the nil pointer in handler.go", false},
		{"great work!", false},
		{"", false},
		{"this is trying my patience", false}, // "try" without "again" - should not match
	}
	for _, tt := range tests {
		got := isRetryIntent(tt.text)
		if got != tt.want {
			t.Errorf("isRetryIntent(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestHasFailedTadpole(t *testing.T) {
	tests := []struct {
		name    string
		context []string
		want    bool
	}{
		{
			"with failure marker",
			[]string{"some message", ":x: Tadpole failed — tests failed", "user: try again"},
			true,
		},
		{
			"no failure marker",
			[]string{"some message", ":white_check_mark: Done! PR: https://github.com/pr/1"},
			false,
		},
		{
			"empty context",
			nil,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasFailedTadpole(tt.context)
			if got != tt.want {
				t.Errorf("hasFailedTadpole() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"this is a long string that needs truncation", 20, "this is a long st..."},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := truncate(tt.input, tt.n)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
		}
	}
}

func TestParseInvestigateResult_ValidFeasible(t *testing.T) {
	result := parseInvestigateResult(
		`{"feasible": true, "task_spec": "Fix nil check in handler.go", "reasoning": "Clear one-line fix"}`,
		false,
	)
	if !result.Feasible {
		t.Error("expected feasible=true")
	}
	if result.TaskSpec != "Fix nil check in handler.go" {
		t.Errorf("unexpected task_spec: %q", result.TaskSpec)
	}
	if result.Reasoning != "Clear one-line fix" {
		t.Errorf("unexpected reasoning: %q", result.Reasoning)
	}
}

func TestParseInvestigateResult_NotFeasible(t *testing.T) {
	result := parseInvestigateResult(
		`{"feasible": false, "task_spec": "", "reasoning": "Could not locate the issue"}`,
		false,
	)
	if result.Feasible {
		t.Error("expected feasible=false")
	}
}

func TestParseInvestigateResult_HitMaxTurns_NoJSON(t *testing.T) {
	// When hitMaxTurns is true and there's no parseable JSON, the result
	// should use the reasonMaxTurns sentinel so the caller knows to resume.
	result := parseInvestigateResult("I was still investigating when I ran out of turns...", true)
	if result.Feasible {
		t.Error("expected feasible=false")
	}
	if result.Reasoning != reasonMaxTurns {
		t.Errorf("expected reasonMaxTurns sentinel, got %q", result.Reasoning)
	}
}

func TestParseInvestigateResult_HitMaxTurns_WithJSON(t *testing.T) {
	// Even if hitMaxTurns is true, if the agent produced valid JSON, use it.
	result := parseInvestigateResult(
		`{"feasible": true, "task_spec": "Fix the bug", "reasoning": "Found it just in time"}`,
		true,
	)
	if !result.Feasible {
		t.Error("expected feasible=true — valid JSON should be used even if max turns hit")
	}
	if result.TaskSpec != "Fix the bug" {
		t.Errorf("unexpected task_spec: %q", result.TaskSpec)
	}
}

func TestParseInvestigateResult_NoJSON_NotMaxTurns(t *testing.T) {
	// When there's no JSON and it's not max turns, the reason should say no JSON found.
	result := parseInvestigateResult("I couldn't find anything relevant.", false)
	if result.Feasible {
		t.Error("expected feasible=false")
	}
	if result.Reasoning == reasonMaxTurns {
		t.Error("should NOT use reasonMaxTurns when hitMaxTurns=false")
	}
}

func TestParseInvestigateResult_StrayBraces(t *testing.T) {
	// Regression test: prose containing "{}" before the real JSON.
	result := parseInvestigateResult(
		`The method protects with ?? {} but misses the other field.

{"feasible": true, "task_spec": "Add null guard", "reasoning": "Clear fix"}`,
		false,
	)
	if !result.Feasible {
		t.Error("expected feasible=true — parser should skip stray braces in prose")
	}
}

func TestParseInvestigateResult_IssueID(t *testing.T) {
	result := parseInvestigateResult(
		`{"feasible": true, "task_spec": "Add refrigerants to seeder", "reasoning": "Clear fix", "issue_id": "PLF-3198"}`,
		false,
	)
	if !result.Feasible {
		t.Error("expected feasible=true")
	}
	if result.IssueID != "PLF-3198" {
		t.Errorf("expected issue_id PLF-3198, got %q", result.IssueID)
	}
}

func TestParseInvestigateResult_EmptyIssueID(t *testing.T) {
	result := parseInvestigateResult(
		`{"feasible": true, "task_spec": "Fix bug", "reasoning": "Found it", "issue_id": ""}`,
		false,
	)
	if result.IssueID != "" {
		t.Errorf("expected empty issue_id, got %q", result.IssueID)
	}
}

func TestFormatTicketContext_Empty(t *testing.T) {
	result := formatTicketContext(nil)
	if result != "" {
		t.Errorf("expected empty string for nil tickets, got %q", result)
	}
}

func TestFormatTicketContext_WithTickets(t *testing.T) {
	tickets := []digest.TicketContext{
		{ID: "PLF-3198", Title: "Add R449A refrigerant", Description: "Need to add R449A to the F-gas seeder", URL: "https://linear.app/t/issue/PLF-3198"},
		{ID: "REP-1577", Title: "Fix report crash"},
	}
	result := formatTicketContext(tickets)
	if !searchString(result, "PLF-3198") {
		t.Error("expected PLF-3198 in output")
	}
	if !searchString(result, "REP-1577") {
		t.Error("expected REP-1577 in output")
	}
	if !searchString(result, "Add R449A refrigerant") {
		t.Error("expected ticket title in output")
	}
	if !searchString(result, "<linked_tickets>") {
		t.Error("expected linked_tickets tags")
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
