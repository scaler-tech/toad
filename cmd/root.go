package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/hergen/toad/internal/config"
	"github.com/hergen/toad/internal/digest"
	toadlog "github.com/hergen/toad/internal/log"
	"github.com/hergen/toad/internal/reviewer"
	"github.com/hergen/toad/internal/ribbit"
	islack "github.com/hergen/toad/internal/slack"
	"github.com/hergen/toad/internal/state"
	"github.com/hergen/toad/internal/tadpole"
	"github.com/hergen/toad/internal/triage"
	"github.com/hergen/toad/internal/tui"
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

	// 4. Check required CLI tools
	if err := checkClaude(); err != nil {
		return err
	}
	if err := checkGH(); err != nil {
		return err
	}

	// 5. Initialize components
	stateDB, err := state.OpenDB()
	if err != nil {
		return fmt.Errorf("opening state db: %w", err)
	}
	defer stateDB.Close()

	// Recover any stale runs and orphaned worktrees from a previous crash
	if _, err := state.RecoverOnStartup(stateDB, cfg.Repo.Path); err != nil {
		slog.Warn("startup recovery failed", "error", err)
	}

	stateManager, err := state.NewPersistentManager(stateDB)
	if err != nil {
		return fmt.Errorf("hydrating state: %w", err)
	}
	triageEngine := triage.New(cfg)
	ribbitEngine := ribbit.New(cfg)

	// Separate concurrency pools: ribbits are fast (seconds), tadpoles are slow (minutes).
	// Ribbit pool is generous so Q&A stays responsive even while tadpoles run.
	ribbitSem := make(chan struct{}, cfg.Limits.MaxConcurrent*3)
	tadpoleSem := make(chan struct{}, cfg.Limits.MaxConcurrent)

	// 6. Initialize Slack client
	slackClient := islack.NewClient(cfg.Slack)

	// Initialize tadpole runner and pool (with Slack client for status updates)
	tadpoleRunner := tadpole.NewRunner(cfg, slackClient, stateManager)
	tadpolePool := tadpole.NewPool(tadpoleSem, tadpoleRunner)

	// 7. Initialize PR review watcher
	prWatcher := reviewer.NewWatcher(stateDB, cfg.Repo.Path, func(ctx context.Context, task tadpole.Task) error {
		return tadpolePool.Spawn(ctx, task)
	}, slackClient)

	// Wire PR review tracking — after a successful ship, register the PR for review watching
	tadpoleRunner.OnShip(func(prURL, branch, runID string, task tadpole.Task) {
		prNum, err := reviewer.ExtractPRNumber(prURL)
		if err != nil {
			slog.Warn("could not extract PR number for review tracking", "url", prURL, "error", err)
			return
		}
		prWatcher.TrackPR(prNum, prURL, branch, runID, task.SlackChannel, task.SlackThreadTS)
	})

	// 8. Initialize digest engine (Toad King) if enabled
	var digestEngine *digest.Engine
	if cfg.Digest.Enabled {
		digestEngine = digest.New(&cfg.Digest, cfg.Triage.Model,
			func(ctx context.Context, task tadpole.Task) error {
				return tadpolePool.Spawn(ctx, task)
			},
			func(channel, threadTS, text string) {
				slackClient.ReplyInThread(channel, threadTS, text)
			},
			stateDB,
		)
	}

	// 9. Set up message handler — dispatch into goroutines so the event loop stays responsive
	slackClient.OnMessage(func(ctx context.Context, msg *islack.IncomingMessage) {
		go handleMessage(ctx, msg, cfg, triageEngine, ribbitEngine, slackClient, stateManager, ribbitSem, tadpolePool, digestEngine)
	})

	// 8. Handle graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	slog.Info("toad is listening",
		"channels", cfg.Slack.Channels,
		"triggers", fmt.Sprintf("emoji=%s keywords=%v", cfg.Slack.Triggers.Emoji, cfg.Slack.Triggers.Keywords),
	)

	// Start PR review watcher
	go prWatcher.Run(ctx)

	// Start digest engine (Toad King) if enabled
	if digestEngine != nil {
		go digestEngine.Run(ctx)
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

	// Write daemon stats to SQLite every 10s for the dashboard
	daemonStartedAt := time.Now()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ds := &state.DaemonStats{
					Heartbeat:        time.Now(),
					StartedAt:        daemonStartedAt,
					PID:              os.Getpid(),
					Ribbits:          daemonCounters.ribbits.Load(),
					Triages:          daemonCounters.triages.Load(),
					TriageByCategory: map[string]int64{
						"bug":      daemonCounters.triageBug.Load(),
						"feature":  daemonCounters.triageFeature.Load(),
						"question": daemonCounters.triageQuestion.Load(),
						"other":    daemonCounters.triageOther.Load(),
					},
					DigestEnabled: cfg.Digest.Enabled,
					DigestDryRun:  cfg.Digest.DryRun,
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
			case <-ctx.Done():
				stateDB.ClearDaemonStats()
				return
			}
		}
	}()

	go func() {
		<-ctx.Done()
		slog.Info("shutting down...")
		tadpolePool.Shutdown(context.Background())
	}()

	return slackClient.Run(ctx)
}

