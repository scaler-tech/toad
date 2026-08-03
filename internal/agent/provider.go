// Package agent abstracts the coding agent CLI (Claude Code, etc.) behind a
// provider interface so different agent backends can be swapped in via config.
package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
// disable the fallback. onSeatFallback, if non-nil, is wired to the
// resulting provider's seat-throttle fallback hook (Claude platform only;
// harmlessly ignored by other platforms) — pass nil if the caller doesn't
// need to observe fallback activations.
func NewProvider(platform, fallbackEnv string, onSeatFallback func()) (Provider, error) {
	switch strings.ToLower(platform) {
	case "claude", "":
		return &ClaudeProvider{FallbackAPIKeyEnv: fallbackEnv, OnSeatFallback: onSeatFallback}, nil
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

// maxTrackedErrLen bounds how much of a failing Run call's error text
// FailureTrackingProvider retains — enough to be useful on the dashboard
// without risking an enormous stderr dump bloating daemon_stats.
const maxTrackedErrLen = 200

// FailureSnapshot is a point-in-time read of a FailureTrackingProvider's
// tracked state — see FailureTrackingProvider's doc comment.
type FailureSnapshot struct {
	// Consecutive is the number of Run calls that have failed in a row,
	// since the last success (or since the provider was created, if there
	// hasn't been one yet). Reset to 0 on every successful Run.
	Consecutive int64
	// LastSuccessAt is the time of the most recent successful Run call, or
	// the zero Time if there hasn't been one yet.
	LastSuccessAt time.Time
	// LastErr is the most recent failing Run call's error text (truncated to
	// maxTrackedErrLen), or "" if there hasn't been a failure yet.
	LastErr string
}

// FailureTrackingProvider wraps a Provider and tracks consecutive Run
// failures/successes (same decorator pattern as ReadDenyingProvider above).
// It exists so a sustained streak of failing Claude CLI calls — an expired
// auth token, a broken CLI install, a seat throttled with no fallback
// configured (see ErrSeatThrottledNoFallback), etc. — is visible on the
// dashboard rather than living only in scattered per-call log lines that
// nobody's watching in real time.
//
// Intended to be wired ONCE around the base Provider in cmd/root.go, before
// ReadDenyingProvider/triage/digest/ribbit each get their own reference —
// see root.go's wiring comment — so every call path (triage, ribbit,
// investigations, digest) feeds the same counters.
type FailureTrackingProvider struct {
	Provider

	mu            sync.Mutex
	consecutive   int64
	lastSuccessAt time.Time
	lastErr       string
}

// Run delegates to the wrapped Provider and updates the tracked
// success/failure state before returning. A nil error resets the
// consecutive-failure counter and records the success time; a non-nil error
// increments the counter and records the (truncated) error text. The
// underlying result/error are returned unchanged either way — this is
// observation only, never altering behavior.
func (p *FailureTrackingProvider) Run(ctx context.Context, opts RunOpts) (*RunResult, error) {
	result, err := p.Provider.Run(ctx, opts)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.consecutive = 0
		p.lastSuccessAt = time.Now()
		return result, nil
	}
	p.consecutive++
	msg := err.Error()
	if len(msg) > maxTrackedErrLen {
		msg = msg[:maxTrackedErrLen]
	}
	p.lastErr = msg
	return result, err
}

// Snapshot returns a point-in-time read of the tracked failure state.
func (p *FailureTrackingProvider) Snapshot() FailureSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return FailureSnapshot{
		Consecutive:   p.consecutive,
		LastSuccessAt: p.lastSuccessAt,
		LastErr:       p.lastErr,
	}
}
