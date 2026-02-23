# Toad - Service Specification

## Context

Toad is a Go CLI daemon that monitors Slack channels, triages messages for code-related issues, responds with codebase ribbits, and spawns autonomous "tadpole" agents that one-shot fix issues and open PRs. Inspired by Stripe's Minions system (1300+ PRs/week) but designed to run locally on a single machine with zero infrastructure overhead. Uses git worktrees for isolation instead of EC2 instances.

## Decisions

- **Language**: Go
- **Claude integration**: CLI subprocess (`claude --print`)
- **Slack**: Direct API via `slack-go` + Socket Mode (WebSocket, no webhook server)
- **Channel access**: Auto-join all public channels on startup; private channels need `/invite`
- **Monitoring**: Passive (all messages) + active (@toad / reaction triggers)
- **Routing**: Triage classifies every triggered message; bugs/features auto-spawn tadpoles, questions go to ribbit
- **Spawned agents**: Called "tadpoles"
- **Repo scope**: Single repo (runs from within a repo directory)
- **Permissions**: `--dangerously-skip-permissions` for tadpoles (isolation via worktrees), `--allowedTools Read,Glob,Grep` for ribbit (read-only)
- **State persistence**: SQLite via `modernc.org/sqlite` (pure Go, no CGo) at `~/.toad/state.db`
- **PR monitoring**: Polling via `gh` CLI (no webhook server)

---

## Architecture Overview

```
+---------------------------------------------------------------------------+
|                          TOAD DAEMON                                      |
|                                                                           |
|  +--------------+    +--------------+    +------------------------+      |
|  | Slack Client  |--->| Event Router |--->| Triage Engine          |      |
|  | (Socket Mode) |    |              |    | (Haiku classify ~1s)   |      |
|  | auto-join all |    +------+-------+    +--------+---------------+      |
|  +--------------+           |                      |                      |
|                    +--------+--------+    +--------+--------+            |
|                    | Digest Engine   |    |                 |            |
|                    | (Toad King)     |  bug/feature     question         |
|                    | batch -> analyze|    |                 |            |
|                    | -> auto-spawn   |    v                 v            |
|                    +--------+--------+  Tadpole Pool    Ribbit          |
|                             |           (auto-spawn)   Responder        |
|                             |                                            |
|  +------------------+  +------------------+  +------------------+       |
|  | State Manager    |  | PR Reviewer      |  | SQLite DB        |       |
|  | in-memory cache  |  | poll gh api      |  | runs             |       |
|  | write-through    |  | detect reviews   |  | thread_memory    |       |
|  | to SQLite        |  | spawn fix        |  | pr_watches       |       |
|  +------------------+  +------------------+  +------------------+       |
+---------------------------------------------------------------------------+
```

---

## Project Structure

```
toad/
+-- main.go                    # Entry point
+-- go.mod
+-- go.sum
|
+-- cmd/
|   +-- root.go                # Cobra root: `toad` daemon, `toad status`
|   +-- run.go                 # `toad run "task"` manual one-shot
|   +-- init.go                # `toad init` interactive setup wizard
|
+-- internal/
|   +-- config/
|   |   +-- config.go          # YAML config loading, defaults, validation
|   |   +-- config_test.go
|   |
|   +-- slack/
|   |   +-- client.go          # Socket Mode, auto-join, bot identity via AuthTest
|   |   +-- events.go          # Event routing, self-message filtering, dedup
|   |   +-- responder.go       # Thread replies, reactions, status updates
|   |
|   +-- triage/
|   |   +-- triage.go          # Classify messages: actionable? size? confidence?
|   |   +-- triage_test.go
|   |
|   +-- ribbit/
|   |   +-- ribbit.go          # Claude + read-only tools, thread memory context
|   |
|   +-- tadpole/
|   |   +-- pool.go            # Concurrency pool with semaphore
|   |   +-- runner.go          # Single tadpole lifecycle: worktree -> claude -> validate -> PR
|   |   +-- runner_test.go
|   |   +-- worktree.go        # Git worktree create/checkout/remove
|   |   +-- worktree_test.go
|   |   +-- claude.go          # Spawn claude CLI, parse JSON envelope
|   |   +-- validate.go        # Run tests/lint, check file count
|   |   +-- validate_test.go
|   |
|   +-- state/
|   |   +-- state.go           # In-memory cache + write-through to DB
|   |   +-- state_test.go
|   |   +-- db.go              # SQLite persistence (runs, thread_memory, pr_watches)
|   |   +-- db_test.go
|   |   +-- recovery.go        # Startup crash recovery: stale runs, orphaned worktrees
|   |
|   +-- reviewer/
|   |   +-- reviewer.go        # Poll GitHub for PR review comments, spawn fix tadpoles
|   |
|   +-- digest/
|   |   +-- digest.go          # Toad King: batch messages, Haiku analysis, auto-spawn
|   |
|   +-- tui/
|   |   +-- theme.go           # Shared toad huh theme + color constants
|   |
|   +-- log/
|       +-- log.go             # Structured logging (slog)
|
+-- docs/
    +-- spec.md                # This document
    +-- plan.md                # Implementation plan + progress tracking
```

