// Package digest implements the Toad King batch analysis engine.
package digest

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/investigation"
	"github.com/scaler-tech/toad/internal/issuetracker"
	"github.com/scaler-tech/toad/internal/state"
)

// Message represents a collected Slack message for batch analysis.
type Message struct {
	Channel     string
	ChannelName string
	User        string
	Text        string
	ThreadTS    string
	Timestamp   string
	BotID       string

	// attempts counts how many times this message has been through a chunk
	// analysis call that ultimately errored (as opposed to succeeding with
	// zero opportunities). Unexported/digest-package-internal: it only
	// matters to flush's own requeue bookkeeping (see
	// requeueOrDropFailedChunk) and is never meant to be constructed or read
	// by other packages, so it's left out of every existing digest.Message{}
	// struct literal across the codebase without needing changes there.
	attempts int
}

// Opportunity is a potential one-shot fix identified by the digest analysis.
type Opportunity struct {
	Summary    string   `json:"summary"`
	Category   string   `json:"category"`
	Confidence float64  `json:"confidence"`
	EstSize    string   `json:"estimated_size"`
	MessageIdx int      `json:"message_index"`
	Keywords   []string `json:"keywords"`
	FilesHint  []string `json:"files_hint"`
	Repo       string   `json:"repo"`
}

// TicketContext holds details about a ticket referenced in a Slack message,
// fetched from the issue tracker to enrich investigation prompts.
type TicketContext struct {
	ID          string // "PLF-3198"
	Title       string
	Description string
	URL         string
	Comments    []TicketComment
}

// TicketComment holds a single comment on a ticket.
type TicketComment struct {
	Author string
	Body   string
}

// InvestigateFunc investigates an opportunity against the codebase before proposing.
type InvestigateFunc func(ctx context.Context, opp Opportunity, msg Message, tickets []TicketContext) (*investigation.Findings, error)

// ProposeFunc hands off approved findings for downstream action (e.g. ticket creation).
type ProposeFunc func(ctx context.Context, f investigation.Findings, msg Message) error

// NotifyFunc sends a Slack message in a thread.
type NotifyFunc func(channel, threadTS, text string)

// ReactFunc adds an emoji reaction to a message.
type ReactFunc func(channel, timestamp, emoji string)

// ClaimFunc atomically claims a thread+scope to prevent duplicate spawns.
// Returns true if the claim succeeded (thread+scope was free).
type ClaimFunc func(threadTS, scope string) bool

// UnclaimFunc releases a thread+scope claim without registering a run (error cleanup).
type UnclaimFunc func(threadTS, scope string)

// ResolveRepoFunc resolves a repo config from triage hints.
type ResolveRepoFunc func(triageRepo string, fileHints []string) *config.RepoConfig

// GetPermalinkFunc returns a permanent URL to a Slack message.
type GetPermalinkFunc func(channel, timestamp string) (string, error)

// chunk is a group of messages to analyze in a single agent call.
type chunk struct {
	messages []Message
	// raw holds the same messages pre-dedup (one entry per original message,
	// no "(xN duplicates)" text collapsing) — used only by
	// requeueOrDropFailedChunk when this chunk's analysis call fails, so the
	// ORIGINAL messages go back in the buffer rather than the dedup-annotated
	// copies (which would defeat a later dedup pass — see buildChunks'
	// rawByChannel doc comment).
	raw   []Message
	label string // for logging, e.g. "#errors (42 msgs)" or "mixed (12 msgs, 4 channels)"
}

// DigestStats holds observable digest engine metrics.
type DigestStats struct {
	BufferSize     int
	NextFlush      time.Time
	TotalProcessed int64
	TotalOpps      int64
	TotalSpawns    int64
}