func handleMessage(
	ctx context.Context,
	msg *islack.IncomingMessage,
	cfg *config.Config,
	triageEngine *triage.Engine,
	ribbitEngine *ribbit.Engine,
	slackClient *islack.Client,
	stateManager *state.Manager,
	ribbitSem chan struct{},
	tadpolePool *tadpole.Pool,
	digestEngine *digest.Engine,
) {
	// Resolve channel name for context
	channelName := slackClient.ResolveChannelName(msg.Channel)

	// TADPOLE REQUEST: :frog: reaction on a toad reply
	// Must be checked BEFORE the bot filter — tadpole requests are reactions on
	// toad's own (bot) messages, so the fetched message will have IsBot=true.
	if msg.IsTadpoleRequest {
		slog.Info("handler: tadpole requested", "channel", channelName, "thread", msg.ThreadTS())
		handleTadpoleRequest(ctx, msg, cfg, triageEngine, slackClient, stateManager, tadpolePool, channelName)
		return
	}

	// Skip bot messages (after tadpole check above)
	if msg.IsBot {
		slog.Debug("handler: skipping bot message", "user", msg.User, "channel", msg.Channel)
		return
	}

	// Feed non-bot messages to digest engine (Toad King) for batch analysis
	if digestEngine != nil {
		digestEngine.Collect(digest.Message{
			Channel:     msg.Channel,
			ChannelName: channelName,
			User:        msg.User,
			Text:        msg.Text,
			ThreadTS:    msg.ThreadTimestamp,
			Timestamp:   msg.Timestamp,
		})
	}

	// EXPLICIT TRIGGER: @toad mention or reaction/keyword trigger
	if msg.IsMention || msg.IsTriggered {
		slog.Debug("handler: triggered path", "mention", msg.IsMention, "triggered", msg.IsTriggered, "channel", channelName)

		// Limit concurrent Claude calls
		select {
		case ribbitSem <- struct{}{}:
			defer func() { <-ribbitSem }()
		case <-ctx.Done():
			return
		}

		handleTriggered(ctx, msg, cfg, triageEngine, ribbitEngine, slackClient, stateManager, tadpolePool, channelName)
		return
	}

	// PASSIVE MONITORING — also respect concurrency limit
	select {
	case ribbitSem <- struct{}{}:
		defer func() { <-ribbitSem }()
	default:
		slog.Debug("handler: skipping passive triage, at concurrency limit")
		return
	}

	slog.Debug("handler: passive path", "channel", channelName, "user", msg.User)
	handlePassive(ctx, msg, triageEngine, ribbitEngine, slackClient, channelName)
}

