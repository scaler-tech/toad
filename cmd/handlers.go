package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/scaler-tech/toad/internal/digest"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/responder"
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
	deps flowDeps,
	ribbitSem chan struct{},
	digestEngine *digest.Engine,
	digestGate *digestChannelGate,
	repoPaths map[string]string,
	botAllowlist []string,
) {
	// "messages seen" for the dashboard's Intake pipeline stage/sparkline —
	// every message toad's handler dispatches on, regardless of what happens
	// next (triaged, filtered, routed to digest, etc.). Best-effort; never
	// blocks the flow (see incrementMetric's doc comment).
	incrementMetric(deps.stateManager.DB(), "intake")

	// Resolve channel name for context
	channelName := slackClient.ResolveChannelName(msg.Channel)

	// TICKET REQUEST: "Create Linear ticket" CTA button click on a toad reply
	// Must be checked BEFORE the bot filter — the button's underlying message
	// is usually toad's own (bot) reply, so the fetched message will often
	// have IsBot=true.
	if msg.IsTicketRequest {
		slog.Info("handler: ticket requested", "channel", channelName, "thread", msg.ThreadTS())
		// sentryCorroborated uses the single corroboration mechanism (see
		// isSentryCorroborated's doc comment in ticketflow.go). This is NOT
		// always false in practice: the button path
		// (internal/slack/interactive.go's handleInteractive) fetches the
		// original thread anchor at threadTS via FetchMessage — which can
		// legitimately be an allowlisted monitoring bot's message, not
		// toad's own. Computed rather than hardcoded so the rule stays in
		// one place regardless of which entry path produced msg.
		sentryCorroborated := isSentryCorroborated(msg.BotID, botAllowlist)
		// nil issueDetails: a bare CTA click has no thread context
		// fetched yet — handleTicketRequest's own guard enriches it from
		// scratch when ThreadContext is empty.
		handleTicketRequest(ctx, msg, slackClient, nil, deps, nil, channelName, ticket.SourceCTA, sentryCorroborated)
		return
	}

	// EXPLICIT TRIGGER: @toad mention or keyword trigger
	if msg.IsMention || msg.IsTriggered {
		slog.Debug("handler: triggered path", "mention", msg.IsMention, "triggered", msg.IsTriggered, "channel", channelName)

		// Limit concurrent agent calls. The slot is released inside
		// handleTriggered itself (not deferred here) — see its doc comment:
		// a message that turns into a long investigation releases ribbitSem
		// early (investigateSem bounds that part instead) so it doesn't
		// starve Q&A behind it; a message that stays on the ribbit-answer
		// path keeps the slot for its whole (short) duration.
		select {
		case ribbitSem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		handleTriggered(ctx, msg, triageEngine, ribbitEngine, slackClient, deps, channelName, repoPaths, ribbitSem, botAllowlist)
		return
	}

	// Feed untriggered messages to digest engine (Toad King) for batch analysis.
	// This includes bot messages (Sentry alerts, CI failures, etc.) — the digest
	// will determine if they're actionable. Triggered messages are handled above.
	// digestGate.enabled gates on the CHANNEL ID (not name) — it's backed by
	// dashboard-writable settings rows the operator uses to opt noisy/
	// marketing/personal channels out of passive analysis without a config
	// change or restart (see digestgate.go).
	if digestEngine != nil && digestGate.enabled(msg.Channel) {
		digestEngine.Collect(digest.Message{
			Channel:         msg.Channel,
			ChannelName:     channelName,
			User:            msg.User,
			Text:            msg.Text,
			ThreadTS:        msg.ThreadTimestamp,
			Timestamp:       msg.Timestamp,
			BotID:           msg.BotID,
			IsMonitoringBot: slices.Contains(botAllowlist, msg.BotID),
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
		// This message just passed the allowlist check above, so it's the
		// one case where external corroboration is genuine — see
		// isSentryCorroborated's doc comment (ticketflow.go).
		sentryCorroborated := isSentryCorroborated(msg.BotID, botAllowlist)
		handleBotIntake(ctx, msg, triageEngine, slackClient, deps, channelName, sentryCorroborated)
		return
	}

	// PASSIVE MONITORING — skip when digest is enabled since it already batch-analyzes
	// all messages more efficiently than individual per-message triage calls.
	if digestEngine != nil {
		return
	}

	select {
	case ribbitSem <- struct{}{}:
	default:
		slog.Debug("handler: skipping passive triage, at concurrency limit")
		return
	}

	// Unlike above, the ribbitSem slot's release is NOT deferred here:
	// handlePassive can now route into the same slow ticket flow
	// handleTriggered's escalate branch uses (Important fix I2), which is
	// bounded by investigateSem instead and can run for minutes — holding
	// ribbitSem for that whole duration would starve concurrent ribbit Q&A
	// behind it, the same rationale as handleTriggered's releaseRibbitSem.
	// handlePassive owns releasing this slot end to end (mirroring
	// handleTriggered's releaseRibbitSem discipline internally).
	slog.Debug("handler: passive path", "channel", channelName, "user", msg.User)
	handlePassive(ctx, msg, triageEngine, ribbitEngine, slackClient, channelName, deps, repoPaths, ribbitSem, botAllowlist)
}

func handleTriggered(
	ctx context.Context,
	msg *islack.IncomingMessage,
	triageEngine *triage.Engine,
	ribbitEngine *ribbit.Engine,
	slackClient *islack.Client,
	deps flowDeps,
	channelName string,
	repoPaths map[string]string,
	ribbitSem chan struct{},
	botAllowlist []string,
) {
	// The caller (handleMessage) has already acquired ribbitSem before
	// dispatching here. Single-release discipline: releaseRibbitSem is the
	// only place that gives the slot back, guarded by released so it's a
	// no-op if called twice. It's called explicitly right before either
	// investigation-shaped branch below (escalate, investigate-and-file) —
	// both are bounded by investigateSem instead and can run for minutes,
	// so holding ribbitSem for that duration would starve concurrent
	// ribbit Q&A behind it — and is NEVER re-acquired afterward, even if an
	// investigation falls through to the ribbit path below. The deferred
	// call is what releases it for the (fast) plain ribbit-answer path,
	// where neither investigation branch runs.
	released := false
	releaseRibbitSem := func() {
		if !released {
			released = true
			<-ribbitSem
		}
	}
	defer releaseRibbitSem()

	threadTS := msg.ThreadTS()

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
	// issueDetails is threaded through to runTriggeredInvestigation/
	// handleTicketRequest below so buildTicketContextBlock (ticketflow.go)
	// can reuse these already-fetched details instead of re-extracting and
	// re-fetching the same tickets from the tracker.
	var issueDetails []issuetracker.IssueDetails
	msg.ThreadContext, issueDetails = enrichWithIssueDetails(ctx, deps.tracker, msg.Text, msg.ThreadContext)

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
	case categoryBug:
		daemonCounters.triageBug.Add(1)
	case categoryFeature:
		daemonCounters.triageFeature.Add(1)
	case "question":
		daemonCounters.triageQuestion.Add(1)
	default:
		daemonCounters.triageOther.Add(1)
	}

	// sentryCorroborated gates every ticket-identity/automation use of
	// msg.SentryRefs and the investigation's Findings.SentryIssueIDs below
	// — see isSentryCorroborated's doc comment (ticketflow.go) for the full
	// rule. This branch is reached for @toad mentions and reaction/keyword
	// triggers — computed rather than assumed false, since a bot can still
	// @mention toad directly (handleAppMention sets IsBot from the event).
	sentryCorroborated := isSentryCorroborated(msg.BotID, botAllowlist)

	// An explicit ticket request must never depend on the triage model call:
	// when triage times out (or misses the phrasing), escalate defaults to
	// false and the request would silently get a Q&A answer instead of a
	// ticket. The deterministic phrase check backstops exactly the explicit
	// phrasings the triage prompt defines for the escalate flag — see
	// shouldForceEscalateForTicketRequest's doc comment for why this must
	// never fire on bot-authored text.
	if shouldForceEscalateForTicketRequest(msg, result.Escalate) {
		slog.Info("explicit ticket-request phrasing detected, forcing escalate", "summary", result.Summary)
		result.Escalate = true
	}

	// ESCALATE: triage flagged this as needing a ticket regardless of
	// category/confidence — route to the same investigate-and-file flow the
	// CTA button uses, just with a different Source for the ticket index.
	if result.Escalate {
		slog.Info("triage escalate flag set, routing to ticket flow", "summary", result.Summary)
		releaseRibbitSem()
		handleTicketRequest(ctx, msg, slackClient, result, deps, issueDetails, channelName, ticket.SourceEscalation, sentryCorroborated)
		return
	}

	// INVESTIGATE + FILE: bugs and features go through a read-only
	// investigation gate before a ticket is filed or proposed. An infeasible
	// (or errored) investigation falls through to the unchanged ribbit path
	// below — v1 semantics.
	//
	// Follow-ups in threads toad already answered converse below instead
	// (the responder sees the prior findings via prior.PriorFindings) —
	// only a first-touch bug/feature REPORT investigates.
	if shouldInvestigateFirstTouch(result) && !hasPriorThreadState(deps.stateManager.DB(), threadTS) {
		if !deps.stateManager.Claim(threadTS) {
			slackClient.ReplyInThread(msg.Channel, threadTS, ":frog: Already working on this thread")
			return
		}

		slog.Info("investigating before filing", "summary", result.Summary, "category", result.Category)
		slackClient.SetStatus(msg.Channel, threadTS, "Investigating the codebase...")
		releaseRibbitSem()

		outcome := runTriggeredInvestigation(ctx, msg, result, channelName, threadTS, deps, issueDetails, sentryCorroborated)

		if outcome.Kind != outcomeFallThrough {
			slackClient.ClearStatus(msg.Channel, threadTS)
			// See ReplyWithOptionalCTA's doc comment: the button is
			// suppressed when the tracker can't actually create issues.
			showCTA := outcome.Kind == outcomeProposed && deps.ticketEngine.ShouldCreateIssues()
			if _, err := slackClient.ReplyWithOptionalCTA(msg.Channel, threadTS, outcome.ReplyText, showCTA); err != nil {
				slog.Warn("investigation reply failed", "error", err)
			}
			return
		}
		// outcomeFallThrough: claim already released inside
		// runTriggeredInvestigation — fall through to ribbit below.
	}

	// Resolve repo for ribbit
	repo := deps.resolver.Resolve(result.Repo, result.FilesHint)

	// RIBBIT: questions, refactors, and other categories get a codebase-aware reply
	slog.Info("generating ribbit", "summary", result.Summary, "category", result.Category)

	// Look up prior thread memory for coherent follow-ups
	var prior *ribbit.PriorContext
	if deps.stateManager.DB() != nil {
		mem, err := deps.stateManager.DB().GetThreadMemory(threadTS)
		if err != nil {
			slog.Warn("failed to look up thread memory", "error", err)
		} else if mem != nil {
			prior = &ribbit.PriorContext{
				Summary:  mem.TriageJSON,
				Response: mem.Response,
			}
			slog.Debug("using thread memory for follow-up", "thread", threadTS)
		}
		if rec, err := deps.stateManager.DB().GetInvestigationByThread(threadTS); err == nil && rec != nil {
			if prior == nil {
				prior = &ribbit.PriorContext{}
			}
			prior.PriorFindings = renderPriorFindings(rec)
		}
	}

	repoPath := ""
	defaultBranch := "main"
	if repo != nil {
		repoPath = repo.Path
		defaultBranch = repo.DefaultBranch
	}
	slackClient.SetStatus(msg.Channel, threadTS, "Reading the codebase...")
	resp, err := ribbitEngine.Respond(ctx, msg.Text, result, msg.ThreadContext, prior, repoPath, defaultBranch, repoPaths)
	if err != nil {
		slog.Error("ribbit generation failed", "error", err)
		slackClient.ClearStatus(msg.Channel, threadTS)
		slackClient.React(msg.Channel, msg.Timestamp, "warning")
		slackClient.ReplyInThread(msg.Channel, msg.ThreadTS(),
			":frog: I found this interesting but had trouble generating a response.")
		return
	}

	// Save thread memory for future follow-ups
	if deps.stateManager.DB() != nil {
		if err := deps.stateManager.DB().SaveThreadMemory(threadTS, msg.Channel, result.Summary, resp.Text); err != nil {
			slog.Warn("failed to save thread memory", "error", err)
		}
	}

	daemonCounters.ribbits.Add(1)
	incrementMetric(deps.stateManager.DB(), "qa")
	// See ReplyWithOptionalCTA's doc comment: the button is suppressed when
	// the tracker can't actually create issues.
	showCTA := (result.Category == categoryBug || result.Category == categoryFeature) && deps.ticketEngine.ShouldCreateIssues()
	if _, err := slackClient.ReplyWithOptionalCTA(msg.Channel, msg.ThreadTS(), resp.Text, showCTA); err != nil {
		slog.Warn("ribbit reply failed", "error", err)
	}
	slackClient.React(msg.Channel, msg.Timestamp, "speech_balloon")

	// Apply a proposed safe ticket edit (explicit asks only — the responder
	// envelope carries one only when the teammate asked). Slack has no
	// implicit ticket, so a missing issue name means nothing to apply.
	if !resp.TicketUpdate.IsZero() {
		if resp.TicketUpdate.Issue == "" {
			slog.Warn("responder proposed a ticket update without naming an issue; skipping")
		} else if !issueReferencedByHumans(deps.tracker, append([]string{msg.Text}, msg.ThreadContext...), resp.TicketUpdate.Issue) {
			slog.Warn("responder proposed update to an issue nobody referenced; skipping",
				"issue", resp.TicketUpdate.Issue, "channel", msg.Channel, "thread", threadTS)
		} else if err := applySlackTicketUpdate(ctx, deps, resp.TicketUpdate); err != nil {
			slog.Warn("applying ticket update from slack responder", "issue", resp.TicketUpdate.Issue, "error", err)
			slackClient.ReplyInThread(msg.Channel, msg.ThreadTS(), ":warning: I could not update "+resp.TicketUpdate.Issue+": "+firstLine(err.Error()))
		}
	}

	// Persist investigated findings so later follow-ups and the CTA path
	// see them.
	if resp.DidInvestigate && resp.FindingsSummary != "" && deps.stateManager.DB() != nil {
		ej, _ := json.Marshal(responder.Envelope{Reply: resp.Text, DidInvestigate: true, FindingsSummary: resp.FindingsSummary})
		if err := deps.stateManager.DB().SaveInvestigation(&state.InvestigationRecord{
			ID:           fmt.Sprintf("slackresp-%s-%d", threadTS, time.Now().UnixNano()),
			ThreadTS:     threadTS,
			Channel:      msg.Channel,
			FindingsJSON: string(ej),
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			slog.Warn("persisting slack responder findings", "error", err)
		}
	}
}

// handlePassive is the untriggered (no @mention/reaction/keyword trigger)
// monitoring path — only reached when digest is disabled (see the caller's
// guard in handleMessage). It now owns releasing the caller's ribbitSem slot
// itself (the caller no longer defers the release) because the escalate/
// ticket-request branch below routes into the same slow investigate-and-file
// flow handleTriggered's escalate branch uses — see releaseRibbitSem's
// pattern in handleTriggered for the identical rationale (that flow is
// bounded by investigateSem, not ribbitSem, and can run for minutes).
func handlePassive(
	ctx context.Context,
	msg *islack.IncomingMessage,
	triageEngine *triage.Engine,
	ribbitEngine *ribbit.Engine,
	slackClient *islack.Client,
	channelName string,
	deps flowDeps,
	repoPaths map[string]string,
	ribbitSem chan struct{},
	botAllowlist []string,
) {
	// Single-release discipline, mirroring handleTriggered's
	// releaseRibbitSem/released pair exactly: the deferred call covers the
	// fast, plain-ribbit-answer path below (and the "not actionable"/"triage
	// failed" early returns); the escalate/ticket-request branch releases
	// early before handing off to the slow ticket flow.
	released := false
	releaseRibbitSem := func() {
		if !released {
			released = true
			<-ribbitSem
		}
	}
	defer releaseRibbitSem()

	result, err := triageEngine.Classify(ctx, msg, channelName)
	if err != nil {
		slog.Debug("passive triage failed", "error", err)
		return
	}

	// Important fix (I2): handlePassive never read result.Escalate nor
	// checked the explicit ticket-request phrase backstop, so a passively-
	// observed message (no @mention/reaction) that explicitly asked for a
	// ticket — or that triage itself flagged Escalate — fell straight into
	// the bug/confidence filter below, where it was almost always dropped
	// as "not a high-confidence bug" instead of ever reaching the ticket
	// flow.
	//
	// !msg.IsBot && (result.Escalate || isExplicitTicketRequest(...)) is the
	// exact same shape as shouldForceEscalateForTicketRequest's condition
	// (ticketflow.go, C2's fix) generalized to also cover Escalate==true —
	// that helper only guards the "escalate is currently false, does the
	// phrase match" half, since handleTriggered already has a separate `if
	// result.Escalate` branch of its own it falls into unconditionally.
	// handlePassive has no such separate branch, so both conditions are
	// checked together here. !msg.IsBot carries the identical rationale:
	// bot-authored text must never trigger an unreviewed escalation.
	if !msg.IsBot && (result.Escalate || isExplicitTicketRequest(msg.Text)) {
		slog.Info("handler: passive escalate/ticket-request detected, routing to ticket flow", "summary", result.Summary)
		releaseRibbitSem()
		sentryCorroborated := isSentryCorroborated(msg.BotID, botAllowlist)
		// nil issueDetails: handleTicketRequest's own guard fetches and
		// enriches thread context from scratch when it's empty, same as a
		// bare CTA click — handlePassive never fetched thread context up
		// front the way handleTriggered does.
		handleTicketRequest(ctx, msg, slackClient, result, deps, nil, channelName, ticket.SourceEscalation, sentryCorroborated)
		return
	}

	if !result.Actionable || result.Confidence <= 0.8 || result.Category != categoryBug {
		slog.Debug("handler: triage not actionable, ignoring", "actionable", result.Actionable, "confidence", result.Confidence, "category", result.Category)
		return
	}

	daemonCounters.triages.Add(1)
	daemonCounters.triageBug.Add(1)
	slog.Info("high-confidence bug detected passively", "summary", result.Summary)

	repo := deps.resolver.Resolve(result.Repo, result.FilesHint)
	repoPath := ""
	defaultBranch := "main"
	if repo != nil {
		repoPath = repo.Path
		defaultBranch = repo.DefaultBranch
	}

	resp, err := ribbitEngine.Respond(ctx, msg.Text, result, msg.ThreadContext, nil, repoPath, defaultBranch, repoPaths)
	if err != nil {
		slog.Warn("passive ribbit failed", "error", err)
		return
	}

	daemonCounters.ribbits.Add(1)
	incrementMetric(deps.stateManager.DB(), "qa")
	if !resp.TicketUpdate.IsZero() {
		slog.Debug("dropping proposed ticket update (surface does not apply updates)", "issue", resp.TicketUpdate.Issue, "channel", msg.Channel)
	}
	// NOTE: pre-refactor this branch built its CTA button on msg.ThreadTS()
	// while posting the reply anchored at msg.Timestamp (the plain-text
	// branch always used msg.Timestamp too). ReplyWithOptionalCTA uses one
	// threadTS for both, so this now posts and embeds the button using
	// msg.Timestamp consistently. Only observable when this passively-
	// monitored message is itself already a thread reply (ThreadTimestamp
	// set) — Slack's chat.postMessage normalizes thread_ts to the thread
	// root regardless of which in-thread ts is passed, so the posted
	// reply's location is unaffected; the only residual difference is which
	// exact message the ticket button's later FetchMessage(channel,
	// threadTS) resolves (the specific reply vs. the thread root).
	if _, err := slackClient.ReplyWithOptionalCTA(msg.Channel, msg.Timestamp, resp.Text, deps.ticketEngine.ShouldCreateIssues()); err != nil {
		slog.Warn("passive ribbit reply failed", "error", err)
	}
}