---

## Module Design

### `cmd/root.go` - CLI Entry Points

```go
// Commands:
// `toad`              - Start daemon (connect to Slack, listen)
// `toad run "task"`   - Manual one-shot tadpole (no Slack)
// `toad status`       - Show active tadpole runs
// `toad init`         - Interactive setup wizard
```

### `cmd/init.go` - Setup Wizard

Two-step interactive setup:
1. Setup instructions with OAuth scopes and events to configure
2. Token input (app-level token + bot token)

No channel selection — toad auto-joins all public channels on startup.
Writes `.toad.yaml` with just the two tokens.

### `internal/config/config.go` - Configuration

```go
type Config struct {
    Slack    SlackConfig    `yaml:"slack"`
    Repo     RepoConfig     `yaml:"repo"`
    Limits   LimitsConfig   `yaml:"limits"`
    Triage   TriageConfig   `yaml:"triage"`
    Claude   ClaudeConfig   `yaml:"claude"`
    Digest   DigestConfig   `yaml:"digest"`
    Log      LogConfig      `yaml:"log"`
}

type DigestConfig struct {
    Enabled           bool     `yaml:"enabled"`             // default: false
    BatchMinutes      int      `yaml:"batch_minutes"`       // default: 5
    MinConfidence     float64  `yaml:"min_confidence"`      // default: 0.95
    MaxAutoSpawnHour  int      `yaml:"max_auto_spawn_hour"` // default: 3
    AllowedCategories []string `yaml:"allowed_categories"`  // default: ["bug"]
    MaxEstSize        string   `yaml:"max_est_size"`        // default: "small"
}
```

Config loading order:
1. Defaults
2. `~/.toad/config.yaml` (global)
3. `.toad.yaml` in repo root (project override)
4. Environment variables (`TOAD_SLACK_APP_TOKEN`, `TOAD_SLACK_BOT_TOKEN`)

Validation requires app_token, bot_token, and repo path. Channels are optional — if empty, all joined channels are monitored.

### `internal/slack/client.go` - Slack Connection

- Auto-joins all public channels on startup via `conversations.join`
- Stores `botUserID` (populated via `AuthTest`) to filter self-messages
- Socket Mode WebSocket connection with reconnection
- Event routing: AppMention, Message, ReactionAdded
- `inChannel()` check — if no explicit channels configured, accepts all events
- Message dedup via `markSeen()` with 5-minute TTL
- Reply tracking for 24h to identify toad's own messages

**Event routing flow:**

```
Socket Mode Event
    |
    +-- EventTypeEventsAPI
    |   +-- AppMention -> reject self -> check channel -> dispatch (IsMention=true)
    |   +-- Message    -> reject self -> check channel -> reject bot/subtype -> dispatch
    |   +-- ReactionAdded -> reject self -> check emoji -> check channel -> dispatch
    |       +-- :frog: on toad reply -> IsTadpoleRequest=true
    |       +-- configured emoji on user msg -> IsTriggered=true
    |
    +-- (other event types ignored)
```

### `internal/triage/triage.go` - Message Classification

