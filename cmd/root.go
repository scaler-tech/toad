// Package cmd implements the CLI commands for the toad daemon.
package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/digest"
	"github.com/scaler-tech/toad/internal/issuetracker"
	toadlog "github.com/scaler-tech/toad/internal/log"
	toadmcp "github.com/scaler-tech/toad/internal/mcp"
	"github.com/scaler-tech/toad/internal/personality"
	"github.com/scaler-tech/toad/internal/preflight"
	"github.com/scaler-tech/toad/internal/reviewer"
	"github.com/scaler-tech/toad/internal/ribbit"
	islack "github.com/scaler-tech/toad/internal/slack"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/tadpole"
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

	// 6. Check required CLI tools
	agentProvider, err := agent.NewProvider(cfg.Agent.Platform)
	if err != nil {
		return fmt.Errorf("agent config: %w", err)
	}
	if err := agentProvider.Check(); err != nil {
		return err
	}
	vcsResolver, err := buildVCSResolver(cfg)
	if err != nil {
		return fmt.Errorf("vcs config: %w", err)
	}

	// 6. Initialize components
	stateDB, err := state.OpenDB()
	if err != nil {
		return fmt.Errorf("opening state db: %w", err)
	}
	defer stateDB.Close()

	// Recover any stale runs and orphaned worktrees from a previous crash
	recovery, err := state.RecoverOnStartup(stateDB)
	if err != nil {
		slog.Warn("startup recovery failed", "error", err)
	}

	stateManager, err := state.NewPersistentManager(stateDB, cfg.Limits.HistorySize)
	if err != nil {
		return fmt.Errorf("hydrating state: %w", err)
	}

	// Initialize personality manager
	var personalityMgr *personality.Manager
	if cfg.Personality.Enabled {
		pfPath := cfg.Personality.FilePath
		if pfPath == "" {
			home, err := toadpath.Home()
			if err != nil {
				return fmt.Errorf("resolving toad home: %w", err)
			}
			pfPath = filepath.Join(home, "personality.yaml")
		}
		pf, err := personality.LoadFile(pfPath)
		if err != nil {
			return fmt.Errorf("loading personality: %w", err)
		}
		personalityMgr, err = personality.NewPersistentManager(stateDB, pf.Traits)
		if err != nil {
			return fmt.Errorf("initializing personality: %w", err)
		}
		personalityMgr.SetLearning(cfg.Personality.LearningEnabled)
		slog.Info("personality loaded", "name", pf.Name, "learning", cfg.Personality.LearningEnabled)
	} else {
		personalityMgr = personality.NewManager(personality.DefaultTraits())
		personalityMgr.SetLearning(false)
	}

	// Build repo profiles and resolver for multi-repo routing
	profiles := config.BuildProfiles(cfg.Repos.List)
	resolver := config.NewResolver(profiles, cfg.Repos.List)

	triageEngine := triage.New(agentProvider, cfg.Triage.Model, profiles)

	// Initialize issue tracker (before ribbit, which uses it for ticket enrichment)
	tracker := issuetracker.NewTracker(cfg.IssueTracker)

	ribbitEngine := ribbit.New(agentProvider, cfg, personalityMgr, tracker)

	// Separate concurrency pools: ribbits are fast (seconds), tadpoles are slow (minutes).
	// Ribbit pool is generous so Q&A stays responsive even while tadpoles run.
	ribbitSem := make(chan struct{}, cfg.Limits.MaxConcurrent*3)
	tadpoleSem := make(chan struct{}, cfg.Limits.MaxConcurrent)

	// 7. Initialize Slack client
	slackClient := islack.NewClient(cfg.Slack)

	// Initialize tadpole runner and pool (with Slack client for status updates)
	tadpoleRunner := tadpole.NewRunner(cfg, agentProvider, slackClient, stateManager, vcsResolver, personalityMgr)
	tadpolePool := tadpole.NewPool(tadpoleSem, tadpoleRunner)

	// 8. Initialize PR review watcher
	prWatcher := reviewer.NewWatcher(stateDB, cfg.Repos.List, func(ctx context.Context, task tadpole.Task) error {
		return tadpolePool.Spawn(ctx, task)
	}, slackClient, agentProvider, cfg.Limits.MaxReviewRounds, cfg.Limits.MaxCIFixRounds, cfg.Triage.Model, vcsResolver, cfg.Limits.ReviewBots)

	// Wire PR review tracking — after a successful ship, register the PR for review watching
	tadpoleRunner.OnShip(func(prURL, branch, runID string, task tadpole.Task) {
		repoPath := ""
		if task.Repo != nil {
			repoPath = task.Repo.Path
		} else if len(cfg.Repos.List) > 0 {
			repoPath = cfg.Repos.List[0].Path
		}
		prNum, err := vcsResolver(repoPath).ExtractPRNumber(prURL)
		if err != nil {
			slog.Warn("could not extract PR number for review tracking", "url", prURL, "error", err)
			return
		}
		prWatcher.TrackPR(prNum, prURL, branch, runID, task.SlackChannel, task.SlackThreadTS, repoPath, task.Summary, task.Description)
	})

	// Wire personality outcome callback for PR terminal state signals
	if cfg.Personality.Enabled {
		prWatcher.OnPersonalityOutcome(func(signal personality.OutcomeSignal) {
			if err := personalityMgr.ProcessOutcome(signal); err != nil {
				slog.Warn("personality outcome processing failed", "error", err)
			}
		})
	}

	// Build path → name map for cross-repo prompts and path scrubbing
	repoPaths := make(map[string]string, len(cfg.Repos.List))
	for _, r := range cfg.Repos.List {
		repoPaths[r.Path] = r.Name
	}

	// Wire path scrubber — prevents absolute filesystem paths from leaking to Slack
	slackClient.SetPathScrubber(repoPaths)

	// 9. Initialize MCP server if enabled (started after context is created below)
	var mcpSrv *toadmcp.Server
	if cfg.MCP.Enabled {
		mcpSrv = toadmcp.New(cfg.MCP, stateDB)

		toadmcp.RegisterLogsTool(mcpSrv.MCPServer(), cfg.Log.File)
		toadmcp.RegisterWatchesTool(mcpSrv.MCPServer(), stateDB)
		toadmcp.RegisterQueryTool(mcpSrv.MCPServer(), stateDB)
		toadmcp.RegisterAskTool(mcpSrv.MCPServer(), &toadmcp.AskDeps{
			Ribbit:   ribbitEngine,
			Triage:   triageEngine,
			Resolver: resolver,
			Repos:    cfg.Repos.List,
			Sessions: toadmcp.NewSessionStore(),
			Sem:      ribbitSem,
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
			Spawn: func(ctx context.Context, task tadpole.Task) error {
				return tadpolePool.Spawn(ctx, task)
			},
			Notify: func(channel, threadTS, text string) {
				slackClient.ReplyInThread(channel, threadTS, text)
			},
			NotifyInvestigation: func(notice digest.InvestigationNotice) {
				text := notice.Text

				// Determine if this is a bot message needing active outreach
				isBot := notice.BotID != ""
				if isBot && len(cfg.Digest.BotList) > 0 {
					isBot = false
					for _, b := range cfg.Digest.BotList {
						if b == notice.BotID {
							isBot = true
							break
						}
					}
				}

				if isBot {
					// Resolve file contributors to Slack mentions
					if len(notice.FilesHint) == 0 {
						slog.Debug("digest dev tag skipped: no files_hint from investigation")
					} else {
						repo := resolver.Resolve(notice.Repo, notice.FilesHint)
						if repo == nil {
							slog.Debug("digest dev tag skipped: could not resolve repo",
								"repo_hint", notice.Repo, "files_hint", notice.FilesHint)
						} else {
							botSet := make(map[string]bool)
							for _, u := range cfg.VCS.BotUsernames {
								botSet[strings.ToLower(u)] = true
							}
							logins := vcsResolver(repo.Path).GetSuggestedReviewers(
								context.Background(), repo.Path, notice.FilesHint, botSet, 2,
							)
							if len(logins) == 0 {
								slog.Debug("digest dev tag skipped: no git contributors found",
									"files_hint", notice.FilesHint, "repo", repo.Name)
							} else {
								resolved := islack.ResolveGitHubToSlack(stateDB, slackClient.API(), logins)
								var mentions []string
								var unresolved []string
								for _, login := range logins {
									if slackID, ok := resolved[login]; ok {
										mentions = append(mentions, fmt.Sprintf("<@%s>", slackID))
									} else {
										unresolved = append(unresolved, login)
									}
								}
								if len(unresolved) > 0 {
									slog.Debug("digest dev tag: some GitHub users not mapped to Slack",
										"unresolved", unresolved)
								}
								if len(mentions) > 0 {
									text += "\n\ncc " + strings.Join(mentions, " ")
								} else {
									slog.Debug("digest dev tag skipped: GitHub logins found but none mapped to Slack",
										"logins", logins)
								}
							}
						}
					}
				} else {
					text += "\n\n_Tag a relevant dev if you'd like someone to take a look._"
				}

				// Post investigation reply with CTA button
				blocks := islack.FixThisBlocks(text, notice.ThreadTS)
				replyTS := ""
				if ts, err := slackClient.ReplyInThreadWithBlocks(
					notice.Channel, notice.ThreadTS, text, blocks,
				); err != nil {
					slog.Warn("digest investigation reply failed", "error", err)
				} else {
					replyTS = ts
				}

				// Crosspost to Linear if bot message with issue refs
				if isBot && tracker != nil && len(notice.IssueRefs) > 0 && replyTS != "" {
					permalink, _ := slackClient.GetPermalink(notice.Channel, replyTS)
					for _, ref := range notice.IssueRefs {
						// Strip Slack mrkdwn header for Markdown-native tracker
						reasoning := strings.TrimPrefix(notice.Text, ":mag: *Investigation findings:*\n\n")
						body := "**Toad investigation findings**\n\n" + reasoning + "\n\n"
						if permalink != "" {
							body += fmt.Sprintf("Toad can fix this automatically — [go to the Slack thread](%s) and click the button to start.", permalink)
						} else {
							body += "Toad can fix this automatically — go to the Slack thread and click the button to start."
						}
						if err := tracker.PostComment(context.Background(), ref, body); err != nil {
							slog.Warn("failed to crosspost investigation to issue tracker",
								"ref", ref.ID, "error", err)
						} else {
							slog.Info("crossposted investigation to issue tracker", "ref", ref.ID)
						}
					}
				}
			},
			Investigate: func(ctx context.Context, opp digest.Opportunity, msg digest.Message, tickets []digest.TicketContext) (*digest.InvestigateResult, error) {
				return investigateOpportunity(ctx, cfg, agentProvider, opp, msg, resolver, tickets)
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
			Personality:      personalityMgr,
		})
	}

	// 10. Set up message handler — dispatch into goroutines so the event loop stays responsive
	var messageWg sync.WaitGroup
	slackClient.OnMessage(func(ctx context.Context, msg *islack.IncomingMessage) {
		messageWg.Add(1)
		go func() {
			defer messageWg.Done()
			handleMessage(ctx, msg, cfg, agentProvider, triageEngine, ribbitEngine, slackClient, stateManager, ribbitSem, tadpolePool, digestEngine, tracker, resolver, repoPaths)
		}()
	})

	if cfg.Personality.Enabled && personalityMgr.LearningEnabled() {
		personalityMgr.SetInterpreter(personality.NewInterpreter(agentProvider))
		slackClient.OnPersonalityReaction(func(ctx context.Context, emoji, channel, ts string) {
			if err := personalityMgr.ProcessEmoji(emoji, fmt.Sprintf("channel:%s ts:%s", channel, ts)); err != nil {
				slog.Debug("personality emoji feedback failed", "emoji", emoji, "error", err)
			}
		})
		slackClient.OnPersonalityText(func(ctx context.Context, text, channel, threadTS string) {
			threadKey := fmt.Sprintf("channel:%s ts:%s", channel, threadTS)
			if err := personalityMgr.ProcessText(ctx, text, threadKey); err != nil {
				slog.Debug("personality text processing failed", "error", err)
			}
		})
	}

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
		"triggers", fmt.Sprintf("emoji=%s keywords=%v", cfg.Slack.Triggers.Emoji, cfg.Slack.Triggers.Keywords),
	)

	// Start MCP server if enabled
	if mcpSrv != nil {
		mcpSrv.Health().Version = Version
		go func() {
			if err := mcpSrv.Start(ctx); err != nil {
				slog.Error("MCP server error", "error", err)
			}
		}()
	}

	// Start PR review watcher
	go prWatcher.Run(ctx)

	// Start periodic repo sync if enabled
	if cfg.Repos.SyncMinutes > 0 {
		interval := time.Duration(cfg.Repos.SyncMinutes) * time.Minute
		go syncRepos(ctx, cfg.Repos.List, interval)
	}

	// Start worktree TTL cleanup if configured
	if cfg.Limits.WorktreeTTLHours > 0 {
		go tadpole.CleanupStaleWorktrees(ctx, time.Duration(cfg.Limits.WorktreeTTLHours)*time.Hour)
	}

	// Start digest engine (Toad King) if enabled
	if digestEngine != nil {
		go digestEngine.Run(ctx)
		// Resume any investigations that were interrupted by a previous crash.
		if recovery != nil && len(recovery.StaleOpportunities) > 0 {
			staleOpps := recovery.StaleOpportunities
			go digestEngine.ResumeInvestigations(ctx, staleOpps)
		}
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

	// Pool shutdown goroutine — drains messages and waits for tadpoles
	poolDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		slog.Info("shutting down...")
		messageWg.Wait()
		grace := 30 * time.Second
		if restartRequested.Load() {
			grace = 30 * time.Minute
			slog.Info("restart mode: waiting up to 30m for tadpoles to finish")
		}
		tadpolePool.Shutdown(grace)
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
					"bug":      daemonCounters.triageBug.Load(),
					"feature":  daemonCounters.triageFeature.Load(),
					"question": daemonCounters.triageQuestion.Load(),
					"other":    daemonCounters.triageOther.Load(),
				},
				DigestEnabled:     cfg.Digest.Enabled,
				DigestDryRun:      cfg.Digest.DryRun,
				DigestCommentMode: cfg.Digest.CommentInvestigation,
				IssueTracker:      cfg.IssueTracker.Enabled,
				IssueProvider:     cfg.IssueTracker.Provider,
				MCPEnabled:        cfg.MCP.Enabled,
				MCPHost:           cfg.MCP.Host,
				MCPPort:           cfg.MCP.Port,
			}
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
