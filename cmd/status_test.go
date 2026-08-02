package cmd

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/state"
)

// TestApiDataHandler_MarshalsNewPayloadFields is a smoke test for the v2
// dashboard payload: it seeds a :memory:-backed DB with one investigation
// (linked to a filed ticket), one unlinked ticket, and a digest opportunity,
// then asserts the new investigations/tickets/aggregates/series fields
// marshal with the expected shape, and that the deleted runs-pipeline fields
// (merge_stats, active, history, pr_noun) are gone from the payload.
func TestApiDataHandler_MarshalsNewPayloadFields(t *testing.T) {
	db := newTestDB(t)

	now := time.Now()

	findingsJSON := `{"feasible":true,"title":"billing export drops rows","root_cause":"grouping bug","confidence":0.91,"repo":"platform-api","sentry_issue_ids":["BILLING-2291"],"reasoning":"looks solid"}`
	if err := db.SaveInvestigation(&state.InvestigationRecord{
		ID:           "inv-1",
		ThreadTS:     "111.222",
		Channel:      "C123",
		Repo:         "platform-api",
		FindingsJSON: findingsJSON,
		DurationMs:   4200,
		CreatedAt:    now,
	}); err != nil {
		t.Fatalf("SaveInvestigation: %v", err)
	}

	if err := db.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey:     "thread:C123:111.222",
		IssueID:         "SCL-1482",
		IssueURL:        "https://linear.app/scl/issue/SCL-1482",
		Source:          "auto",
		InvestigationID: "inv-1",
		CreatedAt:       now,
		LastSeenAt:      now,
		LastStatus:      "Triage",
		LastStateType:   "triage",
	}); err != nil {
		t.Fatalf("UpsertTicketIndex (linked): %v", err)
	}

	if err := db.UpsertTicketIndex(&state.TicketIndexEntry{
		ExternalKey: "thread:C042:333.444",
		IssueID:     "SCL-1490",
		IssueURL:    "https://linear.app/scl/issue/SCL-1490",
		Source:      "cta",
		CreatedAt:   now,
		LastSeenAt:  now,
	}); err != nil {
		t.Fatalf("UpsertTicketIndex (unlinked): %v", err)
	}

	if err := db.SaveDigestOpportunity(&state.DigestOpportunity{
		Summary:    "nightly ingest skips rows",
		Category:   "bug",
		Confidence: 0.7,
		EstSize:    "small",
		Channel:    "data-eng",
		DryRun:     true,
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("SaveDigestOpportunity: %v", err)
	}

	cfg := &config.Config{}
	handler := apiDataHandler(db, cfg)

	req := httptest.NewRequest("GET", "/api/data", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status code: got %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	// First assert against the typed struct so field renames/type changes
	// surface as compile errors, not silently-passing map lookups.
	var resp apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v, body=%s", err, rec.Body.String())
	}

	if len(resp.Investigations) != 1 {
		t.Fatalf("expected 1 investigation, got %d", len(resp.Investigations))
	}
	inv := resp.Investigations[0]
	if inv.ID != "inv-1" {
		t.Errorf("investigation ID: got %q, want %q", inv.ID, "inv-1")
	}
	if inv.Title != "billing export drops rows" {
		t.Errorf("investigation Title: got %q", inv.Title)
	}
	if inv.Confidence != 0.91 {
		t.Errorf("investigation Confidence: got %v, want 0.91", inv.Confidence)
	}
	if !inv.Feasible {
		t.Error("investigation Feasible: got false, want true")
	}
	if len(inv.SentryIssueIDs) != 1 || inv.SentryIssueIDs[0] != "BILLING-2291" {
		t.Errorf("investigation SentryIssueIDs: got %v", inv.SentryIssueIDs)
	}
	if inv.Stale {
		t.Error("investigation Stale: got true, want false (no stale caveat in reasoning)")
	}
	if inv.DurationMs != 4200 {
		t.Errorf("investigation DurationMs: got %d, want 4200", inv.DurationMs)
	}
	if len(inv.Findings) == 0 {
		t.Error("investigation Findings: expected full findings JSON, got empty")
	}
	if inv.Ticket == nil {
		t.Fatal("expected investigation to resolve its filed ticket")
	}
	if inv.Ticket.IssueID != "SCL-1482" {
		t.Errorf("investigation Ticket.IssueID: got %q, want %q", inv.Ticket.IssueID, "SCL-1482")
	}

	if len(resp.Tickets) != 2 {
		t.Fatalf("expected 2 tickets, got %d", len(resp.Tickets))
	}

	if resp.Aggregates == nil {
		t.Fatal("expected aggregates to be present")
	}
	if resp.Aggregates.Today.Investigations != 1 {
		t.Errorf("aggregates.today.investigations: got %d, want 1", resp.Aggregates.Today.Investigations)
	}
	if resp.Aggregates.Today.Filed != 2 {
		t.Errorf("aggregates.today.filed: got %d, want 2", resp.Aggregates.Today.Filed)
	}
	if resp.Aggregates.Today.FiledBySource["auto"] != 1 || resp.Aggregates.Today.FiledBySource["cta"] != 1 {
		t.Errorf("aggregates.today.filed_by_source: got %v", resp.Aggregates.Today.FiledBySource)
	}
	if resp.Aggregates.Week.Investigations != 1 || resp.Aggregates.Month.Investigations != 1 {
		t.Errorf("expected week/month aggregates to also include today's investigation: %+v", resp.Aggregates)
	}

	if resp.Series == nil {
		t.Fatal("expected series to be present")
	}
	if len(resp.Series.InvestHourly) != 24 || len(resp.Series.InvestDaily) != 30 {
		t.Errorf("invest series lengths: hourly=%d daily=%d", len(resp.Series.InvestHourly), len(resp.Series.InvestDaily))
	}
	if len(resp.Series.FiledHourly) != 24 || len(resp.Series.FiledDaily) != 30 {
		t.Errorf("filed series lengths: hourly=%d daily=%d", len(resp.Series.FiledHourly), len(resp.Series.FiledDaily))
	}
	// intake/qa come from the (empty) metrics_hourly table — zero-filled,
	// not nil/absent.
	if len(resp.Series.IntakeHourly) != 24 || len(resp.Series.IntakeDaily) != 30 {
		t.Errorf("intake series lengths: hourly=%d daily=%d", len(resp.Series.IntakeHourly), len(resp.Series.IntakeDaily))
	}
	sum := 0
	for _, v := range resp.Series.IntakeHourly {
		sum += v
	}
	if sum != 0 {
		t.Errorf("expected all-zero intake series (no metrics recorded), got %v", resp.Series.IntakeHourly)
	}

	if resp.Config == nil {
		t.Fatal("expected config to be present")
	}
	// The seeded digest opportunity should surface via digest_counts (the
	// only place the frontend reads digest-opportunity data from — the
	// per-opportunity "opportunities" field was dropped as dead payload
	// weight, see the deleted-fields check below).
	if resp.DigestCounts == nil || resp.DigestCounts.DryRun != 1 {
		t.Fatalf("expected digest_counts.DryRun=1 from the seeded opportunity, got %+v", resp.DigestCounts)
	}

	// Deleted runs-pipeline fields (and the dead per-opportunity payload)
	// must not reappear in the payload.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw response: %v", err)
	}
	for _, deleted := range []string{"merge_stats", "active", "history", "pr_noun", "stats", "opportunities"} {
		if _, ok := raw[deleted]; ok {
			t.Errorf("expected deleted field %q to be absent from payload, but it was present", deleted)
		}
	}
	for _, present := range []string{"investigations", "tickets", "aggregates", "series", "daemon", "config"} {
		if _, ok := raw[present]; !ok {
			t.Errorf("expected field %q to be present in payload", present)
		}
	}
}

