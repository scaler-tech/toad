package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/digest"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/ribbit"
	islack "github.com/scaler-tech/toad/internal/slack"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/tadpole"
	"github.com/scaler-tech/toad/internal/triage"
)

func handleMessage(
	ctx context.Context,
	msg *islack.IncomingMessage,
	cfg *config.Config,
	agentProvider agent.Provider,
	triageEngine *triage.Engine,
	ribbitEngine *ribbit.Engine,
	slackClient *islack.Client,
	stateManager *state.Manager,
	vacationState *state.VacationState,
	ribbitSem chan struct{},
	tadpolePool *tadpole.Pool,
	digestEngine *digest.Engine,
	tracker issuetracker.Tracker,
	resolver *config.Resolver,
	repoPaths map[string]string,
) {
	// Resolve channel name for context
	channelName := slackClient.ResolveChannelName(msg.Channel)

	if msg.IsMention && !msg.IsBot && canToggleVacation(cfg.VacationAdmins, msg.User) {
		if vacationState.Active() && isVacationEndPhrase(msg.Text) {
			handleVacationEnd(msg, slackClient, vacationState, channelName)
			return
		}
		if !vacationState.Active() && isVacationStartPhrase(msg.Text) {
			handleVacationStart(msg, slackClient, vacationState, channelName)
			return
		}
	}

	if vacationState.Active() {
		handleVacationMode(msg, slackClient, stateManager, channelName)
		return
	}

	// TADPOLE REQUEST: :frog: reaction on a toad reply
	// Must be checked BEFORE the bot filter — tadpole requests are reactions on
	// toad's own (bot) messages, so the fetched message will have IsBot=true.
	if msg.IsTadpoleRequest {
		slog.Info("handler: tadpole requested", "channel", channelName, "thread", msg.ThreadTS())
		handleTadpoleRequest(ctx, msg, triageEngine, slackClient, stateManager, tadpolePool, channelName, tracker, resolver, repoPaths)
		return
	}

	// EXPLICIT TRIGGER: @toad mention or reaction/keyword trigger (never from bots)
	if msg.IsMention || msg.IsTriggered {
		slog.Debug("handler: triggered path", "mention", msg.IsMention, "triggered", msg.IsTriggered, "channel", channelName)

		// Limit concurrent agent calls
		select {
		case ribbitSem <- struct{}{}:
			defer func() { <-ribbitSem }()
		case <-ctx.Done():
			return
		}

		handleTriggered(ctx, msg, cfg, agentProvider, triageEngine, ribbitEngine, slackClient, stateManager, tadpolePool, channelName, tracker, resolver, repoPaths)
		return
	}

	// Feed untriggered messages to digest engine (Toad King) for batch analysis.
	// This includes bot messages (Sentry alerts, CI failures, etc.) — the digest
	// will determine if they're actionable. Triggered messages are handled above.
	if digestEngine != nil {
		digestEngine.Collect(digest.Message{
			Channel:     msg.Channel,
			ChannelName: channelName,
			User:        msg.User,
			Text:        msg.Text,
			ThreadTS:    msg.ThreadTimestamp,
			Timestamp:   msg.Timestamp,
			BotID:       msg.BotID,
		})
	}

	// Skip bot messages from individual triage/passive monitoring
	if msg.IsBot {
		return
	}

	// PASSIVE MONITORING — skip when digest is enabled since it already batch-analyzes
	// all messages more efficiently than individual per-message triage calls.
	if digestEngine != nil {
		return
	}

	select {
	case ribbitSem <- struct{}{}:
		defer func() { <-ribbitSem }()
	default:
		slog.Debug("handler: skipping passive triage, at concurrency limit")
		return
	}

	slog.Debug("handler: passive path", "channel", channelName, "user", msg.User)
	handlePassive(ctx, msg, triageEngine, ribbitEngine, slackClient, channelName, resolver, repoPaths)
}

