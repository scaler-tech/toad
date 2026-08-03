package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// execCommand is a seam over exec.CommandContext so tests can swap in a fake
// subprocess without invoking the real "claude" CLI.
var execCommand = exec.CommandContext

// seatThrottlePattern matches Claude CLI error text indicating the
// subscription-seat usage limit has been hit (as opposed to some other
// failure), which is the only condition eligible for API-key fallback. It
// must only ever be applied to process/stderr-level failures (see
// envelopeResultError) — never to an envelope is_error result, whose Result
// text is the AGENT's own free-form output and can legitimately contain
// phrases like "rate limit" while summarizing an unrelated finding.
var seatThrottlePattern = regexp.MustCompile(`(?i)(usage limit|rate limit|out of extra usage)`)

// envelopeResultError wraps a Claude CLI `is_error: true` envelope result —
// i.e. the CLI ran fine as a process, but the agent's own result reports
// failure. This is distinct from a process/stderr-level failure (non-zero
// exit, timeout, unparseable output) and must never be matched against
// seatThrottlePattern: the agent's result text is free-form and frequently
// discusses topics (like "rate limit") that have nothing to do with the
// CLI's own subscription-seat throttling.
type envelopeResultError struct {
	msg string
}

func (e *envelopeResultError) Error() string { return e.msg }

// ErrSeatThrottledNoFallback is wrapped into the returned error (errors.Is-
// able) when the Claude CLI reports a subscription-seat throttle and no
// fallback API key is configured or available (agent.fallback_api_key_env
// unset, or set to an env var that itself isn't populated) — a specific,
// actionable failure mode distinct from a generic CLI error. Logged at
// Error once per occurrence (see Run below); FailureTrackingProvider's
// Snapshot().LastErr carries this text through to the dashboard (C5).
var ErrSeatThrottledNoFallback = errors.New("claude seat throttled and no fallback API key configured (set agent.fallback_api_key_env)")

// ClaudeProvider implements Provider using the Claude Code CLI.
type ClaudeProvider struct {
	// FallbackAPIKeyEnv, if set, names an environment variable holding an
	// Anthropic API key. When a run fails because the subscription seat is
	// throttled, Run retries once with ANTHROPIC_API_KEY set from this
	// variable's value (at metered API cost) so intake keeps flowing.
	FallbackAPIKeyEnv string

	// OnSeatFallback, if set, is invoked (synchronously, before the fallback
	// retry runs) every time a seat-throttle triggers the API-key fallback
	// path. It exists so cmd/ can count fallback activations (dashboard
	// metric) without this package importing internal/state — keep it cheap
	// and non-blocking; Run does not recover from a panic in it.
	OnSeatFallback func()
}

func (c *ClaudeProvider) Check() error {
	_, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found in PATH — install it first: https://docs.anthropic.com/en/docs/claude-code")
	}
	return nil
}

func (c *ClaudeProvider) Run(ctx context.Context, opts RunOpts) (*RunResult, error) {
	result, err := c.runOnce(ctx, opts, nil)
	if err == nil {
		return result, nil
	}

	// Envelope-level failures (the agent's own is_error result) are never
	// eligible for throttle-pattern retry — only process/stderr-level
	// failures are, since the envelope Result text is free-form agent
	// output rather than a CLI diagnostic. See envelopeResultError's doc.
	var envErr *envelopeResultError
	if errors.As(err, &envErr) {
		return nil, err
	}

	if !seatThrottlePattern.MatchString(err.Error()) {
		return nil, err
	}
	if c.FallbackAPIKeyEnv == "" {
		slog.Error("claude seat throttled and no fallback API key configured (set agent.fallback_api_key_env)")
		return nil, fmt.Errorf("%w: %w", ErrSeatThrottledNoFallback, err)
	}
	apiKey := os.Getenv(c.FallbackAPIKeyEnv)
	if apiKey == "" {
		slog.Error("claude seat throttled and no fallback API key configured (set agent.fallback_api_key_env)", "env", c.FallbackAPIKeyEnv)
		return nil, fmt.Errorf("%w: %w", ErrSeatThrottledNoFallback, err)
	}

	slog.Warn("claude seat throttled, retrying via API key", "env", c.FallbackAPIKeyEnv)
	if c.OnSeatFallback != nil {
		c.OnSeatFallback()
	}
	return c.runOnce(ctx, opts, []string{"ANTHROPIC_API_KEY=" + apiKey})
}