// TestApiDataHandler_EmptyDBDegradesGracefully verifies the handler produces
// a valid, empty-but-well-formed payload against a freshly-migrated DB with
// no investigations, tickets, opportunities, or metrics — the state C2's
// changes will initially run against before any call sites are wired up.
func TestApiDataHandler_EmptyDBDegradesGracefully(t *testing.T) {
	db := newTestDB(t)
	handler := apiDataHandler(db, nil)

	req := httptest.NewRequest("GET", "/api/data", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status code: got %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v, body=%s", err, rec.Body.String())
	}
	if len(resp.Investigations) != 0 {
		t.Errorf("expected no investigations, got %d", len(resp.Investigations))
	}
	if len(resp.Tickets) != 0 {
		t.Errorf("expected no tickets, got %d", len(resp.Tickets))
	}
	if resp.Aggregates == nil || resp.Aggregates.Today.Investigations != 0 || resp.Aggregates.Today.Filed != 0 {
		t.Errorf("expected zero-valued aggregates, got %+v", resp.Aggregates)
	}
	if resp.Series == nil || len(resp.Series.InvestHourly) != 24 {
		t.Errorf("expected zero-filled series even with empty DB, got %+v", resp.Series)
	}
	if resp.Daemon == nil || resp.Daemon.Running {
		t.Errorf("expected daemon.running=false with no heartbeat written, got %+v", resp.Daemon)
	}
}

