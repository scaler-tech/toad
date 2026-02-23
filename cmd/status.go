package cmd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/hergen/toad/internal/config"
	"github.com/hergen/toad/internal/state"
)

var statusPort int

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Open the toad monitoring dashboard in your browser",
	RunE:  runStatus,
}

func init() {
	statusCmd.Flags().IntVar(&statusPort, "port", 0, "port to serve dashboard on (default: random available port)")
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
	mux.HandleFunc("/api/data", apiDataHandler(db, cfg))

	addr := fmt.Sprintf("127.0.0.1:%d", statusPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	url := fmt.Sprintf("http://%s", ln.Addr().String())
	fmt.Printf("toad dashboard: %s\n", url)
	openBrowser(url)

	fmt.Println("Press Ctrl+C to stop")
	return http.Serve(ln, mux)
}

type apiResponse struct {
	Daemon        *apiDaemon       `json:"daemon"`
	Stats         *state.Stats     `json:"stats"`
	Active        []apiRun         `json:"active"`
	History       []apiRun         `json:"history"`
	Watches       []apiWatch       `json:"watches"`
	Opportunities []apiOpportunity `json:"opportunities"`
	Config        *apiConfig       `json:"config,omitempty"`
	CCUsage       *apiCCUsage      `json:"cc_usage,omitempty"`
	Now           int64            `json:"now"`
}

type apiOpportunity struct {
	Summary    string  `json:"summary"`
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
	EstSize    string  `json:"est_size"`
	Channel    string  `json:"channel"`
	DryRun     bool    `json:"dry_run"`
	Dismissed  bool    `json:"dismissed"`
	Reasoning  string  `json:"reasoning,omitempty"`
	CreatedAt  int64   `json:"created_at"`
}

type apiDaemon struct {
	Running          bool             `json:"running"`
	Uptime           float64          `json:"uptime_s,omitempty"`
	PID              int              `json:"pid,omitempty"`
	Ribbits          int64            `json:"ribbits"`
	Triages          int64            `json:"triages"`
	TriageByCategory map[string]int64 `json:"triage_by_category,omitempty"`
	DigestEnabled    bool             `json:"digest_enabled"`
	DigestDryRun     bool             `json:"digest_dry_run"`
	DigestBuffer     int              `json:"digest_buffer"`
	DigestNextFlush  int64            `json:"digest_next_flush,omitempty"`
	DigestProcessed  int64            `json:"digest_processed"`
	DigestOpps       int64            `json:"digest_opportunities"`
	DigestSpawns     int64            `json:"digest_spawns"`
}

type apiRun struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	Branch       string  `json:"branch"`
	Task         string  `json:"task"`
	StartedAt    int64   `json:"started_at"`
	Cost         float64 `json:"cost,omitempty"`
	Duration     float64 `json:"duration_s,omitempty"`
	FilesChanged int     `json:"files_changed,omitempty"`
	PRUrl        string  `json:"pr_url,omitempty"`
	Error        string  `json:"error,omitempty"`
}

type apiWatch struct {
	PRNumber int    `json:"pr_number"`
	PRURL    string `json:"pr_url"`
	Branch   string `json:"branch"`
	FixCount int    `json:"fix_count"`
}

