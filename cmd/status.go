package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/toadpath"
	"github.com/scaler-tech/toad/internal/update"
)

var statusPort int
var statusNoBrowser bool

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Open the toad monitoring dashboard in your browser",
	RunE:  runStatus,
}

func init() {
	statusCmd.Flags().IntVar(&statusPort, "port", 0, "port to serve dashboard on (default: random available port)")
	statusCmd.Flags().BoolVar(&statusNoBrowser, "no-browser", false, "don't open browser on start")
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	db, err := state.OpenDB()
	if err != nil {
		return fmt.Errorf("opening state db: %w", err)
	}
	defer db.Close()

	cfg, _ := config.Load() // non-fatal

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(faviconICO)
	})
	mux.HandleFunc("/favicon-16x16.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(favicon16)
	})
	mux.HandleFunc("/favicon-32x32.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(favicon32)
	})
	mux.HandleFunc("/apple-touch-icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(appleTouchIcon)
	})
	mux.HandleFunc("/logo.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(logoPNG)
	})
	mux.HandleFunc("/api/data", apiDataHandler(db, cfg))
	mux.HandleFunc("/api/check-update", apiCheckUpdateHandler())
	mux.HandleFunc("/api/update", apiUpdateHandler())
	mux.HandleFunc("/api/restart", apiRestartHandler(db))
	mux.HandleFunc("/api/auto-update", apiAutoUpdateHandler(db))
	mux.HandleFunc("/api/dev/info", apiDevInfoHandler(cfg))
	mux.HandleFunc("/api/dev/download-log", apiDevDownloadLogHandler(cfg))
	mux.HandleFunc("/api/dev/download-db", apiDevDownloadDBHandler())
	mux.HandleFunc("/api/dev/reset-log", apiDevResetLogHandler(cfg))
	mux.HandleFunc("/api/dev/reset-db", apiDevResetDBHandler())

	// Start auto-update background loop
	go autoUpdateLoop(db)

	addr := fmt.Sprintf("127.0.0.1:%d", statusPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// Resolve actual port (may differ from statusPort if 0 was used)
	actualPort := ln.Addr().(*net.TCPAddr).Port
	mux.HandleFunc("/api/reload-dashboard", apiReloadDashboardHandler(actualPort))

	url := fmt.Sprintf("http://%s", ln.Addr().String())
	fmt.Printf("toad dashboard: %s\n", url)
	if !statusNoBrowser {
		openBrowser(url)
	}

	fmt.Println("Press Ctrl+C to stop")
	return http.Serve(ln, mux)
}

type apiResponse struct {
	Daemon             *apiDaemon          `json:"daemon"`
	Integrations       []apiIntegration    `json:"integrations"`
	DigestCounts       *state.DigestCounts `json:"digest_counts,omitempty"`
	OutcomeCounts      map[string]int      `json:"outcome_counts,omitempty"`
	Config             *apiConfig          `json:"config,omitempty"`
	CCUsage            *apiCCUsage         `json:"cc_usage,omitempty"`
	Investigations     []apiInvestigation  `json:"investigations"`
	Tickets            []apiTicket         `json:"tickets"`
	Aggregates         *apiAggregates      `json:"aggregates,omitempty"`
	Series             *apiSeries          `json:"series,omitempty"`
	AutoUpdate         bool                `json:"auto_update"`
	AutoRestarting     bool                `json:"auto_restarting,omitempty"`
	AutoRestartPID     int                 `json:"auto_restart_pid,omitempty"`
	AutoRestartStarted int64               `json:"auto_restart_started,omitempty"`
	Now                int64               `json:"now"`
}

const (
	pillEnabled  = "enabled"
	pillDisabled = "disabled"
)

type apiIntegration struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "enabled", "disabled", "dry-run", "active", "inactive"
	Detail string `json:"detail,omitempty"`
}