func handleTriggered(
	ctx context.Context,
	msg *islack.IncomingMessage,
	cfg *config.Config,
	triageEngine *triage.Engine,
	ribbitEngine *ribbit.Engine,
	slackClient *islack.Client,
	stateManager *state.Manager,
	tadpolePool *tadpole.Pool,
	channelName string,
) {
	// Check if already working on this thread
	threadTS := msg.ThreadTS()
	if existing := stateManager.GetByThread(threadTS); existing != nil {
		slackClient.ReplyInThread(msg.Channel, threadTS,
			fmt.Sprintf(":frog: Already working on this (status: %s)", existing.Status))
		return
	}

	// Acknowledge
	slackClient.React(msg.Channel, msg.Timestamp, "eyes")

	// Gather conversation context
	if msg.ThreadTimestamp != "" {
		// In a thread: fetch all thread messages
		threadMsgs, err := slackClient.FetchThreadMessages(msg.Channel, msg.ThreadTimestamp)
		if err != nil {
			slog.Warn("failed to fetch thread context", "error", err)
		} else {
			msg.ThreadContext = threadMsgs
		}
	} else {
		// Top-level mention: fetch recent channel messages for context
		recentMsgs, err := slackClient.FetchRecentMessages(msg.Channel, msg.Timestamp, 10)
		if err != nil {
			slog.Warn("failed to fetch channel context", "error", err)
		} else if len(recentMsgs) > 0 {
			msg.ThreadContext = recentMsgs
		}
	}

	// Triage — fast Haiku classification (~1s) to decide: ribbit or tadpole?
	result, err := triageEngine.Classify(ctx, msg, channelName)
	if err != nil {
		slog.Warn("triage failed, proceeding with defaults", "error", err)
		result = &triage.Result{
			Actionable: true,
			Category:   "question",
			Summary:    msg.Text,
			EstSize:    "small",
		}
	}

	// For non-mention triggers, respect triage's actionability decision
	if !msg.IsMention && !result.Actionable {
		slog.Debug("handler: triage said not actionable, skipping", "confidence", result.Confidence)
		slackClient.RemoveReaction(msg.Channel, msg.Timestamp, "eyes")
		return
	}

	slog.Info("triage routed", "category", result.Category, "size", result.EstSize,
		"confidence", result.Confidence, "summary", result.Summary)

	daemonCounters.triages.Add(1)
	switch result.Category {
	case "bug":
		daemonCounters.triageBug.Add(1)
	case "feature":
		daemonCounters.triageFeature.Add(1)
	case "question":
		daemonCounters.triageQuestion.Add(1)
	default:
		daemonCounters.triageOther.Add(1)
	}

	// AUTO-SPAWN: bugs and features go straight to a tadpole — toad is for one-shotting.
	// The PR is the review gate, not the spawn decision.
	if result.Category == "bug" || result.Category == "feature" {
		slog.Info("auto-spawning tadpole", "summary", result.Summary, "category", result.Category)

		if !stateManager.Claim(threadTS) {
			slackClient.ReplyInThread(msg.Channel, threadTS, ":frog: Already working on this thread")
			slackClient.RemoveReaction(msg.Channel, msg.Timestamp, "eyes")
			return
		}
		claimed := true
		defer func() {
			if claimed {
				stateManager.Unclaim(threadTS)
			}
		}()

		task := tadpole.Task{
			Description:   msg.Text,
			Summary:       result.Summary,
			Category:      result.Category,
			EstSize:       result.EstSize,
			SlackChannel:  msg.Channel,
			SlackThreadTS: threadTS,
			TriageResult:  result,
		}

		if err := tadpolePool.Spawn(ctx, task); err != nil {
			slog.Error("auto-spawn failed", "error", err)
			slackClient.SwapReaction(msg.Channel, msg.Timestamp, "eyes", "warning")
			slackClient.ReplyInThread(msg.Channel, threadTS,
				":x: Failed to spawn tadpole: "+err.Error())
			return
		}
		claimed = false
		slackClient.RemoveReaction(msg.Channel, msg.Timestamp, "eyes")
		return
	}

	// RIBBIT: questions, refactors, and other categories get a codebase-aware reply
	slog.Info("generating ribbit", "summary", result.Summary, "category", result.Category)

	// Look up prior thread memory for coherent follow-ups
	var prior *ribbit.PriorContext
	if stateManager.DB() != nil {
		mem, err := stateManager.DB().GetThreadMemory(threadTS)
		if err != nil {
			slog.Warn("failed to look up thread memory", "error", err)
		} else if mem != nil {
			prior = &ribbit.PriorContext{
				Summary:  mem.TriageJSON,
				Response: mem.Response,
			}
			slog.Debug("using thread memory for follow-up", "thread", threadTS)
		}
	}

	resp, err := ribbitEngine.Respond(ctx, msg.Text, result, prior)
	if err != nil {
		slog.Error("ribbit generation failed", "error", err)
		slackClient.SwapReaction(msg.Channel, msg.Timestamp, "eyes", "warning")
		slackClient.ReplyInThread(msg.Channel, msg.ThreadTS(),
			":frog: I found this interesting but had trouble generating a response.")
		return
	}

	// Save thread memory for future follow-ups
	if stateManager.DB() != nil {
		if err := stateManager.DB().SaveThreadMemory(threadTS, msg.Channel, result.Summary, resp.Text); err != nil {
			slog.Warn("failed to save thread memory", "error", err)
		}
	}

	daemonCounters.ribbits.Add(1)
	slackClient.ReplyInThread(msg.Channel, msg.ThreadTS(), resp.Text)
	slackClient.SwapReaction(msg.Channel, msg.Timestamp, "eyes", "speech_balloon")
}

