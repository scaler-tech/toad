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
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/update"
	"github.com/scaler-tech/toad/internal/vcs"
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
	mux.HandleFunc("/api/check-update", apiCheckUpdateHandler())
	mux.HandleFunc("/api/update", apiUpdateHandler())
	mux.HandleFunc("/api/restart", apiRestartHandler(db))
	mux.HandleFunc("/api/auto-update", apiAutoUpdateHandler(db))
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
	openBrowser(url)

	fmt.Println("Press Ctrl+C to stop")
	return http.Serve(ln, mux)
}

type apiResponse struct {
	Daemon             *apiDaemon          `json:"daemon"`
	Integrations       []apiIntegration    `json:"integrations"`
	Stats              *state.Stats        `json:"stats"`
	MergeStats         *apiMergeStats      `json:"merge_stats,omitempty"`
	Active             []apiRun            `json:"active"`
	History            []apiRun            `json:"history"`
	Watches            []apiWatch          `json:"watches"`
	Opportunities      []apiOpportunity    `json:"opportunities"`
	DigestCounts       *state.DigestCounts `json:"digest_counts,omitempty"`
	Config             *apiConfig          `json:"config,omitempty"`
	CCUsage            *apiCCUsage         `json:"cc_usage,omitempty"`
	PRNoun             string              `json:"pr_noun"`
	AutoUpdate         bool                `json:"auto_update"`
	AutoRestarting     bool                `json:"auto_restarting,omitempty"`
	AutoRestartPID     int                 `json:"auto_restart_pid,omitempty"`
	AutoRestartStarted int64               `json:"auto_restart_started,omitempty"`
	Now                int64               `json:"now"`
}

type apiMergeStats struct {
	PRsCreated int     `json:"prs_created"`
	PRsMerged  int     `json:"prs_merged"`
	PRsClosed  int     `json:"prs_closed"`
	PRsOpen    int     `json:"prs_open"`
	MergeRate  float64 `json:"merge_rate"` // -1 if no completed PRs
}

type apiOpportunity struct {
	Summary       string  `json:"summary"`
	Category      string  `json:"category"`
	Confidence    float64 `json:"confidence"`
	EstSize       string  `json:"est_size"`
	Channel       string  `json:"channel"`
	ChannelID     string  `json:"channel_id,omitempty"`
	ThreadTS      string  `json:"thread_ts,omitempty"`
	DryRun        bool    `json:"dry_run"`
	Dismissed     bool    `json:"dismissed"`
	Investigating bool    `json:"investigating"`
	Reasoning     string  `json:"reasoning,omitempty"`
	CreatedAt     int64   `json:"created_at"`
}

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
	DigestBuffer     int              `json:"digest_buffer"`
	DigestNextFlush  int64            `json:"digest_next_flush,omitempty"`
	DigestProcessed  int64            `json:"digest_processed"`
	DigestOpps       int64            `json:"digest_opportunities"`
	DigestSpawns     int64            `json:"digest_spawns"`
	UpdateAvailable  bool             `json:"update_available,omitempty"`
	LatestVersion    string           `json:"latest_version,omitempty"`
}

type apiRun struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	Branch       string  `json:"branch"`
	Task         string  `json:"task"`
	RepoName     string  `json:"repo_name,omitempty"`
	StartedAt    int64   `json:"started_at"`
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

