# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Git Policy

**NEVER create git commits directly.** All commits and releases must go through the `/release` skill/command. Do not run `git commit`, `git add`, or `git push` outside of that flow.

## Build & Test

```bash
go build ./...              # Build all packages
go test ./...               # Run all tests
go test ./internal/state/   # Run tests for a single package
go test ./... -run TestFoo  # Run a specific test
go vet ./...                # Lint
gofmt -l .                  # Check formatting (must be clean before committing)
```

**Before committing:** Always run `gofmt -l .` and fix any output with `gofmt -w <file>`. CI enforces `gofmt` formatting and will fail on unformatted code.

No external test infrastructure needed — tests use in-memory SQLite (`:memory:`) and same-package access to unexported functions.

## Architecture

Toad is a Go daemon that monitors Slack channels, triages messages with Claude Haiku, and either answers questions (ribbit) or investigates bug/feature reports and files (or proposes) a tracking ticket in Linear. Toad no longer writes code, opens PRs, or creates git worktrees — every agent run is read-only.

**Message flow:** Slack event (including allowlisted bots, e.g. Sentry) -> triage (Haiku, ~1s) -> route by category:
- `question`/non-report intents -> responder reply (`internal/responder`: one conversational agent that leans on prior findings and reads code only when needed; ribbit is its Slack adapter)
- Thread follow-ups where toad already answered (thread memory or a stored investigation) always converse via the responder — only a first-touch bug/feature REPORT (triage `intent`) enters the investigate-and-gate flow
- `bug`/`feature` -> read-only investigation -> ticket gate: auto-file iff Sentry-corroborated + confident + feasible, else propose via a "Create Linear ticket" CTA button
- A triage `Escalate` result, or a human clicking the CTA button on a toad message, files a ticket directly — bypassing the auto-file gate, since that's already an explicit sign-off
- Toad is triggered by an `@mention`, a configured keyword (`triggers.keywords` in config), or the CTA button — there is no reaction-based trigger
- Passive (Toad King digest): batches untriggered messages, analyzes with Haiku, and applies the same investigate-then-gate flow to auto-file or propose a ticket
- Linear agent (polled): when connected as a Linear app (`toad linear connect`), a poller (`internal/linearagent`, `linear_agent.poll_seconds`, default 15s) picks up agent sessions created by @-mentioning or delegating to toad in Linear; the OAuth app must have webhooks ENABLED with agent session events (a dead/dummy endpoint is fine — Linear won't create agent sessions without webhooks on, but toad only ever polls), acks with a `thought` activity, runs the responder (conversational agent with prior findings; digs into code only when the ask needs it) and can apply safe ticket edits (title/description/comment via `issuetracker.UpdateIssue` — the only issue mutation toad has; status/assignee/labels are inexpressible), and replies with a `response` activity — never filing tickets or mutating anything beyond that safe subset; delivery is at-least-once — a crash between response and handled-record write repeats the answer rather than losing it

**Investigation lifecycle:** thread claim (`state.Manager.Claim`/`ClaimScoped`) -> repo re-sync (best-effort; failure appends a staleness caveat rather than blocking) -> `investigation.Runner.Run` (read-only Claude CLI subprocess, `--add-dir` for every configured repo) -> `ParseFindings` -> `ticket.Engine.Decide` (auto-file vs propose) -> `FileOrUpdate` (idempotent, keyed by Sentry issue or Slack thread) -> claim released. Findings are always persisted to the `investigations` table (even when not fed to the ticket engine) so a later CTA click or MCP `investigations` lookup can reuse them instead of re-investigating.

**Multi-repo routing:** Config supports multiple repos via `repos:` list. At startup, `BuildProfiles` auto-detects each repo's stack/module from manifest files. Triage and digest prompts include repo profiles so Haiku can suggest a `"repo"` name. The `Resolver` verifies with file-existence stat checks (`resolver.go`), falling back to triage hint, then `primary` repo. Repos are synced periodically (`repos.sync_minutes`) and are read-only — toad never commits, pushes, or opens PRs against them.

**Key patterns:**
- **Write-through state**: `state.Manager` caches runs in-memory maps, writes through to SQLite on every mutation. `NewManager()` is in-memory only (tests), `NewPersistentManager(db)` hydrates from DB.
- **Claim/Unclaim**: thread-scoped reservation prevents duplicate concurrent investigations or ticket requests on the same Slack thread. `Claim` (empty scope) is exclusive — it fails if any claim, scoped or not, already exists on the thread. `ClaimScoped(threadTS, scope)` lets independent flows (e.g. a digest investigation) coexist on the same thread under different scopes while still blocking a second claim on the same scope. Every investigate-and-file path releases its claim via `defer`, on both success and failure.
- **Ticket idempotency**: `ticket.Engine` guards `FileOrUpdate` with a per-external-key mutex (keyed on `sentry:<issue-id>` when Sentry-corroborated, else `thread:<channel>:<ts>`) so concurrent deliveries (duplicate Sentry webhooks, a digest and a Slack thread racing the same bug) can't file duplicate tickets. `state.TicketIndexEntry` (table `ticket_index`) maps that key to the filed issue; a repeat observation posts a "already tracked" comment and bumps `last_seen_at` instead of filing again.
- **Concurrency**: separate semaphores for ribbits (`MaxConcurrent*3`) and investigations (`MaxConcurrent`). Each message is handled in its own goroutine.
- **Channel access**: Bot auto-joins all public channels on startup. If `channels` config is empty, no filtering — events from all joined channels are processed.

**Packages:**
- `cmd/` — Cobra commands: `toad` (daemon), `toad init` (setup), `toad status` (dashboard), `toad restart`, `toad update`, `toad version`. Daemon logic split into `root.go` (bootstrap), `handlers.go` (message routing, bot allowlist), `ticketflow.go` (the investigate-and-file flow: triggered investigations, CTA/escalation ticket requests, digest hooks), `outcomes.go` (hourly Linear status poller), `helpers.go` (utilities)
- `internal/slack/` — Socket Mode client, event routing, dedup, reply tracking
- `internal/triage/` — Haiku classification (actionable, category, size, keywords, files, escalate, intent)
- `internal/responder/` — conversational core (Conversation → JSON envelope: reply, optional safe ticket_update, did_investigate/findings_summary)
- `internal/ribbit/` — Slack adapter over the responder (thread memory, staleness note, linked-ticket enrichment)
- `internal/investigation/` — read-only investigation runner: builds the prompt, invokes the agent with `PermissionReadOnly`, parses the `Findings` verdict (feasibility, root cause, evidence, scope, acceptance criteria, confidence)
- `internal/ticket/` — Ticket Engine: auto-file/propose decision gate, idempotent filing via `ticket_index`, Linear ticket body composition
- `internal/state/` — in-memory + SQLite state, crash recovery, `ticket_index` and `investigations` tables
- `internal/digest/` — Toad King: batch messages, Haiku analysis, guardrails. Split into `digest.go` (engine), `analyze.go` (LLM analysis), `chunking.go` (batching), `guardrails.go` (filtering). Proposes/files tickets through the same gate as the Slack-thread flow — never spawns anything
- `internal/config/` — YAML config loading with cascading defaults, multi-repo profiles and resolver
- `internal/agent/` — Agent CLI abstraction (Claude Code subprocess), MCP config writer, API-key fallback, provider interface for swappable backends
- `internal/vcs/` — VCS provider abstraction (GitHub via `gh`, GitLab via `glab`); the `Provider` interface is now just CLI availability checks (`Check`) and suggested-reviewer lookups (`GetSuggestedReviewers`) — PR/CI status lookups happen directly via ribbit/investigation's own Bash-tool `gh`/`glab` invocations, not through this package
- `internal/issuetracker/` — Linear integration: issue creation/lookup, detail+comment fetching, assignee gating, crossposting
- `internal/linearauth/` — Linear OAuth app identity: token store, exchange/refresh, `AuthSource` (prefers the connected app token, falls back to the personal API key)
- `internal/linearagent/` — Linear agent-session flow: GraphQL session/activity client, work-detection poller (`Poller`), and the processor (`Processor`) that acks with a thought, responds via the responder (prior findings + read-only code tools), applies safe ticket edits on explicit request, replies with a response activity — never files tickets or mutates issues beyond that safe subset
- `internal/mcp/` — Model Context Protocol server: `ask`, `logs`, `investigations`, `query` tools with token auth
- `internal/tui/` — Shared huh theme for init wizard
- `internal/update/` — Auto-update mechanism via Homebrew
- `internal/log/` — Structured logging setup (slog with optional file output)
- `internal/preflight/` — Pre-run validation checks
- `internal/toadpath/` — Home directory resolution (`~/.toad` or `$TOAD_HOME`)

## Important Details

- Claude is invoked as a CLI subprocess (`claude --print --output-format json`), not via API. There is no `--max-turns` flag in this version's CLI, and toad does not pass one.
- Investigations and the responder both run with `--allowedTools Read,Glob,Grep` plus any per-run `Bash(<cmd>:*)` and `mcp__<server>__*` entries; ribbit and Linear sessions use the same read-only permission mode with a VCS-specific Bash allowlist (`gh pr view/list/diff/checks`, `gh issue view/list`, `gh search` on GitHub; the `glab` equivalents on GitLab)
- Triage now emits `intent` (report|question|action|chatter) — routing key for message handling (report enters investigate-and-gate; others go to the responder)
- When `agent.mcp_servers` is configured (e.g. a `sentry` entry), toad writes an MCP config JSON and passes `--mcp-config <path> --strict-mcp-config`; HTTP-type servers get a bearer token from `auth_token_env` if set, command-type servers run as a subprocess
- `agent.fallback_api_key_env` names an env var holding an Anthropic API key: if a run fails because the CLI's subscription seat is throttled (usage/rate limit), toad retries once with `ANTHROPIC_API_KEY` set from that var
- SQLite uses `modernc.org/sqlite` (pure Go, no CGo) with WAL mode; `dbRetry` wrapper retries on SQLITE_BUSY; current schema is version 13 — v10 added `ticket_index` (external key -> filed issue, dedup/status tracking) and `investigations` (findings by ID, looked up by thread or by ticket), v11 added MCP token hashing (`mcp_tokens.expires_at`, forced rotation) and Linear state-type tracking (`ticket_index.last_state_type`), v12 added `metrics_hourly` (dashboard trend sparklines) and `investigations.duration_ms`, v13 added `agent_sessions` (Linear agent-session dedup/handled-state)
- Config loads: defaults -> `~/.toad/config.yaml` -> `.toad.yaml` -> env vars
- All Slack tokens come from env vars (`TOAD_SLACK_APP_TOKEN`, `TOAD_SLACK_BOT_TOKEN`) or `.toad.yaml`
- Slack API calls have a 30-second HTTP timeout to prevent hung goroutines
- State DB at `~/.toad/state.db`
- On startup, `RecoverOnStartup` marks runs left in an active state (e.g. `investigating`) by a previous crash as failed, and returns any digest opportunities stuck mid-investigation so the digest engine can resume them
- Ribbit retries once on empty result
- Digest has a single mode (enabled/disabled); its confidence floor is `digest.min_confidence` (default 0.8 — passing it only proposes a human-gated CTA, or auto-files when Sentry-corroborated)
- Per-channel digest opt-out is runtime state, not config: the dashboard writes `digest_channel:<id>` = `"off"` rows to the shared `settings` table (`state.DB.SetDigestChannelEnabled`/`DisabledDigestChannels`); the daemon's `digestChannelGate` (`cmd/digestgate.go`) polls it at most every 60s and fails open on a DB error, and the daemon publishes its channel inventory to the `known_channels` setting (`cmd/channels.go`) so the dashboard — which has no Slack connection of its own — can render the toggle list
- The "Create Linear ticket" CTA button appears on proposed (non-auto-filed) findings — both from a triggered Slack-thread investigation and from a digest proposal
- Digest triage judgment (vs. blunt channel muting): each collected `digest.Message` carries `IsMonitoringBot` (set from the bot allowlist at Collect time), rendered as a `[user]`/`[bot]`/`[monitoring]` prefix in the analysis prompt so operational automation chatter is never treated as an opportunity; the prompt also applies maximum conservatism in client/customer/RFP-looking channels; and `state.DB.HasRecentOpportunity` suppresses within 24h on any similar row, and within 7 days when that row reached a completed human-visible outcome (proposed or genuinely declined, excluding investigation-error retries)
- An hourly outcome poller (`cmd/outcomes.go`) checks `ticket_index` entries against Linear for status changes and logs transitions — visibility only, it never changes toad's filing behavior
- Linear ticket comments (up to 20) are fetched alongside issue details for investigation context
- Toad never comments on tickets delegated to an agent — delegation marks the intake→delivery handoff
- GitHub Actions: `tag.yml` (manual version tagging with auto-bump) triggers `release.yml` (GoReleaser + Docker)
- `release_notes.channel` (default empty = disabled): when set, toad posts AI-generated release notes to that Slack channel on startup, exactly once per version, tracked via the `last_announced_version` setting (`cmd/releasenotes.go`)
- Linear identity: `toad linear connect` runs the actor=app OAuth flow (client credentials from `TOAD_LINEAR_CLIENT_ID`/`TOAD_LINEAR_CLIENT_SECRET`, callback on `localhost:9482`) and stores the app token in the `settings` table (`linear_oauth_token`); `LinearTracker` prefers it (`Bearer`) over the personal `TOAD_LINEAR_API_TOKEN` (raw header), refreshing once on 401 with API-key fallback, so tickets/comments come from the app identity when connected
- `agent.ProseStyleRules`/`ProseStyleRulesSlim` (`internal/agent/style.go`) is a shared ASD-STE100 (Simplified Technical English) writing-style block folded into every user-facing generation prompt — ribbit replies, investigation findings prose, release notes, and (the slim variant) digest summaries