// Engine collects messages and periodically analyzes them for one-shot opportunities.
type Engine struct {
	cfg              *config.DigestConfig
	agent            agent.Provider
	model            string
	propose          ProposeFunc
	notify           NotifyFunc
	investigate      InvestigateFunc
	react            ReactFunc
	claim            ClaimFunc
	unclaim          UnclaimFunc
	resolveRepo      ResolveRepoFunc
	repoPaths        map[string]string // path → name, for cross-repo prompts and path scrubbing
	repoProfiles     string            // formatted repo profiles for multi-repo prompt, empty for single-repo
	db               *state.DB
	tracker          issuetracker.Tracker
	getPermalink     GetPermalinkFunc
	respectAssignees bool
	staleDays        int

	mu     sync.Mutex
	buffer []Message

	// Hourly spawn rate limiting
	spawnMu    sync.Mutex
	spawnCount int
	spawnHour  int

	// Acted-on issue refs — prevents re-collecting bot notifications about issues
	// toad already investigated (e.g. Linear echoing back investigation findings).
	actedIssuesMu sync.RWMutex
	actedIssues   map[string]time.Time

	// Observable counters
	totalProcessed atomic.Int64
	totalOpps      atomic.Int64
	totalSpawns    atomic.Int64
	lastFlush      atomic.Int64 // unix timestamp
}

// EngineOpts holds all dependencies and configuration for creating a digest Engine.
type EngineOpts struct {
	AgentProvider    agent.Provider
	TriageModel      string
	Propose          ProposeFunc
	Notify           NotifyFunc
	Investigate      InvestigateFunc
	React            ReactFunc
	Claim            ClaimFunc
	Unclaim          UnclaimFunc
	ResolveRepo      ResolveRepoFunc
	RepoPaths        map[string]string
	Profiles         []config.RepoProfile
	DB               *state.DB
	Tracker          issuetracker.Tracker
	GetPermalink     GetPermalinkFunc
	RespectAssignees bool
	StaleDays        int
}

// New creates a digest engine.
func New(cfg *config.DigestConfig, opts EngineOpts) *Engine {
	e := &Engine{
		cfg:              cfg,
		agent:            opts.AgentProvider,
		model:            opts.TriageModel,
		propose:          opts.Propose,
		notify:           opts.Notify,
		investigate:      opts.Investigate,
		claim:            opts.Claim,
		unclaim:          opts.Unclaim,
		react:            opts.React,
		resolveRepo:      opts.ResolveRepo,
		repoPaths:        opts.RepoPaths,
		db:               opts.DB,
		tracker:          opts.Tracker,
		getPermalink:     opts.GetPermalink,
		respectAssignees: opts.RespectAssignees,
		staleDays:        opts.StaleDays,
		spawnHour:        time.Now().Hour(),
		actedIssues:      make(map[string]time.Time),
	}
	if len(opts.Profiles) > 1 {
		e.repoProfiles = config.FormatForPrompt(opts.Profiles)
	}
	return e
}

// scopeKey derives a scope key from an opportunity for scoped claims.
// If an issue tracker is available and the message references a ticket, uses the ticket ID.
// Otherwise, hashes the opportunity summary to produce a stable scope key.
func scopeKey(opp Opportunity, tracker issuetracker.Tracker, msgText string) string {
	if tracker != nil {
		if ref := tracker.ExtractIssueRef(msgText); ref != nil {
			return ref.ID
		}
	}
	h := fnv.New32a()
	h.Write([]byte(opp.Summary))
	return fmt.Sprintf("digest-%x", h.Sum32())
}

