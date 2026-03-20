package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeProvider implements Provider using the Claude Code CLI.
type ClaudeProvider struct{}

func (c *ClaudeProvider) Check() error {
	_, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found in PATH — install it first: https://docs.anthropic.com/en/docs/claude-code")
	}
	return nil
}

func (c *ClaudeProvider) Run(ctx context.Context, opts RunOpts) (*RunResult, error) {
	args, cleanup := buildArgs(opts)
	defer cleanup()

	callCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	slog.Info("running claude",
		"model", opts.Model,
		"max_turns", opts.MaxTurns,
		"permissions", opts.Permissions,
		"workdir", opts.WorkDir,
	)

	start := time.Now()

	cmd := exec.CommandContext(callCtx, "claude", args...)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
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
		"hit_max_turns", result.HitMaxTurns,
		"session_id", result.SessionID,
		"duration", duration,
	)

	return result, nil
}

func (c *ClaudeProvider) Resume(ctx context.Context, sessionID, prompt, workDir string) (*RunResult, error) {
	args := []string{
		"--print",
		"--resume", sessionID,
		"--max-turns", "1",
		"--output-format", "json",
		"-p", prompt,
	}

	start := time.Now()

	cmd := exec.CommandContext(ctx, "claude", args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	duration := time.Since(start)

	if err != nil {
		return nil, fmt.Errorf("claude resume call failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	slog.Debug("claude resume raw output", "len", len(stdout.Bytes()), "duration", duration)

	result, err := parseEnvelope(stdout.Bytes())
	if err != nil {
		return nil, err
	}
	result.Duration = duration
	return result, nil
}

// buildArgs constructs the Claude CLI argument list from RunOpts.
// The returned cleanup function must be called when the args are no longer needed.
func buildArgs(opts RunOpts) ([]string, func()) {
	cleanup := func() {}

	args := []string{
		"--print",
		"--output-format", "json",
	}

	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opts.MaxTurns))
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
		args = append(args, "--allowedTools", tools)
	}

	if len(opts.MCPServers) > 0 {
		if path, err := writeMCPConfig(opts.MCPServers); err == nil {
			args = append(args, "--mcp-config", path)
			cleanup = func() { _ = os.Remove(path) }
		}
	}

	for _, dir := range opts.AdditionalDirs {
		args = append(args, "--add-dir", dir)
	}

	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}

	// -p must be last
	args = append(args, "-p", opts.Prompt)
	return args, cleanup
}

// writeMCPConfig writes a Claude Code MCP config JSON file to a temp path and returns the path.
func writeMCPConfig(servers []MCPServerConfig) (string, error) {
	type serverEntry struct {
		URL string `json:"url"`
	}
	mcpServers := make(map[string]serverEntry, len(servers))
	for _, s := range servers {
		mcpServers[s.Name] = serverEntry{URL: s.URL}
	}
	cfg := struct {
		MCPServers map[string]serverEntry `json:"mcpServers"`
	}{MCPServers: mcpServers}

	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}

	f, err := os.CreateTemp("", "toad-mcp-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
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
		return nil, fmt.Errorf("claude returned error: %s", env.Result)
	}

	return &RunResult{
		Result:      env.Result,
		SessionID:   env.SessionID,
		CostUSD:     env.CostUSD,
		HitMaxTurns: env.Subtype == "error_max_turns",
	}, nil
}
