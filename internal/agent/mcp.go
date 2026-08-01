package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/scaler-tech/toad/internal/config"
)

// mcpServerJSON is the per-server shape written into the Claude CLI's
// --mcp-config file. Only the fields relevant to the server's transport are
// populated; the rest are omitted via omitempty.
type mcpServerJSON struct {
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Command string            `json:"command,omitempty"`
}

// mcpConfigJSON is the top-level document shape expected by `claude --mcp-config`.
type mcpConfigJSON struct {
	MCPServers map[string]mcpServerJSON `json:"mcpServers"`
}

// WriteMCPConfig renders servers into an MCP config JSON file compatible with
// the Claude CLI's --mcp-config flag and writes it inside dir. It returns the
// path to the written file. When servers is empty (or nil), it returns ""
// and a nil error — no file is written and no --mcp-config flag is needed.
//
// HTTP servers (URL set) render as {"type":"http","url":...}, with an
// Authorization: Bearer header populated from os.Getenv(AuthTokenEnv) when
// AuthTokenEnv is set and resolves to a non-empty value; the header is
// omitted otherwise. Command servers (Command set) render as {"command":...}.
func WriteMCPConfig(dir string, servers map[string]config.MCPServerConfig) (string, error) {
	if len(servers) == 0 {
		return "", nil
	}

	rendered := make(map[string]mcpServerJSON, len(servers))
	for name, s := range servers {
		if s.Command != "" {
			rendered[name] = mcpServerJSON{Command: s.Command}
			continue
		}

		entry := mcpServerJSON{Type: "http", URL: s.URL}
		if s.AuthTokenEnv != "" {
			if token := os.Getenv(s.AuthTokenEnv); token != "" {
				entry.Headers = map[string]string{"Authorization": "Bearer " + token}
			}
		}
		rendered[name] = entry
	}

	data, err := json.MarshalIndent(mcpConfigJSON{MCPServers: rendered}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal mcp config: %w", err)
	}

	path := filepath.Join(dir, "mcp-config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write mcp config: %w", err)
	}
	return path, nil
}
