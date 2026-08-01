# Toad v2 — Investigation & Intake Agent (Design Spec)

**Date:** 2026-08-01 · **Status:** Approved direction, spec for implementation planning
**Companion artifact:** https://claude.ai/code/artifact/4d2ca314-ace8-4038-8ccc-3bb5dd79930b

## 1. Mission

Toad v2 drops all coding capability and becomes the **investigation and ticket-scoping
agent** in front of Biome. Pipeline: intake (Slack, incl. Sentry alerts arriving via
Slack) → triage → read-only codebase investigation with production evidence →
structured findings → Linear ticket → human sign-off → Biome executes.

Framing: **toad = intake confidence** (evidence-backed, well-scoped tickets),
**Biome = delivery confidence** (e2e recordings, change summaries). Humans decide
exactly twice: once on the ticket, once on the PR.

Toad also remains a conversational Slack presence: it answers codebase questions
(ribbit), and any answered thread can be escalated into the ticket flow.

## 2. Non-goals

- No code writing, no worktrees, no PRs, no PR review loops (Biome's job).
- No Sentry→toad direct webhook in MVP (Sentry alerts arrive via Slack).
- No AWS MCP in MVP (config slot exists; enablement is a YAML entry later).
- No replacement personality system (a lightweight outcome loop suffices).
- Toad does not push to repos; clones are read-only.

## 3. What is removed (v1 → v2)

| Removed | Notes |
|---|---|
| `internal/tadpole/` | Worktrees, runner, validation, shipping, pool |
| `internal/reviewer/` | PR watch/fix loop; `pr_watches` usage ends |
| `internal/personality/` | 22-trait system + learning + interpreter + store |
| `cmd/run.go` | `toad run` one-shot tadpole CLI |
| Personality HTTP API in `cmd/status.go` | Trait dashboard endpoints |
| `--max-turns` plumbing | CLI ≥2.1.162 removed the flag; strip from `agent.buildArgs`/`Resume`, drop `HitMaxTurns`/`error_max_turns` handling and resume-for-verdict paths that depend on it |

Kept but no longer load-bearing: `internal/vcs/` shrinks to read-only uses
(`GetSuggestedReviewers` for cc-mentions; `gh`/`glab` read commands allowed in ribbit).
`internal/update/`, `internal/tui/`, `internal/preflight/`, `internal/toadpath/` unchanged.

## 4. Architecture (v2)

### 4.1 New package: `internal/investigation`

Absorbs `cmd/investigation.go` (prompts, parsing, `extractFilePaths`) and owns the new
output contract. Replaces `digest.InvestigateResult` (digest currently owns the type,
which is why digest imports tadpole-adjacent code; the move breaks that knot).

```go
type Findings struct {
    Feasible        bool       // retained: "is this a code-change-shaped problem"
    Title           string
    Problem         string
    RootCause       string     // ALWAYS hypothesis-phrased
    Evidence        []Evidence // file:line refs, commit SHAs, Sentry issue IDs
    Scope           []string
    NonGoals        []string
    AcceptanceCriteria []string
    Confidence      float64
    Repo            string
    SentryIssueIDs  []string   // external corroboration
    IssueID         string     // existing Linear ref found in thread/ticket
    FilesFound      []string
    Reasoning       string     // free-text, Slack-postable
}
type Evidence struct { Kind string /* file|commit|sentry|thread */; Ref string; Note string }
```

Runner rules:
- **Sync before investigate**: call repo sync (extracted from `cmd/helpers.go:169
  syncRepos`) for the resolved repo before the agent run; staleness note logic from
  `ribbit.stalenessNote` is reused for the residual case.
- Read-only permissions (`agent.PermissionReadOnly`), all repo paths via `--add-dir`.
- Sentry MCP attached when configured (see 4.4); prompt instructs: pull full issue
  (stack trace, breadcrumbs, frequency) and Seer RCA when a Sentry ref is present.
- Two entry points mirror today's: `FromMessage` (triggered, ~10 turns/2 min budget →
  now timeout-only) and `FromOpportunity` (digest, 10 min timeout).

### 4.2 New package: `internal/ticket`

