package digest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hergen/toad/internal/config"
	"github.com/hergen/toad/internal/state"
	"github.com/hergen/toad/internal/tadpole"
)

// Message represents a collected Slack message for batch analysis.
type Message struct {
	Channel     string
	ChannelName string
	User        string
	Text        string
	ThreadTS    string
	Timestamp   string
}

// Opportunity is a potential one-shot fix identified by the digest analysis.
type Opportunity struct {
	Summary    string  `json:"summary"`
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
	EstSize    string  `json:"estimated_size"`
	MessageIdx int     `json:"message_index"`
	Keywords   []string `json:"keywords"`
	FilesHint  []string `json:"files_hint"`
}

// InvestigateResult holds the outcome of a ribbit investigation.
type InvestigateResult struct {
	Feasible  bool   // whether ribbit thinks this is a clear, small fix
	TaskSpec  string // refined task description for the tadpole
	Reasoning string // why feasible/not (for logging)
}

// InvestigateFunc investigates an opportunity against the codebase before spawning.
type InvestigateFunc func(ctx context.Context, opp Opportunity, msg Message) (*InvestigateResult, error)

// SpawnFunc spawns a tadpole task.
type SpawnFunc func(ctx context.Context, task tadpole.Task) error

// NotifyFunc sends a Slack message in a thread.
type NotifyFunc func(channel, threadTS, text string)

// ReactFunc adds an emoji reaction to a message.
type ReactFunc func(channel, timestamp, emoji string)

// DigestStats holds observable digest engine metrics.
type DigestStats struct {
	BufferSize    int
	NextFlush     time.Time
	TotalProcessed int64
	TotalOpps     int64
	TotalSpawns   int64
}

// Engine collects messages and periodically analyzes them for one-shot opportunities.
type Engine struct {
	cfg         *config.DigestConfig
	model       string
	spawn       SpawnFunc
	notify      NotifyFunc
	investigate InvestigateFunc
	react       ReactFunc
	db          *state.DB

	mu     sync.Mutex
	buffer []Message

	// Hourly spawn rate limiting
	spawnMu    sync.Mutex
	spawnCount int
	spawnHour  int

	// Observable counters
	totalProcessed atomic.Int64
	totalOpps      atomic.Int64
	totalSpawns    atomic.Int64
	lastFlush      atomic.Int64 // unix timestamp
}

// New creates a digest engine.
func New(cfg *config.DigestConfig, triageModel string, spawn SpawnFunc, notify NotifyFunc, investigate InvestigateFunc, react ReactFunc, db *state.DB) *Engine {
	return &Engine{
		cfg:         cfg,
		model:       triageModel,
		spawn:       spawn,
		notify:      notify,
		investigate: investigate,
		react:       react,
		db:          db,
	}
}

// Collect adds a message to the buffer for batch analysis.
func (e *Engine) Collect(msg Message) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.buffer = append(e.buffer, msg)
}