Classifies Slack messages using Claude Haiku (`--max-turns 1`). Returns actionability, confidence, category, size estimate, keywords, and file hints.

Categories: `bug`, `feature`, `question`, `refactor`, `other`
Sizes: `tiny`, `small`, `medium`, `large`

Used for all triggered messages — routes the response:
- `bug`/`feature` -> auto-spawn tadpole
- `question`/other -> ribbit reply

Cost: ~$0.001 per triage, ~1-2 seconds.

### `internal/ribbit/ribbit.go` - Codebase-Aware Responses

1. Claude Sonnet with `--allowedTools Read,Glob,Grep` (read-only, up to 10 turns)
2. Conditional triage hints (summary, category, keywords, file hints) when available
3. Thread memory: `PriorContext` with prior summary + response prepended for follow-up conversations
4. Prompt focuses on answering the actual question and pointing to specific files

### `internal/tadpole/` - Autonomous Fix Agents

**Lifecycle:** worktree -> claude -> validate -> retry loop -> ship (push + PR) -> cleanup

- **worktree.go**: `CreateWorktree` (new branch from local ref, fetch inside worktree), `CheckoutWorktree` (existing branch for review fixes), `RemoveWorktree` (force remove + prune)
- **claude.go**: `RunClaude` with `--dangerously-skip-permissions --output-format json`, per-call timeout
- **validate.go**: `Validate` with service-aware test/lint commands, file count check, 5-min timeout. `resolveChecks` matches changed files to services by path prefix, runs per-service commands from each service directory. Falls back to root-level commands for unmatched files.
- **runner.go**: Full lifecycle orchestrator with Slack status updates (single editable message), reaction lifecycle (:hatching_chick: -> :white_check_mark:/:x:), `OnShip` callback for PR tracking
- **pool.go**: Semaphore-gated concurrency, WaitGroup, panic recovery, 30s graceful shutdown

**Task types:**
- Regular: new worktree + new branch + create PR
- Review fix: checkout existing branch + push follow-up commits (no new PR)

### `internal/state/` - State Management

**state.go**: In-memory cache with RWMutex. Maps: `runs` (runID -> Run), `threads` (threadTS -> runID). Methods: `Track`, `Update`, `Complete`, `GetByThread`, `Claim`/`Unclaim`, `Active`, `History`. `DB()` accessor exposes the underlying database.

Run statuses: `starting` -> `running` -> `validating` -> `shipping` -> `done`/`failed`

**db.go**: SQLite persistence. Write-through from Manager — every mutation writes to both in-memory cache and DB. `NewManager()` for in-memory only (tests, CLI), `NewPersistentManager(db)` hydrates from DB.

Tables:
- `runs` — tadpole execution history
- `thread_memory` — cached triage summary + ribbit response per thread (24h TTL)
- `pr_watches` — toad PRs being monitored for review comments

**recovery.go**: On startup, find stale active runs, mark failed, clean orphaned worktrees from `~/.toad/worktrees/`.

### `internal/reviewer/reviewer.go` - PR Review Feedback

Polls `gh api` every 2 minutes for new review comments on toad-created PRs. Aggregates human comments (skips bots) into a fix task, spawns tadpole on the same branch for follow-up commits. Max 3 fix rounds per PR. Marks watch as closed when PR is merged/closed.

Wired via `OnShip` callback — after a tadpole successfully creates a PR, it's automatically registered for review watching.

### `internal/digest/digest.go` - Channel Digest / Toad King

Collects non-bot messages into an in-memory buffer. Every N minutes, flushes buffer and sends batch to Haiku for analysis in a single call. If Haiku identifies a clear one-shot bug fix with very high confidence, auto-spawns a tadpole with `:crown: Toad King detected...` notification.

**Guardrails (6 layers):**
1. Disabled by default (`digest.enabled: false`)
2. 0.95 confidence threshold
3. Category restriction (bug only by default)
4. Size restriction (tiny/small only)
5. Hourly spawn cap (3/hour default)
6. Existing tadpole guardrails (max_files, test/lint, human PR review)

---

## Message Handling Flow

