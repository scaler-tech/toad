# Toad Setup Guide

The complete guide to installing, configuring, and running toad — from zero to fully operational.

**Table of Contents**

- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [Slack App Setup](#slack-app-setup)
- [Quick Start](#quick-start)
- [Configuration Reference](#configuration-reference)
  - [Config Loading Order](#config-loading-order)
  - [slack](#slack)
  - [repos](#repos)
  - [repos\[\].services](#reposservices)
  - [repos\[\].vcs](#reposvcs)
  - [limits](#limits)
  - [triage](#triage)
  - [claude](#claude)
  - [agent](#agent)
  - [digest](#digest)
  - [issue_tracker](#issue_tracker)
  - [vcs](#vcs)
  - [log](#log)
  - [mcp](#mcp)
  - [personality](#personality)
- [Environment Variables](#environment-variables)
- [CLI Commands](#cli-commands)
- [Interacting with Toad](#interacting-with-toad)
- [Advanced Topics](#advanced-topics)
  - [Multi-Repo Routing](#multi-repo-routing)
  - [PR Review Automation](#pr-review-automation)
  - [Toad King (Digest) Tuning](#toad-king-digest-tuning)
  - [Mixed GitHub/GitLab Setups](#mixed-githubgitlab-setups)
  - [MCP Server](#mcp-server)
- [Troubleshooting](#troubleshooting)
- [Complete Example Config](#complete-example-config)

---

## Prerequisites

Before installing toad, make sure you have:

| Requirement | Why | Install |
|-------------|-----|---------|
| **Go 1.25+** | Toad is written in Go | [go.dev/dl](https://go.dev/dl/) |
| **Claude Code CLI** | Toad invokes Claude as a subprocess | [docs.anthropic.com](https://docs.anthropic.com/en/docs/claude-code) |
| **GitHub CLI** (`gh`) | For creating PRs and interacting with GitHub | [cli.github.com](https://cli.github.com) |
| **GitLab CLI** (`glab`) | Only if using GitLab instead of GitHub | [gitlab.com/gitlab-org/cli](https://gitlab.com/gitlab-org/cli) |
| **Slack workspace** | You need admin access to create a Slack app | [slack.com](https://slack.com) |

Make sure the Claude Code CLI is authenticated (`claude` should work from your terminal), and your VCS CLI is authenticated (`gh auth status` or `glab auth status`).

---

## Installation

### macOS and Linux (Homebrew — recommended)

```bash
brew tap scaler-tech/pkg https://github.com/scaler-tech/pkg
brew install --cask toad
```

> **macOS security note:** If macOS blocks the app with "cannot be opened because the developer cannot be verified", the cask's post-install hook should handle this automatically. If not:
> ```bash
> xattr -d com.apple.quarantine $(which toad)
> ```

### Windows (Scoop)

```bash
scoop bucket add scaler-tech https://github.com/scaler-tech/pkg
scoop install toad
```

### Binary releases

Download pre-built binaries for Windows, macOS, or Linux from the [latest release](https://github.com/scaler-tech/toad/releases/latest).

### Go install

```bash
go install github.com/scaler-tech/toad@latest
```

### Build from source

```bash
git clone https://github.com/scaler-tech/toad.git
cd toad
make build
```

---

## Slack App Setup

Toad connects to Slack via Socket Mode, which means it runs as a daemon on your machine — no public URL or server required.

### Step 1: Create the Slack app

1. Go to [api.slack.com/apps](https://api.slack.com/apps)
2. Click **Create New App** → **From scratch**
3. Name it `toad` (or whatever you like) and select your workspace

### Step 2: Enable Socket Mode

1. In the left sidebar, go to **Socket Mode**
2. Toggle **Enable Socket Mode** on
3. When prompted, create an app-level token:
   - Name: `toad-socket` (or any name)
   - Scope: `connections:write`
4. Copy the token — it starts with `xapp-`. This is your **app token**.

### Step 3: Add bot token scopes

1. Go to **OAuth & Permissions** in the left sidebar
2. Under **Scopes → Bot Token Scopes**, add:
   - `app_mentions:read` — detect when users mention toad
   - `channels:history` — read messages in public channels
   - `channels:join` — auto-join public channels
   - `channels:read` — list channels
   - `chat:write` — send replies
   - `groups:history` — read messages in private channels
   - `groups:read` — list private channels
   - `reactions:read` — detect emoji triggers
   - `reactions:write` — add reaction feedback
   - `users:read` — resolve user names

### Step 4: Subscribe to events

1. Go to **Event Subscriptions** in the left sidebar
2. Toggle **Enable Events** on
3. Under **Subscribe to bot events**, add:
   - `app_mention` — when someone @mentions your bot
   - `message.channels` — messages in public channels
   - `message.groups` — messages in private channels
   - `reaction_added` — emoji reactions on messages

### Step 4b: Add slash command (optional, for MCP server)

If you plan to use the MCP server (Claude Desktop/Code integration):

1. Go to **Slash Commands** in the left sidebar
2. Click **Create New Command**
3. Set:
   - **Command:** `/toad`
   - **Short Description:** `Toad daemon commands`
   - **Usage Hint:** `mcp connect | mcp revoke | mcp status | mcp ping | status | help`
4. Click **Save**

Users can then run `/toad mcp connect` to get an MCP token, `/toad mcp status` to check it, `/toad mcp revoke` to invalidate it, and `/toad status` for daemon info.

### Step 4c: Enable Interactivity (optional, for button CTAs)

If you want toad's passive investigation messages to include a clickable "Fix this" button:

1. Go to **Interactivity & Shortcuts** in the left sidebar
2. Toggle **Interactivity** on
3. For the **Request URL**, leave it empty — Socket Mode handles routing automatically

No additional scopes are needed. When interactivity is enabled, passive ribbit replies show a green "Fix this" button instead of a text prompt. Clicking the button spawns a tadpole immediately.

### Step 5: Install to workspace

1. Go to **Install App** in the left sidebar
2. Click **Install to Workspace** and authorize
3. Copy the **Bot User OAuth Token** — it starts with `xoxb-`. This is your **bot token**.

You now have the two tokens toad needs:
- **App token** (`xapp-...`) — for Socket Mode connection
- **Bot token** (`xoxb-...`) — for reading messages and posting replies

---

## Quick Start

The fastest path to a running toad:

### 1. Run the setup wizard

```bash
toad init
```

This walks you through configuration interactively — Slack tokens, repo path, test/lint commands, and optional features. It saves everything to `.toad.yaml` in your current directory.

### 2. Start the daemon

```bash
toad
```

Toad connects to Slack, auto-joins public channels, and starts listening.

### 3. Verify it works

In any Slack channel toad has joined, type:

```
@toad how does this codebase work?
```

You should see toad reply in-thread with a codebase-aware answer. If that works, try a bug report:

```
@toad there's a typo in the README — "recieve" should be "receive"
```

Toad will triage the message, spawn a tadpole, and open a PR.

---

## Configuration Reference

### Config Loading Order

Config is loaded in this order (later overrides earlier):

1. **Built-in defaults** — sensible out-of-the-box values
2. **`~/.toad/config.yaml`** — global config (applies to all projects)
3. **`.toad.yaml`** — project-local config (in your repo root)
4. **Environment variables** — highest priority, override everything

For tokens, you can use environment variable references in YAML:

```yaml
slack:
  app_token: ${TOAD_SLACK_APP_TOKEN}
  bot_token: ${TOAD_SLACK_BOT_TOKEN}
```

---

### `slack`

Slack connection and trigger settings.

```yaml
slack:
  app_token: xapp-...       # Socket Mode app-level token
  bot_token: xoxb-...       # Bot user OAuth token
  channels: []               # Channels to monitor (empty = all public)
  triggers:
    emoji: frog              # Emoji name to trigger tadpole spawning
    keywords:                # Keywords that trigger toad
      - "toad fix"
      - "toad help"
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `app_token` | string | *(required)* | Slack app-level token for Socket Mode (`xapp-...`) |
| `bot_token` | string | *(required)* | Slack bot OAuth token (`xoxb-...`) |
| `channels` | list | `[]` | Channels to monitor. Empty = join and monitor all public channels. When set, toad only joins listed channels. |
| `triggers.emoji` | string | `"frog"` | React with this emoji on a toad reply to spawn a tadpole |
| `triggers.keywords` | list | `["toad fix", "toad help"]` | Messages starting with these keywords trigger toad |

**Tip:** If you set `channels` to an empty list, toad auto-joins and monitors all public channels. To restrict toad to specific channels, list them by name (without `#`) — toad will only join those channels. Private channels work too: invite toad to the private channel and add its name to the list. Note that channel names are resolved to IDs at startup, so if you add a new channel to the config or invite toad to a new private channel, restart toad to pick it up.

---

### `repos`

Repository configuration. At least one repo is required. Repo entries go under the `list:` key.

```yaml
repos:
  sync_minutes: 0              # Periodic git fetch interval (0 = disabled)
  list:
    - name: my-app
      path: /path/to/your/repo
      default_branch: main
      test_command: go test ./...
      lint_command: go vet ./...
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `sync_minutes` | int | `0` | Periodic `git fetch` interval in minutes. Keeps worktrees up-to-date with remote. `0` = disabled. |

**`repos.list[]` fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | *(required)* | Unique identifier for this repo |
| `path` | string | *(required)* | Absolute path to the repo (relative paths are resolved to absolute) |
| `description` | string | *(auto-detected)* | Repo description for triage context. Falls back to README. |
| `primary` | bool | `false` | Mark as the default repo for ambiguous messages (multi-repo only) |
| `default_branch` | string | `"main"` | Branch to base PRs on |
| `test_command` | string | *(none)* | Command to run tests at repo root |
| `lint_command` | string | *(none)* | Command to run linters at repo root |
| `auto_merge` | bool | `false` | Auto-merge PRs after validation passes |
| `merge_bot_fixups` | bool | `false` | Auto-merge bot fix-up PRs from review rounds |
| `pr_labels` | list | `[]` | Labels to add to all PRs created for this repo |
| `services` | list | `[]` | Per-service lint/test overrides (see [services](#reposservices)) |
| `vcs` | object | *(none)* | Per-repo VCS override (see [repos\[\].vcs](#reposvcs)) |

For single-repo setups, just configure one entry. For multi-repo, see [Multi-Repo Routing](#multi-repo-routing).

---

### `repos[].services`

For monorepos with multiple services that have different test/lint commands:

```yaml
repos:
  list:
    - name: my-monorepo
      path: /path/to/repo
      test_command: go test ./...        # fallback for unmatched files
      lint_command: go vet ./...
      services:
        - path: web-app
          test_command: make test
          lint_command: make stan && make cs
        - path: api
          test_command: pytest
          lint_command: ruff check .
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `path` | string | *(required)* | Subdirectory path relative to repo root |
| `test_command` | string | *(required)* | Test command to run from the service subdirectory |
| `lint_command` | string | *(required)* | Lint command to run from the service subdirectory |

**How it works:** When a tadpole changes files, toad matches them to services by path prefix. Each matched service's commands run from its subdirectory. Files that don't match any service fall back to the repo-level `test_command` and `lint_command`.

---

### `repos[].vcs`

Override the global VCS settings for a specific repo. Useful for mixed-platform setups (e.g., some repos on GitHub, others on GitLab).

```yaml
repos:
  list:
    - name: internal-api
      path: /path/to/repo
      vcs:
        platform: gitlab
        host: gitlab.mycompany.com
```

Fields are the same as the global [`vcs`](#vcs) section. When set, this overrides global VCS settings for this repo only.

---

### `limits`

Resource limits and validation constraints.

```yaml
limits:
  max_concurrent: 2
  max_turns: 30
  timeout_minutes: 10
  max_files_changed: 5
  max_retries: 1
  max_review_rounds: 3
  max_ci_fix_rounds: 2
  history_size: 50
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `max_concurrent` | int | `2` | Max concurrent tadpoles. Ribbit pool is `max_concurrent * 3`. |
| `max_turns` | int | `30` | Max Claude conversation turns per tadpole run |
| `timeout_minutes` | int | `10` | Timeout for each tadpole execution |
| `max_files_changed` | int | `5` | Validation fails if a tadpole changes more files than this |
| `max_retries` | int | `1` | Retry attempts when validation (test/lint) fails |
| `max_review_rounds` | int | `3` | Max PR review → fix cycles before giving up |
| `max_ci_fix_rounds` | int | `2` | Max CI failure → fix cycles before giving up |
| `history_size` | int | `50` | Lines of Slack thread history to include as context |
| `review_bots` | list | `[]` | Bot usernames whose PR comments can trigger fix tadpoles (e.g., `["greptile[bot]"]`) |
| `worktree_ttl_hours` | int | `0` | Auto-remove worktrees older than this many hours. `0` = disabled. |

**Tuning guidance:**
- **`max_concurrent`**: Start with 2. Increase if you have CPU/memory headroom — each tadpole runs a Claude Code subprocess.
- **`max_turns`**: 30 is good for small/medium tasks. For larger tasks, increase to 50–80.
- **`timeout_minutes`**: 10 covers most small fixes. Bump to 15–20 for medium tasks.
- **`max_files_changed`**: Safety guardrail. Keep at 5 for auto-spawned tadpoles. Increase for manual `toad run` tasks.
- **`max_retries`**: 1 is usually enough — the retry includes the failure context, so Claude often fixes on the first retry.
- **`max_review_rounds`**: 3 means toad will address up to 3 rounds of PR review comments.
- **`max_ci_fix_rounds`**: 2 means toad will attempt to fix CI failures up to 2 times.

---

### `triage`

Controls how incoming messages are classified.

```yaml
triage:
  model: haiku
  auto_spawn: false
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `model` | string | `"haiku"` | Claude model for triage classification. Haiku is fast and cheap. |
| `auto_spawn` | bool | `false` | When true, automatically spawn tadpoles for bug/feature messages without requiring an explicit trigger. |

**Note:** Even with `auto_spawn: false`, direct `@toad` mentions with bug/feature descriptions will spawn tadpoles. The `auto_spawn` flag controls whether *any* message in a monitored channel that looks like a bug/feature triggers a tadpole.

---

### `claude`

> **Deprecated:** Use [`agent`](#agent) instead. The `claude` section is still supported for backwards compatibility.

Settings for Claude Code invocation.

```yaml
claude:
  model: sonnet
  append_system_prompt: "Always write tests for new functions."
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `model` | string | `"sonnet"` | Claude model for tadpoles and ribbits |
| `append_system_prompt` | string | `""` | Custom instructions appended to Claude's system prompt |

The `append_system_prompt` is useful for project-specific coding conventions:

```yaml
claude:
  append_system_prompt: |
    - Use functional components with hooks, never class components
    - All API responses must include request_id
    - Write table-driven tests
```

---

### `agent`

Agent platform settings. Replaces the deprecated [`claude`](#claude) section.

```yaml
agent:
  platform: claude
  model: sonnet
  append_system_prompt: "Always write tests for new functions."
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `platform` | string | `"claude"` | Agent platform (currently only `claude`) |
| `model` | string | `"sonnet"` | Claude model for tadpoles and ribbits |
| `append_system_prompt` | string | `""` | Custom instructions appended to the agent's system prompt |

---

### `digest`

The Toad King — passive batch analysis that auto-spawns tadpoles for obvious fixes.

```yaml
digest:
  enabled: false
  dry_run: false
  batch_minutes: 5
  min_confidence: 0.95
  max_auto_spawn_hour: 3
  allowed_categories:
    - bug
  max_est_size: small
  max_chunk_size: 50
  chunk_timeout_secs: 120
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable the digest engine |
| `dry_run` | bool | `false` | Analyze messages but don't spawn tadpoles or post notifications |
| `batch_minutes` | int | `5` | How often to batch-analyze collected messages |
| `min_confidence` | float | `0.95` | Minimum confidence score (0–1) to auto-spawn |
| `max_auto_spawn_hour` | int | `3` | Max tadpoles auto-spawned per hour |
| `allowed_categories` | list | `["bug"]` | Which categories can be auto-spawned (e.g., `bug`, `feature`) |
| `max_est_size` | string | `"small"` | Max estimated task size for auto-spawn (`tiny`, `small`, `medium`, `large`) |
| `max_chunk_size` | int | `50` | Max messages per analysis batch |
| `chunk_timeout_secs` | int | `120` | Timeout for each batch analysis |
| `investigate_timeout_secs` | int | `600` | Timeout for each Sonnet investigation (10 min default) |
| `investigate_max_turns` | int | `25` | Max Claude turns per investigation |
| `comment_investigation` | bool | `false` | Post investigation findings as Slack replies (useful in dry-run mode) |
| `bot_list` | list | `[]` | Only these Slack bot IDs trigger active outreach with @mentions. Empty = all bots. |

See [Toad King (Digest) Tuning](#toad-king-digest-tuning) for advanced usage.

---

### `issue_tracker`

Integration with external issue trackers.

```yaml
issue_tracker:
  enabled: false
  provider: linear
  api_token: ${TOAD_LINEAR_API_TOKEN}
  team_id: TEAM-123
  create_issues: false
  bug_label_id: abc-123
  feature_label_id: def-456
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable issue tracker integration |
| `provider` | string | `"linear"` | Issue tracker provider (currently only `linear`) |
| `api_token` | string | *(required if enabled)* | API authentication token |
| `team_id` | string | *(required if `create_issues`)* | Team ID for issue creation |
| `create_issues` | bool | `false` | Create issues in the tracker when toad finds opportunities |
| `bug_label_id` | string | *(optional)* | Label ID applied to bug issues |
| `feature_label_id` | string | *(optional)* | Label ID applied to feature issues |
| `respect_assignees` | bool | `false` | When true, defer to the ticket assignee instead of spawning a tadpole. Posts findings as a comment on the ticket. |
| `stale_days` | int | `7` | Assignments older than this many days are considered stale and ignored |

**Setting up Linear:**
1. Create a Linear API token at [linear.app/settings/api](https://linear.app/settings/api)
2. Find your team ID in Linear's team settings
3. Optionally create labels for bugs and features, and note their IDs

---

### `vcs`

Global version control platform settings. Can be overridden per-repo with [`repos[].vcs`](#reposvcs).

```yaml
vcs:
  platform: github
  host: ""
  bot_usernames: []
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `platform` | string | `"github"` | VCS platform: `github` or `gitlab` |
| `host` | string | *(none)* | Hostname for self-hosted GitLab (e.g., `gitlab.mycompany.com`) |
| `bot_usernames` | list | `[]` | Bot account usernames to recognize (for filtering bot comments) |

**GitHub** is the default and requires no extra configuration beyond `gh auth login`.

**GitLab** setup:
1. Set `platform: gitlab`
2. For self-hosted GitLab, set `host` to your instance hostname
3. Authenticate with `glab auth login`

**Self-hosted GitLab:**

```yaml
vcs:
  platform: gitlab
  host: gitlab.mycompany.com
```

Or via environment variable:

```bash
export TOAD_GITLAB_HOST=gitlab.mycompany.com
```

---

### `log`

Logging configuration.

```yaml
log:
  level: info
  file: ~/.toad/toad.log
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `level` | string | `"info"` | Log level: `debug`, `info`, `warn`, `error` |
| `file` | string | `~/.toad/toad.log` | Log file path. Use `debug` level for troubleshooting. |

---

### `mcp`

MCP (Model Context Protocol) server for Claude Desktop and Claude Code integration.

```yaml
mcp:
  enabled: false
  host: localhost
  port: 8099
  devs: []
  message: ""
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable the MCP server |
| `host` | string | `"localhost"` | Host to bind the HTTP server to |
| `port` | int | `8099` | Port for the MCP HTTP endpoint |
| `devs` | list | `[]` | Slack user IDs granted `dev` role (access to logs tool) |
| `message` | string | `""` | Optional message included in the DM when a user runs `/toad mcp connect` |

**How it works:** When enabled, toad starts a Streamable HTTP server at `http://{host}:{port}/mcp`. Users authenticate via bearer tokens generated through the `/toad mcp connect` Slack slash command. A public health endpoint is available at `/health`.

---

### `personality`

Adaptive personality system. Toad develops personality traits over time based on team feedback.

```yaml
personality:
  enabled: false
  learning_enabled: true
  file_path: ~/.toad/personality.yaml
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable the personality system |
| `learning_enabled` | bool | `true` | Allow traits to adapt from feedback. When false, uses base traits only. |
| `file_path` | string | `~/.toad/personality.yaml` | Where personality state is persisted |

**How it works:** Toad has 22 personality traits (verbosity, formality, humor, caution, etc.) that influence how it communicates. Traits adjust through dampened learning from:
- **Emoji reactions** on toad's messages (e.g., thumbs up, confused face)
- **Text feedback** in thread replies (e.g., "too verbose", "be more concise")
- **PR outcomes** (merged = positive reinforcement, closed = negative)

The radar chart in `toad status` and the kiosk view visualize current trait values.

---

## Environment Variables

Environment variables override config file values.

| Variable | Config Path | Description |
|----------|-------------|-------------|
| `TOAD_SLACK_APP_TOKEN` | `slack.app_token` | Slack Socket Mode app token (`xapp-...`) |
| `TOAD_SLACK_BOT_TOKEN` | `slack.bot_token` | Slack bot OAuth token (`xoxb-...`) |
| `TOAD_LINEAR_API_TOKEN` | `issue_tracker.api_token` | Linear API token for issue tracking |
| `TOAD_GITLAB_HOST` | `vcs.host` | Self-hosted GitLab hostname |

You can also use `${ENV_VAR}` syntax in YAML values to reference environment variables:

```yaml
slack:
  app_token: ${TOAD_SLACK_APP_TOKEN}
```

**Precedence:** Environment variables > `.toad.yaml` > `~/.toad/config.yaml` > built-in defaults.

---

## CLI Commands

### `toad`

Start the daemon.

```bash
toad
```

Loads config, connects to Slack via Socket Mode, auto-joins public channels, and starts processing messages. If no config exists, runs the setup wizard automatically.

### `toad init`

Interactive setup wizard.

```bash
toad init
```

Walks you through:
1. Slack app setup guide and token input
2. Repository configuration (path, test/lint commands)
3. Toad King (digest) opt-in
4. Advanced options (triggers, limits)

Creates `.toad.yaml` in the current directory.

### `toad run`

CLI one-shot mode — spawn a tadpole without Slack.

```bash
toad run "Fix the login bug in auth.go"
toad run --repo frontend "Add input validation to the signup form"
```

| Flag | Description |
|------|-------------|
| `--repo` | Target repo name (required when multiple repos are configured) |

The tadpole runs the full lifecycle (worktree → Claude → validate → PR) and exits.

### `toad status`

Live monitoring dashboard.

```bash
toad status
toad status --port 8080
```

| Flag | Description |
|------|-------------|
| `--port` | Pin to a specific port (default: random available) |

Opens a web dashboard in your browser with real-time monitoring: daemon status, active runs, run history, triage breakdown, Toad King opportunities, PR watches, merge stats, integration status (Digest, Issue Tracker, MCP, PR Reviewer), Claude Code usage, and config. Auto-refreshes every 3 seconds. Reads directly from SQLite, so it works even when the daemon is stopped.

A kiosk view is available at `/kiosk` — optimized for office TVs and large displays with a two-column layout, large stat tiles, and a clock. Access it at `http://localhost:{port}/kiosk`.

### `toad version`

Print version information.

```bash
toad version
toad version -v   # include commit hash and build date
```

Automatically checks for available updates.

### `toad update`

Self-update to the latest version.

```bash
toad update
```

Uses Homebrew if available, otherwise prints manual update instructions.

### `toad restart`

Gracefully restart the running daemon.

```bash
toad restart
```

Sends a restart signal to the running daemon. The daemon drains in-flight messages, waits for active tadpoles to finish (up to 30 minutes), then restarts with the latest binary and config. Useful after config changes or updates.

---

## Interacting with Toad

### Triggering toad

| Action | How | What happens |
|--------|-----|--------------|
| Ask a question | `@toad how does the auth middleware work?` | Ribbit: Sonnet reads your code and replies in-thread |
| Keyword trigger | `toad fix the broken date parser` | Triage determines if it's a bug/feature/question and routes |
| Request a tadpole | React with :frog: on any toad reply | Spawns a tadpole for the original message |
| Bug/feature (auto) | `@toad` a bug report or feature request | Auto-spawns a tadpole |

### Message flow

1. **Triage** — Every message is classified by Haiku in ~1 second: actionable? category (bug/feature/question)? estimated size? relevant files?
2. **Route** — Bugs and features spawn tadpoles. Questions get ribbit replies. Passive observations get a ribbit with a :frog: CTA.
3. **Tadpole** — Creates a git worktree, runs Claude Code to make changes, validates with your test/lint commands, retries on failure, pushes, and opens a PR.
4. **Ribbit** — Sonnet answers with codebase context using read-only tools (Glob, Grep, Read). Thread memory means follow-up questions stay coherent.
5. **PR Watch** — After a tadpole ships a PR, toad monitors for review comments and auto-spawns fix tadpoles (up to `max_review_rounds` rounds).

### Thread behavior

- Toad always replies in-thread to keep channels clean
- Thread context is preserved — follow-up messages in the same thread are coherent
- The `history_size` limit controls how much thread history is included

---

## Advanced Topics

### Multi-Repo Routing

Toad supports monitoring multiple repositories. When a message comes in, toad determines which repo it relates to.

**Setup:**

```yaml
repos:
  list:
    - name: backend
      path: /home/dev/backend
      primary: true              # fallback when ambiguous
      test_command: go test ./...
      lint_command: go vet ./...
    - name: frontend
      path: /home/dev/frontend
      test_command: npm test
      lint_command: npm run lint
```

**How routing works:**

1. **Profile building:** On startup, toad auto-detects each repo's stack (Go, TypeScript, Python, PHP, Rust) and module name from manifest files (go.mod, package.json, etc.)
2. **Triage hint:** The triage prompt includes repo profiles, so Haiku can suggest a `"repo"` name in its classification
3. **Resolver verification:** The resolver verifies the triage hint by checking if mentioned files actually exist in the suggested repo
4. **Primary fallback:** If the resolver can't determine the repo, it falls back to the repo marked `primary: true`

**Rules:**
- At most one repo can be marked `primary: true`
- Each repo must have a unique `name`
- For `toad run`, use `--repo <name>` to specify the target

---

### PR Review Automation

After a tadpole opens a PR, toad continues watching it:

1. **Review comments** — When reviewers leave comments, toad spawns a fix tadpole that reads the review feedback and makes corrections
2. **CI failures** — If CI fails on the PR, toad spawns a fix tadpole to address the failures
3. **Bot fixup merging** — With `merge_bot_fixups: true`, fix-up commits from review rounds are auto-merged

**Limits:**
- `max_review_rounds` (default: 3) — max rounds of review comment → fix cycles
- `max_ci_fix_rounds` (default: 2) — max rounds of CI failure → fix cycles

These prevent infinite loops where toad keeps trying to fix unfixable issues.

---

### Toad King (Digest) Tuning

The digest engine passively collects all non-bot messages and batch-analyzes them to find auto-spawnable opportunities.

**Recommended first-time setup:**

```yaml
digest:
  enabled: true
  dry_run: true              # start in dry-run to observe before acting
  batch_minutes: 5
  min_confidence: 0.95
  max_auto_spawn_hour: 3
  allowed_categories:
    - bug
  max_est_size: small
```

**Workflow:**
1. Start with `dry_run: true` — toad will log what it *would* spawn without actually doing it
2. Monitor the logs to see if the detections are accurate
3. Once you're confident, set `dry_run: false`
4. Start conservative: only `bug` category, `small` max size, high confidence
5. Gradually relax: add `feature` to `allowed_categories`, increase `max_auto_spawn_hour`

**Guardrails:**
- `min_confidence: 0.95` — only spawn when Haiku is very confident
- `allowed_categories: [bug]` — start with bugs only (most predictable)
- `max_est_size: small` — only tiny/small tasks (less risk of large, wrong changes)
- `max_auto_spawn_hour: 3` — rate limit to prevent runaway spawning

---

### Mixed GitHub/GitLab Setups

If your organization uses both GitHub and GitLab, use per-repo VCS overrides:

```yaml
vcs:
  platform: github             # global default

repos:
  list:
    - name: public-api
      path: /home/dev/public-api
      # uses global github default

    - name: internal-api
      path: /home/dev/internal-api
      vcs:
        platform: gitlab
        host: gitlab.mycompany.com

    - name: data-pipeline
      path: /home/dev/data-pipeline
      vcs:
        platform: gitlab
        host: gitlab.mycompany.com
```

Each repo uses its own VCS provider for PR creation and review monitoring. The global `vcs` settings serve as the default for repos without an explicit override.

---

### MCP Server

The MCP server lets Claude Desktop and Claude Code interact with your toad instance — asking codebase questions and reading daemon logs.

**Setup:**

1. Enable in config:
   ```yaml
   mcp:
     enabled: true
     port: 8099
     devs:
       - U0123ABCDEF    # Slack user IDs with dev access
   ```

2. Add a `/toad` slash command to your Slack app (see [Slack App Setup](#step-4b-add-slash-command-optional-for-mcp-server))

3. In Slack, run `/toad mcp connect` — toad DMs you a personal token and the `claude mcp add` command to run

4. Add to Claude Code:
   ```bash
   claude mcp add toad -- curl -N -H "Authorization: Bearer YOUR_TOKEN" http://localhost:8099/mcp
   ```

**Available tools:**
- **ask** — Ask toad a codebase question. Uses the ribbit engine with read-only tools (Glob, Grep, Read). Supports multi-turn conversation context per user.
- **logs** — Read and filter daemon log lines. Supports line count, level filtering, and regex search. Requires `dev` role.

**Slash commands:**
| Command | Description |
|---------|-------------|
| `/toad mcp connect` | Generate a personal MCP token (sent via DM) |
| `/toad mcp status` | Check your token status |
| `/toad mcp revoke` | Revoke your current token |
| `/toad mcp ping` | Verify MCP server connectivity |

**Health endpoint:**
`GET http://localhost:8099/health` returns daemon status (uptime, active tadpoles/ribbits, Slack connection) — no authentication required.

---

## Troubleshooting

### Toad doesn't respond to messages

- **Check the channel:** If `channels` is set, make sure the channel is listed. Use `channels: []` to monitor all.
- **Check the mention:** Make sure you're using `@toad` (the bot's display name in Slack).
- **Check tokens:** Run `toad` and look for connection errors in the log. Invalid tokens show up immediately.
- **Check events:** Verify your Slack app subscribes to `app_mention`, `message.channels`, `message.groups`, and `reaction_added`.

### "Cannot connect to Slack"

- Verify `app_token` starts with `xapp-` and `bot_token` starts with `xoxb-`
- Make sure Socket Mode is enabled in your Slack app settings
- Check that the app-level token has `connections:write` scope

### Tadpole fails validation

- Check that `test_command` and `lint_command` work when run manually in the repo
- Look at the tadpole's output in the Slack thread — toad posts validation errors
- Increase `max_retries` if the first attempt is close but needs one more try
- Increase `max_turns` if Claude is running out of conversation turns

### PR creation fails

- Make sure `gh` (or `glab`) is authenticated: `gh auth status`
- Check that the repo has a remote origin configured
- For GitLab, verify `host` is set correctly for self-hosted instances

### "Too many files changed"

- The `max_files_changed` limit protects against runaway changes
- Increase the limit in config if the task legitimately needs more files
- Or use `toad run` with a more specific task description to keep changes focused

### Toad King spawning too aggressively

- Increase `min_confidence` (e.g., 0.98)
- Reduce `max_auto_spawn_hour`
- Restrict `allowed_categories` to just `[bug]`
- Reduce `max_est_size` to `tiny`
- Use `dry_run: true` to observe before enabling real spawning

### State or worktree issues

- State DB: `~/.toad/state.db` — delete to reset (toad recreates on startup)
- Worktrees: `~/.toad/worktrees/` — toad cleans orphans on startup, but you can delete manually
- Logs: check `~/.toad/toad.log` (or wherever `log.file` points)
- Auto-update: the dashboard can trigger updates and restarts — look for the update indicator in the dashboard header

### Daemon crashed mid-tadpole

If the daemon crashes or is killed while tadpoles are running, toad handles recovery automatically on the next startup:
- Stale runs (stuck in starting/running/validating/shipping) are marked as failed
- Orphaned worktrees in `~/.toad/worktrees/` are cleaned up
- PR watches continue from where they left off

No manual intervention is needed — just restart toad.

---

## Complete Example Config

A fully annotated `.toad.yaml` with all options:

```yaml
# Slack connection
slack:
  app_token: ${TOAD_SLACK_APP_TOKEN}     # Socket Mode token (xapp-...)
  bot_token: ${TOAD_SLACK_BOT_TOKEN}     # Bot OAuth token (xoxb-...)
  channels: []                            # empty = all public channels
  triggers:
    emoji: frog                           # react with :frog: to spawn tadpole
    keywords:                             # keyword triggers
      - "toad fix"
      - "toad help"

# Repositories
repos:
  sync_minutes: 0                        # periodic git fetch (0 = disabled)
  list:
    - name: backend
      path: /home/dev/backend
      primary: true                         # default for ambiguous messages
      default_branch: main
      test_command: go test ./...
      lint_command: go vet ./...
      auto_merge: false                     # auto-merge passing PRs
      merge_bot_fixups: false               # auto-merge bot fix-up PRs
      pr_labels:                            # labels added to PRs
        - toad
        - automated
      services:                             # monorepo service overrides
        - path: web-app
          test_command: make test
          lint_command: make stan && make cs
        - path: api
          test_command: pytest
          lint_command: ruff check .

    - name: frontend
      path: /home/dev/frontend
      default_branch: main
      test_command: npm test
      lint_command: npm run lint
      vcs:                                  # per-repo VCS override
        platform: gitlab
        host: gitlab.mycompany.com

# Resource limits
limits:
  max_concurrent: 2                       # concurrent tadpoles
  max_turns: 30                           # Claude turns per run
  timeout_minutes: 10                     # per-tadpole timeout
  max_files_changed: 5                    # validation file limit
  max_retries: 1                          # retries on validation failure
  max_review_rounds: 3                    # PR review fix cycles
  max_ci_fix_rounds: 2                    # CI failure fix cycles
  history_size: 50                        # thread history lines
  review_bots: []                       # bot usernames triggering fixes
  worktree_ttl_hours: 0                 # auto-cleanup old worktrees (0 = off)

# Triage classification
triage:
  model: haiku                            # fast + cheap classification
  auto_spawn: false                       # auto-spawn for any bug/feature

# Claude Code settings
claude:
  model: sonnet                           # model for tadpoles and ribbits
  append_system_prompt: |                 # custom coding instructions
    - Write table-driven tests
    - Use functional components with hooks

# Agent settings (replaces claude)
agent:
  platform: claude                        # agent platform
  model: sonnet                           # model for tadpoles and ribbits
  append_system_prompt: |                 # custom coding instructions
    - Write table-driven tests
    - Use functional components with hooks

# Toad King (digest)
digest:
  enabled: false                          # opt-in batch analysis
  dry_run: false                          # analyze without spawning
  batch_minutes: 5                        # batch window
  min_confidence: 0.95                    # confidence threshold
  max_auto_spawn_hour: 3                  # hourly spawn limit
  allowed_categories:                     # categories to auto-spawn
    - bug
  max_est_size: small                     # max task size
  max_chunk_size: 50                      # messages per batch
  chunk_timeout_secs: 120                 # batch analysis timeout
  investigate_timeout_secs: 600           # investigation timeout
  investigate_max_turns: 25               # turns per investigation
  comment_investigation: false            # post findings as replies
  bot_list: []                            # bot IDs for active outreach

# Issue tracking
issue_tracker:
  enabled: false                          # enable Linear integration
  provider: linear
  api_token: ${TOAD_LINEAR_API_TOKEN}
  team_id: ""                             # Linear team ID
  create_issues: false                    # create issues from PRs
  bug_label_id: ""                        # Linear label for bugs
  feature_label_id: ""                    # Linear label for features
  respect_assignees: false                # defer to ticket assignee
  stale_days: 7                           # ignore old assignments

# Version control
vcs:
  platform: github                        # github or gitlab
  host: ""                                # self-hosted GitLab hostname
  bot_usernames: []                       # bot accounts to recognize

# Logging
log:
  level: info                             # debug, info, warn, error
  file: ~/.toad/toad.log                  # log file path

# MCP server (Claude Desktop/Code integration)
mcp:
  enabled: false                          # opt-in MCP server
  host: localhost                         # bind host
  port: 8099                              # HTTP port
  devs: []                                # Slack user IDs with dev access
  message: ""                             # DM message on connect

# Personality system
personality:
  enabled: false                          # opt-in adaptive personality
  learning_enabled: true                  # allow traits to adapt
  file_path: ~/.toad/personality.yaml     # personality state file
```