// Stats returns a snapshot of the digest engine's observable metrics.
func (e *Engine) Stats() DigestStats {
	e.mu.Lock()
	bufLen := len(e.buffer)
	e.mu.Unlock()

	interval := time.Duration(e.cfg.BatchMinutes) * time.Minute
	lastFlush := time.Unix(e.lastFlush.Load(), 0)
	nextFlush := lastFlush.Add(interval)
	if lastFlush.IsZero() {
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

// Run starts the periodic analysis loop. Blocks until ctx is cancelled.
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

	e.totalProcessed.Add(int64(len(msgs)))

	slog.Debug("digest analyzing batch", "messages", len(msgs))

	opportunities, err := e.analyze(ctx, msgs)
	if err != nil {
		slog.Warn("digest analysis failed", "error", err)
		return
	}

	if len(opportunities) == 0 {
		slog.Debug("digest found no opportunities")
		return
	}

	for _, opp := range opportunities {
		if !e.passesGuardrails(opp) {
			slog.Debug("digest opportunity filtered by guardrails",
				"summary", opp.Summary, "confidence", opp.Confidence, "category", opp.Category)
			continue
		}

		e.totalOpps.Add(1)

		// Resolve the original message
		if opp.MessageIdx < 0 || opp.MessageIdx >= len(msgs) {
			slog.Warn("digest opportunity has invalid message index", "idx", opp.MessageIdx)
			continue
		}
		msg := msgs[opp.MessageIdx]

		// Investigation gate: have ribbit check the codebase before spawning
		dismissed := false
		reasoning := ""
		taskDescription := msg.Text
		if e.investigate != nil {
			result, err := e.investigate(ctx, opp, msg)
			if err != nil {
				slog.Warn("digest investigation failed", "error", err, "summary", opp.Summary)
				dismissed = true
				reasoning = fmt.Sprintf("investigation error: %v", err)
			} else if !result.Feasible {
				slog.Info("digest investigation dismissed opportunity",
					"summary", opp.Summary, "reasoning", result.Reasoning)
				dismissed = true
				reasoning = result.Reasoning
			} else {
				slog.Info("digest investigation approved opportunity",
					"summary", opp.Summary, "reasoning", result.Reasoning)
				taskDescription = result.TaskSpec
				reasoning = result.Reasoning
			}
		}

		// Persist opportunity to DB (both dry-run and real, dismissed and approved)
		if e.db != nil {
			dbOpp := &state.DigestOpportunity{
				Summary:    opp.Summary,
				Category:   opp.Category,
				Confidence: opp.Confidence,
				EstSize:    opp.EstSize,
				Channel:    msg.Channel,
				Message:    msg.Text,
				Keywords:   strings.Join(opp.Keywords, ","),
				DryRun:     e.cfg.DryRun,
				Dismissed:  dismissed,
				Reasoning:  reasoning,
				CreatedAt:  time.Now(),
			}
			if err := e.db.SaveDigestOpportunity(dbOpp); err != nil {
				slog.Warn("failed to save digest opportunity", "error", err)
			}
		}

		if dismissed {
			continue
		}

		// Check hourly spawn limit AFTER investigation — dismissed opportunities
		// should not consume spawn slots.
		if !e.trySpawn() {
			slog.Info("digest hourly spawn limit reached", "limit", e.cfg.MaxAutoSpawnHour)
			return
		}

		// In dry-run mode: log and skip spawn/notify
		if e.cfg.DryRun {
			slog.Info("[dry-run] would spawn tadpole",
				"summary", opp.Summary,
				"confidence", opp.Confidence,
				"channel", msg.ChannelName,
			)
			e.totalSpawns.Add(1)
			continue
		}

		slog.Info("Toad King spawning tadpole",
			"summary", opp.Summary,
			"confidence", opp.Confidence,
			"channel", msg.ChannelName,
		)

		threadTS := msg.ThreadTS
		if threadTS == "" {
			threadTS = msg.Timestamp
		}

		task := tadpole.Task{
			Description:   taskDescription,
			Summary:       opp.Summary,
			Category:      opp.Category,
			EstSize:       opp.EstSize,
			SlackChannel:  msg.Channel,
			SlackThreadTS: threadTS,
		}

		if err := e.spawn(ctx, task); err != nil {
			slog.Error("digest spawn failed", "error", err, "summary", opp.Summary)
			if e.notify != nil {
				e.notify(msg.Channel, threadTS,
					":x: Toad King failed to spawn tadpole: "+err.Error())
			}
		} else {
			e.totalSpawns.Add(1)
			// React on original message so people see toad is working on it.
			// The runner handles thread replies (status message + progress updates).
			if e.react != nil {
				e.react(msg.Channel, msg.Timestamp, "hatching_chick")
			}
		}
	}
}

const digestPrompt = `You are the Toad King — a conservative code-change detector. You are given a batch of recent Slack messages from a development team. Your job is to identify ONLY clear, specific, one-shot bug reports or feature requests that a coding agent could fix autonomously.

Messages:
%s

Respond ONLY with valid JSON — an array of opportunities (empty array [] if none, which is the most common case):
[
  {
    "summary": "one line description of the fix",
    "category": "bug|feature",
    "confidence": 0.0-1.0,
    "estimated_size": "tiny|small",
    "message_index": 0,
    "keywords": ["relevant", "code", "terms"],
    "files_hint": ["possible/file.go"]
  }
]

Critical rules:
- MOST batches should return [] — be extremely conservative
- Only flag messages that describe a SPECIFIC, CONCRETE code change
- The message must contain enough detail for a coding agent to act on it WITHOUT asking questions
- Vague complaints, general discussions, or questions should NEVER be flagged
- Only "bug" and "feature" categories are allowed
- Only "tiny" (1-2 lines) or "small" (1 file) estimated sizes
- confidence must be >= 0.95 to be considered
- message_index is 0-based, referring to the message list above

Structured alerts (Sentry, CI, monitoring bots):
- Error alerts with exception names, stack traces, or file paths ARE specific and concrete
- A coding agent CAN investigate an exception class, trace the logic, and propose a fix
- Treat these as bug reports — the exception/error message IS the specification
- Example: a Sentry alert with "SsoAuthException: Tenant ID mismatch" and a file path is actionable`

func (e *Engine) analyze(ctx context.Context, msgs []Message) ([]Opportunity, error) {
	// Format messages as numbered list
	var sb strings.Builder
	for i, msg := range msgs {
		sb.WriteString(fmt.Sprintf("[%d] #%s @%s: %s\n", i, msg.ChannelName, msg.User, msg.Text))
	}

	prompt := fmt.Sprintf(digestPrompt, sb.String())

	args := []string{
		"--print",
		"--dangerously-skip-permissions",
		"--max-turns", "1",
		"--output-format", "json",
		"--model", e.model,
		"-p", prompt,
	}

	analyzeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(analyzeCtx, "claude", args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("claude digest call failed: %w", err)
	}

	slog.Debug("digest raw response", "output", string(output))

	// Parse JSON envelope from --output-format json
	var envelope struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		// Try direct parse
		return parseOpportunities(output)
	}
	if envelope.IsError {
		return nil, fmt.Errorf("claude digest returned error: %s", envelope.Result)
	}

	return parseOpportunities([]byte(envelope.Result))
}