func handleTriggered(
	ctx context.Context,
	msg *islack.IncomingMessage,
	cfg *config.Config,
	agentProvider agent.Provider,
	triageEngine *triage.Engine,
	ribbitEngine *ribbit.Engine,
	slackClient *islack.Client,
	stateManager *state.Manager,
	tadpolePool *tadpole.Pool,
	channelName string,
	tracker issuetracker.Tracker,
	resolver *config.Resolver,
	repoPaths map[string]string,
) {
	// Check if already working on this thread
	threadTS := msg.ThreadTS()
	if existing := stateManager.GetByThread(threadTS); len(existing) > 0 {
		statuses := make([]string, len(existing))
		for i, r := range existing {
			statuses[i] = r.Status
		}
		slackClient.ReplyInThread(msg.Channel, threadTS,
			fmt.Sprintf(":frog: Already working on this thread (%d active: %s)", len(existing), strings.Join(statuses, ", ")))
		return
	}

	// Acknowledge
	slackClient.SetStatus(msg.Channel, threadTS, "Triaging message...")

	// Gather conversation context (retry once on failure)
	if msg.ThreadTimestamp != "" {
		threadMsgs, err := slackClient.FetchThreadMessages(msg.Channel, msg.ThreadTimestamp)
		if err != nil {
			slog.Warn("failed to fetch thread context, retrying", "error", err)
			time.Sleep(1 * time.Second)
			threadMsgs, err = slackClient.FetchThreadMessages(msg.Channel, msg.ThreadTimestamp)
		}
		if err != nil {
			slog.Warn("failed to fetch thread context after retry", "error", err)
		} else {
			msg.ThreadContext = threadMsgs
		}
	} else {
		recentMsgs, err := slackClient.FetchRecentMessages(msg.Channel, msg.Timestamp, 10)
		if err != nil {
			slog.Warn("failed to fetch channel context, retrying", "error", err)
			time.Sleep(1 * time.Second)
			recentMsgs, err = slackClient.FetchRecentMessages(msg.Channel, msg.Timestamp, 10)
		}
		if err != nil {
			slog.Warn("failed to fetch channel context after retry", "error", err)
		} else if len(recentMsgs) > 0 {
			msg.ThreadContext = recentMsgs
		}
	}

	// Enrich thread context by resolving any Linear ticket URLs/references
	// into full issue descriptions so triage and ribbit have real context.
	msg.ThreadContext = enrichWithIssueDetails(ctx, tracker, msg.Text, msg.ThreadContext)

	// Retry detection: if user says "try again" / "retry" in a thread with a previous
	// toad failure, skip triage and re-spawn directly.
	if isRetryIntent(msg.Text) && hasFailedTadpole(msg.ThreadContext) {
		slog.Info("retry intent detected", "channel", channelName, "thread", threadTS)

		if !stateManager.Claim(threadTS) {
			slackClient.ReplyInThread(msg.Channel, threadTS, ":frog: Already working on this thread")
			return
		}
		claimed := true
		defer func() {
			if claimed {
				stateManager.Unclaim(threadTS)
			}
		}()

		taskDescription := buildTaskDescription(msg.Text, msg.ThreadContext)
		repo := resolver.Resolve("", nil)
		if repo == nil {
			slackClient.ClearStatus(msg.Channel, threadTS)
			slackClient.ReplyInThread(msg.Channel, threadTS,
				":frog: I'm not sure which repo this is about — could you mention a file or project name?")
			return
		}

		task := tadpole.Task{
			Description:   taskDescription,
			Summary:       "retry: " + truncate(taskDescription, 60),
			Category:      "bug",
			EstSize:       "small",
			SlackChannel:  msg.Channel,
			SlackThreadTS: threadTS,
			Repo:          repo,
			RepoPaths:     repoPaths,
		}

		slackClient.SetStatus(msg.Channel, threadTS, "Spawning tadpole...")

		if err := tadpolePool.Spawn(ctx, task); err != nil {
			slog.Error("retry spawn failed", "error", err)
			slackClient.ClearStatus(msg.Channel, threadTS)
			slackClient.React(msg.Channel, msg.Timestamp, "warning")
			slackClient.ReplyInThread(msg.Channel, threadTS,
				":x: Failed to spawn tadpole: "+err.Error())
			return
		}
		claimed = false
		return
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
		slog.Info("handler: triage said not actionable, asking for clarification",
			"confidence", result.Confidence, "summary", result.Summary)
		slackClient.ClearStatus(msg.Channel, threadTS)
		slackClient.ReplyInThread(msg.Channel, msg.ThreadTS(),
			fmt.Sprintf(":frog: I'd like to help, but I'm not sure exactly what to change — %s\n\n"+
				"Could you add more detail about the desired behavior? "+
				"Reply in this thread and `@toad` me to try again.",
				result.Summary))
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

	// INVESTIGATE + SPAWN: bugs and features go through an investigation gate before spawning.
	// Sonnet verifies the request is a real code change with enough context. If not, we fall
	// through to ribbit — the user gets a helpful reply instead of a wasted PR.
	if (result.Category == "bug" || result.Category == "feature") && result.Confidence >= 0.5 {
		slog.Info("investigating before spawn", "summary", result.Summary, "category", result.Category)

		taskText := buildTaskDescription(msg.Text, msg.ThreadContext)

		slackClient.SetStatus(msg.Channel, threadTS, "Investigating the codebase...")

		investigation, err := investigateTriggered(ctx, cfg, agentProvider, result, taskText, channelName, resolver)
		if err != nil {
			slog.Warn("triggered investigation failed, falling through to ribbit",
				"error", err, "summary", result.Summary)
			// Fall through to ribbit below
		} else if !investigation.Feasible {
			slog.Info("investigation says not feasible, falling through to ribbit",
				"reasoning", investigation.Reasoning, "summary", result.Summary)
			// Fall through to ribbit below
		} else {
			slog.Info("investigation approved, spawning tadpole",
				"summary", result.Summary, "reasoning", investigation.Reasoning)

			if !stateManager.Claim(threadTS) {
				slackClient.ReplyInThread(msg.Channel, threadTS, ":frog: Already working on this thread")
				return
			}
			claimed := true
			defer func() {
				if claimed {
					stateManager.Unclaim(threadTS)
				}
			}()

			// Use the refined task_spec from investigation — more precise than raw message
			taskDescription := investigation.TaskSpec

			issueRef := tracker.ExtractIssueRef(taskDescription)
			if issueRef == nil {
				issueRef = tracker.ExtractIssueRef(msg.Text)
			}
			if issueRef == nil && tracker.ShouldCreateIssues() {
				ref, err := tracker.CreateIssue(ctx, issuetracker.CreateIssueOpts{
					Title:       result.Summary,
					Description: taskDescription,
					Category:    result.Category,
				})
				if err != nil {
					slog.Warn("failed to create issue", "error", err, "summary", result.Summary)
				} else {
					issueRef = ref
				}
			}

			// Ticket assignee gate: if the ticket is actively assigned,
			// post findings to the ticket instead of spawning a tadpole.
			if cfg.IssueTracker.RespectAssignees && issueRef != nil {
				permalink, _ := slackClient.GetPermalink(msg.Channel, threadTS)
				gate := issuetracker.CheckAssigneeGate(ctx, tracker, issuetracker.GateOpts{
					IssueRef:       issueRef,
					StaleDays:      cfg.IssueTracker.StaleDays,
					Findings:       taskDescription + "\n\n**Reasoning:** " + investigation.Reasoning,
					SlackPermalink: permalink,
				})
				if gate.Gated {
					slackClient.ClearStatus(msg.Channel, threadTS)
					if gate.Done {
						slog.Info("ticket is done, skipping silently",
							"issue", issueRef.ID, "state", gate.Status.State)
					} else {
						slackClient.ReplyInThread(msg.Channel, threadTS,
							fmt.Sprintf(":clipboard: %s is assigned to %s — I posted my findings as a comment on the ticket. "+
								"Say `@toad fix this` if you'd like me to open a PR anyway.",
								issueRef.ID, gate.Status.AssigneeName))
					}
					return
				}
			}

			repo := resolver.Resolve(result.Repo, result.FilesHint)
			if repo == nil {
				slackClient.ClearStatus(msg.Channel, threadTS)
				slackClient.ReplyInThread(msg.Channel, threadTS,
					":frog: I'm not sure which repo this is about — could you mention a file or project name?")
				return
			}

			task := tadpole.Task{
				Description:   taskDescription,
				Summary:       result.Summary,
				Category:      result.Category,
				EstSize:       result.EstSize,
				SlackChannel:  msg.Channel,
				SlackThreadTS: threadTS,
				TriageResult:  result,
				IssueRef:      issueRef,
				Repo:          repo,
				RepoPaths:     repoPaths,
			}

			slackClient.SetStatus(msg.Channel, threadTS, "Spawning tadpole...")

			if err := tadpolePool.Spawn(ctx, task); err != nil {
				slog.Error("auto-spawn failed", "error", err)
				slackClient.ClearStatus(msg.Channel, threadTS)
				slackClient.React(msg.Channel, msg.Timestamp, "warning")
				slackClient.ReplyInThread(msg.Channel, threadTS,
					":x: Failed to spawn tadpole: "+err.Error())
				return
			}
			claimed = false
			return
		}
	}

	// Resolve repo for ribbit
	repo := resolver.Resolve(result.Repo, result.FilesHint)

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

	repoPath := ""
	defaultBranch := "main"
	if repo != nil {
		repoPath = repo.Path
		defaultBranch = repo.DefaultBranch
	}
	slackClient.SetStatus(msg.Channel, threadTS, "Reading the codebase...")
	resp, err := ribbitEngine.Respond(ctx, msg.Text, result, prior, repoPath, defaultBranch, repoPaths)
	if err != nil {
		slog.Error("ribbit generation failed", "error", err)
		slackClient.ClearStatus(msg.Channel, threadTS)
		slackClient.React(msg.Channel, msg.Timestamp, "warning")
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
	if result.Category == "bug" || result.Category == "feature" {
		blocks := islack.FixThisBlocks(resp.Text, msg.ThreadTS())
		if _, err := slackClient.ReplyInThreadWithBlocks(msg.Channel, msg.ThreadTS(), resp.Text, blocks); err != nil {
			slog.Warn("ribbit reply failed", "error", err)
		}
	} else {
		slackClient.ReplyInThread(msg.Channel, msg.ThreadTS(), resp.Text)
	}
	slackClient.React(msg.Channel, msg.Timestamp, "speech_balloon")
}

func handlePassive(
	ctx context.Context,
	msg *islack.IncomingMessage,
	triageEngine *triage.Engine,
	ribbitEngine *ribbit.Engine,
	slackClient *islack.Client,
	channelName string,
	resolver *config.Resolver,
	repoPaths map[string]string,
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

	repo := resolver.Resolve(result.Repo, result.FilesHint)
	repoPath := ""
	defaultBranch := "main"
	if repo != nil {
		repoPath = repo.Path
		defaultBranch = repo.DefaultBranch
	}

	resp, err := ribbitEngine.Respond(ctx, msg.Text, result, nil, repoPath, defaultBranch, repoPaths)
	if err != nil {
		slog.Warn("passive ribbit failed", "error", err)
		return
	}

	daemonCounters.ribbits.Add(1)
	blocks := islack.FixThisBlocks(resp.Text, msg.ThreadTS())
	if _, err := slackClient.ReplyInThreadWithBlocks(msg.Channel, msg.Timestamp, resp.Text, blocks); err != nil {
		slog.Warn("passive ribbit reply failed", "error", err)
	}
}

func handleTadpoleRequest(
	ctx context.Context,
	msg *islack.IncomingMessage,
	triageEngine *triage.Engine,
	slackClient *islack.Client,
	stateManager *state.Manager,
	tadpolePool *tadpole.Pool,
	channelName string,
	tracker issuetracker.Tracker,
	resolver *config.Resolver,
	repoPaths map[string]string,
) {
	threadTS := msg.ThreadTS()
	slackClient.SetStatus(msg.Channel, threadTS, "Triaging message...")

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
		slog.Warn("failed to fetch thread context for tadpole, retrying", "error", err)
		time.Sleep(1 * time.Second)
		threadMsgs, err = slackClient.FetchThreadMessages(msg.Channel, threadTS)
	}
	if err != nil {
		slog.Warn("failed to fetch thread context for tadpole after retry", "error", err)
		slackClient.ClearStatus(msg.Channel, threadTS)
		slackClient.ReplyInThread(msg.Channel, threadTS,
			":x: Couldn't fetch thread context")
		return
	}

	// Use the incoming message text as the primary context. For CTA button clicks
	// this is the investigation finding (not the thread root), so the tadpole knows
	// exactly what to fix.
	threadText := msg.Text
	if threadText == "" && len(threadMsgs) > 0 {
		threadText = threadMsgs[0]
	}

	// Enrich thread context by resolving any Linear ticket URLs/references
	threadMsgs = enrichWithIssueDetails(ctx, tracker, threadText, threadMsgs)

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

	// Detect issue tracker reference (no creation for explicit requests)
	taskDesc := buildTaskDescription(threadText, threadMsgs)
	issueRef := tracker.ExtractIssueRef(taskDesc)

	// Resolve repo from triage
	repo := resolver.Resolve(triageResult.Repo, triageResult.FilesHint)
	if repo == nil {
		slackClient.ClearStatus(msg.Channel, threadTS)
		slackClient.ReplyInThread(msg.Channel, threadTS,
			":frog: I'm not sure which repo this is about — could you mention a file or project name?")
		return
	}

	task := tadpole.Task{
		Description:   taskDesc,
		Summary:       triageResult.Summary,
		Category:      triageResult.Category,
		EstSize:       triageResult.EstSize,
		SlackChannel:  msg.Channel,
		SlackThreadTS: threadTS,
		TriageResult:  triageResult,
		IssueRef:      issueRef,
		Repo:          repo,
		RepoPaths:     repoPaths,
	}

	slog.Info("spawning tadpole",
		"summary", task.Summary,
		"category", task.Category,
		"channel", channelName,
	)
	slackClient.SetStatus(msg.Channel, threadTS, "Spawning tadpole...")

	if err := tadpolePool.Spawn(ctx, task); err != nil {
		slog.Error("failed to spawn tadpole", "error", err)
		slackClient.ClearStatus(msg.Channel, threadTS)
		slackClient.ReplyInThread(msg.Channel, threadTS,
			":x: Failed to spawn tadpole: "+err.Error())
		return
	}
	// Spawn succeeded — Execute will call Track, which overwrites the placeholder.
	// Don't unclaim on defer.
	claimed = false
}

const vacationReply = ":frog: Sorry I am not allowed to do things anymore, because I'm an idiot :(. But I have saved your message! If there is something else you want me to save, e.g. ideas on how to improve me, let me know."

const vacationReplySaveFailed = ":frog: Sorry I am not allowed to do things anymore, because I'm an idiot :(. I tried to save your message but even that failed — please tell a human."

const vacationStartReply = ":frog: :palm_tree: Thanks! I hop away to rest a spell,\nto come back energized and well,\nand when I leap back to my pond again,\nI'll bless you with my findings then! :sparkles:"

const vacationEndReply = ":frog: I'm back! Rested, energized, and ready to bless you with findings again. :sparkles:"

const vacationLockedReply = ":frog: My vacation is locked on by config (`vacation_mode: true`), so I can't come back until that changes."

func handleVacationStart(msg *islack.IncomingMessage, slackClient *islack.Client, vacationState *state.VacationState, channelName string) {
	slog.Info("vacation mode enabled via slack", "channel", channelName, "user", msg.User)
	if err := vacationState.Set(true); err != nil {
		slog.Warn("failed to persist vacation state", "error", err)
	}
	slackClient.ReplyInThread(msg.Channel, msg.ThreadTS(), vacationStartReply)
}

func handleVacationEnd(msg *islack.IncomingMessage, slackClient *islack.Client, vacationState *state.VacationState, channelName string) {
	if vacationState.Forced() {
		slackClient.ReplyInThread(msg.Channel, msg.ThreadTS(), vacationLockedReply)
		return
	}
	slog.Info("vacation mode disabled via slack", "channel", channelName, "user", msg.User)
	if err := vacationState.Set(false); err != nil {
		slog.Warn("failed to persist vacation state", "error", err)
	}
	slackClient.ReplyInThread(msg.Channel, msg.ThreadTS(), vacationEndReply)
}

func isVacationStartPhrase(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "go on vacation") || strings.Contains(t, "vacation time")
}

func isVacationEndPhrase(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "come back") || strings.Contains(t, "back from vacation") || strings.Contains(t, "vacation is over")
}

func canToggleVacation(admins []string, userID string) bool {
	if len(admins) == 0 {
		return true
	}
	return slices.Contains(admins, userID)
}

func handleVacationMode(msg *islack.IncomingMessage, slackClient *islack.Client, stateManager *state.Manager, channelName string) {
	if !isDirectInteraction(msg) {
		return
	}

	slog.Info("vacation mode: declining interaction", "channel", channelName, "user", msg.User)

	reply := vacationReply
	if err := stateManager.SaveVacationMessage(msg.Channel, channelName, msg.User, msg.Text, msg.ThreadTS()); err != nil {
		slog.Warn("vacation mode: failed to save message", "error", err)
		reply = vacationReplySaveFailed
	}
	slackClient.ReplyInThread(msg.Channel, msg.ThreadTS(), reply)
}

func isDirectInteraction(msg *islack.IncomingMessage) bool {
	if msg.IsTadpoleRequest {
		return true
	}
	return (msg.IsMention || msg.IsTriggered) && !msg.IsBot
}