type apiConfigRepo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type apiConfig struct {
	Repos          []apiConfigRepo `json:"repos"`
	MaxConcurrent  int             `json:"max_concurrent"`
	MaxRetries     int             `json:"max_retries"`
	TimeoutMinutes int             `json:"timeout_minutes"`
	DigestEnabled  bool            `json:"digest_enabled"`
	DigestDryRun   bool            `json:"digest_dry_run"`
	DigestInterval int             `json:"digest_interval_min,omitempty"`
	DigestMaxSpawn int             `json:"digest_max_spawn_hour,omitempty"`
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
	// Resolve PR noun once at construction time.
	prNoun := "PR"
	if cfg != nil {
		primaryRepo := config.PrimaryRepo(cfg.Repos)
		resolved := config.ResolvedVCS(primaryRepo, cfg.VCS)
		if p, err := vcs.NewProvider(vcs.ProviderConfig{Platform: resolved.Platform}); err == nil {
			prNoun = p.PRNoun()
		}
	}

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

		watches, err := db.OpenPRWatches(100, 100)
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
			daemon.DigestBuffer = daemonStats.DigestBuffer
			if !daemonStats.DigestNextFlush.IsZero() {
				daemon.DigestNextFlush = daemonStats.DigestNextFlush.Unix()
			}
			daemon.DigestProcessed = daemonStats.DigestProcessed
			daemon.DigestOpps = daemonStats.DigestOpps
			daemon.DigestSpawns = daemonStats.DigestSpawns
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
				if daemon.DigestDryRun {
					digestInt.Status = "dry-run"
					digestInt.Detail = "Dry-run"
				} else {
					digestInt.Status = "enabled"
					digestInt.Detail = "Enabled"
				}
			} else {
				digestInt.Status = "disabled"
				digestInt.Detail = "Disabled"
			}
		} else {
			digestInt.Status = "disabled"
			digestInt.Detail = "Disabled"
		}
		integrations = append(integrations, digestInt)

		// Issue Tracker
		issueInt := apiIntegration{Name: "Issue Tracker"}
		if cfg != nil && cfg.IssueTracker.Enabled {
			issueInt.Status = "enabled"
			provider := cfg.IssueTracker.Provider
			if provider == "" {
				provider = "Linear"
			} else {
				// Capitalize first letter
				issueInt.Detail = strings.ToUpper(provider[:1]) + provider[1:]
				provider = issueInt.Detail
			}
			issueInt.Detail = provider
		} else {
			issueInt.Status = "disabled"
			issueInt.Detail = "Disabled"
		}
		integrations = append(integrations, issueInt)

		// VCS Reviewer
		reviewerInt := apiIntegration{Name: resp.PRNoun + " Reviewer"}
		if daemon.Running {
			reviewerInt.Status = "enabled"
			watchCount := len(watches)
			if watchCount > 0 {
				reviewerInt.Detail = fmt.Sprintf("Active (%d watching)", watchCount)
			} else {
				reviewerInt.Detail = "Active"
			}
		} else {
			reviewerInt.Status = "disabled"
			reviewerInt.Detail = "Inactive"
		}
		integrations = append(integrations, reviewerInt)

		resp.Integrations = integrations

		for _, r := range activeRuns {
			resp.Active = append(resp.Active, apiRun{
				ID:        r.ID,
				Status:    r.Status,
				Branch:    r.Branch,
				Task:      r.Task,
				RepoName:  r.RepoName,
				StartedAt: r.StartedAt.Unix(),
			})
		}

		for _, r := range historyRuns {
			hr := apiRun{
				ID:        r.ID,
				Status:    r.Status,
				Branch:    r.Branch,
				Task:      r.Task,
				RepoName:  r.RepoName,
				StartedAt: r.StartedAt.Unix(),
			}
			if r.Result != nil {
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
				Summary:       o.Summary,
				Category:      o.Category,
				Confidence:    o.Confidence,
				EstSize:       o.EstSize,
				Channel:       o.Channel,
				ChannelID:     o.ChannelID,
				ThreadTS:      o.ThreadTS,
				DryRun:        o.DryRun,
				Dismissed:     o.Dismissed,
				Investigating: o.Investigating,
				Reasoning:     o.Reasoning,
				CreatedAt:     o.CreatedAt.Unix(),
			})
		}

		if digestCounts, err := db.DigestOpportunityCounts(); err == nil {
			resp.DigestCounts = digestCounts
		}

		if ms, err := db.MergeStats(); err == nil {
			resp.MergeStats = &apiMergeStats{
				PRsCreated: ms.PRsCreated,
				PRsMerged:  ms.PRsMerged,
				PRsClosed:  ms.PRsClosed,
				PRsOpen:    ms.PRsOpen,
				MergeRate:  ms.MergeRate(),
			}
		}

		if cfg != nil {
			ac := &apiConfig{
				MaxConcurrent:  cfg.Limits.MaxConcurrent,
				MaxRetries:     cfg.Limits.MaxRetries,
				TimeoutMinutes: cfg.Limits.TimeoutMinutes,
				DigestEnabled:  cfg.Digest.Enabled,
				DigestDryRun:   cfg.Digest.DryRun,
			}
			for _, r := range cfg.Repos {
				ac.Repos = append(ac.Repos, apiConfigRepo{Name: r.Name, Path: r.Path})
			}
			if cfg.Digest.Enabled {
				ac.DigestInterval = cfg.Digest.BatchMinutes
				ac.DigestMaxSpawn = cfg.Digest.MaxAutoSpawnHour
			}
			resp.Config = ac
		}

		resp.CCUsage = fetchCCUsage()

		resp.PRNoun = prNoun
		if v, _ := db.GetSetting("auto_update"); v == "1" {
			resp.AutoUpdate = true
		}
		versionMu.Lock()
		resp.AutoRestarting = autoRestarting
		resp.AutoRestartPID = autoRestartPID
		resp.AutoRestartStarted = autoRestartStarted
		versionMu.Unlock()

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

		// Give the response time to flush, then restart the dashboard process
		go func() {
			time.Sleep(200 * time.Millisecond)
			binary, err := os.Executable()
			if err != nil {
				slog.Error("dashboard reload: could not find executable", "error", err)
				return
			}
			// Ensure the new process uses the same port so the browser can reconnect
			args := []string{binary, "status", "--port", fmt.Sprintf("%d", port)}
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
    --accent: #58a6ff;
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
  .update-badge {
    display: none; font-size: 12px; font-weight: 500;
    padding: 3px 10px; border-radius: 12px;
    background: rgba(255,193,7,0.15); color: var(--amber);
  }
  .action-btn {
    font-size: 12px; font-weight: 500;
    padding: 4px 12px; border-radius: 6px;
    border: 1px solid var(--border); background: var(--surface);
    color: var(--text); cursor: pointer;
    transition: background 0.15s, border-color 0.15s;
  }
  .action-btn:hover { border-color: var(--accent); background: rgba(88,166,255,0.1); }
  .action-btn:disabled { opacity: 0.5; cursor: not-allowed; }
  .action-btn.danger { border-color: var(--amber); color: var(--amber); }
  .action-btn.danger:hover { background: rgba(255,193,7,0.1); }
  .icon-btn {
    display: inline-flex; align-items: center; justify-content: center;
    width: 30px; height: 30px; padding: 0; border-radius: 6px;
    border: 1px solid var(--border); background: var(--surface);
    color: var(--muted); cursor: pointer;
    transition: background 0.15s, border-color 0.15s, color 0.15s;
    position: relative;
  }
  .icon-btn:hover { border-color: var(--accent); background: rgba(88,166,255,0.1); color: var(--text); }
  .icon-btn:disabled { opacity: 0.5; cursor: not-allowed; }
  .icon-btn.danger { border-color: var(--amber); color: var(--amber); }
  .icon-btn.danger:hover { background: rgba(255,193,7,0.1); }
  .icon-btn svg { width: 14px; height: 14px; fill: none; stroke: currentColor; stroke-width: 2; stroke-linecap: round; stroke-linejoin: round; }
  .icon-btn .tooltip {
    position: absolute; bottom: calc(100% + 6px); left: 50%; transform: translateX(-50%);
    background: var(--surface); border: 1px solid var(--border); border-radius: 4px;
    padding: 4px 8px; font-size: 11px; color: var(--text); white-space: nowrap;
    opacity: 0; pointer-events: none; transition: opacity 0.15s;
  }
  .icon-btn:hover .tooltip { opacity: 1; }
  @keyframes spin { to { transform: rotate(360deg); } }
  .icon-btn.spinning svg { animation: spin 1s linear infinite; }
  .toggle-wrap {
    display: inline-flex; align-items: center; gap: 8px;
    cursor: pointer; user-select: none;
  }
  .toggle-track {
    position: relative; width: 32px; height: 18px;
    background: #3a3a3e; border-radius: 9px;
    transition: background 0.15s;
  }
  .toggle-thumb {
    position: absolute; top: 2px; left: 2px;
    width: 14px; height: 14px; border-radius: 50%;
    background: #8a8a8f; transition: transform 0.15s, background 0.15s;
  }
  .toggle-wrap.active .toggle-track { background: rgba(76,175,80,0.25); }
  .toggle-wrap.active .toggle-thumb { transform: translateX(14px); background: var(--green); }
  .toggle-wrap .toggle-text { font-size: 12px; font-weight: 500; color: #8a8a8f; transition: color 0.15s; }
  .toggle-wrap.active .toggle-text { color: var(--green); }
  .toggle-wrap:hover .toggle-text { color: #aaa; }
  .toggle-wrap.active:hover .toggle-text { color: var(--green); }
  .version-mismatch {
    font-size: 11px; color: var(--amber);
    padding: 2px 8px; border-radius: 10px;
    background: rgba(255,193,7,0.1);
  }
  /* Modal overlay */
  .modal-overlay {
    display: none; position: fixed; inset: 0;
    background: rgba(0,0,0,0.6); z-index: 100;
    justify-content: center; align-items: center;
  }
  .modal-overlay.visible { display: flex; }
  .modal {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 12px; padding: 24px; min-width: 420px; max-width: 560px;
  }
  .modal h3 { font-size: 16px; font-weight: 600; margin-bottom: 16px; }
  .modal .modal-footer { text-align: right; margin-top: 16px; }
  .modal .modal-footer .action-btn { margin-left: 8px; }
  /* Restart checklist steps */
  .restart-steps { display: flex; flex-direction: column; gap: 10px; }
  .restart-step {
    display: flex; align-items: center; gap: 10px;
    font-size: 13px; color: #5a5a5f; transition: color 0.2s;
  }
  .restart-step.active { color: var(--text); }
  .restart-step.done { color: var(--green); }
  .restart-step.error { color: var(--red); }
  .restart-step-icon {
    width: 18px; height: 18px; display: flex;
    align-items: center; justify-content: center; flex-shrink: 0;
  }
  .restart-step-icon .dot {
    width: 6px; height: 6px; border-radius: 50%; background: #3a3a3e;
  }
  .restart-step.active .dot {
    background: var(--green);
    box-shadow: 0 0 6px rgba(76,175,80,0.5);
    animation: stepPulse 1s ease-in-out infinite;
  }
  .restart-step.done .dot, .restart-step.error .dot { display: none; }
  .restart-step-icon .check { display: none; font-size: 13px; }
  .restart-step.done .check { display: inline; color: var(--green); }
  .restart-step.error .check { display: inline; color: var(--red); }
  .restart-step .step-detail {
    font-size: 11px; color: var(--dim); margin-left: auto;
    font-family: monospace; white-space: nowrap;
  }
  @keyframes stepPulse {
    0%,100% { opacity: 1; transform: scale(1); }
    50% { opacity: 0.5; transform: scale(1.3); }
  }
  @keyframes pulse { 0%,100%{opacity:1} 50%{opacity:0.3} }

  /* Integrations row */
  .integrations {
    display: flex;
    gap: 8px;
    margin-bottom: 16px;
    flex-wrap: wrap;
  }
  .integration-pill {
    display: inline-flex; align-items: center; gap: 5px;
    font-size: 12px; font-weight: 500;
    padding: 3px 10px; border-radius: 12px;
    background: var(--surface); border: 1px solid var(--border);
  }
  .integration-pill .dot {
    width: 6px; height: 6px; border-radius: 50%;
  }
  .integration-pill.enabled .dot { background: var(--green); }
  .integration-pill.enabled { color: var(--text); }
  .integration-pill.disabled .dot { background: var(--dim); }
  .integration-pill.disabled { color: var(--dim); }
  .integration-pill.dry-run .dot { background: var(--amber); }
  .integration-pill.dry-run { color: var(--amber); }

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
    <span class="update-badge" id="update-badge"></span>
    <span id="version-mismatch" class="version-mismatch" style="display:none"></span>
    <div class="toggle-wrap" id="auto-update-toggle" onclick="toggleAutoUpdate()" title="Automatically update and restart when new versions are available">
      <span class="toggle-track"><span class="toggle-thumb"></span></span>
      <span class="toggle-text">Auto update</span>
    </div>
    <button class="icon-btn" id="btn-check-update" onclick="checkForUpdate()">
      <svg viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8"/><polyline points="21 3 21 8 16 8"/></svg>
      <span class="tooltip">Check for updates</span>
    </button>
    <button class="action-btn" id="btn-update" onclick="doUpdate()" style="display:none" title="Download and install latest version">Update</button>
    <button class="icon-btn danger" id="btn-restart" onclick="doRestart()">
      <svg viewBox="0 0 24 24"><polyline points="1 4 1 10 7 10"/><path d="M3.51 15a9 9 0 1 0 2.13-9.36L1 10"/></svg>
      <span class="tooltip">Restart daemon</span>
    </button>
    <span class="daemon-badge offline" id="daemon-badge">
      <span class="indicator"></span> <span id="daemon-text">Offline</span>
    </span>
    <span class="refresh-info" id="last-refresh"></span>
  </div>
</header>

<div id="error-bar" class="error-bar"></div>

<div class="integrations" id="integrations-row"></div>

<!-- Stats cards -->
<div class="stats" id="stats-row">
  <div class="stat-card"><div class="label">Tadpole Runs</div><div class="value" id="s-total">-</div><div class="sub" id="s-breakdown"></div></div>
  <div class="stat-card"><div class="label">Merge Rate</div><div class="value" id="s-merge">-</div><div class="sub" id="s-merge-sub"></div></div>
  <div class="stat-card"><div class="label">Avg Duration</div><div class="value" id="s-dur">-</div><div class="sub">&nbsp;</div></div>
  <div class="stat-card"><div class="label">Ribbits</div><div class="value" id="s-ribbits">-</div><div class="sub" id="s-ribbits-sub"></div></div>
  <div class="stat-card"><div class="label">Toad King</div><div class="value" id="s-king">-</div><div class="sub" id="s-king-sub">&nbsp;</div></div>
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
      <div class="info-row"><span class="lbl">Approved / Dismissed</span><span class="val" id="d-approved">-</span></div>
      <div class="info-row"><span class="lbl">Auto-spawns</span><span class="val" id="d-spawns">-</span></div>
    </div>
  </section>
</div>

<!-- Digest Opportunities -->
<section id="opportunities-section" style="display:none">
  <h2>Digest Opportunities <span class="count" id="opps-count">0</span></h2>
  <div class="table-wrap" id="opps-wrap"></div>
</section>

<!-- VCS Watches -->
<section id="watches-section" style="display:none">
  <h2><span id="watches-noun">PR</span> Watches <span class="count" id="watches-count">0</span></h2>
  <div class="table-wrap" id="watches-wrap"></div>
</section>

<!-- Config -->
<section id="config-section" style="display:none">
  <h2 class="section-header collapsed" id="config-header" onclick="toggleConfig()">Configuration <span class="chevron">&#x25BC;</span></h2>
  <div class="info-panel" id="config-panel" style="display:none"></div>
</section>

<div class="modal-overlay" id="restart-modal">
  <div class="modal">
    <h3 id="modal-title">Restarting toad</h3>
    <div class="restart-steps" id="restart-steps"></div>
    <div class="modal-footer">
      <span class="refresh-info" id="modal-elapsed"></span>
      <button class="action-btn" id="btn-force-reload" onclick="forceReloadDashboard()" style="display:none;margin-left:8px" title="Force the dashboard to restart on the new binary">Force reload</button>
      <button class="action-btn" onclick="hideRestartModal()" style="margin-left:8px">Close</button>
    </div>
  </div>
</div>

<script>
const MAX_VISIBLE = 5;
let historyExpanded = false;
let oppsExpanded = false;
const expandedReasons = new Set();
let configExpanded = false;

function toggleHistory() { historyExpanded = !historyExpanded; refresh(); }
function toggleOpps() { oppsExpanded = !oppsExpanded; refresh(); }
function toggleReason(id) { if (expandedReasons.has(id)) { expandedReasons.delete(id); } else { expandedReasons.add(id); } refresh(); }
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
function fmtCost(c) { return c ? '$' + c.toFixed(2) : '-'; } // kept for CC usage display
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
    const prNoun = d.pr_noun || 'PR';

    document.getElementById('error-bar').style.display = 'none';
    document.getElementById('last-refresh').textContent = new Date().toLocaleTimeString();

    // Daemon status
    const dm = d.daemon || {};
    const badge = document.getElementById('daemon-badge');
    const dtxt = document.getElementById('daemon-text');
    const ver = dm.version ? 'v' + dm.version : '';
    if (dm.running) {
      badge.className = 'daemon-badge online';
      dtxt.textContent = 'Online \u2014 ' + fmtUptime(dm.uptime_s) + (ver ? ' \u00b7 ' + ver : '') + ' (PID ' + dm.pid + ')';
    } else {
      badge.className = 'daemon-badge offline';
      dtxt.textContent = 'Offline' + (ver ? ' \u00b7 ' + ver : '');
    }

    // Update available indicator
    const updateEl = document.getElementById('update-badge');
    const btnUpdate = document.getElementById('btn-update');
    // Sync auto-update toggle and hide manual buttons when auto-update is on
    const autoToggle = document.getElementById('auto-update-toggle');
    const autoOn = d.auto_update;
    if (autoOn) {
      autoToggle.classList.add('active');
    } else {
      autoToggle.classList.remove('active');
    }
    document.getElementById('btn-check-update').style.display = autoOn ? 'none' : '';

    if (!autoOn && dm.update_available && dm.latest_version) {
      updateEl.textContent = 'v' + dm.latest_version + ' available';
      updateEl.style.display = '';
      btnUpdate.style.display = '';
    } else {
      updateEl.style.display = 'none';
      btnUpdate.style.display = 'none';
    }

    // Version mismatch between daemon and dashboard binary
    const mismatchEl = document.getElementById('version-mismatch');
    if (dm.running && dm.daemon_version && dm.version && dm.daemon_version !== dm.version) {
      mismatchEl.textContent = 'daemon: v' + dm.daemon_version + ' / dashboard: v' + dm.version;
      mismatchEl.style.display = '';
    } else {
      mismatchEl.style.display = 'none';
    }

    // Disable restart button if daemon offline or already draining
    document.getElementById('btn-restart').disabled = !dm.running || dm.draining;

    // Auto-update triggered restart: open modal automatically
    // Use pre-restart PID/started_at from backend (captured before signal was sent)
    // to avoid race where daemon already restarted before frontend polls.
    if (d.auto_restarting && !restartModalOpen) {
      restartOrigPID = d.auto_restart_pid || dm.pid || 0;
      restartOrigStartedAt = d.auto_restart_started || dm.started_at || 0;
      showRestartModal('Auto-updating toad...');
      setStepState('signal', 'done');
      setStepState('drain', 'active');
      restartPhase = 'drain';
    }

    // Update restart modal if open
    if (restartModalOpen) {
      updateRestartModal(dm, d.active || [], now);
    }

    // Integrations pills
    const integ = d.integrations || [];
    let ihtml = '';
    for (const ig of integ) {
      ihtml += '<span class="integration-pill ' + esc(ig.status) + '">'
        + '<span class="dot"></span>' + esc(ig.name) + ': ' + esc(ig.detail)
        + '</span>';
    }
    document.getElementById('integrations-row').innerHTML = ihtml;

    // Stats cards
    const st = d.stats || {};
    document.getElementById('s-total').textContent = num(st.TotalRuns);
    const active = d.active || [];
    document.getElementById('s-breakdown').textContent =
      st.Succeeded + ' passed, ' + st.Failed + ' failed' +
      (active.length > 0 ? ', ' + active.length + ' active' : '');
    // Merge rate card
    const ms = d.merge_stats || {};
    if (ms.prs_created > 0 && ms.merge_rate >= 0) {
      document.getElementById('s-merge').textContent = Math.round(ms.merge_rate) + '%';
      document.getElementById('s-merge').style.color =
        ms.merge_rate >= 70 ? 'var(--green)' : ms.merge_rate >= 40 ? 'var(--amber)' : 'var(--red)';
      document.getElementById('s-merge-sub').textContent =
        ms.prs_merged + ' merged / ' + (ms.prs_merged + ms.prs_closed) + ' resolved'
        + (ms.prs_open > 0 ? ', ' + ms.prs_open + ' open' : '');
    } else if (ms.prs_created > 0) {
      document.getElementById('s-merge').textContent = '-';
      document.getElementById('s-merge').style.color = 'var(--dim)';
      document.getElementById('s-merge-sub').textContent = ms.prs_open + ' open, none resolved yet';
    } else {
      document.getElementById('s-merge').textContent = '-';
      document.getElementById('s-merge').style.color = 'var(--dim)';
      document.getElementById('s-merge-sub').textContent = 'no PRs yet';
    }
    document.getElementById('s-dur').textContent = fmtDuration(st.AvgDuration / 1e9);
    document.getElementById('s-ribbits').textContent = num(dm.ribbits || 0);
    document.getElementById('s-ribbits-sub').textContent =
      st.ThreadCount ? st.ThreadCount + ' thread memories' : 'this session';

    // Toad King stat card
    if (dm.digest_enabled) {
      document.getElementById('s-king').textContent = num(dm.digest_opportunities || 0);
      document.getElementById('s-king').style.color = 'var(--green)';
      document.getElementById('s-king-sub').textContent =
        dm.digest_dry_run ? 'opportunities (dry-run)' : dm.digest_spawns + ' spawned';
    } else {
      document.getElementById('s-king').textContent = 'Off';
      document.getElementById('s-king').style.color = 'var(--dim)';
      document.getElementById('s-king-sub').innerHTML = '&nbsp;';
    }

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
      let html = '<table><tr><th style="width:90px">Status</th><th style="width:22%">Branch</th><th>Task</th><th style="width:60px">Files</th><th style="width:80px">Duration</th><th style="width:22%">' + prNoun + '</th></tr>';
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
    const opps = d.opportunities || [];
    const dc = d.digest_counts || {};
    const approvedCount = dc.Approved || 0;
    const dismissedCount = dc.Dismissed || 0;
    const dryRunCount = dc.DryRun || 0;
    const investigatingCount = dc.Investigating || 0;
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
        document.getElementById('d-opps').textContent = num(approvedCount + dismissedCount + dryRunCount + investigatingCount);
        let approvedText = '<span style="color:var(--green)">' + approvedCount + '</span> / <span style="color:var(--red)">' + dismissedCount + '</span>';
        if (dryRunCount > 0) approvedText += ' <span style="color:var(--dim)">(' + dryRunCount + ' dry-run)</span>';
        if (investigatingCount > 0) approvedText += ' <span style="color:var(--amber)">(' + investigatingCount + ' investigating)</span>';
        document.getElementById('d-approved').innerHTML = approvedText;
        document.getElementById('d-spawns').textContent = num(dm.digest_spawns);
      } else {
        document.getElementById('d-status').innerHTML = '<span style="color:var(--dim)">Disabled</span>';
        document.getElementById('d-buffer').textContent = '-';
        document.getElementById('d-next').textContent = '-';
        document.getElementById('d-processed').textContent = '-';
        document.getElementById('d-opps').textContent = '-';
        document.getElementById('d-approved').textContent = '-';
        document.getElementById('d-spawns').textContent = '-';
      }
    } else {
      dsec.style.display = '';
      document.getElementById('d-status').innerHTML = '<span style="color:var(--dim)">Daemon offline</span>';
      ['d-buffer','d-next','d-processed','d-opps','d-approved','d-spawns'].forEach(id => {
        document.getElementById(id).textContent = '-';
      });
    }

    // Triage section — hide if daemon offline with no data
    const tsec = document.getElementById('triage-section');
    tsec.style.display = (dm.running || (dm.triages || 0) > 0) ? '' : 'none';

    // Digest Opportunities (limited)
    const digestEnabled = (d.config && d.config.digest_enabled) || (dm.running && dm.digest_enabled);
    const oppsSec = document.getElementById('opportunities-section');
    if (opps.length === 0 && !digestEnabled) {
      oppsSec.style.display = 'none';
    } else {
      oppsSec.style.display = '';
      document.getElementById('opps-count').textContent = opps.length;
      let ohtml = '';
      if (opps.length === 0) {
        const dryRunMode = (d.config && d.config.digest_dry_run) || (dm.running && dm.digest_dry_run);
        const modeLabel = dryRunMode ? 'dry-run' : 'live';
        ohtml = '<div class="empty">Toad King is monitoring your channels (' + modeLabel + ' mode). Opportunities will appear here as they are identified.</div>';
        document.getElementById('opps-wrap').innerHTML = ohtml;
      } else {
        ohtml = '<table><tr><th style="width:80px">When</th><th>Summary</th><th style="width:70px">Category</th><th style="width:80px">Confidence</th><th style="width:60px">Size</th><th style="width:80px">Status</th><th style="width:28px"></th></tr>';
        for (let i = 0; i < opps.length; i++) {
          const o = opps[i];
          const hidden = !oppsExpanded && i >= MAX_VISIBLE ? ' display:none;' : '';
          let obadge;
          if (o.investigating) {
            obadge = '<span class="badge badge-running"><span class="spinner"></span> investigating</span>';
          } else if (o.dismissed) {
            obadge = '<span class="badge badge-failed">dismissed</span>';
          } else if (o.dry_run) {
            obadge = '<span class="badge badge-validating">dry-run</span>';
          } else {
            obadge = '<span class="badge badge-done">spawned</span>';
          }
          const dimStyle = o.dismissed ? 'opacity:0.55;' : '';
          const rowStyle = (hidden || dimStyle) ? ' style="' + hidden + dimStyle + '"' : '';
          let reasonTip = '';
          if (o.reasoning) {
            const full = esc(o.reasoning);
            if (full.length <= 120) {
              reasonTip = '<br><span style="color:var(--dim);font-size:11px">' + full + '</span>';
            } else if (expandedReasons.has(i)) {
              reasonTip = '<br><span style="color:var(--dim);font-size:11px">' + full + ' <a href="#" onclick="event.preventDefault();toggleReason(' + i + ')" style="color:var(--accent)">less</a></span>';
            } else {
              reasonTip = '<br><span style="color:var(--dim);font-size:11px">' + full.substring(0, 120) + '… <a href="#" onclick="event.preventDefault();toggleReason(' + i + ')" style="color:var(--accent)">more</a></span>';
            }
          }
          let slackLink = '';
          if (o.channel_id && o.thread_ts) {
            const ts = o.thread_ts.replace('.', '');
            const url = 'https://slack.com/archives/' + o.channel_id + '/p' + ts;
            slackLink = '<a href="' + url + '" target="_blank" rel="noopener" title="View in Slack" style="color:var(--muted);text-decoration:none;opacity:0.6;transition:opacity 0.15s" onmouseover="this.style.opacity=1" onmouseout="this.style.opacity=0.6">'
              + '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="vertical-align:middle"><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/></svg>'
              + '</a>';
          }
          ohtml += '<tr' + rowStyle + '><td>' + relTimeAgo(o.created_at, now) + '</td>'
            + '<td style="white-space:normal">' + esc(o.summary) + reasonTip + '</td>'
            + '<td>' + esc(o.category) + '</td>'
            + '<td class="mono">' + (o.confidence * 100).toFixed(0) + '%</td>'
            + '<td>' + esc(o.est_size) + '</td>'
            + '<td>' + obadge + '</td>'
            + '<td style="text-align:center">' + slackLink + '</td></tr>';
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
    }

    // VCS Watches
    const watches = d.watches || [];
    const wsec = document.getElementById('watches-section');
    document.getElementById('watches-noun').textContent = prNoun;
    if (watches.length === 0) {
      wsec.style.display = 'none';
    } else {
      wsec.style.display = '';
      document.getElementById('watches-count').textContent = watches.length;
      let html = '<table><tr><th style="width:60px">' + prNoun + '</th><th style="width:30%">Branch</th><th style="width:60px">Fixes</th><th>URL</th></tr>';
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
      if (cfg.repos && cfg.repos.length > 0) {
        for (const r of cfg.repos) {
          html += '<div class="info-row"><span class="lbl">Repo: ' + esc(r.name) + '</span><span class="val mono">' + esc(r.path) + '</span></div>';
        }
      }
      html += '<div class="info-row"><span class="lbl">Max concurrent</span><span class="val">' + cfg.max_concurrent + '</span></div>';
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

let restartModalOpen = false;
let restartStartedAt = 0;
let restartOrigPID = 0;
let restartOrigStartedAt = 0;
let restartPhase = ''; // signal, drain, offline, done, error

const RESTART_STEPS = [
  { id: 'signal',  label: 'Sending restart signal' },
  { id: 'drain',   label: 'Draining in-flight work' },
  { id: 'restart', label: 'Restarting daemon' },
  { id: 'online',  label: 'Daemon back online' },
  { id: 'dash',    label: 'Reloading dashboard' },
];

function renderSteps() {
  return RESTART_STEPS.map(s =>
    '<div class="restart-step" id="rs-' + s.id + '">'
    + '<span class="restart-step-icon"><span class="dot"></span><span class="check"></span></span>'
    + '<span class="step-label">' + s.label + '</span>'
    + '<span class="step-detail" id="rsd-' + s.id + '"></span>'
    + '</div>'
  ).join('');
}

function setStepState(id, state, detail) {
  const el = document.getElementById('rs-' + id);
  if (!el) return;
  el.className = 'restart-step' + (state ? ' ' + state : '');
  const checkEl = el.querySelector('.check');
  if (state === 'done') checkEl.textContent = '\u2713';
  else if (state === 'error') checkEl.textContent = '\u2717';
  else checkEl.textContent = '';
  const detailEl = document.getElementById('rsd-' + id);
  if (detailEl) detailEl.textContent = detail || '';
}

function showRestartModal(title) {
  restartModalOpen = true;
  restartStartedAt = Date.now();
  restartPhase = 'signal';
  document.getElementById('modal-title').textContent = title;
  document.getElementById('restart-steps').innerHTML = renderSteps();
  document.getElementById('modal-elapsed').textContent = '';
  document.getElementById('btn-force-reload').style.display = 'none';
  document.getElementById('restart-modal').classList.add('visible');
  setStepState('signal', 'active');
  // Show emergency force-reload button after 15s if still stuck
  setTimeout(() => {
    if (restartModalOpen && restartPhase !== 'done') {
      document.getElementById('btn-force-reload').style.display = '';
    }
  }, 15000);
}

function hideRestartModal() {
  restartModalOpen = false;
  restartPhase = '';
  document.getElementById('restart-modal').classList.remove('visible');
  const btn = document.getElementById('btn-restart');
  btn.disabled = false;
  btn.classList.remove('spinning');
}

function updateRestartModal(dm, active, now) {
  const elapsed = Math.round((Date.now() - restartStartedAt) / 1000);
  document.getElementById('modal-elapsed').textContent = fmtDuration(elapsed) + ' elapsed';

  if (restartPhase === 'done' || restartPhase === 'error') return;

  // Phase: signal sent, waiting for draining to start
  if (restartPhase === 'signal' && dm.draining) {
    setStepState('signal', 'done');
    setStepState('drain', 'active');
    restartPhase = 'drain';
  }

  // Phase: draining — show active task count
  if (restartPhase === 'drain') {
    if (active.length > 0) {
      const tasks = active.map(r => esc(r.task || r.branch)).join(', ');
      setStepState('drain', 'active', active.length + ' task' + (active.length > 1 ? 's' : ''));
    } else {
      setStepState('drain', 'active');
    }
  }

  // Daemon went offline — draining done, now restarting binary
  if (!dm.running && (restartPhase === 'drain' || restartPhase === 'signal')) {
    setStepState('signal', 'done');
    setStepState('drain', 'done');
    setStepState('restart', 'active');
    restartPhase = 'offline';
  }

  // Timeout: daemon didn't come back after 2 minutes offline
  if (restartPhase === 'offline' && elapsed > 120) {
    setStepState('restart', 'error', 'timed out');
    document.getElementById('modal-elapsed').innerHTML =
      '<button class="action-btn" onclick="hideRestartModal()">Dismiss</button>';
    restartPhase = 'error';
    return;
  }

  // Detect restart complete: daemon is running, not draining, and started_at changed
  if (dm.running && !dm.draining && restartOrigPID) {
    const restarted = dm.pid !== restartOrigPID || (dm.started_at && dm.started_at !== restartOrigStartedAt);
    if (restarted && restartPhase !== 'signal') {
      setStepState('signal', 'done');
      setStepState('drain', 'done');
      setStepState('restart', 'done');
      setStepState('online', 'done', 'PID ' + dm.pid);
      setStepState('dash', 'active');
      restartPhase = 'done';
      // Reload dashboard process so it runs the new binary
      setTimeout(async () => {
        try {
          await fetch('/api/reload-dashboard');
        } catch (e) { /* expected — server restarts */ }
        // Wait for new dashboard to come up, then reload page
        const retryReload = setInterval(() => {
          fetch('/api/data').then(() => {
            clearInterval(retryReload);
            window.location.reload();
          }).catch(() => {});
        }, 500);
        // Give up after 10s
        setTimeout(() => clearInterval(retryReload), 10000);
      }, 500);
    }
  }
}

async function checkForUpdate() {
  const btn = document.getElementById('btn-check-update');
  const tip = btn.querySelector('.tooltip');
  btn.disabled = true;
  btn.classList.add('spinning');
  tip.textContent = 'Checking...';
  try {
    const resp = await fetch('/api/check-update');
    const d = await resp.json();
    btn.classList.remove('spinning');
    if (d.available) {
      tip.textContent = 'v' + d.latest + ' available!';
    } else {
      tip.textContent = 'Up to date';
    }
    refresh();
  } catch (e) {
    btn.classList.remove('spinning');
    tip.textContent = 'Error checking';
  }
  setTimeout(() => { btn.disabled = false; tip.textContent = 'Check for updates'; }, 3000);
}

async function doUpdate() {
  const btn = document.getElementById('btn-update');
  btn.disabled = true;
  btn.textContent = 'Updating...';
  try {
    const resp = await fetch('/api/update');
    const d = await resp.json();
    if (d.status === 'started' || d.status === 'running') {
      btn.textContent = 'Installing...';
      const poll = setInterval(async () => {
        try {
          const cr = await fetch('/api/data');
          const cd = await cr.json();
          if (!cd.daemon || !cd.daemon.update_available) {
            clearInterval(poll);
            btn.textContent = 'Updated!';
            btn.disabled = false;
            refresh();
            setTimeout(() => { btn.style.display = 'none'; }, 2000);
          }
        } catch (e) { /* retry */ }
      }, 3000);
      setTimeout(() => { clearInterval(poll); btn.disabled = false; btn.textContent = 'Update'; }, 120000);
    }
  } catch (e) {
    btn.textContent = 'Error';
    setTimeout(() => { btn.disabled = false; btn.textContent = 'Update'; }, 3000);
  }
}

async function triggerRestart(title) {
  showRestartModal(title);
  try {
    const resp = await fetch('/api/restart');
    const d = await resp.json();
    if (d.error) {
      setStepState('signal', 'error', d.error);
      restartPhase = 'error';
      document.getElementById('modal-elapsed').innerHTML =
        '<button class="action-btn" onclick="hideRestartModal()">Dismiss</button>';
      return;
    }
    restartOrigPID = d.pid || 0;
    restartOrigStartedAt = d.started_at || 0;
    setStepState('signal', 'done');
    setStepState('drain', 'active');
    restartPhase = 'drain';
  } catch (e) {
    setStepState('signal', 'error', 'failed');
    restartPhase = 'error';
    document.getElementById('modal-elapsed').innerHTML =
      '<button class="action-btn" onclick="hideRestartModal()">Dismiss</button>';
  }
}

async function doRestart() {
  const btn = document.getElementById('btn-restart');
  btn.disabled = true;
  btn.classList.add('spinning');
  await triggerRestart('Restarting toad...');
}

async function forceReloadDashboard() {
  const btn = document.getElementById('btn-force-reload');
  btn.disabled = true;
  btn.textContent = 'Reloading...';
  try {
    await fetch('/api/reload-dashboard');
  } catch (e) { /* expected — server restarts */ }
  // Poll until the new dashboard responds, then reload the page
  const poll = setInterval(() => {
    fetch('/api/data').then(() => {
      clearInterval(poll);
      window.location.reload();
    }).catch(() => {});
  }, 500);
  setTimeout(() => { clearInterval(poll); btn.disabled = false; btn.textContent = 'Force reload'; }, 15000);
}

async function toggleAutoUpdate() {
  const toggle = document.getElementById('auto-update-toggle');
  const enabling = !toggle.classList.contains('active');
  try {
    await fetch('/api/auto-update?enabled=' + (enabling ? '1' : '0'), { method: 'POST' });
    toggle.classList.toggle('active');
  } catch (e) {
    // silently fail, next refresh will sync state
  }
}

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && restartModalOpen) hideRestartModal();
});

refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>
`
