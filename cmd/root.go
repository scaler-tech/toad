// Package cmd implements the CLI commands for the toad daemon.
package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/digest"
	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	toadlog "github.com/scaler-tech/toad/internal/log"
	toadmcp "github.com/scaler-tech/toad/internal/mcp"
	"github.com/scaler-tech/toad/internal/preflight"
	"github.com/scaler-tech/toad/internal/ribbit"
	islack "github.com/scaler-tech/toad/internal/slack"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/ticket"
	"github.com/scaler-tech/toad/internal/toadpath"
	"github.com/scaler-tech/toad/internal/triage"
	"github.com/scaler-tech/toad/internal/tui"
	"github.com/scaler-tech/toad/internal/update"
)

// daemonCounters tracks live metrics for the stats reporter.
var daemonCounters struct {
	ribbits        atomic.Int64
	triages        atomic.Int64
	triageBug      atomic.Int64
	triageFeature  atomic.Int64
	triageQuestion atomic.Int64
	triageOther    atomic.Int64
	// botIntakeDropped counts every silently-dropped allowlisted-bot intake
	// message (runBotIntake's several drop points — triage failure with no
	// Sentry refs, non-actionable, claim conflict, or an investigation that
	// fell through) — see runBotIntake's doc comment (ticketflow.go). Surfaced
	// on the dashboard so a sustained drop rate is visible rather than only
	// living in Warn-level logs.
	botIntakeDropped atomic.Int64
}

