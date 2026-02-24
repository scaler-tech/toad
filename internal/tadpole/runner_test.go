package tadpole

import (
	"strings"
	"testing"

	"github.com/hergen/toad/internal/issuetracker"
	"github.com/hergen/toad/internal/triage"
)

func TestBuildTadpolePrompt(t *testing.T) {
	task := Task{
		Description: "Fix the nil pointer in handler.go",
		Summary:     "nil pointer fix",
		Category:    "bug",
	}
	prompt := buildTadpolePrompt(task, 5, nil)

	if !strings.Contains(prompt, "Fix the nil pointer in handler.go") {
		t.Error("prompt should contain task description")
	}
	if !strings.Contains(prompt, "Stay under 5 files changed") {
		t.Error("prompt should contain max files limit")
	}
	if !strings.Contains(prompt, "tadpole") {
		t.Error("prompt should identify agent as tadpole")
	}
}

func TestBuildTadpolePrompt_WithTriageHints(t *testing.T) {
	task := Task{
		Description: "Fix bug",
		TriageResult: &triage.Result{
			Keywords:  []string{"nil", "pointer"},
			FilesHint: []string{"handler.go", "server.go"},
		},
	}
	prompt := buildTadpolePrompt(task, 5, nil)

	if !strings.Contains(prompt, "nil, pointer") {
		t.Error("prompt should contain keywords")
	}
	if !strings.Contains(prompt, "handler.go, server.go") {
		t.Error("prompt should contain file hints")
	}
}

func TestBuildTadpolePrompt_NoTriageResult(t *testing.T) {
	task := Task{
		Description: "Fix bug",
	}
	prompt := buildTadpolePrompt(task, 5, nil)

	// Should not panic and should still contain rules
	if !strings.Contains(prompt, "Rules") {
		t.Error("prompt should contain rules even without triage")
	}
}

func TestBuildRetryPrompt(t *testing.T) {
	vr := &ValidationResult{
		Passed:     false,
		TestPassed: false,
		LintPassed: true,
		TestOutput: "FAIL: TestFoo\n    expected 1, got 2",
		FailReason: "tests failed",
	}
	prompt := buildRetryPrompt(vr)

	if !strings.Contains(prompt, "tests failed") {
		t.Error("prompt should contain fail reason")
	}
	if !strings.Contains(prompt, "FAIL: TestFoo") {
		t.Error("prompt should contain test output")
	}
	if strings.Contains(prompt, "Lint output") {
		t.Error("prompt should NOT contain lint section when lint passed")
	}
}

func TestBuildRetryPrompt_BothFailed(t *testing.T) {
	vr := &ValidationResult{
		Passed:     false,
		TestPassed: false,
		LintPassed: false,
		TestOutput: "test failure",
		LintOutput: "lint error",
		FailReason: "tests and lint failed",
	}
	prompt := buildRetryPrompt(vr)

	if !strings.Contains(prompt, "test failure") {
		t.Error("prompt should contain test output")
	}
	if !strings.Contains(prompt, "lint error") {
		t.Error("prompt should contain lint output")
	}
}

func TestBuildBranchSlug_WithIssueRef(t *testing.T) {
	task := Task{
		Summary: "fix nil pointer in handler",
		IssueRef: &issuetracker.IssueRef{
			Provider: "linear",
			ID:       "PLF-3125",
		},
	}
	got := buildBranchSlug(task)
	if !strings.HasPrefix(got, "plf-3125-") {
		t.Errorf("expected slug to start with 'plf-3125-', got %q", got)
	}
	if len(got) > 40 {
		t.Errorf("slug should be max 40 chars, got %d: %q", len(got), got)
	}
}

func TestBuildBranchSlug_WithoutIssueRef(t *testing.T) {
	task := Task{
		Summary: "fix nil pointer in handler",
	}
	got := buildBranchSlug(task)
	// Without issue ref, returns raw summary (Slugify is done in CreateWorktree)
	if got != "fix nil pointer in handler" {
		t.Errorf("expected raw summary, got %q", got)
	}
}

func TestBuildBranchSlug_Truncation(t *testing.T) {
	task := Task{
		Summary: "fix a very long description that exceeds the limit",
		IssueRef: &issuetracker.IssueRef{
			Provider: "linear",
			ID:       "PLF-3125",
		},
	}
	got := buildBranchSlug(task)
	if len(got) > 40 {
		t.Errorf("slug should be max 40 chars, got %d: %q", len(got), got)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("slug should not end with hyphen, got %q", got)
	}
}