type apiConfig struct {
	RepoPath       string  `json:"repo_path"`
	DefaultBranch  string  `json:"default_branch"`
	MaxConcurrent  int     `json:"max_concurrent"`
	MaxBudget      float64 `json:"max_budget_usd"`
	MaxRetries     int     `json:"max_retries"`
	TimeoutMinutes int     `json:"timeout_minutes"`
	DigestEnabled  bool    `json:"digest_enabled"`
	DigestInterval int     `json:"digest_interval_min,omitempty"`
	DigestMaxSpawn int     `json:"digest_max_spawn_hour,omitempty"`
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

func apiDataHandler(db *state.DB, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats, err := db.Stats()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		activeRuns, err := db.ActiveRuns()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		historyRuns, err := db.History(20)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		watches, err := db.OpenPRWatches()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		opportunities, err := db.RecentDigestOpportunities(20)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		daemonStats, _ := db.ReadDaemonStats()

		now := time.Now()
		resp := apiResponse{
			Stats: stats,
			Now:   now.Unix(),
		}

		// Daemon status — running if heartbeat within last 30s
		daemon := &apiDaemon{}
		if daemonStats != nil && now.Sub(daemonStats.Heartbeat) < 30*time.Second {
			daemon.Running = true
			daemon.Uptime = now.Sub(daemonStats.StartedAt).Seconds()
			daemon.PID = daemonStats.PID
			daemon.Ribbits = daemonStats.Ribbits
			daemon.Triages = daemonStats.Triages
			daemon.TriageByCategory = daemonStats.TriageByCategory
			daemon.DigestEnabled = daemonStats.DigestEnabled
			daemon.DigestDryRun = daemonStats.DigestDryRun
			daemon.DigestBuffer = daemonStats.DigestBuffer
			if !daemonStats.DigestNextFlush.IsZero() {
				daemon.DigestNextFlush = daemonStats.DigestNextFlush.Unix()
			}
			daemon.DigestProcessed = daemonStats.DigestProcessed
			daemon.DigestOpps = daemonStats.DigestOpps
			daemon.DigestSpawns = daemonStats.DigestSpawns
		}
		resp.Daemon = daemon

		for _, r := range activeRuns {
			resp.Active = append(resp.Active, apiRun{
				ID:        r.ID,
				Status:    r.Status,
				Branch:    r.Branch,
				Task:      r.Task,
				StartedAt: r.StartedAt.Unix(),
			})
		}

		for _, r := range historyRuns {
			hr := apiRun{
				ID:        r.ID,
				Status:    r.Status,
				Branch:    r.Branch,
				Task:      r.Task,
				StartedAt: r.StartedAt.Unix(),
			}
			if r.Result != nil {
				hr.Cost = r.Result.Cost
				hr.Duration = r.Result.Duration.Seconds()
				hr.FilesChanged = r.Result.FilesChanged
				hr.PRUrl = r.Result.PRUrl
				hr.Error = r.Result.Error
			}
			resp.History = append(resp.History, hr)
		}

		for _, pw := range watches {
			resp.Watches = append(resp.Watches, apiWatch{
				PRNumber: pw.PRNumber,
				PRURL:    pw.PRURL,
				Branch:   pw.Branch,
				FixCount: pw.FixCount,
			})
		}

		for _, o := range opportunities {
			resp.Opportunities = append(resp.Opportunities, apiOpportunity{
				Summary:    o.Summary,
				Category:   o.Category,
				Confidence: o.Confidence,
				EstSize:    o.EstSize,
				Channel:    o.Channel,
				DryRun:     o.DryRun,
				Dismissed:  o.Dismissed,
				Reasoning:  o.Reasoning,
				CreatedAt:  o.CreatedAt.Unix(),
			})
		}

		if cfg != nil {
			ac := &apiConfig{
				RepoPath:       cfg.Repo.Path,
				DefaultBranch:  cfg.Repo.DefaultBranch,
				MaxConcurrent:  cfg.Limits.MaxConcurrent,
				MaxBudget:      cfg.Limits.MaxBudgetUSD,
				MaxRetries:     cfg.Limits.MaxRetries,
				TimeoutMinutes: cfg.Limits.TimeoutMinutes,
				DigestEnabled:  cfg.Digest.Enabled,
			}
			if cfg.Digest.Enabled {
				ac.DigestInterval = cfg.Digest.BatchMinutes
				ac.DigestMaxSpawn = cfg.Digest.MaxAutoSpawnHour
			}
			resp.Config = ac
		}

		resp.CCUsage = fetchCCUsage()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
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

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>toad dashboard</title>
<style>
  :root {
    --bg: #0d1117;
    --surface: #161b22;
    --border: #30363d;
    --text: #e6edf3;
    --dim: #7d8590;
    --green: #4CAF50;
    --red: #FF5252;
    --amber: #FFC107;
    --blue: #58a6ff;
    --purple: #bc8cff;
  }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
    background: var(--bg);
    color: var(--text);
    padding: 24px;
    max-width: 1200px;
    margin: 0 auto;
  }

  /* Header */
  header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 20px;
  }
  header h1 { font-size: 20px; font-weight: 600; }
  .header-right { display: flex; align-items: center; gap: 16px; }
  .daemon-badge {
    display: inline-flex; align-items: center; gap: 6px;
    font-size: 12px; font-weight: 500;
    padding: 3px 10px; border-radius: 12px;
  }
  .daemon-badge.online { background: rgba(76,175,80,0.15); color: var(--green); }
  .daemon-badge.offline { background: rgba(255,82,82,0.15); color: var(--red); }
  .daemon-badge .indicator {
    width: 7px; height: 7px; border-radius: 50%; background: currentColor;
  }
  .daemon-badge.online .indicator { animation: pulse 2s ease-in-out infinite; }
  .refresh-info { color: var(--dim); font-size: 12px; }
  @keyframes pulse { 0%,100%{opacity:1} 50%{opacity:0.3} }

  /* Stats cards */
  .stats {
    display: grid;
    grid-template-columns: repeat(6, 1fr);
    gap: 10px;
    margin-bottom: 20px;
  }
  .stat-card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 14px 12px;
    text-align: center;
  }
  .stat-card .label {
    color: var(--dim); font-size: 11px;
    text-transform: uppercase; letter-spacing: 0.5px;
  }
  .stat-card .value {
    font-size: 26px; font-weight: 700;
    color: var(--green); margin-top: 2px;
  }
  .stat-card .sub {
    font-size: 11px; color: var(--dim); margin-top: 2px;
  }

  /* Two-column grid */
  .two-col {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 16px;
    margin-bottom: 20px;
  }

  /* Section */
  section { margin-bottom: 20px; }
  section h2 {
    font-size: 13px; font-weight: 600;
    color: var(--green);
    margin-bottom: 8px;
    display: flex; align-items: center; gap: 6px;
    text-transform: uppercase; letter-spacing: 0.5px;
  }
  section h2 .count {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 10px; padding: 1px 7px;
    font-size: 11px; color: var(--dim);
  }

  /* Tables */
  table { width: 100%; border-collapse: collapse; font-size: 13px; }
  th {
    text-align: left; color: var(--dim); font-weight: 500;
    font-size: 11px; text-transform: uppercase; letter-spacing: 0.5px;
    padding: 7px 10px; border-bottom: 1px solid var(--border);
  }
  td {
    padding: 7px 10px;
    border-bottom: 1px solid var(--border);
    vertical-align: top;
    max-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  tr:last-child td { border-bottom: none; }
  .table-wrap {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 8px; overflow: hidden;
  }

  /* Info panels */
  .info-panel {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 8px; padding: 14px 16px;
  }
  .info-row {
    display: flex; justify-content: space-between;
    padding: 4px 0; font-size: 13px;
  }
  .info-row .lbl { color: var(--dim); }
  .info-row .val { font-weight: 500; }
  .info-row .val.green { color: var(--green); }
  .info-row .val.amber { color: var(--amber); }
  .info-row .val.red { color: var(--red); }
  .info-row .val.purple { color: var(--purple); }

  /* Status badges */
  .badge {
    display: inline-flex; align-items: center; gap: 4px;
    font-size: 12px; font-weight: 500;
    padding: 2px 8px; border-radius: 12px;
  }
  .badge-running { background: rgba(255,193,7,0.15); color: var(--amber); }
  .badge-validating { background: rgba(255,193,7,0.15); color: var(--amber); }
  .badge-shipping { background: rgba(76,175,80,0.15); color: var(--green); }
  .badge-starting { background: rgba(125,133,144,0.15); color: var(--dim); }
  .badge-done { background: rgba(76,175,80,0.15); color: var(--green); }
  .badge-failed { background: rgba(255,82,82,0.15); color: var(--red); }
  .badge .spinner {
    width: 10px; height: 10px;
    border: 2px solid transparent; border-top-color: currentColor;
    border-radius: 50%; animation: spin 0.8s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }

  /* Links */
  a { color: var(--blue); text-decoration: none; }
  a:hover { text-decoration: underline; }

  /* Empty state */
  .empty { color: var(--dim); padding: 16px; text-align: center; font-size: 13px; }

  /* Error bar */
  .error-bar {
    background: rgba(255,82,82,0.1); border: 1px solid var(--red);
    border-radius: 6px; padding: 8px 12px;
    color: var(--red); font-size: 13px;
    margin-bottom: 16px; display: none;
  }

  .mono {
    font-family: "SF Mono","Fira Code","Fira Mono",Menlo,Consolas,monospace;
    font-size: 12px;
  }

  /* Toggle buttons for expand/collapse */
  .toggle-row {
    text-align: center; padding: 8px;
    cursor: pointer; color: var(--blue);
    font-size: 12px; font-weight: 500;
    border-top: 1px solid var(--border);
  }
  .toggle-row:hover { text-decoration: underline; }

  /* Collapsible config */
  .section-header {
    cursor: pointer; user-select: none;
  }
  .section-header .chevron {
    transition: transform 0.2s;
    display: inline-block; font-size: 10px;
  }
  .section-header.collapsed .chevron { transform: rotate(-90deg); }

  /* Active runs padding boost */
  #active-wrap td { padding: 9px 12px; }
  #active-wrap th { padding: 9px 12px; }

  @media (max-width: 800px) {
    .stats { grid-template-columns: repeat(3, 1fr); }
    .two-col { grid-template-columns: 1fr; }
    body { padding: 12px; }
  }
</style>
</head>
<body>

<header>
  <h1>&#x1f438; toad dashboard</h1>
  <div class="header-right">
    <span class="daemon-badge offline" id="daemon-badge">
      <span class="indicator"></span> <span id="daemon-text">Offline</span>
    </span>
    <span class="refresh-info" id="last-refresh"></span>
  </div>
</header>

<div id="error-bar" class="error-bar"></div>

<!-- Stats cards -->
<div class="stats" id="stats-row">
  <div class="stat-card"><div class="label">Tadpole Runs</div><div class="value" id="s-total">-</div><div class="sub" id="s-breakdown"></div></div>
  <div class="stat-card"><div class="label">Success Rate</div><div class="value" id="s-rate">-</div><div class="sub" id="s-rate-sub"></div></div>
  <div class="stat-card"><div class="label">Total Cost</div><div class="value" id="s-cost">-</div><div class="sub" id="s-cost-sub"></div></div>
  <div class="stat-card"><div class="label">Avg Duration</div><div class="value" id="s-dur">-</div><div class="sub">&nbsp;</div></div>
  <div class="stat-card"><div class="label">Ribbits</div><div class="value" id="s-ribbits">-</div><div class="sub" id="s-ribbits-sub"></div></div>
  <div class="stat-card"><div class="label">CC Usage</div><div class="value" id="s-cc">-</div><div class="sub" id="s-cc-sub">&nbsp;</div></div>
</div>

<!-- Active Runs -->
<section id="active-section">
  <h2>Active Runs <span class="count" id="active-count">0</span></h2>
  <div class="table-wrap" id="active-wrap"></div>
</section>

<!-- Recent History -->
<section id="history-section">
  <h2>Recent History <span class="count" id="history-count"></span></h2>
  <div class="table-wrap" id="history-wrap"></div>
</section>

<!-- Two-column: Triage + Digest -->
<div class="two-col">
  <section id="triage-section">
    <h2>Triage</h2>
    <div class="info-panel" id="triage-panel">
      <div class="info-row"><span class="lbl">Total classified</span><span class="val" id="t-total">-</span></div>
      <div class="info-row"><span class="lbl">Bug</span><span class="val amber" id="t-bug">-</span></div>
      <div class="info-row"><span class="lbl">Feature</span><span class="val green" id="t-feature">-</span></div>
      <div class="info-row"><span class="lbl">Question</span><span class="val purple" id="t-question">-</span></div>
      <div class="info-row"><span class="lbl">Other</span><span class="val" id="t-other">-</span></div>
    </div>
  </section>
  <section id="digest-section">
    <h2>Digest (Toad King)</h2>
    <div class="info-panel" id="digest-panel">
      <div class="info-row"><span class="lbl">Status</span><span class="val" id="d-status">-</span></div>
      <div class="info-row"><span class="lbl">Messages in buffer</span><span class="val" id="d-buffer">-</span></div>
      <div class="info-row"><span class="lbl">Next flush</span><span class="val" id="d-next">-</span></div>
      <div class="info-row"><span class="lbl">Messages processed</span><span class="val" id="d-processed">-</span></div>
      <div class="info-row"><span class="lbl">Opportunities found</span><span class="val" id="d-opps">-</span></div>
      <div class="info-row"><span class="lbl">Auto-spawns</span><span class="val" id="d-spawns">-</span></div>
    </div>
  </section>
</div>

<!-- Digest Opportunities -->
<section id="opportunities-section" style="display:none">
  <h2>Digest Opportunities <span class="count" id="opps-count">0</span></h2>
  <div class="table-wrap" id="opps-wrap"></div>
</section>

<!-- PR Watches -->
<section id="watches-section" style="display:none">
  <h2>PR Watches <span class="count" id="watches-count">0</span></h2>
  <div class="table-wrap" id="watches-wrap"></div>
</section>

<!-- Config -->
<section id="config-section" style="display:none">
  <h2 class="section-header collapsed" id="config-header" onclick="toggleConfig()">Configuration <span class="chevron">&#x25BC;</span></h2>
  <div class="info-panel" id="config-panel" style="display:none"></div>
</section>

<script>
const MAX_VISIBLE = 5;
let historyExpanded = false;
let oppsExpanded = false;
let configExpanded = false;

function toggleHistory() { historyExpanded = !historyExpanded; refresh(); }
function toggleOpps() { oppsExpanded = !oppsExpanded; refresh(); }
function toggleConfig() {
  configExpanded = !configExpanded;
  const panel = document.getElementById('config-panel');
  const header = document.getElementById('config-header');
  panel.style.display = configExpanded ? '' : 'none';
  header.className = 'section-header' + (configExpanded ? '' : ' collapsed');
}

function fmtDuration(seconds) {
  if (!seconds) return '-';
  const s = Math.round(seconds);
  if (s >= 3600) {
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    return h + 'h ' + String(m).padStart(2,'0') + 'm';
  }
  const m = Math.floor(s / 60);
  const sec = s % 60;
  if (m > 0) return m + 'm ' + String(sec).padStart(2,'0') + 's';
  return sec + 's';
}
function fmtUptime(seconds) {
  if (!seconds) return '-';
  const s = Math.round(seconds);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return d + 'd ' + h + 'h';
  if (h > 0) return h + 'h ' + m + 'm';
  return m + 'm';
}
function fmtCost(c) { return c ? '$' + c.toFixed(2) : '-'; }
function fmtRate(total, succeeded) {
  if (!total) return '-';
  return (succeeded / total * 100).toFixed(1) + '%';
}
function statusBadge(status, spinning) {
  const cls = 'badge badge-' + status;
  const spin = spinning ? '<span class="spinner"></span>' : '';
  const icon = status === 'done' ? '&#x2713;' : status === 'failed' ? '&#x2717;' : '';
  return '<span class="' + cls + '">' + spin + icon + ' ' + status + '</span>';
}
function shortURL(url) {
  if (!url) return '';
  try {
    const u = new URL(url);
    const path = u.pathname.replace(/^\//, '');
    const short = path.length > 40 ? path.slice(0, 37) + '...' : path;
    return '<a href="' + esc(url) + '" target="_blank">' + esc(short) + '</a>';
  } catch { return esc(url); }
}
function esc(s) {
  if (!s) return '';
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}
function relTime(unixTs, now) {
  const diff = unixTs - now;
  if (diff <= 0) return 'now';
  if (diff < 60) return Math.round(diff) + 's';
  return Math.round(diff / 60) + 'm';
}
function relTimeAgo(unixTs, now) {
  const diff = now - unixTs;
  if (diff < 60) return 'just now';
  if (diff < 3600) return Math.floor(diff / 60) + 'm ago';
  if (diff < 86400) return Math.floor(diff / 3600) + 'h ago';
  return Math.floor(diff / 86400) + 'd ago';
}
function num(n) { return n != null ? n.toLocaleString() : '-'; }

async function refresh() {
  try {
    const resp = await fetch('/api/data');
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    const d = await resp.json();
    const now = d.now;

    document.getElementById('error-bar').style.display = 'none';
    document.getElementById('last-refresh').textContent = new Date().toLocaleTimeString();

    // Daemon status
    const dm = d.daemon || {};
    const badge = document.getElementById('daemon-badge');
    const dtxt = document.getElementById('daemon-text');
    if (dm.running) {
      badge.className = 'daemon-badge online';
      dtxt.textContent = 'Online \u2014 ' + fmtUptime(dm.uptime_s) + ' (PID ' + dm.pid + ')';
    } else {
      badge.className = 'daemon-badge offline';
      dtxt.textContent = 'Offline';
    }

    // Stats cards
    const st = d.stats || {};
    document.getElementById('s-total').textContent = num(st.TotalRuns);
    const active = d.active || [];
    document.getElementById('s-breakdown').textContent =
      st.Succeeded + ' passed, ' + st.Failed + ' failed' +
      (active.length > 0 ? ', ' + active.length + ' active' : '');
    document.getElementById('s-rate').textContent = fmtRate(st.TotalRuns, st.Succeeded);
    document.getElementById('s-rate').style.color =
      st.TotalRuns > 0 && (st.Succeeded / st.TotalRuns) >= 0.8 ? 'var(--green)' : 'var(--amber)';
    document.getElementById('s-rate-sub').textContent =
      st.Succeeded + '/' + st.TotalRuns + ' succeeded';
    document.getElementById('s-cost').textContent = fmtCost(st.TotalCost);
    document.getElementById('s-cost-sub').textContent =
      st.TotalRuns > 0 ? fmtCost(st.TotalCost / st.TotalRuns) + '/run avg' : '';
    document.getElementById('s-dur').textContent = fmtDuration(st.AvgDuration / 1e9);
    document.getElementById('s-ribbits').textContent = num(dm.ribbits || 0);
    document.getElementById('s-ribbits-sub').textContent =
      st.ThreadCount ? st.ThreadCount + ' thread memories' : 'this session';

    // CC Usage card
    const cc = d.cc_usage;
    if (cc && cc.five_hour) {
      const u5 = cc.five_hour.utilization;
      const color = u5 > 80 ? 'var(--red)' : u5 > 50 ? 'var(--amber)' : 'var(--green)';
      document.getElementById('s-cc').textContent = Math.round(u5) + '%';
      document.getElementById('s-cc').style.color = color;
      let sub = '';
      if (cc.seven_day) sub += '7d: ' + Math.round(cc.seven_day.utilization) + '%';
      if (cc.extra_usage && cc.extra_usage.is_enabled) {
        if (sub) sub += ' \u00b7 ';
        sub += 'extra: ' + Math.round(cc.extra_usage.utilization) + '%';
      }
      document.getElementById('s-cc-sub').textContent = sub;
    } else {
      document.getElementById('s-cc').textContent = '-';
      document.getElementById('s-cc').style.color = 'var(--dim)';
      document.getElementById('s-cc-sub').innerHTML = '&nbsp;';
    }

    // Active runs
    document.getElementById('active-count').textContent = active.length;
    if (active.length === 0) {
      document.getElementById('active-wrap').innerHTML = '<div class="empty" style="color:var(--green)">&#x2714; All clear</div>';
    } else {
      let html = '<table><tr><th style="width:110px">Status</th><th style="width:25%">Branch</th><th>Task</th><th style="width:80px">Duration</th></tr>';
      for (const r of active) {
        const elapsed = now - r.started_at;
        html += '<tr><td>' + statusBadge(r.status, true) + '</td>'
          + '<td class="mono">' + esc(r.branch) + '</td>'
          + '<td>' + esc(r.task) + '</td>'
          + '<td class="mono">' + fmtDuration(elapsed) + '</td></tr>';
      }
      html += '</table>';
      document.getElementById('active-wrap').innerHTML = html;
    }

    // History (limited)
    const history = d.history || [];
    document.getElementById('history-count').textContent = history.length;
    if (history.length === 0) {
      document.getElementById('history-wrap').innerHTML = '<div class="empty">No completed runs</div>';
    } else {
      let html = '<table><tr><th style="width:90px">Status</th><th style="width:22%">Branch</th><th>Task</th><th style="width:60px">Files</th><th style="width:65px">Cost</th><th style="width:80px">Duration</th><th style="width:22%">PR</th></tr>';
      for (let i = 0; i < history.length; i++) {
        const r = history[i];
        const hidden = !historyExpanded && i >= MAX_VISIBLE ? ' style="display:none"' : '';
        const pr = r.status === 'done' && r.pr_url
          ? shortURL(r.pr_url)
          : '<span style="color:var(--dim)">' + esc(r.error || '-') + '</span>';
        html += '<tr' + hidden + '><td>' + statusBadge(r.status, false) + '</td>'
          + '<td class="mono">' + esc(r.branch) + '</td>'
          + '<td>' + esc(r.task) + '</td>'
          + '<td class="mono">' + (r.files_changed || '-') + '</td>'
          + '<td class="mono">' + fmtCost(r.cost) + '</td>'
          + '<td class="mono">' + fmtDuration(r.duration_s) + '</td>'
          + '<td>' + pr + '</td></tr>';
      }
      html += '</table>';
      if (history.length > MAX_VISIBLE) {
        if (historyExpanded) {
          html += '<div class="toggle-row" onclick="toggleHistory()">Show less</div>';
        } else {
          html += '<div class="toggle-row" onclick="toggleHistory()">Show all (' + history.length + ')</div>';
        }
      }
      document.getElementById('history-wrap').innerHTML = html;
    }

    // Triage panel
    const cat = dm.triage_by_category || {};
    document.getElementById('t-total').textContent = num(dm.triages || 0);
    document.getElementById('t-bug').textContent = num(cat.bug || 0);
    document.getElementById('t-feature').textContent = num(cat.feature || 0);
    document.getElementById('t-question').textContent = num(cat.question || 0);
    document.getElementById('t-other').textContent = num(cat.other || 0);

    // Digest panel
    const dsec = document.getElementById('digest-section');
    if (dm.running) {
      dsec.style.display = '';
      if (dm.digest_enabled) {
        if (dm.digest_dry_run) {
          document.getElementById('d-status').innerHTML = '<span style="color:var(--amber)">Enabled (dry-run)</span>';
        } else {
          document.getElementById('d-status').innerHTML = '<span style="color:var(--green)">Enabled</span>';
        }
        document.getElementById('d-buffer').textContent = num(dm.digest_buffer);
        document.getElementById('d-next').textContent =
          dm.digest_next_flush ? 'in ' + relTime(dm.digest_next_flush, now) : '-';
        document.getElementById('d-processed').textContent = num(dm.digest_processed);
        document.getElementById('d-opps').textContent = num(dm.digest_opportunities);
        document.getElementById('d-spawns').textContent = num(dm.digest_spawns);
      } else {
        document.getElementById('d-status').innerHTML = '<span style="color:var(--dim)">Disabled</span>';
        document.getElementById('d-buffer').textContent = '-';
        document.getElementById('d-next').textContent = '-';
        document.getElementById('d-processed').textContent = '-';
        document.getElementById('d-opps').textContent = '-';
        document.getElementById('d-spawns').textContent = '-';
      }
    } else {
      dsec.style.display = '';
      document.getElementById('d-status').innerHTML = '<span style="color:var(--dim)">Daemon offline</span>';
      ['d-buffer','d-next','d-processed','d-opps','d-spawns'].forEach(id => {
        document.getElementById(id).textContent = '-';
      });
    }

    // Triage section — hide if daemon offline with no data
    const tsec = document.getElementById('triage-section');
    tsec.style.display = (dm.running || (dm.triages || 0) > 0) ? '' : 'none';

    // Digest Opportunities (limited)
    const opps = d.opportunities || [];
    const oppsSec = document.getElementById('opportunities-section');
    if (opps.length === 0) {
      oppsSec.style.display = 'none';
    } else {
      oppsSec.style.display = '';
      document.getElementById('opps-count').textContent = opps.length;
      let ohtml = '<table><tr><th style="width:80px">When</th><th>Summary</th><th style="width:70px">Category</th><th style="width:80px">Confidence</th><th style="width:60px">Size</th><th style="width:80px">Status</th></tr>';
      for (let i = 0; i < opps.length; i++) {
        const o = opps[i];
        const hidden = !oppsExpanded && i >= MAX_VISIBLE ? ' display:none;' : '';
        let obadge;
        if (o.dismissed) {
          obadge = '<span class="badge badge-failed">dismissed</span>';
        } else if (o.dry_run) {
          obadge = '<span class="badge badge-validating">dry-run</span>';
        } else {
          obadge = '<span class="badge badge-done">spawned</span>';
        }
        const dimStyle = o.dismissed ? 'opacity:0.55;' : '';
        const rowStyle = (hidden || dimStyle) ? ' style="' + hidden + dimStyle + '"' : '';
        const reasonTip = o.reasoning ? '<br><span style="color:var(--dim);font-size:11px">' + esc(o.reasoning).substring(0, 120) + '</span>' : '';
        ohtml += '<tr' + rowStyle + '><td>' + relTimeAgo(o.created_at, now) + '</td>'
          + '<td style="white-space:normal">' + esc(o.summary) + reasonTip + '</td>'
          + '<td>' + esc(o.category) + '</td>'
          + '<td class="mono">' + (o.confidence * 100).toFixed(0) + '%</td>'
          + '<td>' + esc(o.est_size) + '</td>'
          + '<td>' + obadge + '</td></tr>';
      }
      ohtml += '</table>';
      if (opps.length > MAX_VISIBLE) {
        if (oppsExpanded) {
          ohtml += '<div class="toggle-row" onclick="toggleOpps()">Show less</div>';
        } else {
          ohtml += '<div class="toggle-row" onclick="toggleOpps()">Show all (' + opps.length + ')</div>';
        }
      }
      document.getElementById('opps-wrap').innerHTML = ohtml;
    }

    // PR Watches
    const watches = d.watches || [];
    const wsec = document.getElementById('watches-section');
    if (watches.length === 0) {
      wsec.style.display = 'none';
    } else {
      wsec.style.display = '';
      document.getElementById('watches-count').textContent = watches.length;
      let html = '<table><tr><th style="width:60px">PR</th><th style="width:30%">Branch</th><th style="width:60px">Fixes</th><th>URL</th></tr>';
      for (const w of watches) {
        html += '<tr><td class="mono">#' + w.pr_number + '</td>'
          + '<td class="mono">' + esc(w.branch) + '</td>'
          + '<td>' + w.fix_count + '/3</td>'
          + '<td>' + shortURL(w.pr_url) + '</td></tr>';
      }
      html += '</table>';
      document.getElementById('watches-wrap').innerHTML = html;
    }

    // Config (collapsed by default)
    const cfgSec = document.getElementById('config-section');
    const cfg = d.config;
    if (cfg) {
      cfgSec.style.display = '';
      let html = '';
      html += '<div class="info-row"><span class="lbl">Repo</span><span class="val mono">' + esc(cfg.repo_path) + '</span></div>';
      html += '<div class="info-row"><span class="lbl">Default branch</span><span class="val">' + esc(cfg.default_branch) + '</span></div>';
      html += '<div class="info-row"><span class="lbl">Max concurrent</span><span class="val">' + cfg.max_concurrent + '</span></div>';
      html += '<div class="info-row"><span class="lbl">Max budget</span><span class="val">' + fmtCost(cfg.max_budget_usd) + '/run</span></div>';
      html += '<div class="info-row"><span class="lbl">Max retries</span><span class="val">' + cfg.max_retries + '</span></div>';
      html += '<div class="info-row"><span class="lbl">Timeout</span><span class="val">' + cfg.timeout_minutes + 'm</span></div>';
      if (cfg.digest_enabled) {
        html += '<div class="info-row"><span class="lbl">Digest interval</span><span class="val">' + cfg.digest_interval_min + 'm</span></div>';
        html += '<div class="info-row"><span class="lbl">Digest max spawn/hr</span><span class="val">' + cfg.digest_max_spawn_hour + '</span></div>';
      }
      document.getElementById('config-panel').innerHTML = html;
      document.getElementById('config-panel').style.display = configExpanded ? '' : 'none';
      document.getElementById('config-header').className = 'section-header' + (configExpanded ? '' : ' collapsed');
    } else {
      cfgSec.style.display = 'none';
    }
  } catch (e) {
    const bar = document.getElementById('error-bar');
    bar.textContent = 'Failed to fetch data: ' + e.message;
    bar.style.display = 'block';
  }
}

refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>
`