func parseOpportunities(data []byte) ([]Opportunity, error) {
	text := strings.TrimSpace(string(data))

	// Find the JSON array by matching brackets (handles trailing prose from Haiku)
	start := strings.Index(text, "[")
	if start < 0 {
		return nil, fmt.Errorf("parsing digest opportunities: no JSON array found")
	}
	end := findMatchingBracket(text, start)
	if end < 0 {
		return nil, fmt.Errorf("parsing digest opportunities: unmatched '['")
	}
	text = text[start : end+1]

	var opps []Opportunity
	if err := json.Unmarshal([]byte(text), &opps); err != nil {
		return nil, fmt.Errorf("parsing digest opportunities: %w", err)
	}
	return opps, nil
}

// findMatchingBracket finds the index of the ']' that matches the '[' at pos,
// accounting for nested brackets and JSON strings.
func findMatchingBracket(s string, pos int) int {
	depth := 0
	inString := false
	escaped := false
	for i := pos; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		ch := s[i]
		if inString {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (e *Engine) passesGuardrails(opp Opportunity) bool {
	// Confidence check
	if opp.Confidence < e.cfg.MinConfidence {
		return false
	}

	// Category check
	allowed := false
	for _, cat := range e.cfg.AllowedCategories {
		if opp.Category == cat {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}

	// Size check
	maxSize := e.cfg.MaxEstSize
	if maxSize == "tiny" && opp.EstSize != "tiny" {
		return false
	}
	if maxSize == "small" && opp.EstSize != "tiny" && opp.EstSize != "small" {
		return false
	}

	return true
}

// trySpawn checks and increments the hourly spawn counter.
// Returns true if under the limit, false if at capacity.
func (e *Engine) trySpawn() bool {
	e.spawnMu.Lock()
	defer e.spawnMu.Unlock()

	currentHour := time.Now().Hour()
	if currentHour != e.spawnHour {
		e.spawnCount = 0
		e.spawnHour = currentHour
	}

	if e.spawnCount >= e.cfg.MaxAutoSpawnHour {
		return false
	}
	e.spawnCount++
	return true
}