// Collect adds a message to the buffer for batch analysis.
// Bot messages referencing issues toad already acted on are silently dropped
// to prevent feedback loops (e.g. Linear echoing back investigation findings).
func (e *Engine) Collect(msg Message) {
	if msg.BotID != "" && e.tracker != nil {
		refs := e.tracker.ExtractAllIssueRefs(msg.Text)
		for _, ref := range refs {
			if e.isActedIssue(ref.ID) {
				slog.Debug("digest skipping bot message: references acted-on issue",
					"issue", ref.ID, "bot", msg.BotID)
				return
			}
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.buffer = append(e.buffer, msg)
}

const actedIssueTTL = 4 * time.Hour

// recordActedIssue marks an issue ID as acted on, preventing re-collection.
func (e *Engine) recordActedIssue(issueID string) {
	e.actedIssuesMu.Lock()
	defer e.actedIssuesMu.Unlock()
	if e.actedIssues == nil {
		e.actedIssues = make(map[string]time.Time)
	}
	e.actedIssues[issueID] = time.Now()

	// Prune expired entries
	if len(e.actedIssues) > 100 {
		now := time.Now()
		for id, t := range e.actedIssues {
			if now.Sub(t) > actedIssueTTL {
				delete(e.actedIssues, id)
			}
		}
	}
}

func (e *Engine) isActedIssue(issueID string) bool {
	e.actedIssuesMu.RLock()
	defer e.actedIssuesMu.RUnlock()
	t, ok := e.actedIssues[issueID]
	return ok && time.Since(t) < actedIssueTTL
}

// ResumeInvestigations re-runs opportunities that were interrupted mid-investigation
// by a crash. Each opportunity is investigated individually and the existing DB row
// is updated on completion. Rows stay as investigating=true until done, so another
// crash during resume will pick them up again on the next restart.
func (e *Engine) ResumeInvestigations(ctx context.Context, opps []*state.DigestOpportunity) {
	if len(opps) == 0 {
		return
	}

	slog.Info("resuming interrupted investigations", "count", len(opps))

	for _, dbOpp := range opps {
		if ctx.Err() != nil {
			return
		}

		msg := Message{
			Channel:     dbOpp.ChannelID,
			ChannelName: dbOpp.Channel,
			Text:        dbOpp.Message,
			ThreadTS:    dbOpp.ThreadTS,
			Timestamp:   dbOpp.ThreadTS,
		}
		opp := Opportunity{
			Summary:    dbOpp.Summary,
			Category:   dbOpp.Category,
			Confidence: dbOpp.Confidence,
			EstSize:    dbOpp.EstSize,
			Keywords:   strings.Split(dbOpp.Keywords, ","),
		}

		// Run investigation
		dismissed := false
		reasoning := ""
		var investigatedFiles []string
		taskDescription := msg.Text
		var findings *investigation.Findings
		if e.investigate != nil {
			result, err := e.investigate(ctx, opp, msg, nil)
			if err != nil {
				slog.Warn("resumed investigation failed", "error", err, "summary", opp.Summary)
				dismissed = true
				reasoning = fmt.Sprintf("investigation error: %v", err)
			} else if !result.Feasible {
				slog.Info("resumed investigation dismissed", "summary", opp.Summary)
				dismissed = true
				reasoning = result.Reasoning
			} else {
				slog.Info("resumed investigation approved", "summary", opp.Summary)
				taskDescription = result.Reasoning
				reasoning = result.Reasoning
				investigatedFiles = result.FilesFound
				findings = result
			}
		}

		// Update existing DB row — clears investigating flag
		dbOpp.Investigating = false
		dbOpp.Dismissed = dismissed
		dbOpp.Reasoning = reasoning
		if e.db != nil {
			if err := e.db.UpdateDigestOpportunity(dbOpp); err != nil {
				slog.Warn("failed to update resumed opportunity", "error", err)
			}
		}

		if dismissed {
			continue
		}

		// Propose the fix
		if !e.trySpawn() {
			slog.Info("resumed investigation hit hourly spawn limit", "summary", opp.Summary)
			continue
		}

		threadTS := msg.ThreadTS

		scope := scopeKey(Opportunity{Summary: opp.Summary}, e.tracker, msg.Text)
		if e.claim != nil {
			if !e.claim(threadTS, scope) {
				slog.Info("resumed investigation: thread+scope already claimed", "summary", opp.Summary, "scope", scope)
				continue
			}
		}

		if findings == nil {
			findings = syntheticFindings(taskDescription, opp, investigatedFiles)
		}

		if err := e.propose(ctx, *findings, msg); err != nil {
			slog.Error("resumed investigation: propose failed", "error", err, "summary", opp.Summary)
			if e.unclaim != nil {
				e.unclaim(threadTS, scope)
			}
			if e.notify != nil {
				e.notify(msg.Channel, threadTS,
					":x: Toad King failed to propose fix: "+err.Error())
			}
			continue
		}

		e.unclaimAfterPropose(threadTS, scope)

		e.totalSpawns.Add(1)
	}
}

// Stats returns a snapshot of the digest engine's observable metrics.
func (e *Engine) Stats() DigestStats {
	e.mu.Lock()
	bufLen := len(e.buffer)
	e.mu.Unlock()

	interval := time.Duration(e.cfg.BatchMinutes) * time.Minute
	lastFlush := time.Unix(e.lastFlush.Load(), 0)
	nextFlush := lastFlush.Add(interval)
	if e.lastFlush.Load() == 0 {
		nextFlush = time.Time{}
	}

	return DigestStats{
		BufferSize:     bufLen,
		NextFlush:      nextFlush,
		TotalProcessed: e.totalProcessed.Load(),
		TotalOpps:      e.totalOpps.Load(),
		TotalSpawns:    e.totalSpawns.Load(),
	}
}

// Run starts the periodic analysis loop. Blocks until ctx is canceled.
func (e *Engine) Run(ctx context.Context) {
	interval := time.Duration(e.cfg.BatchMinutes) * time.Minute
	slog.Info("digest engine started", "interval", interval, "min_confidence", e.cfg.MinConfidence)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.flush(ctx)
		case <-ctx.Done():
			slog.Info("digest engine stopped")
			return
		}
	}
}

