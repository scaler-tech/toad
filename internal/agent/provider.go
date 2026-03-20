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

// MCPServerConfig describes a remote MCP server the agent should connect to.
type MCPServerConfig struct {
	Name string
	URL  string
}

// RunOpts configures a single agent invocation.
type RunOpts struct {
	Prompt              string
	Model               string
	WorkDir             string // working directory; empty = inherit process cwd
	MaxTurns            int
	Timeout             time.Duration
	Permissions         Permission
	AdditionalDirs      []string          // extra directories the agent can access
	AppendSystemPrompt  string            // optional system prompt addition
	AllowedBashCommands []string          // bash command prefixes allowed in read-only mode (e.g. ["gh"])
	MCPServers          []MCPServerConfig // MCP servers the agent should connect to
}

// RunResult holds the parsed output of an agent invocation.
type RunResult struct {
	Result      string
	SessionID   string
	CostUSD     float64
	Duration    time.Duration
	HitMaxTurns bool
}

// Provider is the interface that agent backends must implement.
type Provider interface {
	// Run executes the agent with the given options.
	Run(ctx context.Context, opts RunOpts) (*RunResult, error)

	// Resume continues a previous session by ID. Providers that do not
	// support session resumption return ErrResumeNotSupported.
	Resume(ctx context.Context, sessionID, prompt, workDir string) (*RunResult, error)

	// Check verifies the agent CLI is available on this system.
	Check() error
}

// ErrResumeNotSupported is returned by providers that cannot resume sessions.
var ErrResumeNotSupported = fmt.Errorf("agent provider does not support session resumption")

// NewProvider returns a Provider for the given platform name.
func NewProvider(platform string) (Provider, error) {
	switch strings.ToLower(platform) {
	case "claude", "":
		return &ClaudeProvider{}, nil
	default:
		return nil, fmt.Errorf("unsupported agent platform: %q", platform)
	}
}
