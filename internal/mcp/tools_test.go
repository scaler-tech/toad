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

func TestFormatInvestigation_ByTicket(t *testing.T) {
	db := openTestDB(t)
	created := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)

	if err := db.SaveInvestigation(&state.InvestigationRecord{
		ID:           "inv-1",
		ThreadTS:     "1722500000.000100",
		Channel:      "C123",
		Repo:         "toad",
		FindingsJSON: `{"summary":"billing crash"}`,
		CreatedAt:    created,
	}); err != nil {
		t.Fatalf("SaveInvestigation: %v", err)
	}
	if err := db.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey:     "thread:C123:1722500000.000100",
		IssueID:         "SCL-1482",
		IssueURL:        "https://linear.app/scl/issue/SCL-1482",
		Source:          "auto",
		InvestigationID: "inv-1",
		CreatedAt:       created,
		LastSeenAt:      created,
	}); err != nil {
		t.Fatalf("UpsertTicketIndex: %v", err)
	}

	result, err := formatInvestigation(db, "SCL-1482", "")
	if err != nil {
		t.Fatalf("formatInvestigation: %v", err)
	}
	if !strings.Contains(result, "inv-1") {
		t.Errorf("expected investigation ID in header, got: %q", result)
	}
	if !strings.Contains(result, "toad") {
		t.Errorf("expected repo in header, got: %q", result)
	}
	if !strings.Contains(result, `{"summary":"billing crash"}`) {
		t.Errorf("expected findings JSON in result, got: %q", result)
	}
}

func TestFormatInvestigation_ByThread(t *testing.T) {
	db := openTestDB(t)
	created := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)

	if err := db.SaveInvestigation(&state.InvestigationRecord{
		ID:           "inv-2",
		ThreadTS:     "1722500000.000200",
		Channel:      "C456",
		Repo:         "hopper",
		FindingsJSON: `{"summary":"upload timeout"}`,
		CreatedAt:    created,
	}); err != nil {
		t.Fatalf("SaveInvestigation: %v", err)
	}

	result, err := formatInvestigation(db, "", "1722500000.000200")
	if err != nil {
		t.Fatalf("formatInvestigation: %v", err)
	}
	if !strings.Contains(result, "inv-2") {
		t.Errorf("expected investigation ID in header, got: %q", result)
	}
	if !strings.Contains(result, `{"summary":"upload timeout"}`) {
		t.Errorf("expected findings JSON in result, got: %q", result)
	}
}

func TestFormatInvestigation_TicketPreferredOverThread(t *testing.T) {
	db := openTestDB(t)
	created := time.Now()

	if err := db.SaveInvestigation(&state.InvestigationRecord{
		ID:           "inv-ticket",
		ThreadTS:     "thread-a",
		Channel:      "C1",
		Repo:         "toad",
		FindingsJSON: `{"summary":"from ticket"}`,
		CreatedAt:    created,
	}); err != nil {
		t.Fatalf("SaveInvestigation: %v", err)
	}
	if err := db.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey:     "thread:C1:thread-a",
		IssueID:         "SCL-1",
		Source:          "auto",
		InvestigationID: "inv-ticket",
		CreatedAt:       created,
		LastSeenAt:      created,
	}); err != nil {
		t.Fatalf("UpsertTicketIndex: %v", err)
	}
	if err := db.SaveInvestigation(&state.InvestigationRecord{
		ID:           "inv-thread",
		ThreadTS:     "thread-b",
		Channel:      "C2",
		Repo:         "toad",
		FindingsJSON: `{"summary":"from thread"}`,
		CreatedAt:    created,
	}); err != nil {
		t.Fatalf("SaveInvestigation: %v", err)
	}

	// Both ticket and thread set: ticket should win.
	result, err := formatInvestigation(db, "SCL-1", "thread-b")
	if err != nil {
		t.Fatalf("formatInvestigation: %v", err)
	}
	if !strings.Contains(result, "inv-ticket") {
		t.Errorf("expected ticket-resolved investigation to win, got: %q", result)
	}
	if strings.Contains(result, "inv-thread") {
		t.Errorf("expected thread investigation to be ignored when ticket set, got: %q", result)
	}
}

func TestFormatInvestigation_NotFound(t *testing.T) {
	db := openTestDB(t)

	result, err := formatInvestigation(db, "SCL-9999", "")
	if err != nil {
		t.Fatalf("formatInvestigation: %v", err)
	}
	if !strings.Contains(result, "no investigation found") {
		t.Errorf("expected friendly not-found message, got: %q", result)
	}
	if !strings.Contains(result, "SCL-9999") {
		t.Errorf("expected ticket ref in not-found message, got: %q", result)
	}

	result, err = formatInvestigation(db, "", "nonexistent-thread")
	if err != nil {
		t.Fatalf("formatInvestigation: %v", err)
	}
	if !strings.Contains(result, "no investigation found") {
		t.Errorf("expected friendly not-found message, got: %q", result)
	}
}

func TestFormatInvestigation_BothEmpty(t *testing.T) {
	db := openTestDB(t)

	result, err := formatInvestigation(db, "", "")
	if err != nil {
		t.Fatalf("formatInvestigation: %v", err)
	}
	if result != "provide ticket or thread" {
		t.Errorf("got %q, want %q", result, "provide ticket or thread")
	}
}

func openTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.OpenDBAt(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
