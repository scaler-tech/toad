package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/scaler-tech/toad/internal/config"
)

func TestWriteMCPConfig_Empty(t *testing.T) {
	path, err := WriteMCPConfig(t.TempDir(), map[string]config.MCPServerConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path for empty servers map, got %q", path)
	}
}

func TestWriteMCPConfig_Nil(t *testing.T) {
	path, err := WriteMCPConfig(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path for nil servers map, got %q", path)
	}
}

func TestWriteMCPConfig_HTTPServerWithBearerToken(t *testing.T) {
	t.Setenv("SENTRY_TOKEN", "secret-token-123")

	dir := t.TempDir()
	path, err := WriteMCPConfig(dir, map[string]config.MCPServerConfig{
		"sentry": {
			URL:          "https://mcp.sentry.dev",
			AuthTokenEnv: "SENTRY_TOKEN",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}

	got := readRenderedConfig(t, path)
	servers, ok := got["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcpServers key, got: %v", got)
	}
	sentry, ok := servers["sentry"].(map[string]any)
	if !ok {
		t.Fatalf("expected sentry server entry, got: %v", servers)
	}
	if sentry["type"] != "http" {
		t.Errorf("type = %v, want http", sentry["type"])
	}
	if sentry["url"] != "https://mcp.sentry.dev" {
		t.Errorf("url = %v, want https://mcp.sentry.dev", sentry["url"])
	}
	headers, ok := sentry["headers"].(map[string]any)
	if !ok {
		t.Fatalf("expected headers, got: %v", sentry)
	}
	if headers["Authorization"] != "Bearer secret-token-123" {
		t.Errorf("Authorization = %v, want %q", headers["Authorization"], "Bearer secret-token-123")
	}
}

func TestWriteMCPConfig_HTTPServerNoAuthTokenEnv(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(dir, map[string]config.MCPServerConfig{
		"public": {URL: "https://mcp.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := readRenderedConfig(t, path)
	servers := got["mcpServers"].(map[string]any)
	public := servers["public"].(map[string]any)
	if _, exists := public["headers"]; exists {
		t.Errorf("expected no headers key when AuthTokenEnv is empty, got: %v", public)
	}
}

func TestWriteMCPConfig_HTTPServerAuthTokenEnvUnset(t *testing.T) {
	const unsetVar = "TOAD_TEST_TOTALLY_UNSET_TOKEN_VAR"
	_ = os.Unsetenv(unsetVar)

	dir := t.TempDir()
	path, err := WriteMCPConfig(dir, map[string]config.MCPServerConfig{
		"svc": {URL: "https://example.com", AuthTokenEnv: unsetVar},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := readRenderedConfig(t, path)
	servers := got["mcpServers"].(map[string]any)
	svc := servers["svc"].(map[string]any)
	if _, exists := svc["headers"]; exists {
		t.Errorf("expected no headers key when env var is unset, got: %v", svc)
	}
}

func TestWriteMCPConfig_CommandServer(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(dir, map[string]config.MCPServerConfig{
		"local": {Command: "/usr/local/bin/mymcp"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := readRenderedConfig(t, path)
	servers := got["mcpServers"].(map[string]any)
	local := servers["local"].(map[string]any)
	if local["command"] != "/usr/local/bin/mymcp" {
		t.Errorf("command = %v, want /usr/local/bin/mymcp", local["command"])
	}
	if _, exists := local["type"]; exists {
		t.Errorf("expected no type key for command server, got: %v", local)
	}
	if _, exists := local["url"]; exists {
		t.Errorf("expected no url key for command server, got: %v", local)
	}
}

func TestWriteMCPConfig_WritesToDir(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(dir, map[string]config.MCPServerConfig{
		"svc": {URL: "https://example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("expected file written inside %q, got %q", dir, path)
	}
}

func readRenderedConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written config: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("invalid JSON written: %v", err)
	}
	return got
}