// TestApiDataHandler_PopulatesConcurrencyGaugesAndSyncStatus is C2's
// end-to-end check that the gauges/sync fields DaemonStats already carried
// (written by root.go's stats ticker via state.RepoSyncStatus,
// concurrencyGauge, etc.) surface correctly through /api/data: the
// investigate/ribbit slot gauges, the per-repo sync_error/last_sync_at on
// apiConfig.Repos, and the derived daemon.sync_warning alert field.
func TestApiDataHandler_PopulatesConcurrencyGaugesAndSyncStatus(t *testing.T) {
	db := newTestDB(t)

	now := time.Now()
	if err := db.WriteDaemonStats(&state.DaemonStats{
		Heartbeat:           now,
		StartedAt:           now,
		PID:                 1234,
		InvestigateSlots:    2,
		InvestigateInFlight: 1,
		RibbitSlots:         6,
		RibbitInFlight:      0,
		RepoSync: map[string]state.RepoSyncStatus{
			"app":      {LastSyncAt: now.Add(-4 * time.Minute), CheckedAt: now.Add(-4 * time.Minute)},
			"platform": {LastError: "fetch failed: network unreachable", CheckedAt: now.Add(-2 * time.Minute)},
		},
	}); err != nil {
		t.Fatalf("WriteDaemonStats: %v", err)
	}

	cfg := &config.Config{
		Repos: config.ReposConfig{List: []config.RepoConfig{
			{Name: "app", Path: "/repos/app"},
			{Name: "platform", Path: "/repos/platform"},
		}},
	}
	handler := apiDataHandler(db, cfg)

	req := httptest.NewRequest("GET", "/api/data", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status code: got %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v, body=%s", err, rec.Body.String())
	}

	if !resp.Daemon.Running {
		t.Fatal("expected daemon.running=true with a fresh heartbeat")
	}
	if resp.Daemon.InvestigateSlots != 2 || resp.Daemon.InvestigateInFlight != 1 {
		t.Errorf("investigate gauges: got slots=%d inFlight=%d, want 2,1", resp.Daemon.InvestigateSlots, resp.Daemon.InvestigateInFlight)
	}
	if resp.Daemon.RibbitSlots != 6 || resp.Daemon.RibbitInFlight != 0 {
		t.Errorf("ribbit gauges: got slots=%d inFlight=%d, want 6,0", resp.Daemon.RibbitSlots, resp.Daemon.RibbitInFlight)
	}

	if resp.Daemon.SyncWarning == nil {
		t.Fatal("expected sync_warning to surface the failing 'platform' repo")
	}
	if resp.Daemon.SyncWarning.Repo != "platform" {
		t.Errorf("sync_warning.repo = %q, want %q", resp.Daemon.SyncWarning.Repo, "platform")
	}
	if resp.Daemon.SyncWarning.Error != "fetch failed: network unreachable" {
		t.Errorf("sync_warning.error = %q", resp.Daemon.SyncWarning.Error)
	}

	if resp.Config == nil || len(resp.Config.Repos) != 2 {
		t.Fatalf("expected 2 repos in config, got %+v", resp.Config)
	}
	byName := map[string]apiConfigRepo{}
	for _, r := range resp.Config.Repos {
		byName[r.Name] = r
	}
	if byName["app"].LastSyncAt == 0 {
		t.Error("expected 'app' to have a non-zero last_sync_at")
	}
	if byName["app"].SyncError != "" {
		t.Errorf("expected 'app' to have no sync_error, got %q", byName["app"].SyncError)
	}
	if byName["platform"].SyncError != "fetch failed: network unreachable" {
		t.Errorf("platform sync_error = %q", byName["platform"].SyncError)
	}
}

// TestApiDataHandler_NoSyncFailuresMeansNoWarning verifies a healthy
// (all-successful, or never-attempted) RepoSync map produces no
// sync_warning — the alert must not fire on data that isn't actually a
// problem.
func TestApiDataHandler_NoSyncFailuresMeansNoWarning(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	if err := db.WriteDaemonStats(&state.DaemonStats{
		Heartbeat: now,
		StartedAt: now,
		PID:       1234,
		RepoSync: map[string]state.RepoSyncStatus{
			"app": {LastSyncAt: now, CheckedAt: now},
		},
	}); err != nil {
		t.Fatalf("WriteDaemonStats: %v", err)
	}

	handler := apiDataHandler(db, &config.Config{})
	req := httptest.NewRequest("GET", "/api/data", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var resp apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v, body=%s", err, rec.Body.String())
	}
	if resp.Daemon.SyncWarning != nil {
		t.Errorf("expected no sync_warning when nothing failed, got %+v", resp.Daemon.SyncWarning)
	}
}

// TestSyncWarningFor covers the tie-break rule directly: with multiple
// concurrently-failing repos, the most recently-checked failure wins.
func TestSyncWarningFor(t *testing.T) {
	now := time.Now()
	repoSync := map[string]state.RepoSyncStatus{
		"older-failure": {LastError: "stale error", CheckedAt: now.Add(-10 * time.Minute)},
		"newer-failure": {LastError: "fresh error", CheckedAt: now.Add(-1 * time.Minute)},
		"healthy":       {LastSyncAt: now, CheckedAt: now},
	}
	got := syncWarningFor(repoSync)
	if got == nil {
		t.Fatal("expected a warning, got nil")
		return
	}
	if got.Repo != "newer-failure" {
		t.Errorf("expected the more recently checked failure to win, got repo=%q", got.Repo)
	}

	if got := syncWarningFor(nil); got != nil {
		t.Errorf("expected nil for an empty map, got %+v", got)
	}
	if got := syncWarningFor(map[string]state.RepoSyncStatus{"ok": {LastSyncAt: now}}); got != nil {
		t.Errorf("expected nil when nothing has an error, got %+v", got)
	}
}
