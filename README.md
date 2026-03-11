# 🐸 toad

**The AI teammate that finds bugs before your users do.**

Toad is a self-hosted Go daemon that watches your Slack channels, identifies bugs from conversations and alerts, verifies them against your codebase, and opens fix PRs — all before anyone files a ticket. It's like having a senior engineer who reads every message and quietly fixes things.

## 👑 What makes toad different

Most AI coding tools wait for you to ask. Toad doesn't.

**The Toad King** passively monitors every message in your Slack workspace, batch-analyzes them with Haiku, investigates feasibility against your actual codebase, and autonomously spawns fix agents for high-confidence one-shot bugs. No @mentions, no tickets, no human in the loop until PR review.

On top of that, toad handles the full reactive path too — @mention it with a bug report and get a PR in minutes, ask it a question and get a codebase-grounded answer in seconds.

**Why toad over Copilot, Devin, or Claude Code?**

| | Toad | Copilot Agent | Devin | Claude Code |
|---|---|---|---|---|
| Proactive bug detection | Yes (Toad King) | No | No | No |
| Self-hosted (code stays local) | Yes | No | No | No |
| PR review feedback loop | Yes (3 rounds) | No | No | No |
| CI failure auto-fix | Yes | No | No | No |
| Cost | Your existing Claude sub | Per-seat | Per-seat | Per-seat |
| Slack-native | Yes | No | Coming | Coming |
| MCP integration (Claude Desktop) | Yes | No | No | No |
| Adaptive personality | Yes | No | No | No |

## 🐣 How it works

```
Slack message → Triage (Haiku, ~1s) → Route by category:
  👑 Toad King   → passive batch analysis → investigate → auto-fix PR
  🐣 bug/feature → spawn tadpole → worktree → Claude Code → validate → PR
  🐸 question    → ribbit reply (Sonnet + read-only codebase tools)
```

**Tadpoles** run the full lifecycle autonomously: create a git worktree, invoke Claude Code, validate with your test/lint commands, retry on failure, push and open a PR. After shipping, toad watches for review comments and CI failures, auto-spawning fix tadpoles for up to 3 rounds.

**Ribbits** are for when you just need an answer. Mention toad with a question and it reads your codebase with read-only tools, then replies in-thread. Thread memory means follow-ups stay coherent.

**Passive detection** — Even without @mentions, the Toad King can identify bugs from alerts and conversations. When it finds something fixable, it posts investigation findings with a "Fix this" button that spawns a tadpole on click.

## 🐸 The glossary

Everything in toad is named after the lifecycle of a frog:

| Term | What it means |
|------|---------------|
| 🐸 **Toad** | The daemon — sits in your Slack pond, watching |
| 🥚 **Triage** | Every message classified by Haiku in ~1s |
| 🐸 **Ribbit** | Codebase-aware answer to a question |
| 🐣 **Tadpole** | Autonomous coding agent — worktree, Claude Code, validate, PR |
| 👑 **Toad King** | Passive monitoring → investigation → auto-fix |
| 🔁 **PR Watch** | Review comment and CI failure auto-fixing |
| 🧠 **Personality** | 22-trait adaptive personality with dampened learning |

## 📋 Requirements

- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) (`claude`), authenticated
- [GitHub CLI](https://cli.github.com) (`gh`) or [GitLab CLI](https://gitlab.com/gitlab-org/cli) (`glab`), authenticated
- A Slack app with Socket Mode enabled

## 🚀 Install

### macOS and Linux

```bash
brew tap scaler-tech/pkg https://github.com/scaler-tech/pkg
brew install --cask toad
```

> **macOS security note:** If macOS blocks the app, the cask's post-install hook should handle it. If not: `xattr -d com.apple.quarantine $(which toad)`

### Windows

```bash
scoop bucket add scaler-tech https://github.com/scaler-tech/pkg
scoop install toad
```

### Other options

```bash
# Binary releases
# Download from https://github.com/scaler-tech/toad/releases/latest

# Go install
go install github.com/scaler-tech/toad@latest

# Build from source
git clone https://github.com/scaler-tech/toad.git && cd toad && make build
```

## 🔧 Quick start

```bash
toad init    # Setup wizard — Slack tokens, repo config, Toad King opt-in
toad         # Start the daemon
```

Toad connects to Slack via Socket Mode, auto-joins public channels, and starts listening. Mention `@toad` in any channel with a question or bug report and watch it work.

> For detailed Slack app setup, configuration, and advanced features, see the **[Setup Guide](SETUP.md)**.

**Server deployment:** When running under a process supervisor (systemd, supervisord), set `SUPERVISED=1` so toad exits cleanly on restart instead of self-replacing. See the [Setup Guide](SETUP.md#running-under-a-process-supervisor) for examples.

## 🐸 CLI commands

| Command | Description |
|---------|-------------|
| `toad` | Start the daemon |
| `toad init` | Interactive setup wizard |
| `toad run "task"` | Spawn a tadpole from the CLI (no Slack needed) |
| `toad status` | Open live monitoring dashboard in browser |
| `toad version` | Print version info |
| `toad update` | Self-update to latest version |
| `toad restart` | Gracefully restart the daemon |

## 🏛️ Architecture

```
cmd/
  root.go          Daemon, message routing
  run.go           CLI one-shot mode
  init.go          Setup wizard
  status.go        Web dashboard

internal/
  slack/           Socket Mode client, event routing, dedup
  triage/          Haiku classification
  ribbit/          Q&A with read-only tools
  tadpole/         Worktree, agent runner, validation, shipping
  state/           In-memory + SQLite state, crash recovery
  reviewer/        PR review + CI watcher, fix tadpole spawning
  digest/          Toad King: batch analysis, investigation, auto-spawn
  config/          YAML config with cascading defaults, multi-repo profiles
  personality/     Adaptive 22-trait personality with dampened learning
  agent/           Coding agent provider abstraction (see PROVIDERS.md)
  vcs/             VCS provider abstraction (see PROVIDERS.md)
  issuetracker/    Issue tracker abstraction (see PROVIDERS.md)
  mcp/             MCP server for Claude Desktop/Code integration
```

Three packages use a provider/plugin pattern for extensibility. See the `PROVIDERS.md` in each directory for the interface contracts, current implementations, and how to add new ones.

### Key design decisions

- **Single binary, zero infra** — Go binary, git worktrees, your Claude subscription. No Docker, no cloud.
- **Three-tier intelligence** — Haiku for triage (~$0.001), Sonnet for investigation (read-only), Sonnet for execution (full tools).
- **6-layer guardrails on proactive spawning** — disabled by default, 0.95 confidence threshold, category + size restrictions, hourly cap, existing validation + human PR review.
- **Write-through state** — in-memory cache + SQLite for crash recovery and dashboard.
- **MCP server** — optional Streamable HTTP endpoint lets Claude Desktop and Claude Code query toad (ask questions, read logs) via authenticated tokens managed through Slack.

## 🔌 MCP Server

Toad includes an optional MCP (Model Context Protocol) server that lets Claude Desktop and Claude Code interact with your toad instance directly — ask codebase questions, read daemon logs, and check health.

**Setup:**
1. Enable in config: `mcp.enabled: true` and set `mcp.port: 8099`
2. Add a `/toad` slash command to your Slack app (see [Setup Guide](SETUP.md))
3. Run `/toad mcp connect` in Slack to get a personal token
4. Add toad as an MCP server in Claude Desktop or Claude Code

**Available tools:**
- `ask` — Ask toad a codebase question (uses ribbit engine with read-only tools)
- `logs` — Read and filter daemon logs (dev role required)
- `watches` — List open PR watches being monitored (dev role required)

> For full setup instructions, see the **[Setup Guide](SETUP.md#mcp-server)**.

## 🧠 Personality System

Toad develops a personality over time based on team feedback. 22 traits (verbosity, formality, humor, caution, etc.) adjust through dampened learning from emoji reactions, text feedback in threads, and PR outcomes.

**Setup:**
1. Enable in config: `personality.enabled: true`
2. Optionally disable learning: `personality.learning_enabled: false` (uses base traits only)

Personality data is stored at `~/.toad/personality.yaml` and visualized as a radar chart in the dashboard and kiosk view.

> For configuration details, see the **[Setup Guide](SETUP.md#personality)**.

## 🛠️ Development

```bash
make build    # Build binary
make test     # Run tests with race detector
make lint     # Run golangci-lint
make vet      # Run go vet
make fmt      # Format code
```

## 📄 License

[Elastic License 2.0 (ELv2)](LICENSE) — free to use, modify, and distribute. You may not offer toad as a hosted/managed service.

---

*Built with [Claude Code](https://docs.anthropic.com/en/docs/claude-code). Toad eats bugs. 🐸*