func (e *Engine) flush(ctx context.Context) {
	// Drain buffer atomically
	e.mu.Lock()
	msgs := e.buffer
	e.buffer = nil
	e.mu.Unlock()

	e.lastFlush.Store(time.Now().Unix())

	if len(msgs) == 0 {
		return
	}

	// Only count genuinely-new messages toward totalProcessed — a message
	// with attempts > 0 was already counted on the flush where it was first
	// drained from the buffer, before requeueOrDropFailedChunk put it back
	// for this retry. Without this guard, a requeued message would be
	// double-counted in this metric on every retry attempt.
	newCount := 0
	for _, m := range msgs {
		if m.attempts == 0 {
			newCount++
		}
	}
	e.totalProcessed.Add(int64(newCount))

	chunks := e.buildChunks(msgs)
	gatedTickets := map[string]bool{} // tracks tickets already gated/commented on in this flush
	slog.Debug("digest analyzing batch", "messages", len(msgs), "chunks", len(chunks))

	for _, ch := range chunks {
		slog.Debug("digest analyzing chunk", "label", ch.label, "messages", len(ch.messages))

		// Scale timeout for oversized chunks (single channels that exceed MaxChunkSize)
		baseTimeout := time.Duration(e.cfg.ChunkTimeoutSecs) * time.Second
		chunkTimeout := baseTimeout
		maxSize := e.cfg.MaxChunkSize
		if maxSize <= 0 {
			maxSize = 50
		}
		if len(ch.messages) > maxSize {
			// Proportionally longer: 2x messages = 2x timeout
			chunkTimeout = baseTimeout * time.Duration(len(ch.messages)) / time.Duration(maxSize)
		}
		opportunities, err := e.analyzeWithRetry(ctx, ch, chunkTimeout)
		if err != nil {
			// Critical fix (C4): analyzeWithRetry's error used to be
			// discarded entirely (`opportunities, _ := ...`), silently
			// dropping every message in this chunk with no re-attempt and no
			// visibility. Re-queue the chunk's messages (bounded) instead —
			// see requeueOrDropFailedChunk's doc comment.
			e.requeueOrDropFailedChunk(ch, err)
			continue
		}

		if len(opportunities) == 0 {
			continue
		}

		if !e.processOpportunities(ctx, ch.messages, opportunities, gatedTickets) {
			return // spawn limit reached
		}
	}
}

// maxChunkRequeueAttempts bounds how many times a chunk that failed analysis
// (a genuine agent/parse error — analyzeWithRetry itself already retries
// once internally on a timeout — as opposed to a clean "no opportunities"
// result) is re-queued into the buffer for a later flush to retry. Prevents
// a persistently-broken chunk (e.g. content that reliably breaks the model
// call) from being requeued forever.
const maxChunkRequeueAttempts = 3

// requeueOrDropFailedChunk is flush's failure path for a chunk whose
// analyzeWithRetry call itself errored. It requeues ch.raw (the pre-dedup
// originals), NOT ch.messages (the dedup-annotated copies used for analysis)
// — see buildChunks' rawByChannel doc comment for why requeuing the
// annotated copies would defeat a later dedup pass. Each message's attempts
// counter is incremented; messages still under maxChunkRequeueAttempts are
// re-appended to e.buffer (under the same mutex Collect uses) so the next
// flush retries them alongside whatever's newly arrived. flush itself only
// counts a message toward e.totalProcessed when its attempts field is still
// zero, so a requeued message isn't double-counted in that metric across
// retries. Messages that have exhausted their attempts are dropped and
// logged at Error with a count; a genuine requeue is logged at Warn.
// Success (a chunk that analyzes cleanly, even to zero opportunities) is
// entirely unaffected — this is only reached on a genuine analysis error.
func (e *Engine) requeueOrDropFailedChunk(ch chunk, err error) {
	toRequeue := make([]Message, 0, len(ch.raw))
	dropped := 0
	for _, m := range ch.raw {
		m.attempts++
		if m.attempts >= maxChunkRequeueAttempts {
			dropped++
			continue
		}
		toRequeue = append(toRequeue, m)
	}

	if dropped > 0 {
		slog.Error("digest chunk analysis failed repeatedly, dropping messages that exhausted retries",
			"label", ch.label, "error", err, "dropped", dropped, "max_attempts", maxChunkRequeueAttempts)
	}
	if len(toRequeue) > 0 {
		slog.Warn("digest chunk analysis failed, requeuing messages for the next flush",
			"label", ch.label, "error", err, "requeued", len(toRequeue))
		e.mu.Lock()
		e.buffer = append(e.buffer, toRequeue...)
		e.mu.Unlock()
	}
}

