package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/digest"
	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/ribbit"
	islack "github.com/scaler-tech/toad/internal/slack"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/ticket"
	"github.com/scaler-tech/toad/internal/triage"
)

func handleMessage(
	ctx context.Context,
	msg *islack.IncomingMessage,
	triageEngine *triage.Engine,
	ribbitEngine *ribbit.Engine,
	slackClient *islack.Client,
	stateManager *state.Manager,
	ribbitSem chan struct{},
	investigateSem chan struct{},
	digestEngine *digest.Engine,
	tracker issuetracker.Tracker,
	resolver *config.Resolver,
	repoPaths map[string]string,
	investRunner *investigation.Runner,
	ticketEngine *ticket.Engine,
	botAllowlist []string,
) {
	// Resolve channel name for context
	channelName := slackClient.ResolveChannelName(msg.Channel)

	// TICKET REQUEST: :frog: reaction on a toad reply
	// Must be checked BEFORE the bot filter — ticket requests are reactions on
	// toad's own (bot) messages, so the fetched message will have IsBot=true.
	if msg.IsTicketRequest {
		slog.Info("handler: ticket requested", "channel", channelName, "thread", msg.ThreadTS())
		handleTicketRequest(ctx, msg, slackClient, nil, stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, channelName, ticket.SourceCTA)
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

		handleTriggered(ctx, msg, triageEngine, ribbitEngine, slackClient, stateManager, channelName, tracker, resolver, repoPaths, investRunner, ticketEngine, investigateSem)
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

	if msg.IsBot {
		// Non-allowlisted bots are dropped from individual triage/passive
		// monitoring (digest still saw them above). Allowlisted intake bots
		// (e.g. a Sentry app) feed intake, never conversation (controller
		// ruling): no ribbitSem (a bot alert storm must never starve human
		// @toad mentions of a ribbit slot), no conversational replies
		// ("ask for clarification", ribbit fallback) — just triage, and only
		// an actionable bug/feature proceeds into the investigate-and-file
		// flow, bounded by investigateSem. handleBotIntake silently drops
		// everything else.
		if !slices.Contains(botAllowlist, msg.BotID) {
			return
		}

		slog.Debug("handler: allowlisted bot message, routing to intake", "bot_id", msg.BotID, "channel", channelName)
		handleBotIntake(ctx, msg, triageEngine, slackClient, stateManager, channelName, tracker, resolver, investRunner, ticketEngine, investigateSem)
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
	triageEngine *triage.Engine,
	ribbitEngine *ribbit.Engine,
	slackClient *islack.Client,
	stateManager *state.Manager,
	channelName string,
	tracker issuetracker.Tracker,
	resolver *config.Resolver,
	repoPaths map[string]string,
	investRunner *investigation.Runner,
	ticketEngine *ticket.Engine,
	investigateSem chan struct{},
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

	// Triage — fast Haiku classification (~1s) to decide category for ribbit.
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

	// ESCALATE: triage flagged this as needing a ticket regardless of
	// category/confidence — route to the same investigate-and-file flow the
	// CTA button uses, just with a different Source for the ticket index.
	if result.Escalate {
		slog.Info("triage escalate flag set, routing to ticket flow", "summary", result.Summary)
		handleTicketRequest(ctx, msg, slackClient, result, stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem, channelName, ticket.SourceEscalation)
		return
	}

	// INVESTIGATE + FILE: bugs and features go through a read-only
	// investigation gate before a ticket is filed or proposed. An infeasible
	// (or errored) investigation falls through to the unchanged ribbit path
	// below — v1 semantics.
	if (result.Category == "bug" || result.Category == "feature") && result.Confidence >= 0.5 {
		if !stateManager.Claim(threadTS) {
			slackClient.ReplyInThread(msg.Channel, threadTS, ":frog: Already working on this thread")
			return
		}

		slog.Info("investigating before filing", "summary", result.Summary, "category", result.Category)
		slackClient.SetStatus(msg.Channel, threadTS, "Investigating the codebase...")

		outcome := runTriggeredInvestigation(ctx, msg, result, channelName, threadTS,
			stateManager, tracker, resolver, investRunner, ticketEngine, investigateSem)

		if outcome.Kind != outcomeFallThrough {
			slackClient.ClearStatus(msg.Channel, threadTS)
			if outcome.Kind == outcomeProposed {
				blocks := islack.TicketBlocks(outcome.ReplyText, threadTS)
				if _, err := slackClient.ReplyInThreadWithBlocks(msg.Channel, threadTS, outcome.ReplyText, blocks); err != nil {
					slog.Warn("investigation reply failed", "error", err)
				}
			} else if _, err := slackClient.ReplyInThread(msg.Channel, threadTS, outcome.ReplyText); err != nil {
				slog.Warn("investigation reply failed", "error", err)
			}
			return
		}
		// outcomeFallThrough: claim already released inside
		// runTriggeredInvestigation — fall through to ribbit below.
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
		blocks := islack.TicketBlocks(resp.Text, msg.ThreadTS())
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
	blocks := islack.TicketBlocks(resp.Text, msg.ThreadTS())
	if _, err := slackClient.ReplyInThreadWithBlocks(msg.Channel, msg.Timestamp, resp.Text, blocks); err != nil {
		slog.Warn("passive ribbit reply failed", "error", err)
	}
}
