# Agent Providers

Toad uses a `Provider` interface to interact with coding agents. In toad v2 the
agent no longer writes code or ships PRs (tadpole coding was dropped) — it's
used for read-only work: triage classification, investigation, Q&A (ribbit),
and digest analysis, all through `Run()` calls with different permission
levels.

## Current Providers

| Provider | Platform key | CLI tool | Status |
|----------|-------------|----------|--------|
| Claude Code | `claude` | `claude` | Implemented |

## Possible Additions

| Provider | Platform key | CLI tool | Notes |
|----------|-------------|----------|-------|
| OpenAI Codex CLI | `codex` | `codex` | Similar CLI-based architecture, JSON output |
| Open Code | `opencode` | `opencode` | Open-source alternative |
| Aider | `aider` | `aider` | Git-aware coding assistant |
| Goose | `goose` | `goose` | Block's open-source agent |

## The Provider Interface

```go
type Provider interface {
    Run(ctx context.Context, opts RunOpts) (*RunResult, error)
    Check() error
}
```

**`Run`** executes the agent with a prompt and returns the result text. Callsites in Toad v2: triage classification, digest analysis, ribbit Q&A, and investigation — all short, bounded-turn reasoning tasks now that coding runs are gone.

**`Check`** verifies the CLI tool is installed (e.g., `exec.LookPath`).

### Permission Levels

| Level | Claude Code mapping | Purpose |
|-------|-------------------|---------|
| `PermissionNone` | No tool flags | Text-only reasoning (triage, digest analysis) |
| `PermissionReadOnly` | `--allowedTools Read,Glob,Grep` | Read-only codebase access (ribbit, investigation) |
| `PermissionFull` | `--dangerously-skip-permissions` | Full read/write; defined but currently unused — no v2 flow writes code |

### RunOpts → CLI Args

The provider translates `RunOpts` fields to CLI arguments:

| Field | Claude Code flag | Required |
|-------|-----------------|----------|
| `Prompt` | `-p <prompt>` (always last) | Yes |
| `Model` | `--model <model>` | No |
| `MaxTurns` | `--max-turns <n>` | No |
| `WorkDir` | `cmd.Dir = <path>` | No |
| `Timeout` | `context.WithTimeout` | No |
| `Permissions` | See table above | No (defaults to None) |
| `AdditionalDirs` | `--add-dir <path>` (repeated) | No |
| `AppendSystemPrompt` | `--append-system-prompt <text>` | No |

All providers must also pass `--print` (non-interactive) and `--output-format json` equivalents.

## Adding a New Provider

### 1. Create the provider file

Create `internal/agent/<name>.go` implementing the `Provider` interface:

```go
type CodexProvider struct{}

func (c *CodexProvider) Check() error {
    _, err := exec.LookPath("codex")
    if err != nil {
        return fmt.Errorf("codex CLI not found in PATH")
    }
    return nil
}

func (c *CodexProvider) Run(ctx context.Context, opts RunOpts) (*RunResult, error) {
    // 1. Build CLI args from opts
    // 2. Execute subprocess with ctx
    // 3. Parse output into RunResult
}
```

### 2. Register in the factory

In `provider.go`, add a case to `NewProvider`:

```go
case "codex":
    return &CodexProvider{}, nil
```

### 3. Add to config validation

In `internal/config/config.go`, add to `validAgentPlatforms`:

```go
validAgentPlatforms := map[string]bool{"claude": true, "codex": true}
```

### 4. Write tests

Add `internal/agent/<name>_test.go` covering:
- Output parsing (success, error, invalid output, edge cases)
- Argument building for all permission levels

## Known Challenges for New Providers

### Prompt tool name references (largest effort)

Prompts throughout the codebase reference Claude Code-specific tool names:

```
"Search the codebase to find the relevant code (use Glob, Grep, Read)"
"use Glob to find files, Grep to search content, Read to examine code"
```

These appear in: triage, ribbit, digest, and investigation prompts in `cmd/root.go`. A new provider with different tool names (e.g., Codex uses `read_file`, `search`) would need either:
- A prompt template system with per-provider tool name mappings
- Generic wording that doesn't reference specific tools (less effective)

### Permission models

Each agent handles permissions differently. The three-level model maps well to Claude Code's flags, but other agents may have coarser or finer-grained control. The provider implementation must map Toad's three levels to whatever the agent supports.

### Multi-repo access

`AdditionalDirs` maps to Claude Code's `--add-dir` flag, giving the agent read access to other repos. Agents without this concept would only work with the primary repo in multi-repo setups.