The single ticket author. Responsibilities:
- **Compose** the Linear ticket body from `investigation.Findings` (contract in §5).
- **Gate** (`Decide(f Findings) Decision`): `AutoFile` only when
  `len(f.SentryIssueIDs) > 0 && f.Confidence >= cfg.Ticket.AutoFileConfidence`
  (default 0.85) — external corroboration required, model confidence alone never
  auto-files. Everything else → `Propose` (Slack CTA).
- **File**: `tracker.CreateIssue` with extended opts; auto-filed tickets land in the
  Linear **Triage** state (config `ticket.triage_state_id`, optional — Linear defaults
  new issues to the team's default/triage state when unset).
- **Idempotency**: consult/write the `ticket_index` table (§4.5) keyed
  `sentry:<ID>` or `thread:<channel>:<threadTS>`. Existing entry → `PostComment` update
  on the existing issue instead of a new one; Slack reply links the existing ticket.

### 4.3 Slack flow re-target

- **CTA**: `toad_fix` action / `FixThisBlocks` ("Let Toad fix this") becomes
  `toad_ticket` / `TicketBlocks` ("Create Linear ticket"). Click → ticket creation from
  the thread's stored findings (not a tadpole). Optimistic replace text: ":ticket:
  Ticket created by <user>" (was ":hatching_chick: Tadpole spawned…").
- **Escalation**: the existing trigger-emoji-on-toad-reply path
  (`events.go:180` → `IsTadpoleRequest`) is renamed `IsTicketRequest` and routes to
  the escalation flow: run/refresh an investigation over the thread, then propose or
  file. Same for explicit asks ("make a ticket for this") — triage learns an
  `escalate` signal (see 4.6).
- **Sentry intake**: today `cmd/handlers.go:81` drops all bot messages after digest
  collection. v2 adds `intake.bot_allowlist` (config): messages whose `BotID` is
  allowlisted continue into triage. A Sentry detector extracts the Sentry issue
  URL/ID from `extractFullText` output (Block Kit + attachments already captured by
  `internal/slack/client.go:303`) and stores it on `IncomingMessage.SentryRefs`.
  Idempotency check happens **before** investigation (re-alert → comment on existing
  ticket, cheap).
- The latest findings per thread are persisted (see §4.5 `investigations` table) so
  CTA clicks and escalations reuse the investigation instead of re-running it
  (re-run only if older than 24h or the user asks).

### 4.4 Agent layer (`internal/agent`)

- `RunOpts` gains `MCPConfigPath string` (or inline servers) → emits
  `--mcp-config <path> --strict-mcp-config`, and allowed MCP tools are appended to
  `--allowedTools` (e.g. `mcp__sentry__*`). Config section `agent.mcp_servers`
  (name → command/url/env), rendered to a temp JSON file at startup.
- Remove `--max-turns` (see §3). `MaxTurns` field deleted from `RunOpts`; budgets are
  timeout-based (existing `Timeout` support).
- **Seat-throttle fallback**: provider detects rate-limit/usage-limit errors from the
  CLI and retries once with `ANTHROPIC_API_KEY` injected into the subprocess env
  (config `agent.fallback_api_key_env`, default unset = no fallback). Log loudly.

### 4.5 State (`internal/state`)

- New migration (v10): `ticket_index(external_key TEXT PRIMARY KEY, issue_id TEXT,
  issue_url TEXT, source TEXT, created_at DATETIME, last_seen_at DATETIME,
  last_status TEXT, status_checked_at DATETIME)`.
- New migration: `investigations(id TEXT PRIMARY KEY, thread_ts, channel, repo,
  findings_json, created_at)` — feeds thread reuse (§4.3) and the MCP
  `investigations` tool (§4.7).
- `runs` table reused for investigation runs: statuses become
  `starting|investigating|done|failed`; run IDs `invest-<ms>-<hex>`. Worktree columns
  ignored; `RecoverOnStartup` keeps phase 1 (mark stale runs failed) and drops the
  worktree scan.
- Findings for thread reuse (CTA clicks, escalations) come from the `investigations`
  table keyed by `thread_ts`; `thread_memory` is unchanged (ribbit conversational
  memory only).
- Drop writes to `pr_watches` and `personality_adjustments` (tables remain; no data
  migration needed).

### 4.6 Triage (`internal/triage`)

- Categories unchanged (`bug|feature|question|other`) — downstream meaning changes:
  bug/feature → investigation→ticket (was: tadpole).
- Prompt additions: Sentry-alert awareness (bot messages carrying stack traces are
  `bug` with the Sentry ref in `files_hint`/summary), and an `escalate: bool` output
  field for explicit "make this a ticket" intent in threads toad already answered.

### 4.7 Digest, MCP, outcome loop

- **Digest** re-target: `SpawnFunc` (tadpole.Task) is replaced by a
  `ProposeFunc(ctx, investigation.Findings, digest.Message) error` that routes into
  the same `internal/ticket` gate. Digest findings from chatter lack Sentry
  corroboration, so in practice they Propose (comment + CTA) — same code path,
  no special casing. Personality hook in `passesGuardrails`/`analyze` is deleted;
  `cfg.MinConfidence` is the floor (keep the existing 0.85 dry-run+comment special
  case as the plain default behavior).
- **MCP server**: keep `ask`/`logs`/`query` (drop `watches` with reviewer); add
  `investigations` tool: lookup by Linear ref or thread → returns findings JSON.
  This is Biome's enrichment bridge; tickets must remain self-sufficient.
- **Outcome loop** (lightweight): hourly poll over recent `ticket_index` rows via
  `tracker.GetIssueStatus`; record status transitions (accepted = left Triage/assigned;
  rejected = cancelled/duplicate). Surfaced in `toad status` and the daily log. No
  behavior adaptation in MVP beyond visibility.

### 4.8 Config additions

```yaml
intake:
  bot_allowlist: ["B0SENTRYBOT"]        # BotIDs allowed into triage (digest still sees all)
ticket:
  auto_file: true                        # master switch for the auto-file path
  auto_file_confidence: 0.85
  triage_state_id: ""                    # optional Linear workflow state UUID
agent:
  mcp_servers:
    sentry: { url: "https://mcp.sentry.dev/mcp", auth_token_env: "TOAD_SENTRY_MCP_TOKEN" }
  fallback_api_key_env: "TOAD_ANTHROPIC_API_KEY"
```
Follows the existing pattern: struct field on `Config`, defaults in `defaults()`,
env in `applyEnv`, checks in `Validate`.

### 4.9 Issuetracker extensions

- `CreateIssueOpts` gains `StateID string`, `Labels []string` (beyond bug/feature),
  and returns the created issue's **InternalID** (UUID) on the `IssueRef` (needed for
  efficient `PostComment`).
