package tadpole

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// ClaudeRunOpts configures a Claude CLI invocation.
type ClaudeRunOpts struct {
	WorktreePath       string
	Prompt             string
	Model              string
	MaxTurns           int
	TimeoutMinutes     int
	AppendSystemPrompt string
}

// ClaudeRunOutput holds the parsed result of a Claude CLI run.
type ClaudeRunOutput struct {
	Result    string
	SessionID string
	IsError   bool
	CostUSD   float64
	Duration  time.Duration
}

// RunClaude spawns the Claude CLI in a worktree and returns the parsed output.
func RunClaude(ctx context.Context, opts ClaudeRunOpts) (*ClaudeRunOutput, error) {
	args := []string{
		"--print",
		"--dangerously-skip-permissions",
		"--max-turns", fmt.Sprintf("%d", opts.MaxTurns),
		"--output-format", "json",
		"--model", opts.Model,
	}

	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}

	args = append(args, "-p", opts.Prompt)

	timeout := time.Duration(opts.TimeoutMinutes) * time.Minute
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	slog.Info("running claude",
		"model", opts.Model,
		"max_turns", opts.MaxTurns,
		"timeout_minutes", opts.TimeoutMinutes,
		"worktree", opts.WorktreePath,
	)

	start := time.Now()

	cmd := exec.CommandContext(callCtx, "claude", args...)
	cmd.Dir = opts.WorktreePath
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	duration := time.Since(start)

	if err != nil {
		// Check if it was a timeout
		if callCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("claude timed out after %d minutes", opts.TimeoutMinutes)
		}
		return nil, fmt.Errorf("claude failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	output := stdout.Bytes()
	slog.Debug("claude raw output", "len", len(output), "duration", duration)

	// Parse JSON envelope
	var envelope struct {
		Result    string  `json:"result"`
		IsError   bool    `json:"is_error"`
		SessionID string  `json:"session_id"`
		CostUSD   float64 `json:"cost_usd"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		// Fall back to raw text
		return &ClaudeRunOutput{
			Result:   strings.TrimSpace(string(output)),
			Duration: duration,
		}, nil
	}

	if envelope.IsError {
		return &ClaudeRunOutput{
			Result:   envelope.Result,
			IsError:  true,
			Duration: duration,
			CostUSD:  envelope.CostUSD,
		}, fmt.Errorf("claude returned error: %s", envelope.Result)
	}

	return &ClaudeRunOutput{
		Result:    envelope.Result,
		SessionID: envelope.SessionID,
		IsError:   false,
		CostUSD:   envelope.CostUSD,
		Duration:  duration,
	}, nil
}
