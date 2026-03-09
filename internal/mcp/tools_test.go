package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/state"
)

func TestReadLogs(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "toad.log")

	lines := []string{
		`time=2026-03-09T10:00:00Z level=INFO msg="server started" port=8099`,
		`time=2026-03-09T10:00:01Z level=DEBUG msg="processing message" channel=general`,
		`time=2026-03-09T10:00:02Z level=ERROR msg="triage failed" error="timeout"`,
		`time=2026-03-09T10:00:03Z level=INFO msg="ribbit complete" duration=2.5s`,
		`time=2026-03-09T10:00:04Z level=WARN msg="rate limited" user=U123`,
	}
	os.WriteFile(logFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644)

	// Read last 3 lines
	result, err := readLogs(logFile, 3, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(result), "\n")
	if len(got) != 3 {
		t.Errorf("expected 3 lines, got %d: %q", len(got), result)
	}

	// Filter by level
	result, _ = readLogs(logFile, 100, "ERROR", "", "")
	if !strings.Contains(result, "triage failed") {
		t.Errorf("expected error line, got: %q", result)
	}
	if strings.Contains(result, "server started") {
		t.Error("should not contain INFO when filtering ERROR")
	}

	// Search filter (substring)
	result, _ = readLogs(logFile, 100, "", "ribbit", "")
	if !strings.Contains(result, "ribbit complete") {
		t.Errorf("expected ribbit line, got: %q", result)
	}

	// Search filter (regex)
	result, _ = readLogs(logFile, 100, "", "triage.*timeout", "")
	if !strings.Contains(result, "triage failed") {
		t.Errorf("expected triage line via regex, got: %q", result)
	}

	// Regex is case-insensitive
	result, _ = readLogs(logFile, 100, "", "RIBBIT.*complete", "")
	if !strings.Contains(result, "ribbit complete") {
		t.Errorf("expected case-insensitive regex match, got: %q", result)
	}

	// Invalid regex falls back to substring
	result, _ = readLogs(logFile, 100, "", "[invalid", "")
	if !strings.Contains(result, "No matching") {
		t.Errorf("expected no matches for invalid regex as substring, got: %q", result)
	}

	// No matches
	result, _ = readLogs(logFile, 100, "", "nonexistent", "")
	if !strings.Contains(result, "No matching") {
		t.Errorf("expected no matches message, got: %q", result)
	}
}

func TestFormatWatches(t *testing.T) {
	watches := []*state.PRWatch{
		{
			PRNumber:         9461,
			PRURL:            "https://github.com/org/repo/pull/9461",
			Branch:           "fix/upload-validation",
			FixCount:         1,
			CIFixCount:       2,
			ConflictFixCount: 0,
			OriginalSummary:  "Fix upload validation for empty CSV",
			CreatedAt:        time.Now().Add(-2 * time.Hour),
		},
		{
			PRNumber:  9462,
			PRURL:     "https://github.com/org/repo/pull/9462",
			Branch:    "feat/add-key-row",
			CreatedAt: time.Now().Add(-30 * time.Minute),
		},
	}

	result := formatWatches(watches)

	if !strings.Contains(result, "2 open PR watches") {
		t.Errorf("expected header with count, got: %q", result)
	}
	if !strings.Contains(result, "PR #9461") {
		t.Error("expected PR 9461 in output")
	}
	if !strings.Contains(result, "PR #9462") {
		t.Error("expected PR 9462 in output")
	}
	if !strings.Contains(result, "Review fixes: 1  CI fixes: 2") {
		t.Error("expected fix counts in output")
	}
	if !strings.Contains(result, "Fix upload validation") {
		t.Error("expected summary in output")
	}
	// PR 9462 has no summary, so "Summary:" should not appear for it
	parts := strings.SplitN(result, "PR #9462", 2)
	if len(parts) == 2 && strings.Contains(parts[1], "Summary:") {
		t.Error("should not show Summary line when empty")
	}
}

func TestFormatWatches_Empty(t *testing.T) {
	result := formatWatches(nil)
	if !strings.Contains(result, "0 open PR watches") {
		t.Errorf("expected empty header, got: %q", result)
	}
}
