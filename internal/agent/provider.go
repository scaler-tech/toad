// Package agent abstracts the coding agent CLI (Claude Code, etc.) behind a
// provider interface so different agent backends can be swapped in via config.
package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Permission controls what tools the agent is allowed to use.
type Permission int

const (
	// PermissionNone disables all tool use — pure text reasoning.
	PermissionNone Permission = iota
	// PermissionReadOnly allows read-only file access tools.
	PermissionReadOnly
	// PermissionFull grants unrestricted tool access including file writes.
	PermissionFull
)

// RunOpts configures a single agent invocation.
type RunOpts struct {
	Prompt              string
	Model               string
	WorkDir             string // working directory; empty = inherit process cwd
	Timeout             time.Duration
	Permissions         Permission
	AdditionalDirs      []string // extra directories the agent can access
	AppendSystemPrompt  string   // optional system prompt addition
	AllowedBashCommands []string // bash command prefixes allowed in read-only mode (e.g. ["gh"])
	MCPConfigPath       string   // path to an MCP config file; emits --mcp-config <path> --strict-mcp-config
	AllowedMCPTools     []string // MCP tool names appended to --allowedTools (e.g. "mcp__sentry__*")
	DeniedReadPaths     []string // absolute directory paths the agent may never Read from, regardless of Permissions (e.g. the toad home dir, where the MCP bearer token lives)
}

// RunResult holds the parsed output of an agent invocation.
type RunResult struct {
	Result    string
	SessionID string
	CostUSD   float64
	Duration  time.Duration
}

// Provider is the interface that agent backends must implement.
type Provider interface {
	// Run executes the agent with the given options.
	Run(ctx context.Context, opts RunOpts) (*RunResult, error)

	// Check verifies the agent CLI is available on this system.
	Check() error
}

// NewProvider returns a Provider for the given platform name. fallbackEnv
// names an environment variable holding an Anthropic API key to fall back to
// when the subscription seat is throttled (Claude platform only); pass "" to
// disable the fallback.
func NewProvider(platform, fallbackEnv string) (Provider, error) {
	switch strings.ToLower(platform) {
	case "claude", "":
		return &ClaudeProvider{FallbackAPIKeyEnv: fallbackEnv}, nil
	default:
		return nil, fmt.Errorf("unsupported agent platform: %q", platform)
	}
}

// ReadDenyingProvider wraps a Provider and merges a fixed set of
// DeniedReadPaths into every RunOpts before delegating. It exists so callers
// that construct a Provider once and hand it to several read-only agent
// classes (investigations, ribbit) can deny access to a sensitive directory
// (e.g. the toad home dir, where mcp-config.json and its bearer token live)
// without threading a new parameter through each of those packages' own
// RunOpts construction.
type ReadDenyingProvider struct {
	Provider
	DeniedReadPaths []string
}

// Run merges DeniedReadPaths into opts (preserving any paths the caller
// already set) before delegating to the wrapped Provider.
func (p *ReadDenyingProvider) Run(ctx context.Context, opts RunOpts) (*RunResult, error) {
	if len(p.DeniedReadPaths) > 0 {
		merged := make([]string, 0, len(opts.DeniedReadPaths)+len(p.DeniedReadPaths))
		merged = append(merged, opts.DeniedReadPaths...)
		merged = append(merged, p.DeniedReadPaths...)
		opts.DeniedReadPaths = merged
	}
	return p.Provider.Run(ctx, opts)
}