type apiDaemon struct {
	Running          bool             `json:"running"`
	Draining         bool             `json:"draining,omitempty"`
	Version          string           `json:"version"`
	DaemonVersion    string           `json:"daemon_version,omitempty"`
	Uptime           float64          `json:"uptime_s,omitempty"`
	StartedAt        int64            `json:"started_at,omitempty"`
	PID              int              `json:"pid,omitempty"`
	Ribbits          int64            `json:"ribbits"`
	Triages          int64            `json:"triages"`
	TriageByCategory map[string]int64 `json:"triage_by_category,omitempty"`
	DigestEnabled    bool             `json:"digest_enabled"`
	DigestDryRun     bool             `json:"digest_dry_run"`
	DigestComment    bool             `json:"digest_comment_mode,omitempty"`
	DigestBuffer     int              `json:"digest_buffer"`
	DigestNextFlush  int64            `json:"digest_next_flush,omitempty"`
	DigestProcessed  int64            `json:"digest_processed"`
	DigestOpps       int64            `json:"digest_opportunities"`
	DigestSpawns     int64            `json:"digest_spawns"`
	UpdateAvailable  bool             `json:"update_available,omitempty"`
	LatestVersion    string           `json:"latest_version,omitempty"`

	// Concurrency gauges for the dashboard's "Live now" card. Zero/omitted
	// until the daemon's investigate/ribbit semaphore call sites (owned by
	// another lane) are wired to populate the matching DaemonStats fields.
	InvestigateSlots    int `json:"investigate_slots,omitempty"`
	InvestigateInFlight int `json:"investigate_in_flight,omitempty"`
	RibbitSlots         int `json:"ribbit_slots,omitempty"`
	RibbitInFlight      int `json:"ribbit_in_flight,omitempty"`
}

type apiConfigRepo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type apiConfig struct {
	Repos          []apiConfigRepo `json:"repos"`
	MaxConcurrent  int             `json:"max_concurrent"`
	TimeoutMinutes int             `json:"timeout_minutes"`
	DigestEnabled  bool            `json:"digest_enabled"`
	DigestDryRun   bool            `json:"digest_dry_run"`
	DigestComment  bool            `json:"digest_comment_mode,omitempty"`
	DigestInterval int             `json:"digest_interval_min,omitempty"`
	DigestMaxSpawn int             `json:"digest_max_spawn_hour,omitempty"`
	MCPEnabled     bool            `json:"mcp_enabled"`
	MCPHost        string          `json:"mcp_host,omitempty"`
	MCPPort        int             `json:"mcp_port,omitempty"`
}

type apiCCUsage struct {
	FiveHour   *ccWindow `json:"five_hour,omitempty"`
	SevenDay   *ccWindow `json:"seven_day,omitempty"`
	ExtraUsage *ccExtra  `json:"extra_usage,omitempty"`
}

type ccWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type ccExtra struct {
	Enabled      bool    `json:"is_enabled"`
	MonthlyLimit float64 `json:"monthly_limit"`
	UsedCredits  float64 `json:"used_credits"`
	Utilization  float64 `json:"utilization"`
}

// apiInvestigationTicket is the tracking ticket filed for an investigation
// (if any), embedded inline so the dashboard drawer doesn't need a second
// round-trip to resolve it.
type apiInvestigationTicket struct {
	IssueID       string `json:"issue_id"`
	IssueURL      string `json:"issue_url,omitempty"`
	Source        string `json:"source,omitempty"`
	LastStatus    string `json:"last_status,omitempty"`
	LastStateType string `json:"last_state_type,omitempty"`
}

// apiInvestigation is a row in the dashboard's Investigations tab. Findings
// carries the full findings JSON verbatim (root_cause, evidence, scope,
// non_goals, acceptance_criteria, etc.) so the drawer can render every
// section without a second fetch.
type apiInvestigation struct {
	ID             string                  `json:"id"`
	ThreadTS       string                  `json:"thread_ts,omitempty"`
	Channel        string                  `json:"channel,omitempty"`
	Title          string                  `json:"title"`
	Repo           string                  `json:"repo,omitempty"`
	Confidence     float64                 `json:"confidence"`
	Feasible       bool                    `json:"feasible"`
	SentryIssueIDs []string                `json:"sentry_issue_ids,omitempty"`
	Stale          bool                    `json:"stale,omitempty"`
	DurationMs     int64                   `json:"duration_ms,omitempty"`
	CreatedAt      int64                   `json:"created_at"`
	Findings       json.RawMessage         `json:"findings,omitempty"`
	Ticket         *apiInvestigationTicket `json:"ticket,omitempty"`
}

