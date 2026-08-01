package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnvelope_Valid(t *testing.T) {
	output := []byte(`{"result":"hello world","is_error":false,"session_id":"abc123","total_cost_usd":0.05,"subtype":""}`)
	r, err := parseEnvelope(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Result != "hello world" {
		t.Errorf("result = %q, want %q", r.Result, "hello world")
	}
	if r.SessionID != "abc123" {
		t.Errorf("session_id = %q, want %q", r.SessionID, "abc123")
	}
	if r.CostUSD != 0.05 {
		t.Errorf("cost = %v, want 0.05", r.CostUSD)
	}
}

func TestParseEnvelope_Error(t *testing.T) {
	output := []byte(`{"result":"something went wrong","is_error":true}`)
	_, err := parseEnvelope(output)
	if err == nil {
		t.Fatal("expected error for is_error=true")
	}
	if got := err.Error(); got != "claude returned error: something went wrong" {
		t.Errorf("error = %q", got)
	}
}

func TestParseEnvelope_InvalidJSON(t *testing.T) {
	output := []byte(`not json at all`)
	r, err := parseEnvelope(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Result != "not json at all" {
		t.Errorf("result = %q, want fallback to raw text", r.Result)
	}
}

func TestParseEnvelope_EmptyResult(t *testing.T) {
	output := []byte(`{"result":"","is_error":false}`)
	r, err := parseEnvelope(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Result != "" {
		t.Errorf("result = %q, want empty", r.Result)
	}
}

func TestBuildArgs_PermissionNone(t *testing.T) {
	args := buildArgs(RunOpts{
		Model:  "haiku",
		Prompt: "classify this",
	})
	assertContains(t, args, "--print")
	assertContains(t, args, "--output-format")
	assertContains(t, args, "--model")
	assertNotContains(t, args, "--dangerously-skip-permissions")
	assertNotContains(t, args, "--allowedTools")
	assertNotContains(t, args, "--max-turns")
	// -p must be second-to-last
	if args[len(args)-2] != "-p" || args[len(args)-1] != "classify this" {
		t.Errorf("expected -p as last flag, got: %v", args[len(args)-2:])
	}
}

func TestBuildArgs_PermissionReadOnly(t *testing.T) {
	args := buildArgs(RunOpts{
		Model:       "sonnet",
		Permissions: PermissionReadOnly,
		Prompt:      "investigate",
	})
	assertContains(t, args, "--allowedTools")
	assertNotContains(t, args, "--dangerously-skip-permissions")
}

func TestBuildArgs_PermissionFull(t *testing.T) {
	args := buildArgs(RunOpts{
		Model:       "sonnet",
		Permissions: PermissionFull,
		Prompt:      "fix it",
	})
	assertContains(t, args, "--permission-mode")
	assertContains(t, args, "acceptEdits")
	assertContains(t, args, "--allowedTools")
	assertNotContains(t, args, "--dangerously-skip-permissions")
}

func TestBuildArgs_AdditionalDirs(t *testing.T) {
	args := buildArgs(RunOpts{
		Prompt:         "explore",
		AdditionalDirs: []string{"/repo/a", "/repo/b"},
	})
	count := 0
	for _, a := range args {
		if a == "--add-dir" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 --add-dir flags, got %d", count)
	}
}

func TestBuildArgs_AppendSystemPrompt(t *testing.T) {
	args := buildArgs(RunOpts{
		Prompt:             "do work",
		AppendSystemPrompt: "extra instructions",
	})
	assertContains(t, args, "--append-system-prompt")
}

func TestBuildArgs_NoModel(t *testing.T) {
	args := buildArgs(RunOpts{Prompt: "test"})
	assertNotContains(t, args, "--model")
}

func TestBuildArgs_NoMaxTurns(t *testing.T) {
	args := buildArgs(RunOpts{Prompt: "test"})
	assertNotContains(t, args, "--max-turns")
}

func TestBuildResumeArgs_NoMaxTurns(t *testing.T) {
	args := buildResumeArgs("sess-123", "continue please")
	assertNotContains(t, args, "--max-turns")
	assertContains(t, args, "--resume")
	assertContains(t, args, "sess-123")
}

func TestBuildArgs_PermissionReadOnlyWithBash(t *testing.T) {
	args := buildArgs(RunOpts{
		Model:               "sonnet",
		Permissions:         PermissionReadOnly,
		Prompt:              "investigate",
		AllowedBashCommands: []string{"gh pr view", "gh issue view"},
	})

	// Find the --allowedTools value
	var tools string
	for i, a := range args {
		if a == "--allowedTools" && i+1 < len(args) {
			tools = args[i+1]
			break
		}
	}
	if tools == "" {
		t.Fatal("expected --allowedTools flag")
	}
	if !strings.Contains(tools, "Bash(gh pr view:*)") {
		t.Errorf("expected tools to contain Bash(gh pr view:*), got %q", tools)
	}
	if !strings.Contains(tools, "Bash(gh issue view:*)") {
		t.Errorf("expected tools to contain Bash(gh issue view:*), got %q", tools)
	}
	if !strings.Contains(tools, "Read") {
		t.Errorf("expected tools to contain Read, got %q", tools)
	}
}

func TestBuildArgs_MCPConfigPath(t *testing.T) {
	args := buildArgs(RunOpts{
		Prompt:        "investigate",
		MCPConfigPath: "/tmp/mcp-config.json",
	})
	assertContains(t, args, "--mcp-config")
	assertContains(t, args, "/tmp/mcp-config.json")
	assertContains(t, args, "--strict-mcp-config")
}

func TestBuildArgs_NoMCPConfigPath(t *testing.T) {
	args := buildArgs(RunOpts{Prompt: "test"})
	assertNotContains(t, args, "--mcp-config")
	assertNotContains(t, args, "--strict-mcp-config")
}

func TestBuildArgs_PermissionReadOnlyWithMCPTools(t *testing.T) {
	args := buildArgs(RunOpts{
		Model:           "sonnet",
		Permissions:     PermissionReadOnly,
		Prompt:          "investigate",
		AllowedMCPTools: []string{"mcp__sentry__*"},
	})

	var tools string
	for i, a := range args {
		if a == "--allowedTools" && i+1 < len(args) {
			tools = args[i+1]
			break
		}
	}
	if tools == "" {
		t.Fatal("expected --allowedTools flag")
	}
	if !strings.Contains(tools, "mcp__sentry__*") {
		t.Errorf("expected tools to contain mcp__sentry__*, got %q", tools)
	}
	if !strings.Contains(tools, "Read") {
		t.Errorf("expected tools to contain Read, got %q", tools)
	}
}

func TestBuildArgs_PermissionReadOnlyWithBashAndMCPTools(t *testing.T) {
	args := buildArgs(RunOpts{
		Permissions:         PermissionReadOnly,
		Prompt:              "investigate",
		AllowedBashCommands: []string{"gh pr view"},
		AllowedMCPTools:     []string{"mcp__sentry__*"},
	})

	var tools string
	for i, a := range args {
		if a == "--allowedTools" && i+1 < len(args) {
			tools = args[i+1]
			break
		}
	}
	want := "Read,Glob,Grep,Bash(gh pr view:*),mcp__sentry__*"
	if tools != want {
		t.Errorf("tools = %q, want %q", tools, want)
	}
}

// fakeScript describes a single canned CLI invocation for the execCommand seam.
type fakeScript struct {
	stdout   string
	stderr   string
	exitCode int
}

// newFakeExecCommand returns an execCommand-shaped function that, on each
// successive call, executes the next script in order (as a real subprocess,
// so cmd.Run/cmd.Env behave exactly as they would against the real "claude"
// binary). It also returns the slice of *exec.Cmd it produced, so tests can
// inspect what env/args were ultimately set on them after the code under
// test has finished mutating and running them.
func newFakeExecCommand(t *testing.T, scripts []fakeScript) (*[]*exec.Cmd, func(ctx context.Context, name string, arg ...string) *exec.Cmd) {
	t.Helper()
	dir := t.TempDir()
	calls := &[]*exec.Cmd{}
	idx := 0
	fn := func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		if idx >= len(scripts) {
			t.Fatalf("execCommand invoked more times (%d) than scripts provided (%d)", idx+1, len(scripts))
		}
		s := scripts[idx]
		scriptPath := filepath.Join(dir, fmt.Sprintf("fake-claude-%d.sh", idx))
		idx++

		body := "#!/bin/sh\n"
		if s.stdout != "" {
			body += "cat <<'FAKE_STDOUT_EOF'\n" + s.stdout + "\nFAKE_STDOUT_EOF\n"
		}
		if s.stderr != "" {
			body += "cat <<'FAKE_STDERR_EOF' >&2\n" + s.stderr + "\nFAKE_STDERR_EOF\n"
		}
		body += fmt.Sprintf("exit %d\n", s.exitCode)

		if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
			t.Fatalf("write fake script: %v", err)
		}

		cmd := exec.CommandContext(ctx, scriptPath)
		*calls = append(*calls, cmd)
		return cmd
	}
	return calls, fn
}

func TestRun_ThrottleFallback_Success(t *testing.T) {
	calls, fake := newFakeExecCommand(t, []fakeScript{
		{stderr: "Claude AI usage limit reached, retry later", exitCode: 1},
		{stdout: `{"result":"done via fallback","is_error":false,"session_id":"s1","total_cost_usd":0.01}`, exitCode: 0},
	})
	origExecCommand := execCommand
	execCommand = fake
	defer func() { execCommand = origExecCommand }()

	t.Setenv("FAKE_ANTHROPIC_KEY", "sk-ant-test-value")

	p := &ClaudeProvider{FallbackAPIKeyEnv: "FAKE_ANTHROPIC_KEY"}
	result, err := p.Run(context.Background(), RunOpts{Prompt: "do work"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Result != "done via fallback" {
		t.Errorf("result = %q, want %q", result.Result, "done via fallback")
	}

	if len(*calls) != 2 {
		t.Fatalf("expected 2 executions, got %d", len(*calls))
	}
	first := (*calls)[0]
	for _, e := range first.Env {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
			t.Errorf("first execution should not carry ANTHROPIC_API_KEY, got env %v", first.Env)
		}
	}
	second := (*calls)[1]
	found := false
	for _, e := range second.Env {
		if e == "ANTHROPIC_API_KEY=sk-ant-test-value" {
			found = true
		}
	}
	if !found {
		t.Errorf("second execution env missing ANTHROPIC_API_KEY=sk-ant-test-value, got %v", second.Env)
	}
}

func TestRun_ThrottleNoFallbackEnv_ReturnsOriginalError(t *testing.T) {
	calls, fake := newFakeExecCommand(t, []fakeScript{
		{stderr: "Claude AI usage limit reached, retry later", exitCode: 1},
	})
	origExecCommand := execCommand
	execCommand = fake
	defer func() { execCommand = origExecCommand }()

	// FallbackAPIKeyEnv left empty entirely.
	p := &ClaudeProvider{}
	_, err := p.Run(context.Background(), RunOpts{Prompt: "do work"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage limit") {
		t.Errorf("expected original throttle error preserved, got: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 execution, got %d", len(*calls))
	}
}

func TestRun_ThrottleFallbackEnvUnset_ReturnsOriginalError(t *testing.T) {
	calls, fake := newFakeExecCommand(t, []fakeScript{
		{stderr: "rate limit exceeded, try again shortly", exitCode: 1},
	})
	origExecCommand := execCommand
	execCommand = fake
	defer func() { execCommand = origExecCommand }()

	// FallbackAPIKeyEnv points at a var that isn't actually set in the environment.
	os.Unsetenv("FAKE_ANTHROPIC_KEY_UNSET")
	p := &ClaudeProvider{FallbackAPIKeyEnv: "FAKE_ANTHROPIC_KEY_UNSET"}
	_, err := p.Run(context.Background(), RunOpts{Prompt: "do work"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("expected original throttle error preserved, got: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 execution (no fallback without env value), got %d", len(*calls))
	}
}

func TestRun_NonThrottleError_NoFallback(t *testing.T) {
	calls, fake := newFakeExecCommand(t, []fakeScript{
		{stderr: "something unrelated broke", exitCode: 1},
	})
	origExecCommand := execCommand
	execCommand = fake
	defer func() { execCommand = origExecCommand }()

	t.Setenv("FAKE_ANTHROPIC_KEY", "sk-ant-test-value")
	p := &ClaudeProvider{FallbackAPIKeyEnv: "FAKE_ANTHROPIC_KEY"}
	_, err := p.Run(context.Background(), RunOpts{Prompt: "do work"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "something unrelated broke") {
		t.Errorf("expected original error preserved, got: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 execution for non-throttle error, got %d", len(*calls))
	}
}

func assertContains(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, a := range args {
		if a == flag {
			return
		}
	}
	t.Errorf("expected args to contain %q, got: %v", flag, args)
}

func assertNotContains(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, a := range args {
		if a == flag {
			t.Errorf("expected args NOT to contain %q, got: %v", flag, args)
			return
		}
	}
}