- `resolveTeamID`'s in-place mutation made thread-safe (sync.Once) since v2 calls
  Create from multiple goroutines.

## 5. Ticket contract (the Biome interface)

Every toad-authored ticket contains, in order: **Title** · **Problem** (symptoms,
who reported, frequency) · **Root cause — hypothesis** (with `file:line`, commit,
and Sentry links inline) · **Scope** (bullets) · **Non-goals** (bullets) ·
**Acceptance criteria** (checklist) · footer links (Slack permalink, Sentry issue,
`toad:investigation <id>`). Labels: `repo:<name>` + bug/feature label. A Biome agent
must be able to start from the ticket alone; the MCP `investigations` tool is
enrichment, never a dependency.

## 6. Deployment

Company host. `claude` CLI authenticated with a dedicated seat via `claude
setup-token` (`CLAUDE_CODE_OAUTH_TOKEN`); API-key fallback per §4.4. Repos as
read-only clones under the daemon user; `repos.sync_minutes` stays as the background
freshener, plus pre-investigation sync. State at `~/.toad/state.db` as today.

## 7. Testing & migration strategy

- Every new/changed package keeps the in-memory pattern (`:memory:` SQLite,
  `agent.MockProvider`). New golden tests: ticket body composition, gate decisions,
  Sentry ref extraction, idempotency upsert, MCP config rendering.
- `gofmt -l .`, `go vet ./...`, `go test ./...` green at the end of every phase.
- Work happens on branch `v2` in-place; `main` remains releasable v1 until cutover.
- CLAUDE.md is rewritten in the final phase to describe v2.

## 8. Risks & mitigations (carried from review)

1. Confidently-wrong RCA poisons Biome → hypothesis phrasing + evidence requirement +
   corroboration gate (§4.2), outcome visibility (§4.7).
2. Seat limits cap company intake → API fallback (§4.4).
3. Sentry alert noise → idempotency-before-investigation (§4.3), digest dedup retained.
4. Stale clones → sync-before-investigate (§4.1).
5. Biome⇄toad network path → tickets self-sufficient; MCP optional (§5).