var rootCmd = &cobra.Command{
	Use:   "toad",
	Short: "AI-powered code assistant that lives in Slack",
	Long:  "Toad monitors Slack channels, triages messages for code-related issues, and responds with codebase-aware ribbits.",
	RunE:  runDaemon,
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func runDaemon(cmd *cobra.Command, args []string) error {
	// 1. Load config
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// 2. Set up logging
	toadlog.Setup(cfg.Log.Level, cfg.Log.File)

	// 3. Validate — auto-init if config is missing and we're in a terminal
	if err := config.Validate(cfg); err != nil {
		if !term.IsTerminal(os.Stderr.Fd()) {
			return fmt.Errorf("config validation: %w", err)
		}

		fmt.Print(tui.StyledMessage("No config found — starting setup..."))
		if initErr := runInit(nil, nil); initErr != nil {
			return fmt.Errorf("init wizard: %w", initErr)
		}

		// Re-load and re-validate after init
		cfg, err = config.Load()
		if err != nil {
			return fmt.Errorf("loading config after init: %w", err)
		}
		if err := config.Validate(cfg); err != nil {
			return fmt.Errorf("config validation after init: %w", err)
		}

		// Re-setup logging with potentially new config
		toadlog.Setup(cfg.Log.Level, cfg.Log.File)
	}

	// 4. Preflight checks — fail fast on missing tools or bad repo paths
	if results := preflight.Run(cfg); len(preflight.Errors(results)) > 0 {
		return fmt.Errorf("%s", preflight.FormatErrors(preflight.Errors(results)))
	}

	// 5. Print version and check for updates
	slog.Info("starting toad", "version", Version)
	if info, checkErr := update.Check(Version); checkErr == nil && info != nil && info.Available {
		slog.Warn("update available", "current", info.Current, "latest", info.Latest)
	}

	// 6. Initialize components — opened before the agent provider below so
	// its seat-fallback callback (which records a dashboard metric) has a DB
	// to write to.
	stateDB, err := state.OpenDB()
	if err != nil {
		return fmt.Errorf("opening state db: %w", err)
	}
	defer stateDB.Close()

	// 6. Check required CLI tools
	agentProvider, err := agent.NewProvider(cfg.Agent.Platform, cfg.Agent.FallbackAPIKeyEnv, func() {
		incrementMetric(stateDB, "seat_fallback")
	})
	if err != nil {
		return fmt.Errorf("agent config: %w", err)
	}
	if err := agentProvider.Check(); err != nil {
		return err
	}

	// Wire FailureTrackingProvider ONCE, around the base provider, before
	// any other decorator/consumer gets a reference — readOnlyProvider
	// (below) wraps this same value, and triageEngine/digest's AgentProvider
	// are constructed from it further down, so every call path (triage,
	// ribbit, investigations, digest) feeds the same tracked counters. See
	// C5: failureTracker.Snapshot() is read by the stats-writer goroutine
	// below to populate DaemonStats.Claude* for the dashboard.
	failureTracker := &agent.FailureTrackingProvider{Provider: agentProvider}
	agentProvider = failureTracker

	// Warm up the agent CLI in the background: the first call after a daemon
	// start is consistently the slowest (CLI process cold start, credential
	// refresh) and has repeatedly blown through the triage timeout in
	// production — a trivial PermissionNone call here absorbs that cost so
	// the first real triage runs against a warm CLI. Runs through the
	// failure tracker deliberately: a failing warm-up IS a genuine early
	// signal (e.g. expired auth) and should count toward the dashboard alert.
	go func() {
		warmCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		start := time.Now()
		_, warmErr := agentProvider.Run(warmCtx, agent.RunOpts{
			Prompt:      "Reply with exactly: ok",
			Model:       cfg.Triage.Model,
			Timeout:     2 * time.Minute,
			Permissions: agent.PermissionNone,
		})
		if warmErr != nil {
			slog.Warn("agent CLI warm-up failed", "error", warmErr, "duration", time.Since(start).Round(time.Millisecond))
			return
		}
		slog.Info("agent CLI warmed up", "duration", time.Since(start).Round(time.Millisecond))
	}()

	if _, err := buildVCSResolver(cfg); err != nil {
		return fmt.Errorf("vcs config: %w", err)
	}

	// Recover any stale runs from a previous crash
	recovery, err := state.RecoverOnStartup(stateDB)
	if err != nil {
		slog.Warn("startup recovery failed", "error", err)
	}

	stateManager, err := state.NewPersistentManager(stateDB)
	if err != nil {
		return fmt.Errorf("hydrating state: %w", err)
	}

	// Build repo profiles and resolver for multi-repo routing
	profiles := config.BuildProfiles(cfg.Repos.List)
	resolver := config.NewResolver(profiles, cfg.Repos.List)

	triageEngine := triage.New(agentProvider, cfg.Triage.Model, profiles)

	// Initialize issue tracker (before ribbit, which uses it for ticket enrichment)
	tracker := issuetracker.NewTracker(cfg.IssueTracker)

	// Resolve the toad home dir once — it's needed both to write the MCP
	// config file below and to deny read-only agents access to it (it holds
	// mcp-config.json and, via AuthTokenEnv-resolved headers, MCP bearer
	// tokens). A read-only agent (ribbit, investigations) that could Read
	// arbitrary files would otherwise be able to exfiltrate that token.
	toadHome, toadHomeErr := toadpath.Home()
	if toadHomeErr != nil {
		slog.Warn("resolving toad home failed, investigations/ribbit will run without mcp and without the read-deny guard", "error", toadHomeErr)
	}

	// readOnlyProvider is handed to every read-only agent class (ribbit,
	// investigations) instead of the bare agentProvider so buildArgs always
	// emits a --disallowedTools guard against the toad home dir, regardless
	// of what each package's own RunOpts construction sets. agentProvider
	// itself (unwrapped) still goes to triage and the digest engine, whose
	// runs are PermissionNone (no tool access at all, so nothing to deny).
	var readOnlyProvider agent.Provider = agentProvider
	if toadHomeErr == nil {
		readOnlyProvider = &agent.ReadDenyingProvider{
			Provider:        agentProvider,
			DeniedReadPaths: []string{toadHome},
		}
	}

	ribbitEngine := ribbit.New(readOnlyProvider, cfg, tracker)

	// Separate concurrency pools: ribbits are fast (seconds), investigations are slow (minutes).
	// Ribbit pool is generous so Q&A stays responsive even while investigations run.
	ribbitSem := make(chan struct{}, cfg.Limits.MaxConcurrent*3)
	investigateSem := make(chan struct{}, cfg.Limits.MaxConcurrent)
	// mcpAskSem is intentionally its own pool, separate from ribbitSem (I5):
	// the MCP `ask` tool used to share ribbitSem directly, so a busy MCP
	// client could exhaust the same slots Slack-triggered ribbit replies
	// need, starving human Q&A in Slack. No new config key — reuses
	// cfg.Limits.MaxConcurrent, same as investigateSem.
	mcpAskSem := make(chan struct{}, cfg.Limits.MaxConcurrent)

	// 7. Initialize Slack client
	slackClient := islack.NewClient(cfg.Slack)

	// Build path → name map for cross-repo prompts and path scrubbing
	repoPaths := make(map[string]string, len(cfg.Repos.List))
	for _, r := range cfg.Repos.List {
		repoPaths[r.Path] = r.Name
	}

	// Wire path scrubber — prevents absolute filesystem paths from leaking to Slack
	slackClient.SetPathScrubber(repoPaths)

	// Write the MCP config the investigation agent will run with (e.g. a
	// Sentry MCP server for pulling issue/Seer detail). WriteMCPConfig
	// requires its dir to already exist — safe here since state.OpenDB()
	// above already created the toad home directory.
	var mcpPath string
	if toadHomeErr == nil {
		if p, err := agent.WriteMCPConfig(toadHome, cfg.Agent.MCPServers); err != nil {
			slog.Warn("writing mcp config failed, investigations will run without mcp", "error", err)
		} else {
			mcpPath = p
		}
	}

	// Build the MCP tool allowlist dynamically from whatever servers are
	// actually configured (rather than hardcoding "sentry") — every
	// configured agent.mcp_servers entry gets its own mcp__<name>__*
	// wildcard. Sorted for deterministic --allowedTools ordering.
	allowedMCP := make([]string, 0, len(cfg.Agent.MCPServers))
	for name := range cfg.Agent.MCPServers {
		allowedMCP = append(allowedMCP, "mcp__"+name+"__*")
	}
	sort.Strings(allowedMCP)

	// repoSync tracks the outcome of every repo sync attempt (this
	// pre-investigation sync and the periodic background sync below) so the
	// dashboard can show per-repo freshness and a sync-warning alert. See its
	// doc comment (helpers.go).
	repoSync := newRepoSyncTracker()
	trackedSyncRepoNow := repoSync.wrap(SyncRepoNow)

	investRunner := investigation.NewRunner(readOnlyProvider, cfg.Agent.Model, mcpPath, allowedMCP, investigation.RepoSyncer(trackedSyncRepoNow), repoPaths)
	ticketEngine := ticket.New(tracker, stateDB, cfg.Ticket, slackClient.GetPermalink)

	// flowDeps bundles the six dependencies every investigate-and-file entry
	// point in cmd/ needs (see its doc comment in ticketflow.go) — built once
	// here and passed down as a single value instead of six positional params.
	deps := flowDeps{
		stateManager:             stateManager,
		tracker:                  tracker,
		resolver:                 resolver,
		investRunner:             investRunner,
		ticketEngine:             ticketEngine,
		investigateSem:           investigateSem,
		investigateTimeout:       time.Duration(cfg.Limits.TimeoutMinutes) * time.Minute,
		delegates:                cfg.IssueTracker.Delegates,
		resolveRequesterIdentity: resolveRequesterIdentity(slackClient),
	}

	// 9. Initialize MCP server if enabled (started after context is created below)
	var mcpSrv *toadmcp.Server
	if cfg.MCP.Enabled {
		mcpSrv = toadmcp.New(cfg.MCP, stateDB)

		toadmcp.RegisterLogsTool(mcpSrv.MCPServer(), cfg.Log.File)
		toadmcp.RegisterInvestigationsTool(mcpSrv.MCPServer(), stateDB)
		toadmcp.RegisterQueryTool(mcpSrv.MCPServer(), stateDB)
		toadmcp.RegisterAskTool(mcpSrv.MCPServer(), &toadmcp.AskDeps{
			Ribbit:   ribbitEngine,
			Triage:   triageEngine,
			Resolver: resolver,
			Repos:    cfg.Repos.List,
			Sessions: toadmcp.NewSessionStore(),
			Sem:      mcpAskSem,
		})
	}

	// Always wire up slash command handler (status/help work without MCP)
	slashCmds := islack.NewSlashCommandHandler(stateDB, slackClient.API(), cfg.MCP)
	slackClient.SetMCPHandler(slashCmds)

	// Initialize digest engine (Toad King) if enabled
	var digestEngine *digest.Engine
	if cfg.Digest.Enabled {
		digestEngine = digest.New(&cfg.Digest, digest.EngineOpts{
			AgentProvider: agentProvider,
			TriageModel:   cfg.Triage.Model,
			Propose: func(ctx context.Context, f investigation.Findings, msg digest.Message) error {
				// See isSentryCorroborated's doc comment (ticketflow.go) for
				// the full rule — never merely because the digest batched a
				// message with a Sentry reference in its text.
				sentryCorroborated := isSentryCorroborated(msg.BotID, cfg.Intake.BotAllowlist)
				return proposeFromDigest(ctx, ticketEngine, stateDB, slackClient.ReplyWithOptionalCTA, f, msg, sentryCorroborated, cfg.IssueTracker.Delegates)
			},
			Notify: func(channel, threadTS, text string) {
				slackClient.ReplyInThread(channel, threadTS, text)
			},
			Investigate: func(ctx context.Context, opp digest.Opportunity, msg digest.Message, tickets []digest.TicketContext) (*investigation.Findings, error) {
				return investigateFromDigest(ctx, resolver, investRunner, investigateSem, stateDB, cfg.Digest.InvestigateTimeoutSecs, opp, msg, tickets, cfg.Intake.BotAllowlist)
			},
			React: func(channel, timestamp, emoji string) {
				slackClient.React(channel, timestamp, emoji)
			},
			Claim:       stateManager.ClaimScoped,
			Unclaim:     stateManager.UnclaimScoped,
			ResolveRepo: resolver.Resolve,
			RepoPaths:   repoPaths,
			Profiles:    profiles,
			DB:          stateDB,
			Tracker:     tracker,
			GetPermalink: func(channel, timestamp string) (string, error) {
				return slackClient.GetPermalink(channel, timestamp)
			},
			RespectAssignees: cfg.IssueTracker.RespectAssignees,
			StaleDays:        cfg.IssueTracker.StaleDays,
		})
	}

	// 10. Set up message handler — dispatch into goroutines so the event loop stays responsive
	var messageWg sync.WaitGroup
	slackClient.OnMessage(func(ctx context.Context, msg *islack.IncomingMessage) {
		messageWg.Add(1)
		go func() {
			defer messageWg.Done()
			handleMessage(ctx, msg, triageEngine, ribbitEngine, slackClient, deps, ribbitSem, digestEngine, repoPaths, cfg.Intake.BotAllowlist)
		}()
	})

	// 11. Handle graceful shutdown (SIGINT/SIGTERM exit, SIGUSR1 restart)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var restartRequested atomic.Bool
	restartCh := make(chan os.Signal, 1)
	notifyRestart(restartCh)
	go func() {
		select {
		case <-restartCh:
			slog.Info("restart requested (SIGUSR1), draining work...")
			restartRequested.Store(true)
			cancel() // trigger the same graceful shutdown path
		case <-ctx.Done():
		}
	}()

	repoNames := make([]string, len(cfg.Repos.List))
	for i, r := range cfg.Repos.List {
		repoNames[i] = r.Name
	}
	slog.Info("toad is listening",
		"channels", cfg.Slack.Channels,
		"repos", repoNames,
		"triggers", fmt.Sprintf("keywords=%v", cfg.Slack.Triggers.Keywords),
	)

	// bgWg tracks the five background goroutines below (MCP server, repo
	// sync, digest engine's Run + ResumeInvestigations, outcome poller) —
	// Important fix (I6): these were all bare goroutines with nothing
	// waiting on them before shutdown closed stateDB, so any of them still
	// mid-write when the DB closed could error or lose work. Waited on
	// (bounded, ~35s) right before stateDB.Close() on both the normal-exit
	// and restart/exec paths — see the wait call near the bottom of this
	// function.
	var bgWg sync.WaitGroup

	// Start MCP server if enabled
	if mcpSrv != nil {
		mcpSrv.Health().Version = Version
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			// mcpSrv.Start blocks until ctx is canceled AND its own internal
			// graceful-shutdown goroutine finishes calling http.Server.Shutdown
			// (bounded to its own 30s internal timeout) — so this goroutine
			// genuinely doesn't finish until the MCP server has drained,
			// making bgWg.Wait() below meaningful for it.
			if err := mcpSrv.Start(ctx); err != nil {
				slog.Error("MCP server error", "error", err)
			}
		}()
	}

	// Start periodic repo sync if enabled
	if cfg.Repos.SyncMinutes > 0 {
		interval := time.Duration(cfg.Repos.SyncMinutes) * time.Minute
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			syncRepos(ctx, cfg.Repos.List, interval, trackedSyncRepoNow)
		}()
	}

	// Start digest engine (Toad King) if enabled
	if digestEngine != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			digestEngine.Run(ctx)
		}()
		// Resume any investigations that were interrupted by a previous crash.
		if recovery != nil && len(recovery.StaleOpportunities) > 0 {
			staleOpps := recovery.StaleOpportunities
			bgWg.Add(1)
			go func() {
				defer bgWg.Done()
				digestEngine.ResumeInvestigations(ctx, staleOpps)
			}()
		}
	}

	// Outcome poller: watch what happens to tickets toad has filed so the
	// team can see whether they're landing. Classification prefers the
	// tracker's state TYPE when available (completed = done, canceled =
	// rejected, triage = pending, backlog/unstarted/started = accepted),
	// falling back to name matching otherwise — see classifyOutcome.
	// Visibility only — no behavior adaptation. Skipped entirely when no
	// tracker is configured.
	if _, isNoop := tracker.(issuetracker.NoopTracker); !isNoop {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runOutcomePoller(ctx, stateDB, tracker, time.Hour)
		}()
	}

	// Prune expired thread memories every hour
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if n, err := stateDB.PruneThreadMemory(state.ThreadMemoryTTL); err != nil {
					slog.Warn("thread memory prune failed", "error", err)
				} else if n > 0 {
					slog.Info("pruned thread memories", "count", n)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Shutdown goroutine — drains in-flight message handlers before we close the DB.
	poolDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		slog.Info("shutting down...")
		messageWg.Wait()
		close(poolDone)
	}()

	// Write daemon stats to SQLite every 10s for the dashboard
	daemonStartedAt := time.Now()
	statsDone := make(chan struct{})
	go func() {
		defer close(statsDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		writeStats := func(draining bool) {
			ds := &state.DaemonStats{
				Heartbeat: time.Now(),
				StartedAt: daemonStartedAt,
				PID:       os.Getpid(),
				Version:   Version,
				Draining:  draining,
				Ribbits:   daemonCounters.ribbits.Load(),
				Triages:   daemonCounters.triages.Load(),
				TriageByCategory: map[string]int64{
					categoryBug:     daemonCounters.triageBug.Load(),
					categoryFeature: daemonCounters.triageFeature.Load(),
					"question":      daemonCounters.triageQuestion.Load(),
					"other":         daemonCounters.triageOther.Load(),
				},
				BotIntakeDropped: daemonCounters.botIntakeDropped.Load(),
				DigestEnabled:    cfg.Digest.Enabled,
				IssueTracker:     cfg.IssueTracker.Enabled,
				IssueProvider:    cfg.IssueTracker.Provider,
				MCPEnabled:       cfg.MCP.Enabled,
				MCPHost:          cfg.MCP.Host,
				MCPPort:          cfg.MCP.Port,
				RepoSync:         repoSync.snapshot(),
			}
			ds.InvestigateSlots, ds.InvestigateInFlight = concurrencyGauge(investigateSem)
			ds.RibbitSlots, ds.RibbitInFlight = concurrencyGauge(ribbitSem)
			failureSnap := failureTracker.Snapshot()
			ds.ClaudeConsecutiveFailures = failureSnap.Consecutive
			ds.ClaudeLastSuccessAt = failureSnap.LastSuccessAt
			ds.ClaudeLastError = failureSnap.LastErr
			if digestEngine != nil {
				dstats := digestEngine.Stats()
				ds.DigestBuffer = dstats.BufferSize
				ds.DigestNextFlush = dstats.NextFlush
				ds.DigestProcessed = dstats.TotalProcessed
				ds.DigestOpps = dstats.TotalOpps
				ds.DigestSpawns = dstats.TotalSpawns
			}
			if err := stateDB.WriteDaemonStats(ds); err != nil {
				slog.Debug("failed to write daemon stats", "error", err)
			}
		}

		for {
			select {
			case <-ticker.C:
				writeStats(false)
			case <-ctx.Done():
				if restartRequested.Load() {
					// Keep heartbeating with draining=true so the dashboard
					// can show a modal with in-flight work until we're done.
					writeStats(true)
					for {
						select {
						case <-ticker.C:
							writeStats(true)
						case <-poolDone:
							stateDB.ClearDaemonStats()
							return
						}
					}
				}
				stateDB.ClearDaemonStats()
				return
			}
		}
	}()

	slackErr := slackClient.Run(ctx)
	<-poolDone
	<-statsDone // wait for stats writer to finish before closing DB

	// I6: wait (bounded ~35s) for the background goroutines tracked by bgWg
	// (digest engine, MCP server, outcome poller, repo sync) to finish
	// before stateDB is closed — on BOTH exit paths below: the normal
	// return (whose deferred stateDB.Close() at the top of this function
	// fires right after) and the restart/exec path's explicit Close() call.
	// Bounded so a stuck goroutine can't hang shutdown forever.
	waitForBackgroundWork(&bgWg, 35*time.Second)

	if restartRequested.Load() {
		// Under a process supervisor, return cleanly and let the supervisor
		// restart us. syscall.Exec confuses supervisors that track the child PID.
		if os.Getenv("SUPERVISED") != "" {
			slog.Info("restart: exiting for supervisor restart")
			return nil
		}

		binary, err := os.Executable()
		if err != nil {
			return fmt.Errorf("restart: could not find executable: %w", err)
		}
		slog.Info("restarting toad", "binary", binary)
		// Close DB explicitly since execReplace may replace the process
		// and deferred Close() won't run.
		_ = stateDB.Close()
		return execReplace(binary, os.Args, os.Environ())
	}

	return slackErr
}
