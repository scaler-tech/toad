# Toad Setup Guide

The complete guide to installing, configuring, and running toad — from zero to a working investigation-and-ticket pipeline.

**Table of Contents**

- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [Slack App Setup](#slack-app-setup)
- [Quick Start](#quick-start)
- [Configuration Walkthrough](#configuration-walkthrough)
- [Environment Variables](#environment-variables)
- [Finding Your Sentry Bot's ID](#finding-your-sentry-bots-id)
- [Running as a Daemon](#running-as-a-daemon)
- [Smoke-Test Checklist](#smoke-test-checklist)
- [Troubleshooting](#troubleshooting)

---

## Prerequisites

| Requirement | Why | Get it |
|---|---|---|
| **Go 1.25+** | Toad is written in Go | [go.dev/dl](https://go.dev/dl/) (only needed to build from source) |
| **Claude Code CLI** (`claude`), with a subscription seat | Toad invokes Claude as a subprocess for triage, ribbit, and investigation | [docs.anthropic.com](https://docs.anthropic.com/en/docs/claude-code) |
| **GitHub CLI** (`gh`) or **GitLab CLI** (`glab`) | Read-only PR/issue lookups used by ribbit (investigations are Read/Glob/Grep + MCP only, no `gh`/`glab`) | [cli.github.com](https://cli.github.com) / [gitlab.com/gitlab-org/cli](https://gitlab.com/gitlab-org/cli) |
| **A Slack app** with Socket Mode | Toad's transport — no public URL needed | [api.slack.com/apps](https://api.slack.com/apps) |
| **A Linear API token + team** | Toad files tickets here | [linear.app/settings/api](https://linear.app/settings/api) |
| **A Sentry MCP token** (optional) | Lets investigations pull stack traces/issue context directly | [mcp.sentry.dev](https://mcp.sentry.dev) |

Authenticate the Claude CLI before starting toad — run `claude` interactively once, or generate a headless token with:

```bash
claude setup-token
```

This gives toad a subscription seat to run against. If that seat gets throttled (usage or rate limits), toad can retry once against a plain Anthropic API key — see [`agent.fallback_api_key_env`](#agent) below.

Authenticate your VCS CLI too (`gh auth login` or `glab auth login`) — toad uses it for read-only lookups only; it never pushes, merges, or opens anything with it.

---

## Installation

### macOS and Linux (Homebrew)

```bash
brew tap scaler-tech/pkg https://github.com/scaler-tech/pkg
brew install --cask toad
```

> **macOS security note:** If macOS blocks the app with "cannot be opened because the developer cannot be verified", the cask's post-install hook should handle it. If not: `xattr -d com.apple.quarantine $(which toad)`

### Windows (Scoop)

```bash
scoop bucket add scaler-tech https://github.com/scaler-tech/pkg
scoop install toad
```

### Other options

```bash
# Binary releases: https://github.com/scaler-tech/toad/releases/latest

# Go install
go install github.com/scaler-tech/toad@latest

# Build from source
git clone https://github.com/scaler-tech/toad.git && cd toad && make build
```

---

## Slack App Setup

Toad connects via Socket Mode, so it runs as a daemon on your machine or server — no inbound webhook, no public URL.

### 1. Create the app

Go to [api.slack.com/apps](https://api.slack.com/apps) → **Create New App** → **From scratch**. Name it `toad` and pick your workspace.

### 2. Enable Socket Mode

**Settings → Socket Mode** → toggle on. When prompted, generate an app-level token with scope `connections:write`. It starts with `xapp-` — this is your **app token**.

### 3. Add bot token scopes

**OAuth & Permissions → Scopes → Bot Token Scopes** — add exactly what the code uses:

| Scope | Why |
|---|---|
| `app_mentions:read` | Detect `@toad` mentions |
| `channels:history` | Read messages in public channels |
| `channels:join` | Auto-join public channels on startup |
| `channels:read` | List channels, resolve names |
| `chat:write` | Post replies and status updates |
| `groups:history` | Read messages in private channels toad's been invited to |
| `groups:read` | List private channels |
| `reactions:write` | Post acknowledgment reactions |
| `users:read` | Resolve display names |

### 4. Subscribe to events

**Event Subscriptions** → toggle on → **Subscribe to bot events**: `app_mention`, `message.channels`, `message.groups`.

### 5. Enable Interactivity (for the "Create Linear ticket" button)

**Interactivity & Shortcuts** → toggle **Interactivity** on. Leave the Request URL empty — Socket Mode routes button clicks automatically. No extra scopes needed.

### 6. Add a slash command (optional, for the MCP server)

**Slash Commands → Create New Command**: command `/toad`, description "Toad daemon commands", usage hint `mcp connect | mcp status | mcp revoke | status | help`. This is what users run to get their personal MCP bearer token.

### 7. Install to workspace

**Install App → Install to Workspace**. Copy the **Bot User OAuth Token** (`xoxb-...`) — your **bot token**.

You now have both tokens toad needs: `xapp-...` (Socket Mode) and `xoxb-...` (reading/posting).

---

## Quick Start

```bash
toad init
```

Walks you through Slack tokens, repo paths, and the Toad King (digest) opt-in, then writes `.toad.yaml` in the current directory. The wizard doesn't yet cover every v2 section — notably `intake.bot_allowlist` and `ticket` tuning — so open the generated file afterward and add those by hand. [`.toad.yaml.example`](.toad.yaml.example) has every key with a comment; treat it as the reference, not something to copy wholesale.

```bash
toad
```

Starts the daemon: loads config, connects to Slack, auto-joins public channels, starts listening.

Verify with a question in any channel toad has joined:

```
@toad how does the ticket engine decide auto-file vs propose?
```

You should get an in-thread, codebase-grounded reply within a few seconds. See the [smoke-test checklist](#smoke-test-checklist) for the full pass.

---

## Configuration Walkthrough

Config loads in this order (later wins): built-in defaults → `~/.toad/config.yaml` → `.toad.yaml` (project-local) → environment variables.

### `slack`

```yaml
slack:
  app_token: "xapp-..."
  bot_token: "xoxb-..."
  channels: []          # empty = auto-join and monitor all public channels
  triggers:
    keywords:
      - "toad fix"
      - "toad help"
```

`channels` empty means toad joins and watches every public channel. Set it to specific channel names to restrict — private channels need an explicit `/invite @toad` plus a config entry. Restart after adding a channel; names resolve to IDs at startup.

### `repos`

```yaml
repos:
  sync_minutes: 10   # periodic git fetch; 0 = disabled
  list:
    - name: backend
      path: /home/dev/backend
      primary: true            # fallback when a message's repo is ambiguous
      default_branch: main
    - name: frontend
      path: /home/dev/frontend
```

Repos are read-only context for investigations — toad never commits, pushes, or opens anything against them. At least one repo is required. `name` must be unique per repo; at most one repo may be `primary: true`. On startup toad auto-detects each repo's stack/module from manifest files (go.mod, package.json, etc.) and uses that to help triage suggest a repo for ambiguous messages; the resolver double-checks that suggestion against real files before trusting it, falling back to the primary repo.

### `limits`

```yaml
limits:
  max_concurrent: 2     # concurrent investigations; ribbit pool is 3x this
  timeout_minutes: 10   # ribbit run timeout
```

### `triage`

```yaml
triage:
  model: haiku   # fast, cheap classification (~1s)
```

### `intake`

```yaml
intake:
  bot_allowlist:
    - "B0123456789"   # Slack Bot IDs allowed into triage (e.g. Sentry)
```

Only messages from allowlisted bot IDs enter the investigate-and-file pipeline as intake; everything else from a bot is dropped from individual triage (the digest engine still sees it for batch analysis). This is also what makes a finding "Sentry-corroborated" for the auto-file gate — see [finding your Sentry bot's ID](#finding-your-sentry-bots-id).

### `agent`

```yaml
agent:
  platform: claude
  model: sonnet
  # append_system_prompt: "Extra instructions for every agent run"
  # fallback_api_key_env: "ANTHROPIC_API_KEY"
  # mcp_servers:
  #   sentry:
  #     url: "https://mcp.sentry.dev/mcp"
  #     auth_token_env: "TOAD_SENTRY_MCP_TOKEN"
```

`fallback_api_key_env` names an environment variable holding a plain Anthropic API key. If a run fails because the CLI's subscription seat hit a usage/rate limit, toad retries once with `ANTHROPIC_API_KEY` set from that variable's value — so investigations don't just die when the seat is temporarily throttled.

`mcp_servers` are extra MCP servers made available during read-only investigations. An HTTP-type entry (`url` set) gets an `Authorization: Bearer` header populated from `os.Getenv(auth_token_env)` when that env var resolves to a non-empty value; a command-type entry (`command` set instead) runs as a local subprocess and doesn't use `auth_token_env`. At least one of `url` or `command` is required per entry.

### `ticket`

```yaml
ticket:
  auto_file: true               # gate: auto-file when corroborated + confident + feasible
  auto_file_confidence: 0.85    # confidence floor to clear the gate
  # triage_state_id: ""         # optional Linear workflow state ID for new tickets
```

Auto-filing requires **all** of: `auto_file: true`, at least one corroborating Sentry issue ID on the finding, `confidence >= auto_file_confidence`, and a feasible verdict. Short of all four, toad proposes via the Slack CTA instead. See [Troubleshooting](#auto_file-silently-downgraded) for what happens if `auto_file` is on but the tracker can't actually create issues.

### `digest`

```yaml
digest:
  enabled: false                # opt-in
  batch_minutes: 5
  min_confidence: 0.8
  max_auto_spawn_hour: 3
  allowed_categories: ["bug"]
  max_est_size: "medium"
  # max_chunk_size: 50
  # chunk_timeout_secs: 120
  # investigate_timeout_secs: 600
  # bot_list: []
```

This is Toad King: passive batch analysis of channel traffic that feeds the same investigate-then-gate flow as triggered messages, never spawning anything of its own — it proposes via Slack CTA or auto-files only when Sentry-corroborated (see `ticket.auto_file` above). Digest has a single mode: `enabled: true` or `false`. The confidence floor is `min_confidence` (default 0.8; passing it only proposes a human-gated CTA, or auto-files when Sentry-corroborated). `bot_list`, if set, restricts which bot IDs the digest will actively engage with — leave empty to consider all.

### `issue_tracker`

```yaml
issue_tracker:
  enabled: false
  provider: linear
  # api_token: ""    # or leave unset and use TOAD_LINEAR_API_TOKEN instead (recommended)
  team_id: ""
  create_issues: false
  # bug_label_id: ""
  # feature_label_id: ""
  # respect_assignees: false
  # stale_days: 7
```

This is the tracker connection itself; `ticket.auto_file` above is the separate gate on top of it. Both `enabled` and `create_issues` must be true for toad to be capable of filing anything — see the troubleshooting note below on what happens if they aren't.

When a reporter explicitly asks toad to assign or hand off a ticket ("assign this to me", "assign to dejan", "give this to biome"), toad resolves each requested name against the workspace's Linear users (by display name, real name, or email) and routes it to whichever slot fits: a human user becomes the issue's assignee, an app/agent user (like Biome, which Linear provisions as an OAuth-app user) becomes its native delegate — no separate config needed, since Linear itself already knows the difference. Unresolved names are dropped with a warning rather than blocking the filing, and Slack's reply says so.

Prefer the `TOAD_LINEAR_API_TOKEN` environment variable over the `api_token` YAML key — it keeps the token out of a config file that might get committed or copied around. If you do set `api_token` directly in `.toad.yaml`, `chmod 0600` the file (owner read/write only) since it then holds a live credential in plain text.

### `vcs`

```yaml
vcs:
  platform: github   # or gitlab
  # host: ""         # self-hosted GitLab hostname
  # bot_usernames: []
```

Used for ribbit's read-only PR/CI/issue lookups (`gh pr view`, `gh issue view`, etc. — or the `glab` equivalents). Investigations do not get `gh`/`glab` — they run with Read/Glob/Grep plus any configured MCP tools only. Per-repo overrides are supported via `repos[].vcs` with the same fields.

### `release_notes`

```yaml
release_notes:
  channel: "toad-dev"   # channel NAME; empty (default) disables the feature
```

When toad starts up running a new version (typically after the Homebrew auto-update + supervisord restart), it posts AI-generated release notes to this channel — exactly once per version, tracked via an internal setting. The first startup after enabling the feature just records the current version silently, so upgrading to *this* release doesn't itself trigger an announcement. Leave `channel` unset to disable.

### `log`

```yaml
log:
  level: info   # debug, info, warn, error
  # file: ~/.toad/toad.log
```

### `mcp`

```yaml
mcp:
  enabled: false
  host: localhost
  port: 8099
  # tls: false            # serves the endpoint over real TLS — requires cert_file/key_file below,
  #                       # and affects the URL shown in /toad mcp connect DMs
  # cert_file: ""         # TLS certificate path; required when tls is true
  # key_file: ""          # TLS private key path; required when tls is true
  # token_ttl_days: 90    # MCP bearer token lifetime in days; 0 = tokens never expire
  devs: []         # Slack user IDs granted the logs/query/investigations dev role
  message: ""      # optional message included in the connect DM
```

`tls` isn't cosmetic: when set, toad actually serves HTTPS on `mcp.host`/`mcp.port`, and startup validation fails closed if `cert_file` or `key_file` is missing. `token_ttl_days` bounds how long a `/toad mcp connect`-issued bearer token stays valid (default 90 days); set it to `0` for tokens that never expire.

---

## Environment Variables

| Variable | Config path | Notes |
|---|---|---|
| `TOAD_SLACK_APP_TOKEN` | `slack.app_token` | `xapp-...` |
| `TOAD_SLACK_BOT_TOKEN` | `slack.bot_token` | `xoxb-...` |
| `TOAD_LINEAR_API_TOKEN` | `issue_tracker.api_token` | Linear API token |
| `TOAD_GITLAB_HOST` | `vcs.host` | Self-hosted GitLab hostname (global only; per-repo overrides must go in YAML) |
| `TOAD_LOG_LEVEL` | `log.level` | Overrides the configured log level |
| *(name you choose via `agent.fallback_api_key_env`)* | — | Typically `ANTHROPIC_API_KEY`. Holds the API key toad retries with when the Claude subscription seat is throttled. |
| *(name you choose via `agent.mcp_servers.<name>.auth_token_env`)* | — | e.g. `TOAD_SENTRY_MCP_TOKEN` for a Sentry MCP entry. Holds the bearer token sent to that MCP server. |

Environment variables always win over both config files — set one of the above and the matching config key can be left unset. There's no `${VAR}` expansion inside `.toad.yaml` itself; a value like `${TOAD_LINEAR_API_TOKEN}` written into the YAML would be used literally, not substituted. Use the environment variable directly (unset in YAML), or write the plain value into YAML.

---

## Finding Your Sentry Bot's ID

`intake.bot_allowlist` gates which Slack bot IDs are allowed into triage — and, for anything that comes from an allowlisted bot, is also what makes a finding "Sentry-corroborated" for ticket auto-filing. To find the bot ID for your Sentry Slack integration (or any other alerting bot):

1. In the Slack channel where the bot posts, find one of its messages.
2. Open the message actions menu → **Copy link**, or view raw event data — the message JSON includes a `bot_id` field (starts with `B`).
3. Alternatively, run `toad` with `log.level: debug` — every bot message is logged with its `bot_id` at debug level as it comes in.
4. Add it to config:

   ```yaml
   intake:
     bot_allowlist:
       - "B0123456789"
   ```

Restart toad to pick up the change.

---

## Running as a Daemon

Under a process supervisor (systemd, supervisord), set `SUPERVISED=1` so toad exits cleanly on restart/update instead of self-replacing via `syscall.Exec`.

**systemd example:**

```ini
[Service]
Environment=SUPERVISED=1
Environment=TOAD_SLACK_APP_TOKEN=xapp-...
Environment=TOAD_SLACK_BOT_TOKEN=xoxb-...
Environment=TOAD_LINEAR_API_TOKEN=lin_api_...
ExecStart=/usr/local/bin/toad
Restart=always
```

**supervisord example:**

```ini
[program:toad]
command=/usr/local/bin/toad
environment=SUPERVISED=1,TOAD_SLACK_APP_TOKEN="xapp-...",TOAD_SLACK_BOT_TOKEN="xoxb-...",TOAD_LINEAR_API_TOKEN="lin_api_..."
autorestart=true
```

`toad status` (optionally `--port <n>`) opens a live dashboard reading directly from SQLite — it works even while the daemon is stopped. `toad restart` gracefully restarts a running daemon (useful after config changes). `toad update` self-updates via Homebrew when available.

**Upgrading and MCP tokens:** the schema migration that hardens MCP token storage (hashing at rest, expiry) wipes any existing MCP bearer tokens as part of the upgrade — this is a one-time effect of that migration, not something later upgrades repeat. After upgrading past it, anyone using `/toad mcp connect` (Claude Desktop/Code, Biome, etc.) needs to re-run it in Slack to get a fresh token.

---

## Smoke-Test Checklist

Run through this after any fresh install or config change:

- [ ] **Question** — `@toad <question about the codebase>` in a joined channel gets an in-thread, codebase-grounded reply.
- [ ] **Bug report + CTA** — `@toad <plausible bug description>` triggers triage → investigation → either an auto-filed ticket link, or a "Create Linear ticket" button on the findings message.
- [ ] **Duplicate click** — click "Create Linear ticket" twice (or re-trigger the same report) and confirm the second pass posts a re-observation comment on the existing ticket rather than filing a duplicate.
- [ ] **Bot alert** — post (or simulate) a message from an allowlisted bot ID and confirm it's routed to intake, not dropped or treated as passive chatter.
- [ ] **Escalate** — click the "Create Linear ticket" CTA button on one of toad's replies (or ask explicitly, e.g. "toad, create a ticket for this") and confirm a ticket files immediately, bypassing the gate.
- [ ] **Restart recovery** — kill `toad` mid-investigation, restart it, and confirm the run is marked failed (not left stuck) and normal processing resumes without manual cleanup.

---

## Troubleshooting

### Toad doesn't respond to messages

- If `channels` is set, confirm the channel is listed (`channels: []` monitors all public channels).
- Confirm you're using `@toad` (the bot's actual display name).
- Check the log for connection errors — bad tokens fail immediately.
- Confirm the Slack app is subscribed to `app_mention`, `message.channels`, `message.groups`.

### "Cannot connect to Slack"

- `app_token` must start with `xapp-`, `bot_token` with `xoxb-`.
- Socket Mode must be enabled, and the app-level token must have `connections:write`.

### <a name="auto_file-silently-downgraded"></a>`ticket.auto_file` silently downgraded to `false`

Toad validates this at config load: `ticket.auto_file` defaults to `true`, but a stock install has `issue_tracker.enabled: false`, which means the tracker can never create issues. Since `ticket.auto_file: true` at the config level is indistinguishable from the untouched default, toad can't tell "you explicitly asked for auto-file" from "you never configured a tracker" — so instead of a hard error at boot, it logs a warning and downgrades `ticket.auto_file` to `false` for that run:

```
ticket.auto_file is enabled but the issue tracker cannot create issues
(issue_tracker.enabled and issue_tracker.create_issues must both be true) —
downgrading ticket.auto_file to false
```

If you see this and expected auto-filing to work, set both `issue_tracker.enabled: true` and `issue_tracker.create_issues: true`, and make sure `issue_tracker.api_token` and `issue_tracker.team_id` are populated (required whenever `create_issues` is on).

### Investigation seems to be missing context

- Confirm the relevant repo is listed under `repos.list` and its `path` exists on disk.
- If you expect Sentry stack traces in the investigation, confirm `agent.mcp_servers.sentry` is configured and its `auth_token_env` resolves to a real token.
- `log.level: debug` surfaces the resolved repo, bot ID, and gate decision for each message.

### MCP tools return unauthorized

- Run `/toad mcp connect` in Slack to (re)issue your personal token, then `/toad mcp status` to confirm it's live.
- `logs` and `query` require the `dev` role — add your Slack user ID to `mcp.devs`. `ask` and `investigations` work with any valid connected token; `investigations` in particular is Biome's context bridge, so it's deliberately not gated on `mcp.devs` — Biome connects with a regular user token, not a dev one.

### State issues

- State DB: `~/.toad/state.db` — safe to delete; toad recreates it on startup.
- Logs: wherever `log.file` points (default `~/.toad/toad.log`).
- A crash mid-investigation is handled automatically on the next startup: stale runs are marked failed, and any digest opportunities stuck mid-investigation are picked back up. No manual cleanup needed.
