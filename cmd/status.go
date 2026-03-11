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
	"github.com/scaler-tech/toad/internal/personality"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/toadpath"
	"github.com/scaler-tech/toad/internal/update"
	"github.com/scaler-tech/toad/internal/vcs"
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

	// Initialize personality manager for API endpoints
	var personalityMgr *personality.Manager
	if cfg != nil && cfg.Personality.Enabled {
		pfPath := cfg.Personality.FilePath
		if pfPath == "" {
			home, herr := toadpath.Home()
			if herr == nil {
				pfPath = filepath.Join(home, "personality.yaml")
			}
		}
		if pfPath != "" {
			if pf, perr := personality.LoadFile(pfPath); perr == nil {
				if mgr, merr := personality.NewPersistentManager(db, pf.Traits); merr == nil {
					mgr.SetLearning(cfg.Personality.LearningEnabled)
					personalityMgr = mgr
				}
			}
		}
	}
	if personalityMgr == nil {
		personalityMgr = personality.NewManager(personality.DefaultTraits())
		personalityMgr.SetLearning(false)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	})
	mux.HandleFunc("/kiosk", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(kioskHTML))
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
	mux.HandleFunc("/api/personality", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			_ = personalityMgr.Reload() // re-read from DB (daemon writes via WAL)
			eff := personalityMgr.Effective()
			base := personalityMgr.Base()
			recent, _ := personalityMgr.RecentAdjustments(20)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"effective":   eff,
				"base":        base,
				"adjustments": recent,
				"learning":    personalityMgr.LearningEnabled(),
			})
		case "POST":
			var req struct {
				Trait string  `json:"trait"`
				Value float64 `json:"value"`
				Note  string  `json:"note"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid request body", 400)
				return
			}
			if req.Value < 0.0 || req.Value > 1.0 {
				http.Error(w, "value must be between 0.0 and 1.0", 400)
				return
			}
			if req.Trait == "" {
				http.Error(w, "trait is required", 400)
				return
			}
			if err := personalityMgr.ManualAdjust(req.Trait, req.Value, req.Note); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			w.WriteHeader(200)
		}
	})
	mux.HandleFunc("/api/personality/export", func(w http.ResponseWriter, r *http.Request) {
		pf, err := personalityMgr.Export("exported", "Exported from dashboard")
		if err != nil {
			http.Error(w, "export failed", 500)
			return
		}
		data, err := pf.Marshal()
		if err != nil {
			http.Error(w, "marshal failed", 500)
			return
		}
		w.Header().Set("Content-Type", "application/x-yaml")
		w.Header().Set("Content-Disposition", "attachment; filename=personality.yaml")
		w.Write(data)
	})
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

func apiDataHandler(db *state.DB, cfg *config.Config) http.HandlerFunc {
	// Resolve PR noun once at construction time.
	prNoun := "PR"
	if cfg != nil {
		primaryRepo := config.PrimaryRepo(cfg.Repos.List)
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

		// VCS Reviewer
		reviewerInt := apiIntegration{Name: resp.PRNoun + " Reviewer"}
		if daemon.Running {
			reviewerInt.Status = pillEnabled
			watchCount := len(watches)
			if watchCount > 0 {
				reviewerInt.Detail = fmt.Sprintf("Active (%d watching)", watchCount)
			} else {
				reviewerInt.Detail = "Active"
			}
		} else {
			reviewerInt.Status = pillDisabled
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
			for _, r := range cfg.Repos.List {
				ac.Repos = append(ac.Repos, apiConfigRepo{Name: r.Name, Path: r.Path})
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