func handlePassive(
	ctx context.Context,
	msg *islack.IncomingMessage,
	triageEngine *triage.Engine,
	ribbitEngine *ribbit.Engine,
	slackClient *islack.Client,
	channelName string,
) {
	result, err := triageEngine.Classify(ctx, msg, channelName)
	if err != nil {
		slog.Debug("passive triage failed", "error", err)
		return
	}

	if !result.Actionable || result.Confidence <= 0.8 || result.Category != "bug" {
		slog.Debug("handler: triage not actionable, ignoring", "actionable", result.Actionable, "confidence", result.Confidence, "category", result.Category)
		return
	}

	daemonCounters.triages.Add(1)
	daemonCounters.triageBug.Add(1)
	slog.Info("high-confidence bug detected passively", "summary", result.Summary)

	resp, err := ribbitEngine.Respond(ctx, msg.Text, result, nil)
	if err != nil {
		slog.Warn("passive ribbit failed", "error", err)
		return
	}

	daemonCounters.ribbits.Add(1)
	slackClient.ReplyInThread(msg.Channel, msg.Timestamp,
		resp.Text+"\n\n_React :frog: if you'd like me to fix this._")
}

func handleTadpoleRequest(
	ctx context.Context,
	msg *islack.IncomingMessage,
	cfg *config.Config,
	triageEngine *triage.Engine,
	slackClient *islack.Client,
	stateManager *state.Manager,
	tadpolePool *tadpole.Pool,
	channelName string,
) {
	threadTS := msg.ThreadTS()

	// Atomically claim this thread — prevents duplicate tadpoles from racing
	// between the check and the eventual Track call inside Execute.
	if !stateManager.Claim(threadTS) {
		slackClient.ReplyInThread(msg.Channel, threadTS,
			":frog: Already working on this thread")
		return
	}
	// Unclaim on error so the thread can be retried
	claimed := true
	defer func() {
		if claimed {
			stateManager.Unclaim(threadTS)
		}
	}()

	// Fetch thread context to understand the full conversation
	threadMsgs, err := slackClient.FetchThreadMessages(msg.Channel, threadTS)
	if err != nil {
		slog.Warn("failed to fetch thread context for tadpole", "error", err)
		slackClient.ReplyInThread(msg.Channel, threadTS,
			":x: Couldn't fetch thread context")
		return
	}

	// Re-triage the thread content to get summary/category/size
	threadText := ""
	if len(threadMsgs) > 0 {
		threadText = threadMsgs[0] // original message
	}
	if threadText == "" {
		threadText = msg.Text
	}

	triageMsg := &islack.IncomingMessage{
		Text:          threadText,
		Channel:       msg.Channel,
		ThreadContext: threadMsgs,
	}

	triageResult, err := triageEngine.Classify(ctx, triageMsg, channelName)
	if err != nil {
		slog.Warn("tadpole triage failed, using defaults", "error", err)
		triageResult = &triage.Result{
			Actionable: true,
			Category:   "bug",
			Summary:    threadText,
			EstSize:    "small",
		}
	}

	task := tadpole.Task{
		Description:   threadText,
		Summary:       triageResult.Summary,
		Category:      triageResult.Category,
		EstSize:       triageResult.EstSize,
		SlackChannel:  msg.Channel,
		SlackThreadTS: threadTS,
		TriageResult:  triageResult,
	}

	slog.Info("spawning tadpole",
		"summary", task.Summary,
		"category", task.Category,
		"channel", channelName,
	)

	if err := tadpolePool.Spawn(ctx, task); err != nil {
		slog.Error("failed to spawn tadpole", "error", err)
		slackClient.ReplyInThread(msg.Channel, threadTS,
			":x: Failed to spawn tadpole: "+err.Error())
		return
	}
	// Spawn succeeded — Execute will call Track, which overwrites the placeholder.
	// Don't unclaim on defer.
	claimed = false
}

func checkClaude() error {
	_, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found in PATH — install it first: https://docs.anthropic.com/en/docs/claude-code")
	}
	return nil
}

func checkGH() error {
	_, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("gh CLI not found in PATH — install it first: https://cli.github.com")
	}
	return nil
}