```
Message arrives
    |
    +-- IsTadpoleRequest? -> fetch thread -> triage -> spawn tadpole
    |
    +-- IsBot? -> skip
    |
    +-- Feed to digest engine (if enabled)
    |
    +-- IsMention or IsTriggered?
    |   +-- Triage (Haiku ~1s)
    |   +-- bug/feature? -> auto-spawn tadpole (Claim thread first)
    |   +-- question?    -> lookup thread memory -> ribbit -> save thread memory
    |
    +-- Passive monitoring
        +-- Triage -> high confidence bug? -> ribbit reply with :frog: CTA
```

---

## Config File (`.toad.yaml`)

```yaml
slack:
  app_token: "xapp-1-..."
  bot_token: "xoxb-..."
  # channels is optional — if omitted, toad monitors all joined channels
  # channels:
  #   - "C0123456789"
  triggers:
    emoji: "frog"
    keywords:
      - "toad fix"
      - "toad help"

repo:
  default_branch: "main"
  # Root-level commands (fallback for files not matching any service)
  # test_command: "go test ./..."
  # lint_command: "golangci-lint run"
  #
  # Per-service lint/test — tadpole detects which services were changed
  # and runs only the relevant commands from each service directory
  services:
    - path: "web-app"
      test_command: "make test"
      lint_command: "make stan && make cs"
    - path: "esg-api"
      test_command: "make tests"
      lint_command: "make lint"
    - path: "audit-service"
      lint_command: "make stan && make cs"
    - path: "excel-service"
      test_command: "make test"
      lint_command: "make stan && make cs"
    - path: "estimations-service"
      test_command: "make test"
      lint_command: "make lint"

limits:
  max_concurrent: 2
  max_turns: 30
  timeout_minutes: 10
  max_files_changed: 5
  max_budget_usd: 1.00
  max_retries: 1

triage:
  model: "haiku"

claude:
  model: "sonnet"

digest:
  enabled: false
  batch_minutes: 5
  min_confidence: 0.95
  max_auto_spawn_hour: 3
  allowed_categories:
    - "bug"
  max_est_size: "small"

log:
  level: "info"
  file: "~/.toad/toad.log"
```

---

## Slack App Setup

### Required OAuth Scopes (Bot Token)

| Scope | Purpose |
|-------|---------|
| `app_mentions:read` | Detect @toad mentions |
| `channels:history` | Read messages in public channels |
| `channels:join` | Auto-join public channels on startup |
| `channels:read` | List and get channel info |
| `chat:write` | Post messages and thread replies |
| `groups:history` | Read messages in private channels |
| `groups:read` | List private channels |
| `reactions:read` | Detect reactions |
| `reactions:write` | Add reactions (acknowledgment, done) |
| `users:read` | Resolve user names |

### Event Subscriptions (Bot Events)

| Event | Purpose |
|-------|---------|
| `app_mention` | Triggers on @toad |
| `message.channels` | All messages in public channels bot is in |
| `message.groups` | All messages in private channels bot is in |
| `reaction_added` | Triggers on emoji reaction |

### App-Level Token
- Scope: `connections:write` (for Socket Mode)

### Setup steps:
1. Create Slack App at api.slack.com/apps
2. Enable Socket Mode
3. Add Bot Token Scopes listed above
4. Subscribe to Events listed above
5. Generate App-Level Token with `connections:write`
6. Install to workspace
7. Run `toad init` to configure tokens

Toad auto-joins all public channels on startup. For private channels, use `/invite @YourBot`.

---

## Startup Sequence

1. Load config (defaults -> global -> project -> env vars)
2. Set up structured logging
3. Validate config (auto-init wizard if missing and in terminal)
4. Check `claude` and `gh` CLI tools are in PATH
5. Open SQLite database (`~/.toad/state.db`)
6. Run crash recovery (mark stale runs failed, clean orphaned worktrees)
7. Hydrate state manager from DB
8. Initialize triage, ribbit, tadpole pool
9. Initialize PR review watcher + wire `OnShip` callback
10. Initialize digest engine (if `digest.enabled`)
11. Connect to Slack (AuthTest + auto-join public channels)
12. Start background goroutines: PR watcher, digest engine, thread memory pruning
13. Enter Socket Mode event loop