// runOnce executes a single Claude CLI invocation. extraEnv, when non-empty,
// is appended to the subprocess environment (on top of a full copy of the
// current process environment) — used for the API-key fallback retry.
func (c *ClaudeProvider) runOnce(ctx context.Context, opts RunOpts, extraEnv []string) (*RunResult, error) {
	args := buildArgs(opts)

	callCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	slog.Info("running claude",
		"model", opts.Model,
		"permissions", opts.Permissions,
		"workdir", opts.WorkDir,
	)

	start := time.Now()

	cmd := execCommand(callCtx, "claude", args...)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	duration := time.Since(start)

	if err != nil {
		if callCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("claude timed out after %s", opts.Timeout)
		}
		return nil, fmt.Errorf("claude failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	output := stdout.Bytes()
	slog.Debug("claude raw output", "len", len(output), "duration", duration)

	if stderrStr := strings.TrimSpace(stderr.String()); stderrStr != "" {
		slog.Debug("claude stderr", "stderr", stderrStr)
	}

	result, err := parseEnvelope(output)
	if err != nil {
		return nil, err
	}
	result.Duration = duration

	slog.Debug("claude result parsed",
		"result_len", len(result.Result),
		"cost_usd", result.CostUSD,
		"session_id", result.SessionID,
		"duration", duration,
	)

	return result, nil
}

// buildArgs constructs the Claude CLI argument list from RunOpts.
func buildArgs(opts RunOpts) []string {
	args := []string{
		"--print",
		"--output-format", "json",
	}

	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}

	switch opts.Permissions {
	case PermissionFull:
		args = append(args, "--permission-mode", "acceptEdits",
			"--allowedTools", "Read,Write,Edit,Glob,Grep,Bash,Agent")
	case PermissionReadOnly:
		tools := "Read,Glob,Grep"
		for _, cmd := range opts.AllowedBashCommands {
			tools += ",Bash(" + cmd + ":*)"
		}
		for _, t := range opts.AllowedMCPTools {
			tools += "," + t
		}
		args = append(args, "--allowedTools", tools)
	}

	if len(opts.DeniedReadPaths) > 0 {
		denied := make([]string, 0, len(opts.DeniedReadPaths))
		for _, p := range opts.DeniedReadPaths {
			// "//" anchors the rule to an absolute filesystem path (Claude
			// Code permission-rule syntax) rather than one relative to the
			// project/settings directory.
			denied = append(denied, fmt.Sprintf("Read(//%s/**)", p))
		}
		args = append(args, "--disallowedTools", strings.Join(denied, ","))
	}

	if opts.MCPConfigPath != "" {
		args = append(args, "--mcp-config", opts.MCPConfigPath, "--strict-mcp-config")
	}

	for _, dir := range opts.AdditionalDirs {
		args = append(args, "--add-dir", dir)
	}

	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}

	// -p must be last
	args = append(args, "-p", opts.Prompt)
	return args
}

// claudeEnvelope is the JSON structure returned by `claude --output-format json`.
type claudeEnvelope struct {
	Result    string  `json:"result"`
	IsError   bool    `json:"is_error"`
	SessionID string  `json:"session_id"`
	CostUSD   float64 `json:"total_cost_usd"`
	Subtype   string  `json:"subtype"`
}

// parseEnvelope parses Claude's JSON output envelope into a RunResult.
func parseEnvelope(output []byte) (*RunResult, error) {
	var env claudeEnvelope
	if err := json.Unmarshal(output, &env); err != nil {
		// Not a JSON envelope; treat as raw text output.
		return &RunResult{ //nolint:nilerr // intentional fallback to raw text
			Result: strings.TrimSpace(string(output)),
		}, nil
	}

	if env.IsError {
		return nil, &envelopeResultError{msg: fmt.Sprintf("claude returned error: %s", env.Result)}
	}

	return &RunResult{
		Result:    env.Result,
		SessionID: env.SessionID,
		CostUSD:   env.CostUSD,
	}, nil
}
