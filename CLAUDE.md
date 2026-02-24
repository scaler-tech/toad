# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test

```bash
go build ./...              # Build all packages
go test ./...               # Run all tests
go test ./internal/state/   # Run tests for a single package
go test ./... -run TestFoo  # Run a specific test
go vet ./...                # Lint
```

No external test infrastructure needed — tests use in-memory SQLite (`:memory:`) and same-package access to unexported functions.

## Architecture

Toad is a Go daemon that monitors Slack channels, triages messages with Claude Haiku, and either answers questions (ribbit) or spawns autonomous coding agents (tadpoles) that create PRs.

**Message flow:** Slack event -> triage (Haiku, ~1s) -> route by category:
- `bug`/`feature` -> auto-spawn tadpole (worktree -> Claude -> validate -> PR)
- `question` -> ribbit reply (Claude + read-only tools)
- Passive: high-confidence bugs get a ribbit with :frog: CTA

**Tadpole lifecycle:** `CreateWorktree` -> `RunClaude` (CLI subprocess) -> `Validate` (test+lint+file count) -> retry loop -> `ship` (push + `gh pr create`) -> `RemoveWorktree`

**Multi-repo routing:** Config supports multiple repos via `repos:` list. At startup, `BuildProfiles` auto-detects each repo's stack/module from manifest files. Triage and digest prompts include repo profiles so Haiku can suggest a `"repo"` name. The `Resolver` verifies with file-existence stat checks (`resolver.go`), falling back to triage hint, then `primary` repo.

**Service-aware validation:** When `repos[].services` is configured, `resolveChecks()` in `validate.go` matches changed files to services by path prefix and runs each service's lint/test commands from its subdirectory. Unmatched files fall back to root-level commands. This ensures tadpole PRs pass per-service CI (e.g. PHP services use `make stan && make cs`, Python services use `make lint`).

**Key patterns:**
- **Write-through state**: `state.Manager` caches runs in-memory maps, writes through to SQLite on every mutation. `NewManager()` is in-memory only (tests), `NewPersistentManager(db)` hydrates from DB.
- **Claim/Unclaim**: Atomic thread reservation prevents duplicate tadpoles from TOCTOU races. `Claim` reserves with empty placeholder, `Track` fills in the run ID, `Unclaim` on error removes placeholder only.
- **Concurrency**: Separate semaphores for ribbits (`MaxConcurrent*3`) and tadpoles (`MaxConcurrent`). Each runs in its own goroutine.
- **Channel access**: Bot auto-joins all public channels on startup. If `channels` config is empty, no filtering — events from all joined channels are processed.

**Packages:**
- `cmd/` — Cobra commands: `toad` (daemon), `toad run` (CLI one-shot), `toad init` (setup), `toad status`
- `internal/slack/` — Socket Mode client, event routing, dedup, reply tracking
- `internal/triage/` — Haiku classification (actionable, category, size, keywords, files)
- `internal/ribbit/` — Sonnet with read-only tools, thread memory context
- `internal/tadpole/` — Worktree, Claude runner, validation, shipping, pool
- `internal/state/` — In-memory + SQLite state, crash recovery
- `internal/reviewer/` — Poll GitHub for PR review comments, spawn fix tadpoles
- `internal/digest/` — Toad King: batch messages, Haiku analysis, auto-spawn with guardrails
- `internal/config/` — YAML config loading with cascading defaults, multi-repo profiles and resolver
- `internal/tui/` — Shared huh theme for init wizard

## Important Details

- Claude is invoked as a CLI subprocess (`claude --print --output-format json`), not via API
- Tadpoles use `--dangerously-skip-permissions`, ribbit uses `--allowedTools Read,Glob,Grep`
- SQLite uses `modernc.org/sqlite` (pure Go, no CGo) with WAL mode
- Config loads: defaults -> `~/.toad/config.yaml` -> `.toad.yaml` -> env vars
- All Slack tokens come from env vars (`TOAD_SLACK_APP_TOKEN`, `TOAD_SLACK_BOT_TOKEN`) or `.toad.yaml`
- State DB at `~/.toad/state.db`, worktrees at `~/.toad/worktrees/`
- On startup, `RecoverOnStartup` marks stale runs as failed and cleans orphaned worktrees