// composeGateFindingsText renders the text posted to a ticket's assignee-gate
// comment (issuetracker.GateOpts.Findings). Prefers the richer Problem +
// Root-cause fields from a completed investigation's Findings when available
// (f non-nil and either is non-blank) over the flat reasoning string, falling
// back to reasoning alone otherwise (f nil — no real investigation ran — or
// an investigation whose Problem/RootCause both came back blank).
//
// Fixes a duplication bug: the prior composition was always
// "taskDescription + \"\\n\\n**Reasoning:** \" + reasoning", but on the path
// where an investigation actually ran, taskDescription and reasoning are
// both set to the exact same result.Reasoning string (see the investigate
// block above) — so the gate comment rendered the same paragraph twice
// (carried finding from Task 4's review).
func composeGateFindingsText(f *investigation.Findings, reasoning string) string {
	if f != nil && (strings.TrimSpace(f.Problem) != "" || strings.TrimSpace(f.RootCause) != "") {
		return f.Problem + "\n\n**Root cause (hypothesis):** " + f.RootCause
	}
	return reasoning
}

// processOpportunities handles the investigation, persistence, and spawn logic
// for a set of opportunities from a single chunk. Returns false when the hourly
// spawn limit is reached (caller should stop processing further chunks).
// gatedTickets tracks ticket IDs already gated in this flush to avoid duplicate comments.
func (e *Engine) processOpportunities(ctx context.Context, msgs []Message, opportunities []Opportunity, gatedTickets map[string]bool) bool {
	for _, opp := range opportunities {
		if !e.passesGuardrails(opp) {
			slog.Info("digest near-miss filtered by guardrails",
				"summary", opp.Summary, "confidence", opp.Confidence, "category", opp.Category, "size", opp.EstSize)
			continue
		}

		e.totalOpps.Add(1)

		// Cross-batch dedup: skip if a similar opportunity was already processed recently.
		// Uses keyword overlap to catch semantically equivalent issues with different wording.
		if e.db != nil {
			kw := strings.Join(opp.Keywords, ",")
			if recent, err := e.db.HasRecentOpportunity(opp.Summary, kw, 1*time.Hour); err == nil && recent {
				slog.Info("digest skipping duplicate opportunity (similar recently processed)",
					"summary", opp.Summary)
				continue
			}
		}

		// Resolve the original message
		if opp.MessageIdx < 0 || opp.MessageIdx >= len(msgs) {
			slog.Warn("digest opportunity has invalid message index", "idx", opp.MessageIdx)
			continue
		}
		msg := msgs[opp.MessageIdx]

		threadTS := msg.ThreadTS
		if threadTS == "" {
			threadTS = msg.Timestamp
		}

		// Compute a scope key for scoped claims — uses ticket ID if available,
		// otherwise a hash of the opportunity summary.
		scope := scopeKey(opp, e.tracker, msg.Text)

		// Claim thread+scope early — before investigation — so a concurrent CTA spawn
		// doesn't race with us during the (slow) Sonnet investigation call.
		if e.claim != nil && !e.claim(threadTS, scope) {
			slog.Info("digest skipping: thread+scope already claimed", "summary", opp.Summary, "thread", threadTS, "scope", scope)
			continue
		}

		// Save opportunity to DB in investigating state before the Sonnet deep-dive,
		// so the dashboard shows an "investigating" spinner while work is in progress.
		var dbOpp *state.DigestOpportunity
		if e.db != nil {
			dbOpp = &state.DigestOpportunity{
				Summary:    opp.Summary,
				Category:   opp.Category,
				Confidence: opp.Confidence,
				EstSize:    opp.EstSize,
				Channel:    msg.ChannelName,
				ChannelID:  msg.Channel,
				ThreadTS:   threadTS,
				Message:    msg.Text,
				Keywords:   strings.Join(opp.Keywords, ","),
				// DryRun is intentionally left at its zero value (false): v2's
				// digest has a single mode (enabled/disabled) — the column
				// stays in the schema for historical rows (see
				// state.DigestCounts.DryRun), but no new row is ever written
				// as dry-run again.
				Investigating: true,
				CreatedAt:     time.Now(),
			}
			if err := e.db.SaveDigestOpportunity(dbOpp); err != nil {
				slog.Warn("failed to save investigating opportunity", "error", err)
				dbOpp = nil
			}
		}

		// Fetch ticket details for all issue references in the message.
		// These enrich the investigation prompt so the LLM can pick the right ticket.
		var tickets []TicketContext
		var allRefs []*issuetracker.IssueRef
		if e.tracker != nil {
			allRefs = e.tracker.ExtractAllIssueRefs(msg.Text)
			for _, ref := range allRefs {
				tc := TicketContext{ID: ref.ID, URL: ref.URL}
				details, err := e.tracker.GetIssueDetails(ctx, ref)
				if err != nil {
					slog.Warn("failed to fetch ticket details", "id", ref.ID, "error", err)
				} else if details != nil {
					tc.Title = details.Title
					tc.Description = details.Description
					if tc.URL == "" {
						tc.URL = details.URL
					}
					for _, c := range details.Comments {
						tc.Comments = append(tc.Comments, TicketComment{
							Author: c.Author,
							Body:   c.Body,
						})
					}
				}
				tickets = append(tickets, tc)
			}
		}

		// Investigation gate: have ribbit check the codebase before spawning
		dismissed := false
		reasoning := ""
		investigatedIssueID := ""
		var investigatedFiles []string
		taskDescription := msg.Text
		var findings *investigation.Findings
		if e.investigate != nil {
			result, err := e.investigate(ctx, opp, msg, tickets)
			if err != nil {
				slog.Warn("digest investigation failed", "error", err, "summary", opp.Summary)
				dismissed = true
				reasoning = fmt.Sprintf("%s%v", state.InvestigationErrorPrefix, err)
			} else if !result.Feasible {
				slog.Info("digest investigation dismissed opportunity",
					"summary", opp.Summary, "reasoning", result.Reasoning)
				dismissed = true
				reasoning = result.Reasoning
			} else {
				slog.Info("digest investigation approved opportunity",
					"summary", opp.Summary, "reasoning", result.Reasoning)
				taskDescription = result.Reasoning
				reasoning = result.Reasoning
				investigatedIssueID = result.IssueID
				investigatedFiles = result.FilesFound
				findings = result
			}
		}

		// Update the DB row now that investigation is complete
		if dbOpp != nil {
			dbOpp.Dismissed = dismissed
			dbOpp.Reasoning = reasoning
			dbOpp.Investigating = false
			if err := e.db.UpdateDigestOpportunity(dbOpp); err != nil {
				slog.Warn("failed to update digest opportunity", "error", err)
			}
		}

		if dismissed {
			if e.unclaim != nil {
				e.unclaim(threadTS, scope)
			}
			continue
		}

		// Record acted-on issue refs to prevent re-collecting bot notifications
		// (e.g. Linear/Jira echoing back toad's investigation findings).
		for _, ref := range allRefs {
			e.recordActedIssue(ref.ID)
		}

		// Check hourly spawn limit AFTER investigation — dismissed opportunities
		// should not consume spawn slots.
		if !e.trySpawn() {
			slog.Info("digest hourly spawn limit reached", "limit", e.cfg.MaxAutoSpawnHour)
			if e.unclaim != nil {
				e.unclaim(threadTS, scope)
			}
			return false
		}

		slog.Info("Toad King proposing fix",
			"summary", opp.Summary,
			"confidence", opp.Confidence,
			"channel", msg.ChannelName,
		)

		// Detect or create issue tracker reference.
		// Priority: investigation-selected ticket > task_spec extraction > msg.Text extraction > create new.
		var issueRef *issuetracker.IssueRef
		if e.tracker != nil {
			if investigatedIssueID != "" {
				for _, ref := range allRefs {
					if ref.ID == investigatedIssueID {
						issueRef = ref
						slog.Info("using investigation-selected ticket", "id", ref.ID)
						break
					}
				}
			}
			if issueRef == nil {
				issueRef = e.tracker.ExtractIssueRef(taskDescription)
			}
			if issueRef == nil {
				issueRef = e.tracker.ExtractIssueRef(msg.Text)
			}
			if issueRef == nil && e.tracker.ShouldCreateIssues() {
				ref, err := e.tracker.CreateIssue(ctx, issuetracker.CreateIssueOpts{
					Title:       opp.Summary,
					Description: taskDescription,
					Category:    opp.Category,
				})
				if err != nil {
					slog.Warn("failed to create issue", "error", err, "summary", opp.Summary)
				} else {
					issueRef = ref
				}
			}
		}

		// Ticket assignee gate: if the ticket is actively assigned,
		// post findings to the ticket instead of spawning.
		// If the ticket is Done/Canceled, skip silently — no comment, no spawn.
		// Dedup: if we already gated this ticket in this flush, skip without another comment.
		if e.respectAssignees && issueRef != nil {
			if gatedTickets[issueRef.ID] {
				slog.Info("digest skipping: ticket already gated in this flush",
					"issue", issueRef.ID, "summary", opp.Summary)
				if e.unclaim != nil {
					e.unclaim(threadTS, scope)
				}
				continue
			}
			permalink := ""
			if e.getPermalink != nil {
				permalink, _ = e.getPermalink(msg.Channel, msg.Timestamp)
			}
			gate := issuetracker.CheckAssigneeGate(ctx, e.tracker, issuetracker.GateOpts{
				IssueRef:       issueRef,
				StaleDays:      e.staleDays,
				Findings:       composeGateFindingsText(findings, reasoning),
				SlackPermalink: permalink,
			})
			if gate.Gated {
				gatedTickets[issueRef.ID] = true
				if !gate.Done && e.notify != nil {
					e.notify(msg.Channel, threadTS,
						fmt.Sprintf(":clipboard: %s is assigned to %s — I posted my findings as a comment on the ticket. "+
							"Say `@toad fix this` if you'd like me to investigate anyway.",
							issueRef.ID, gate.Status.AssigneeName))
				}
				if e.unclaim != nil {
					e.unclaim(threadTS, scope)
				}
				continue
			}
		}

		if findings == nil {
			findings = syntheticFindings(taskDescription, opp, investigatedFiles)
		}
		if issueRef != nil {
			findings.IssueID = issueRef.ID
		}

		// The :crown: spawn-announcement notice is now sent by the propose
		// implementation, which owns downstream outreach.
		if err := e.propose(ctx, *findings, msg); err != nil {
			slog.Error("digest propose failed", "error", err, "summary", opp.Summary)
			if e.unclaim != nil {
				e.unclaim(threadTS, scope)
			}
			if e.notify != nil {
				e.notify(msg.Channel, threadTS,
					":x: Toad King failed to propose fix: "+err.Error())
			}
		} else {
			e.totalSpawns.Add(1)
			e.unclaimAfterPropose(threadTS, scope)
		}
	}
	return true
}

// syntheticFindings builds a fallback Findings verdict for the two propose
// call sites above, used whenever no real investigation ran (e.investigate
// is nil) or a real one produced no result — both proposal paths need a
// *Findings to hand to e.propose regardless.
func syntheticFindings(taskDescription string, opp Opportunity, files []string) *investigation.Findings {
	return &investigation.Findings{
		Feasible:   true,
		Reasoning:  taskDescription,
		Repo:       opp.Repo,
		FilesFound: files,
	}
}

// unclaimAfterPropose releases the scoped thread+scope claim after a
// successful propose call. Propose succeeding has already served the
// purpose the scoped claim existed to protect (no concurrent CTA
// spawn racing the investigation) — HasRecentOpportunity, actedIssues, and
// the ticket_index carry dedup forward from this point on, so holding the
// claim forever (the prior behavior) would only block a legitimate
// follow-up on the same thread+scope (carried finding from Task 6/15's
// review). A no-op when e.unclaim is nil.
func (e *Engine) unclaimAfterPropose(threadTS, scope string) {
	if e.unclaim != nil {
		e.unclaim(threadTS, scope)
	}
}
