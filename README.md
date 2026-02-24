# 🐸 toad

An AI-powered coding agent that lives in your Slack workspace. Drop a bug report or feature request in any channel, and toad spawns an autonomous agent that writes the code, runs your tests, and opens a PR — all in minutes.

## 🐸 The pond — what is toad?

Toad turns Slack into an engineering intake queue. Instead of bugs and feature requests piling up, toad listens, understands, and acts. It's built on [Claude Code](https://docs.anthropic.com/en/docs/claude-code) and designed for teams that want AI handling the small stuff so humans can focus on the big stuff.

Everything in toad is named after the lifecycle of a frog. Here's the glossary:

| Term | What it means |
|------|---------------|
| 🐸 **Toad** | The daemon itself — sits in your Slack pond, watching for messages |
| 🥚 **Triage** | Every message gets classified by Haiku in ~1 second: is it a bug? feature? question? how big? |
| 🐸 **Ribbit** | A codebase-aware reply to a question — Sonnet reads your code with read-only tools and answers in-thread |
| 🐣 **Tadpole** | An autonomous coding agent — creates a git worktree, invokes Claude Code, validates with your tests, and opens a PR |
| 👑 **Toad King** | The digest engine — passively watches all messages, batch-analyzes them, and auto-spawns tadpoles for obvious one-shot fixes |
| 🔁 **PR Watch** | After a tadpole ships a PR, toad watches for review comments and auto-spawns fix tadpoles (up to 3 rounds) |

## 🐣 How it works

```
Slack message → Triage (Haiku, ~1s) → Route by category:
  🐣 bug/feature  → spawn tadpole → worktree → Claude Code → validate → PR
  🐸 question     → ribbit reply (Sonnet + read-only codebase tools)
  👀 passive bug  → ribbit with 🐸 CTA to spawn tadpole on demand
```

**Tadpoles** run the full lifecycle autonomously: create a git worktree, invoke Claude Code to make changes, validate with your test/lint commands, retry on failure, then push and open a PR. The PR is the review gate — toad ships fast and lets humans approve.

**Ribbits** are for when you just need an answer. Mention toad with a question and it reads your codebase using Sonnet with read-only tools (Glob, Grep, Read), then replies in-thread with context-aware answers. Thread memory means follow-ups stay coherent.

**The Toad King** (optional) goes a step further — it passively collects every non-bot message in your channels, batch-analyzes them with Haiku on an interval, and auto-spawns tadpoles for high-confidence one-shot fixes. Think of it as a vigilant frog that catches bugs before anyone files them.

## 📋 Requirements