// apiTicket is a row in the dashboard's Tickets tab, mapped from
// state.TicketIndexEntry.
type apiTicket struct {
	Key           string `json:"key"`
	IssueID       string `json:"issue_id"`
	IssueURL      string `json:"issue_url,omitempty"`
	Source        string `json:"source,omitempty"`
	LastStatus    string `json:"last_status,omitempty"`
	LastStateType string `json:"last_state_type,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	LastSeenAt    int64  `json:"last_seen_at"`
}

// apiRangeAggregate summarizes investigation/filing volume over a fixed
// lookback window (today, 7 days, 30 days).
type apiRangeAggregate struct {
	Investigations int            `json:"investigations"`
	Filed          int            `json:"filed"`
	FiledBySource  map[string]int `json:"filed_by_source"`
}

type apiAggregates struct {
	Today apiRangeAggregate `json:"today"`
	Week  apiRangeAggregate `json:"week"`
	Month apiRangeAggregate `json:"month"`
}

// apiSeries holds the dashboard's trend sparklines. Invest/Filed series are
// derived directly from investigations/ticket_index created_at timestamps;
// Intake/QA come from the generic metrics_hourly counters (state.MetricSeries),
// which are empty until a caller starts incrementing them — an empty series
// is a valid, expected state, not an error.
type apiSeries struct {
	InvestHourly []int `json:"invest_hourly"`
	InvestDaily  []int `json:"invest_daily"`
	FiledHourly  []int `json:"filed_hourly"`
	FiledDaily   []int `json:"filed_daily"`
	IntakeHourly []int `json:"intake_hourly"`
	IntakeDaily  []int `json:"intake_daily"`
	QAHourly     []int `json:"qa_hourly"`
	QADaily      []int `json:"qa_daily"`
}

// dbQueryTimeout bounds the ad-hoc timestamp-bucketing queries the dashboard
// payload builder runs directly against the DB (below), separate from the
// state package's own dbTimeout since these can scan more rows.
const dbQueryTimeout = 5 * time.Second

// fetchTimestamps returns every created_at value from `table` no older than
// since. table is always one of a small set of literal names controlled by
// this file, never user input. Returns nil (not an error) on any query
// failure so callers degrade to empty series/aggregates.
func fetchTimestamps(db *state.DB, table string, since time.Time) []time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT created_at FROM %s WHERE created_at >= ?", table), since) //nolint:gosec // table is a fixed literal, never user input
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ticketFilingRow is a minimal projection of a ticket_index row used to
// build both the source-filed aggregates and the filed-tickets series.
type ticketFilingRow struct {
	createdAt time.Time
	source    string
}

func fetchTicketFilings(db *state.DB, since time.Time) []ticketFilingRow {
	ctx, cancel := context.WithTimeout(context.Background(), dbQueryTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, "SELECT created_at, COALESCE(source,'') FROM ticket_index WHERE created_at >= ?", since)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []ticketFilingRow
	for rows.Next() {
		var r ticketFilingRow
		if err := rows.Scan(&r.createdAt, &r.source); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// hourlySeriesFromTimestamps buckets times into the last `hours` UTC-hour
// buckets ending at now, zero-filled the same way as state.MetricSeries.
func hourlySeriesFromTimestamps(times []time.Time, hours int, now time.Time) []int {
	series := make([]int, hours)
	counts := make(map[string]int, len(times))
	for _, t := range times {
		counts[t.UTC().Format("2006-01-02T15")]++
	}
	for i := 0; i < hours; i++ {
		hour := now.UTC().Add(-time.Duration(hours-1-i) * time.Hour)
		series[i] = counts[hour.Format("2006-01-02T15")]
	}
	return series
}

// dailySeriesFromTimestamps buckets times into the last `days` UTC-day
// buckets ending at now, zero-filled.
func dailySeriesFromTimestamps(times []time.Time, days int, now time.Time) []int {
	series := make([]int, days)
	counts := make(map[string]int, len(times))
	for _, t := range times {
		counts[t.UTC().Format("2006-01-02")]++
	}
	for i := 0; i < days; i++ {
		day := now.UTC().AddDate(0, 0, -(days - 1 - i))
		series[i] = counts[day.Format("2006-01-02")]
	}
	return series
}

// buildRangeAggregate summarizes investigation/filing volume no older than
// since, from the widest-window fetches the caller already made.
func buildRangeAggregate(investTimes []time.Time, filings []ticketFilingRow, since time.Time) apiRangeAggregate {
	agg := apiRangeAggregate{FiledBySource: map[string]int{}}
	for _, t := range investTimes {
		if !t.Before(since) {
			agg.Investigations++
		}
	}
	for _, f := range filings {
		if !f.createdAt.Before(since) {
			agg.Filed++
			src := f.source
			if src == "" {
				src = "unknown"
			}
			agg.FiledBySource[src]++
		}
	}
	return agg
}

func apiDataHandler(db *state.DB, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		resp := apiResponse{Now: now.Unix()}

		daemonStats, _ := db.ReadDaemonStats()

		// Daemon status — running if heartbeat within last 30s
		daemon := &apiDaemon{Version: Version}
		if daemonStats != nil && now.Sub(daemonStats.Heartbeat) < 30*time.Second {
			daemon.Running = true
			daemon.Draining = daemonStats.Draining
			daemon.Uptime = now.Sub(daemonStats.StartedAt).Seconds()
			daemon.StartedAt = daemonStats.StartedAt.Unix()
			daemon.PID = daemonStats.PID
			if daemonStats.Version != "" {
				daemon.DaemonVersion = daemonStats.Version
			}
			daemon.Ribbits = daemonStats.Ribbits
			daemon.Triages = daemonStats.Triages
			daemon.TriageByCategory = daemonStats.TriageByCategory
			daemon.DigestEnabled = daemonStats.DigestEnabled
			daemon.DigestDryRun = daemonStats.DigestDryRun
			daemon.DigestComment = daemonStats.DigestCommentMode
			daemon.DigestBuffer = daemonStats.DigestBuffer
			if !daemonStats.DigestNextFlush.IsZero() {
				daemon.DigestNextFlush = daemonStats.DigestNextFlush.Unix()
			}
			daemon.DigestProcessed = daemonStats.DigestProcessed
			daemon.DigestOpps = daemonStats.DigestOpps
			daemon.DigestSpawns = daemonStats.DigestSpawns
			daemon.InvestigateSlots = daemonStats.InvestigateSlots
			daemon.InvestigateInFlight = daemonStats.InvestigateInFlight
			daemon.RibbitSlots = daemonStats.RibbitSlots
			daemon.RibbitInFlight = daemonStats.RibbitInFlight
		}
		if info := checkVersion(); info != nil && info.Available {
			daemon.UpdateAvailable = true
			daemon.LatestVersion = info.Latest
		}
		resp.Daemon = daemon

		// Build integrations status
		var integrations []apiIntegration

		// Digest
		digestInt := apiIntegration{Name: "Digest"}
		if daemon.Running {
			if daemon.DigestEnabled {
				if daemon.DigestDryRun && daemon.DigestComment {
					digestInt.Status = "comment"
					digestInt.Detail = "Comment mode"
				} else if daemon.DigestDryRun {
					digestInt.Status = "dry-run"
					digestInt.Detail = "Dry-run"
				} else {
					digestInt.Status = pillEnabled
					digestInt.Detail = "Enabled"
				}
			} else {
				digestInt.Status = pillDisabled
				digestInt.Detail = "Disabled"
			}
		} else {
			digestInt.Status = pillDisabled
			digestInt.Detail = "Disabled"
		}
		integrations = append(integrations, digestInt)

		// Issue Tracker — prefer live daemon state, fall back to local config
		issueInt := apiIntegration{Name: "Issue Tracker"}
		itEnabled := daemon.Running && daemonStats != nil && daemonStats.IssueTracker
		itProvider := ""
		if daemon.Running && daemonStats != nil {
			itProvider = daemonStats.IssueProvider
		}
		if !daemon.Running && cfg != nil {
			itEnabled = cfg.IssueTracker.Enabled
			itProvider = cfg.IssueTracker.Provider
		}
		if itEnabled {
			issueInt.Status = pillEnabled
			if itProvider == "" {
				itProvider = "linear"
			}
			issueInt.Detail = strings.ToUpper(itProvider[:1]) + itProvider[1:]
		} else {
			issueInt.Status = pillDisabled
			issueInt.Detail = "Disabled"
		}
		integrations = append(integrations, issueInt)

		// MCP Server — prefer live daemon state, fall back to local config
		mcpInt := apiIntegration{Name: "MCP Server"}
		mcpEnabled := daemon.Running && daemonStats != nil && daemonStats.MCPEnabled
		mcpHost, mcpPort := "", 0
		if daemon.Running && daemonStats != nil {
			mcpHost = daemonStats.MCPHost
			mcpPort = daemonStats.MCPPort
		}
		if !daemon.Running && cfg != nil {
			mcpEnabled = cfg.MCP.Enabled
			mcpHost = cfg.MCP.Host
			mcpPort = cfg.MCP.Port
		}
		if mcpEnabled {
			mcpInt.Status = pillEnabled
			mcpInt.Detail = fmt.Sprintf("%s:%d", mcpHost, mcpPort)
		} else {
			mcpInt.Status = pillDisabled
			mcpInt.Detail = "Disabled"
		}
		integrations = append(integrations, mcpInt)

		resp.Integrations = integrations

		if digestCounts, err := db.DigestOpportunityCounts(); err == nil {
			resp.DigestCounts = digestCounts
		}

		if oc, err := outcomeCounts(db); err == nil {
			resp.OutcomeCounts = oc
		}

		if cfg != nil {
			ac := &apiConfig{
				MaxConcurrent:  cfg.Limits.MaxConcurrent,
				TimeoutMinutes: cfg.Limits.TimeoutMinutes,
				DigestEnabled:  cfg.Digest.Enabled,
				DigestDryRun:   cfg.Digest.DryRun,
				DigestComment:  cfg.Digest.CommentInvestigation,
			}
			for _, rp := range cfg.Repos.List {
				ac.Repos = append(ac.Repos, apiConfigRepo{Name: rp.Name, Path: rp.Path})
			}
			if cfg.Digest.Enabled {
				ac.DigestInterval = cfg.Digest.BatchMinutes
				ac.DigestMaxSpawn = cfg.Digest.MaxAutoSpawnHour
			}
			ac.MCPEnabled = cfg.MCP.Enabled
			if cfg.MCP.Enabled {
				ac.MCPHost = cfg.MCP.Host
				ac.MCPPort = cfg.MCP.Port
			}
			resp.Config = ac
		}

		resp.CCUsage = fetchCCUsage()

		if v, _ := db.GetSetting("auto_update"); v == "1" {
			resp.AutoUpdate = true
		}
		versionMu.Lock()
		resp.AutoRestarting = autoRestarting
		resp.AutoRestartPID = autoRestartPID
		resp.AutoRestartStarted = autoRestartStarted
		versionMu.Unlock()

		// --- Investigations tab ---
		investigations, err := db.RecentInvestigations(50)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		resp.Investigations = make([]apiInvestigation, 0, len(investigations))
		for _, rec := range investigations {
			var findings investigation.Findings
			_ = json.Unmarshal([]byte(rec.FindingsJSON), &findings)

			ai := apiInvestigation{
				ID:             rec.ID,
				ThreadTS:       rec.ThreadTS,
				Channel:        rec.Channel,
				Title:          findings.Title,
				Repo:           rec.Repo,
				Confidence:     findings.Confidence,
				Feasible:       findings.Feasible,
				SentryIssueIDs: findings.SentryIssueIDs,
				Stale:          strings.Contains(findings.Reasoning, staleCaveat),
				DurationMs:     rec.DurationMs,
				CreatedAt:      rec.CreatedAt.Unix(),
				Findings:       json.RawMessage(rec.FindingsJSON),
			}
			if ticket, err := db.TicketForInvestigation(rec.ID); err == nil && ticket != nil {
				ai.Ticket = &apiInvestigationTicket{
					IssueID:       ticket.IssueID,
					IssueURL:      ticket.IssueURL,
					Source:        ticket.Source,
					LastStatus:    ticket.LastStatus,
					LastStateType: ticket.LastStateType,
				}
			}
			resp.Investigations = append(resp.Investigations, ai)
		}

		// --- Tickets tab ---
		ticketEntries, err := db.RecentTicketIndex(50)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		resp.Tickets = make([]apiTicket, 0, len(ticketEntries))
		for _, e := range ticketEntries {
			resp.Tickets = append(resp.Tickets, apiTicket{
				Key:           e.ExternalKey,
				IssueID:       e.IssueID,
				IssueURL:      e.IssueURL,
				Source:        e.Source,
				LastStatus:    e.LastStatus,
				LastStateType: e.LastStateType,
				CreatedAt:     e.CreatedAt.Unix(),
				LastSeenAt:    e.LastSeenAt.Unix(),
			})
		}

		// --- Aggregates + series (today/7d/30d) ---
		monthStart := now.AddDate(0, 0, -30)
		investTimes := fetchTimestamps(db, "investigations", monthStart)
		filings := fetchTicketFilings(db, monthStart)

		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		weekStart := now.AddDate(0, 0, -7)

		resp.Aggregates = &apiAggregates{
			Today: buildRangeAggregate(investTimes, filings, todayStart),
			Week:  buildRangeAggregate(investTimes, filings, weekStart),
			Month: buildRangeAggregate(investTimes, filings, monthStart),
		}

		filedTimes := make([]time.Time, len(filings))
		for i, f := range filings {
			filedTimes[i] = f.createdAt
		}
		resp.Series = &apiSeries{
			InvestHourly: hourlySeriesFromTimestamps(investTimes, 24, now),
			InvestDaily:  dailySeriesFromTimestamps(investTimes, 30, now),
			FiledHourly:  hourlySeriesFromTimestamps(filedTimes, 24, now),
			FiledDaily:   dailySeriesFromTimestamps(filedTimes, 30, now),
			IntakeHourly: db.MetricSeries("intake", 24, now),
			IntakeDaily:  db.MetricSeriesDaily("intake", 30, now),
			QAHourly:     db.MetricSeries("qa", 24, now),
			QADaily:      db.MetricSeriesDaily("qa", 30, now),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func apiCheckUpdateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Force refresh by clearing cache
		versionMu.Lock()
		versionCacheAt = time.Time{}
		versionMu.Unlock()

		info := checkVersion()
		resp := map[string]any{"checked": true}
		if info != nil {
			resp["available"] = info.Available
			resp["latest"] = info.Latest
			resp["current"] = info.Current
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func apiUpdateHandler() http.HandlerFunc {
	var (
		mu      sync.Mutex
		running bool
	)

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		mu.Lock()
		if running {
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"status": "running"})
			return
		}
		running = true
		mu.Unlock()

		go func() {
			defer func() {
				mu.Lock()
				running = false
				mu.Unlock()
			}()

			hasBrew := exec.Command("brew", "--version").Run() == nil //nolint:gosec
			if !hasBrew {
				slog.Warn("update: homebrew not found")
				return
			}

			if out, err := exec.Command("brew", "update").CombinedOutput(); err != nil {
				slog.Warn("update: brew update failed", "output", strings.TrimSpace(string(out)))
				return
			}

			if out, err := exec.Command("brew", "upgrade", "--cask", "scaler-tech/pkg/toad").CombinedOutput(); err != nil {
				msg := strings.TrimSpace(string(out))
				if !strings.Contains(msg, "already installed") {
					slog.Warn("update: brew upgrade failed", "output", msg)
					return
				}
			}

			// Mark version as up-to-date so the dashboard stops showing
			// "update available". The dashboard binary still has the old Version,
			// but the on-disk binary is updated — a restart will pick it up.
			versionMu.Lock()
			versionCache = &update.Info{Available: false}
			versionCacheAt = time.Now()
			versionMu.Unlock()
		}()

		json.NewEncoder(w).Encode(map[string]any{"status": "started"})
	}
}

func apiRestartHandler(db *state.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		stats, err := db.ReadDaemonStats()
		if err != nil || stats == nil || time.Since(stats.Heartbeat) > 30*time.Second {
			json.NewEncoder(w).Encode(map[string]any{"error": "no running daemon found"})
			return
		}

		pid := stats.PID
		if pid <= 0 {
			json.NewEncoder(w).Encode(map[string]any{"error": "invalid daemon PID"})
			return
		}

		if err := signalRestart(pid); err != nil {
			json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("failed to signal PID %d: %v", pid, err)})
			return
		}

		json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": pid, "started_at": stats.StartedAt.Unix()})
	}
}

func apiReloadDashboardHandler(port int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})

		// Give the response time to flush, then restart the dashboard process.
		go func() {
			time.Sleep(200 * time.Millisecond)

			// Under a process supervisor, exit cleanly and let the supervisor
			// restart us with the new binary. syscall.Exec confuses supervisors
			// that track the child PID.
			if os.Getenv("SUPERVISED") != "" {
				slog.Info("dashboard reload: exiting for supervisor restart")
				os.Exit(0)
			}

			binary, err := os.Executable()
			if err != nil {
				slog.Error("dashboard reload: could not find executable", "error", err)
				return
			}
			// Ensure the new process uses the same port so the browser can reconnect
			args := []string{binary, "status", "--port", fmt.Sprintf("%d", port), "--no-browser"}
			slog.Info("reloading dashboard process", "binary", binary, "port", port)
			if err := execReplace(binary, args, os.Environ()); err != nil {
				slog.Error("dashboard reload failed", "error", err)
			}
		}()
	}
}

func apiAutoUpdateHandler(db *state.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodPost {
			enabled := r.URL.Query().Get("enabled") == "1"
			val := "0"
			if enabled {
				val = "1"
			}
			if err := db.SetSetting("auto_update", val); err != nil {
				json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"enabled": enabled})
			return
		}

		v, _ := db.GetSetting("auto_update")
		json.NewEncoder(w).Encode(map[string]any{"enabled": v == "1"})
	}
}

// autoUpdateLoop runs in the background while the dashboard is open.
// When auto-update is enabled, it checks for new versions every minute.
// ETag caching means repeat checks return 304 (free, no rate limit hit).
func autoUpdateLoop(db *state.DB) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		v, _ := db.GetSetting("auto_update")
		if v != "1" {
			continue
		}

		// Use the daemon's actual running version for the comparison,
		// not the dashboard binary version (which may be stale).
		checkVer := Version
		if stats, err := db.ReadDaemonStats(); err == nil && stats != nil && stats.Version != "" {
			checkVer = stats.Version
		}

		info, err := update.Check(checkVer)
		if err != nil || info == nil || !info.Available {
			continue
		}

		slog.Info("auto-update: new version available", "current", info.Current, "latest", info.Latest)

		// Run brew upgrade
		hasBrew := exec.Command("brew", "--version").Run() == nil //nolint:gosec // fixed binary
		if !hasBrew {
			slog.Warn("auto-update: homebrew not found, skipping")
			continue
		}

		if out, err := exec.Command("brew", "update").CombinedOutput(); err != nil {
			slog.Warn("auto-update: brew update failed", "output", strings.TrimSpace(string(out)))
			continue
		}

		if out, err := exec.Command("brew", "upgrade", "--cask", "scaler-tech/pkg/toad").CombinedOutput(); err != nil {
			msg := strings.TrimSpace(string(out))
			if !strings.Contains(msg, "already installed") {
				slog.Warn("auto-update: brew upgrade failed", "output", msg)
				continue
			}
		}

		slog.Info("auto-update: update installed, sending restart signal")

		// Mark as up-to-date so we don't re-trigger on next tick
		versionMu.Lock()
		versionCache = &update.Info{Available: false}
		versionCacheAt = time.Now()
		versionMu.Unlock()

		// Signal daemon to restart (skip if already draining)
		stats, err := db.ReadDaemonStats()
		if err != nil || stats == nil || time.Since(stats.Heartbeat) > 30*time.Second {
			slog.Warn("auto-update: daemon not running, skipping restart")
			continue
		}
		if stats.Draining {
			slog.Info("auto-update: daemon already restarting, skipping signal")
			continue
		}
		if stats.PID > 0 {
			versionMu.Lock()
			autoRestarting = true
			autoRestartPID = stats.PID
			autoRestartStarted = stats.StartedAt.Unix()
			versionMu.Unlock()
			if err := signalRestart(stats.PID); err != nil {
				slog.Warn("auto-update: failed to signal daemon", "pid", stats.PID, "error", err)
				versionMu.Lock()
				autoRestarting = false
				versionMu.Unlock()
			} else {
				slog.Info("auto-update: restart signal sent", "pid", stats.PID)
			}
		}
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	if err := cmd.Start(); err != nil {
		slog.Debug("could not open browser", "error", err)
	}
}

var (
	ccUsageCache   *apiCCUsage
	ccUsageCacheAt time.Time
	ccUsageMu      sync.Mutex
)

var (
	versionCache       *update.Info
	versionCacheAt     time.Time
	versionMu          sync.Mutex
	autoRestarting     bool  // set by autoUpdateLoop when restart is triggered
	autoRestartPID     int   // daemon PID before restart signal was sent
	autoRestartStarted int64 // daemon started_at before restart signal was sent
)

func checkVersion() *update.Info {
	versionMu.Lock()
	defer versionMu.Unlock()

	if time.Since(versionCacheAt) < 30*time.Minute {
		return versionCache
	}
	defer func() { versionCacheAt = time.Now() }()

	info, err := update.Check(Version)
	if err != nil || info == nil {
		versionCache = nil
		return nil
	}
	versionCache = info
	return info
}

func fetchCCUsage() *apiCCUsage {
	ccUsageMu.Lock()
	defer ccUsageMu.Unlock()

	if time.Since(ccUsageCacheAt) < 60*time.Second {
		return ccUsageCache
	}

	defer func() { ccUsageCacheAt = time.Now() }()
	ccUsageCache = nil

	token := resolveCCToken()
	if token == "" {
		return nil
	}

	req, err := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	var usage apiCCUsage
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		return nil
	}

	ccUsageCache = &usage
	return ccUsageCache
}

func resolveCCToken() string {
	if t := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); t != "" {
		return t
	}

	home, err := os.UserHomeDir()
	if err == nil {
		data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
		if err == nil {
			if t := extractOAuthToken(data); t != "" {
				return t
			}
		}
	}

	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
		if err == nil {
			if t := extractOAuthToken(out); t != "" {
				return t
			}
		}
	}

	return ""
}

func extractOAuthToken(data []byte) string {
	var creds map[string]json.RawMessage
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	raw, ok := creds["claudeAiOauth"]
	if !ok {
		return ""
	}
	var oauth struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &oauth); err != nil {
		return ""
	}
	return oauth.AccessToken
}

// --- Dev mode API handlers ---

func devLogPath(cfg *config.Config) string {
	if cfg != nil && cfg.Log.File != "" {
		return cfg.Log.File
	}
	home, err := toadpath.Home()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "toad.log")
}

func devDBPath() string {
	home, err := toadpath.Home()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "state.db")
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return info.Size()
}

func apiDevInfoHandler(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		logPath := devLogPath(cfg)
		dbPath := devDBPath()
		json.NewEncoder(w).Encode(map[string]any{
			"log_path":  logPath,
			"log_size":  fileSize(logPath),
			"db_path":   dbPath,
			"db_size":   fileSize(dbPath),
			"toad_home": func() string { h, _ := toadpath.Home(); return h }(),
		})
	}
}

// serveFileDownload reads a file into memory and serves it as a download with
// explicit Content-Length so the browser can finalize the download cleanly.
// http.ServeFile keeps connections alive which can cause downloads to hang.
func serveFileDownload(w http.ResponseWriter, filename, contentType, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Header().Set("Connection", "close")
	w.Write(data)
}

func apiDevDownloadLogHandler(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := devLogPath(cfg)
		if path == "" {
			http.Error(w, "log file not configured", 404)
			return
		}
		serveFileDownload(w, "toad.log", "text/plain", path)
	}
}

func apiDevDownloadDBHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := devDBPath()
		if path == "" {
			http.Error(w, "db path not found", 404)
			return
		}
		serveFileDownload(w, "state.db", "application/octet-stream", path)
	}
}

func apiDevResetLogHandler(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		path := devLogPath(cfg)
		if path == "" {
			json.NewEncoder(w).Encode(map[string]any{"error": "log file not configured"})
			return
		}
		if err := os.Truncate(path, 0); err != nil {
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func apiDevResetDBHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		path := devDBPath()
		if path == "" {
			json.NewEncoder(w).Encode(map[string]any{"error": "db path not found"})
			return
		}
		// Remove the DB and WAL/SHM files
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(path + suffix)
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}
