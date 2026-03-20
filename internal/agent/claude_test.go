package agent

import (
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
	if r.HitMaxTurns {
		t.Error("expected HitMaxTurns=false")
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

func TestParseEnvelope_MaxTurns(t *testing.T) {
	output := []byte(`{"result":"","is_error":false,"subtype":"error_max_turns","session_id":"sess42"}`)
	r, err := parseEnvelope(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.HitMaxTurns {
		t.Error("expected HitMaxTurns=true")
	}
	if r.SessionID != "sess42" {
		t.Errorf("session_id = %q, want %q", r.SessionID, "sess42")
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
	args, cleanup := buildArgs(RunOpts{
		Model:    "haiku",
		MaxTurns: 1,
		Prompt:   "classify this",
	})
	defer cleanup()
	assertContains(t, args, "--print")
	assertContains(t, args, "--output-format")
	assertContains(t, args, "--model")
	assertNotContains(t, args, "--dangerously-skip-permissions")
	assertNotContains(t, args, "--allowedTools")
	// -p must be second-to-last
	if args[len(args)-2] != "-p" || args[len(args)-1] != "classify this" {
		t.Errorf("expected -p as last flag, got: %v", args[len(args)-2:])
	}
}

func TestBuildArgs_PermissionReadOnly(t *testing.T) {
	args, cleanup := buildArgs(RunOpts{
		Model:       "sonnet",
		Permissions: PermissionReadOnly,
		Prompt:      "investigate",
	})
	defer cleanup()
	assertContains(t, args, "--allowedTools")
	assertNotContains(t, args, "--dangerously-skip-permissions")
}

func TestBuildArgs_PermissionFull(t *testing.T) {
	args, cleanup := buildArgs(RunOpts{
		Model:       "sonnet",
		Permissions: PermissionFull,
		Prompt:      "fix it",
	})
	defer cleanup()
	assertContains(t, args, "--permission-mode")
	assertContains(t, args, "acceptEdits")
	assertContains(t, args, "--allowedTools")
	assertNotContains(t, args, "--dangerously-skip-permissions")
}

func TestBuildArgs_AdditionalDirs(t *testing.T) {
	args, cleanup := buildArgs(RunOpts{
		Prompt:         "explore",
		AdditionalDirs: []string{"/repo/a", "/repo/b"},
	})
	defer cleanup()
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
	args, cleanup := buildArgs(RunOpts{
		Prompt:             "do work",
		AppendSystemPrompt: "extra instructions",
	})
	defer cleanup()
	assertContains(t, args, "--append-system-prompt")
}

func TestBuildArgs_NoModel(t *testing.T) {
	args, cleanup := buildArgs(RunOpts{Prompt: "test"})
	defer cleanup()
	assertNotContains(t, args, "--model")
}

func TestBuildArgs_NoMaxTurns(t *testing.T) {
	args, cleanup := buildArgs(RunOpts{Prompt: "test"})
	defer cleanup()
	assertNotContains(t, args, "--max-turns")
}

func TestBuildArgs_PermissionReadOnlyWithBash(t *testing.T) {
	args, cleanup := buildArgs(RunOpts{
		Model:               "sonnet",
		Permissions:         PermissionReadOnly,
		Prompt:              "investigate",
		AllowedBashCommands: []string{"gh"},
	})
	defer cleanup()

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
	if !strings.Contains(tools, "Bash(gh:*)") {
		t.Errorf("expected tools to contain Bash(gh:*), got %q", tools)
	}
	if !strings.Contains(tools, "Read") {
		t.Errorf("expected tools to contain Read, got %q", tools)
	}
}

func TestBuildArgs_WithMCPServers(t *testing.T) {
	args, cleanup := buildArgs(RunOpts{
		Permissions: PermissionReadOnly,
		Prompt:      "investigate",
		MCPServers:  []MCPServerConfig{{Name: "linear", URL: "http://localhost:8099"}},
	})
	defer cleanup()
	assertContains(t, args, "--mcp-config")
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