- Go 1.25+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) (`claude`)
- [GitHub CLI](https://cli.github.com) (`gh`), authenticated
- A Slack app with Socket Mode enabled

## 🚀 Install

### macOS and Linux

Install with [Homebrew](https://brew.sh/) (recommended):

```bash
brew tap cdre-ai/tap https://github.com/cdre-ai/homebrew-tap
brew install --cask toad
```

> **macOS security note:** If macOS blocks the app with "cannot be opened because the developer cannot be verified", the cask's post-install hook should handle this automatically. If not:
> ```bash
> xattr -d com.apple.quarantine $(which toad)
> ```

### Windows

Install with [Scoop](https://scoop.sh/):

```bash
scoop bucket add cdre-ai https://github.com/cdre-ai/homebrew-tap
scoop install toad
```

### Binary releases

Download pre-built binaries for Windows, macOS, or Linux from the [latest release](https://github.com/cdre-ai/toad/releases/latest).

### Go install

```bash
go install github.com/cdre-ai/toad@latest
```

### Build from source

```bash
git clone https://github.com/cdre-ai/toad.git
cd toad
make build
```

## 🔧 Setup

Run the interactive setup wizard:

```bash
toad init
```

This walks you through creating a Slack app and saves credentials to `.toad.yaml`. You'll need:

1. **Create a Slack app** at [api.slack.com/apps](https://api.slack.com/apps)
2. **Enable Socket Mode** and generate an app-level token (`xapp-...`) with `connections:write` scope
3. **Add bot token scopes**: `app_mentions:read`, `channels:history`, `channels:join`, `channels:read`, `chat:write`, `groups:history`, `groups:read`, `reactions:read`, `reactions:write`, `users:read`
4. **Subscribe to events**: `app_mention`, `message.channels`, `message.groups`, `reaction_added`
5. **Install to workspace** and copy the bot token (`xoxb-...`)

## 🐸 Usage

### Start the daemon

```bash
toad
```

Toad connects to Slack via Socket Mode, auto-joins public channels, and starts listening. Mention `@toad` or use trigger keywords to interact.

### 🐣 CLI one-shot mode

Spawn a tadpole directly from the command line without Slack:

```bash
toad run "Fix the login bug in auth.go"
toad run --repo frontend "Fix the login bug"  # multi-repo: specify which repo
```

### 📊 Monitoring dashboard

```bash
toad status
```

Opens a live web dashboard in your browser showing daemon status, active runs, history, costs, triage breakdown, digest stats, PR watches, and config. Refreshes every 2 seconds.

Use `--port 8080` to pin to a specific port.

## ⚙️ Configuration

Config is loaded in order (later overrides earlier):

1. Built-in defaults
2. `~/.toad/config.yaml` (global)
3. `.toad.yaml` (project-local)
4. Environment variables

### Example `.toad.yaml`

```yaml
slack:
  app_token: ${TOAD_SLACK_APP_TOKEN}  # or set env var directly
  bot_token: ${TOAD_SLACK_BOT_TOKEN}
  channels: []  # empty = all public channels
  triggers:
    emoji: frog
    keywords: ["toad fix", "toad help"]

repos:
  - name: my-app
    path: /path/to/your/repo       # absolute or relative (resolved to absolute)
    default_branch: main
    test_command: go test ./...
    lint_command: go vet ./...
    services:  # optional: per-service validation
      - path: web-app
        test_command: make test
        lint_command: make stan && make cs
  # - name: another-repo           # add more repos for multi-repo setups
  #   path: /path/to/another/repo
  #   primary: true                 # designate one as primary fallback

limits:
  max_concurrent: 2       # concurrent tadpoles
  max_turns: 30           # Claude conversation turns per run
  timeout_minutes: 10
  max_files_changed: 5    # fail if more files changed
  max_budget_usd: 1.00    # per-run cost cap
  max_retries: 1

triage:
  model: haiku

claude:
  model: sonnet
  append_system_prompt: ""  # extra instructions for Claude

digest:
  enabled: false          # opt-in batch analysis (Toad King)
  batch_minutes: 5
  min_confidence: 0.95
  max_auto_spawn_hour: 3
  allowed_categories: [bug]
  max_est_size: small

issue_tracker:
  enabled: false          # enable issue tracker integration
  provider: linear        # only "linear" for now
  # api_token: ""         # or TOAD_LINEAR_API_TOKEN env var
  # team_id: ""           # Linear team ID for issue creation
  create_issues: false    # create issues for opportunities without one

log:
  level: info
  file: ~/.toad/toad.log
```

### Environment variables

Tokens can be set via environment instead of config:

```bash
export TOAD_SLACK_APP_TOKEN=xapp-...
export TOAD_SLACK_BOT_TOKEN=xoxb-...
export TOAD_LINEAR_API_TOKEN=lin_api_...  # optional, for Linear integration
```

## 💬 Interacting with toad

| Action | How |
|--------|-----|
| 🐸 Ask a question | `@toad how does the auth middleware work?` |
| 🐸 Trigger on keyword | `toad fix the broken date parser` |
| 🐣 Request a tadpole | React with 🐸 on any toad reply |
| 🐣 Bug/feature (auto) | `@toad` a bug report or feature request — auto-spawns a tadpole |

### 🥚 → 🐣 → 🐸 Message flow

1. **🥚 Triage** — Haiku classifies the message (~1 second): actionable? category? size?
2. **Route** — Bugs and features spawn tadpoles. Questions get ribbit replies.
3. **🐣 Tadpole** — Creates worktree, runs Claude Code, validates, retries, ships PR.
4. **🐸 Ribbit** — Sonnet answers with codebase-aware context using read-only tools.
5. **🔁 PR Watch** — After shipping, toad monitors for review comments and auto-fixes.

## 👑 Digest (Toad King)

When enabled, the digest engine passively collects all non-bot messages and periodically batch-analyzes them with Haiku to detect one-shot opportunities (clear, specific bugs or tiny features). High-confidence matches are auto-spawned as tadpoles with guardrails:

- Confidence must be >= 0.95
- Only allowed categories (default: bugs only)
- Only tiny/small estimated size
- Rate-limited to N spawns per hour

Enable in config:

```yaml
digest:
  enabled: true
```

## 🏗️ Service-aware validation

For monorepos with multiple services, configure per-service test/lint commands:

```yaml
repos:
  - name: my-monorepo
    path: /path/to/repo
    services:
      - path: web-app
        test_command: make test
        lint_command: make stan && make cs
      - path: api
        test_command: pytest
        lint_command: ruff check .
```

When a tadpole changes files, toad matches them to services by path prefix and runs each service's commands from its subdirectory. Unmatched files fall back to root-level commands.

## 🏛️ Architecture

```
cmd/
  root.go          Cobra command: toad (daemon), message routing
  run.go           toad run (CLI one-shot)
  init.go          toad init (setup wizard)
  status.go        toad status (web dashboard)
  version.go       toad version (build info via ldflags)
  update.go        toad update (self-update via Homebrew)

internal/
  slack/           Socket Mode client, event routing, dedup
  triage/          Haiku classification (category, size, keywords, files)
  ribbit/          Sonnet responses with read-only codebase tools
  tadpole/         Worktree, Claude runner, validation, shipping, pool
  state/           In-memory + SQLite state, crash recovery
  reviewer/        PR review comment watcher, fix tadpole spawning
  digest/          Toad King: batch analysis, auto-spawn with guardrails
  issuetracker/    Generic issue tracker interface (Linear integration)
  update/          Version checking and self-update
  config/          YAML config with cascading defaults
  tui/             Shared theme for init wizard
  log/             Structured logging setup
```

### 💾 State & recovery

State is persisted to SQLite (`~/.toad/state.db`) with WAL mode. On startup, toad marks stale runs as failed and cleans orphaned worktrees. The dashboard reads directly from SQLite, so it works even when the daemon is stopped.

### 🏊 Concurrency

Separate semaphores keep Q&A responsive while tadpoles run:
- **Ribbit pool**: `max_concurrent * 3` (fast, seconds)
- **Tadpole pool**: `max_concurrent` (slow, minutes)

## 🛠️ Development

```bash
make build                  # Build binary
make test                   # Run tests with race detector
make lint                   # Run golangci-lint
make vet                    # Run go vet
make fmt                    # Format code
make clean                  # Remove binary and dist/
```

To test a single package:

```bash
go test ./internal/state/
```

## 📄 License

MIT

---

*Built with [Claude Code](https://docs.anthropic.com/en/docs/claude-code). Toad eats bugs. 🐸*
