# Toad v2 — Investigation & Intake Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Strip toad's coding pipeline and rebuild it as an investigation + Linear-ticket agent per `docs/superpowers/specs/2026-08-01-toad-v2-intake-agent-design.md`.

**Architecture:** Contracts-first phasing. Phase 1 lands all shared types, config, and DB migrations. Phase 2 excises tadpole/reviewer/personality serially (one agent — it touches everything). Phase 3 fans out three parallel workstreams over **disjoint file sets**. Phase 4 wires the daemon serially. Phase 5 validates and rewrites docs.

**Tech Stack:** Go 1.25, modernc.org/sqlite, slack-go socketmode, Claude Code CLI subprocess, Linear GraphQL.

## Global Constraints

- **NEVER `git commit` / `git push` directly** (repo CLAUDE.md). Tasks end at "verify green"; the human operator commits via `/release` at each phase checkpoint. Work happens on branch `v2` (operator creates it).
- Before any checkpoint: `gofmt -l .` (must print nothing), `go vet ./...`, `go test ./...` all green.
- Category strings stay exactly `"bug" | "feature" | "question" | "other"` (no enum exists; do not invent one).
- Confidence floors: auto-file default `0.85`; digest floor default `0.95` (`cfg.Digest.MinConfidence`).
- All new tables via numbered migration entries in `internal/state/db.go:208-221` (next free version: 10).
- No new external Go dependencies.
- Tests use `:memory:` SQLite (`state.OpenDBAt(":memory:")` pattern) and `agent.MockProvider` (`internal/agent/mock.go:9`).
- **Parallel-agent rule:** an agent may only edit files its task lists. If a needed change falls outside that set, stop and report instead of editing.

## File ownership map (Phase 3 conflict avoidance)

| Workstream | Owns (exclusive during Phase 3) |
|---|---|
| W-A agent/investigation | `internal/agent/*`, `internal/investigation/*` |
| W-B ticket | `internal/ticket/*`, `internal/issuetracker/*` |
| W-C slack/triage | `internal/slack/*`, `internal/triage/*` |
| (frozen in Phase 3) | `cmd/*`, `internal/digest/*`, `internal/state/*`, `internal/config/*`, `internal/mcp/*` |

---

## Phase 0 — Baseline (operator)

### Task 0: Branch and baseline

- [ ] Operator: `git checkout -b v2` from current `main`.
- [ ] Run `go build ./... && go test ./... && gofmt -l .` — record baseline green.

---

## Phase 1 — Contracts first (SERIAL, one agent)

### Task 1: `internal/investigation` package — types + parser move

**Files:**
- Create: `internal/investigation/investigation.go`, `internal/investigation/parse.go`, `internal/investigation/parse_test.go`
- Modify: none yet (cmd/investigation.go is retired in Task 11; digest switches in Task 4)

**Interfaces:**
- Produces (later tasks rely on these exact names):

```go
package investigation

type Evidence struct {
    Kind string `json:"kind"` // "file" | "commit" | "sentry" | "thread"
    Ref  string `json:"ref"`  // "billing/export/aggregate.py:118", "a41c9f2", "BILLING-2291"
    Note string `json:"note"`
}

type Findings struct {
    Feasible           bool       `json:"feasible"`
    Title              string     `json:"title"`
    Problem            string     `json:"problem"`
    RootCause          string     `json:"root_cause"` // hypothesis-phrased
    Evidence           []Evidence `json:"evidence"`
    Scope              []string   `json:"scope"`
    NonGoals           []string   `json:"non_goals"`
    AcceptanceCriteria []string   `json:"acceptance_criteria"`
    Confidence         float64    `json:"confidence"`
    Repo               string     `json:"repo"`
    SentryIssueIDs     []string   `json:"sentry_issue_ids"`
    IssueID            string     `json:"issue_id"`    // existing Linear ref, if any
    FilesFound         []string   `json:"files_found"` // from extractFilePaths
    Reasoning          string     `json:"reasoning"`   // Slack-postable prose
}

func ParseFindings(resultText string) (*Findings, error)
```

- [ ] **Step 1:** Write `parse_test.go` with three cases: clean JSON object; JSON wrapped in ```json fences; JSON embedded after prose (scan from `{"feasible"`). Assert all fields round-trip and that a missing `confidence` yields `0`. Include one malformed-input case asserting a non-nil error containing `no valid JSON`.
- [ ] **Step 2:** `go test ./internal/investigation/` — expect FAIL (package empty).
- [ ] **Step 3:** Implement `investigation.go` (types above) and `parse.go`. Port the three-strategy parser from `cmd/investigation.go:301` (`parseInvestigateResult`) — prefix scan for `{"feasible"` → `stripCodeFences` → last-`"feasible"` backscan — but target `Findings` and drop the `hitMaxTurns` parameter entirely. Copy `stripCodeFences` (`cmd/helpers.go:113`) and `findMatchingBrace` (`cmd/helpers.go:133`) into `parse.go` as unexported helpers (cmd versions are deleted in Task 11). Also port `extractFilePaths` + `knownExts` (`cmd/investigation.go:371-427`) into `parse.go` as `ExtractFilePaths(text string) []string`.
- [ ] **Step 4:** `go test ./internal/investigation/` — PASS. `go build ./...` still green (nothing imports it yet).

### Task 2: Config additions

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (extend existing)

**Interfaces — produces exactly:**

```go
type IntakeConfig struct {
    BotAllowlist []string `yaml:"bot_allowlist"` // Slack BotIDs allowed into triage
}
type TicketConfig struct {
    AutoFile           bool    `yaml:"auto_file"`
    AutoFileConfidence float64 `yaml:"auto_file_confidence"` // default 0.85
    TriageStateID      string  `yaml:"triage_state_id"`
}
type MCPServerConfig struct {
    URL          string `yaml:"url"`
    Command      string `yaml:"command"`
    AuthTokenEnv string `yaml:"auth_token_env"`
}
// AgentConfig gains:
//   MCPServers        map[string]MCPServerConfig `yaml:"mcp_servers"`
//   FallbackAPIKeyEnv string                     `yaml:"fallback_api_key_env"`
// Config gains: Intake IntakeConfig `yaml:"intake"`; Ticket TicketConfig `yaml:"ticket"`
```

- [ ] **Step 1:** Failing test: YAML overlay setting `intake.bot_allowlist`, `ticket.auto_file_confidence: 0.9`, `agent.mcp_servers.sentry.url`, `agent.fallback_api_key_env` loads into the struct; and `defaults()` yields `Ticket.AutoFile == true`, `Ticket.AutoFileConfidence == 0.85`.
- [ ] **Step 2:** Run — FAIL (unknown fields).
- [ ] **Step 3:** Add fields to `Config` (`config.go:14-27`), `AgentConfig` (`config.go:126`); defaults in `defaults()` (`config.go:152-219`): `Ticket: TicketConfig{AutoFile: true, AutoFileConfidence: 0.85}`. In `Validate` (`config.go:291`): if any `MCPServerConfig` has both `URL` and `Command` empty → error `agent.mcp_servers.<name>: url or command required`.
- [ ] **Step 4:** `go test ./internal/config/` — PASS.

### Task 3: State migrations + accessors

**Files:**
- Modify: `internal/state/db.go`
- Test: `internal/state/db_test.go` (extend existing)

**Interfaces — produces exactly:**

```go
type TicketIndexEntry struct {
    ExternalKey string    // "sentry:BILLING-2291" | "thread:C123:1722500000.000100"
    IssueID     string    // "SCL-1482"
    IssueURL    string
    Source      string    // "auto" | "cta" | "digest" | "escalation"
    CreatedAt   time.Time
    LastSeenAt  time.Time
    LastStatus  string
    StatusCheckedAt time.Time
}
func (db *DB) UpsertTicketIndex(e *TicketIndexEntry) error         // ON CONFLICT updates last_seen_at
func (db *DB) GetTicketIndex(externalKey string) (*TicketIndexEntry, error) // nil, nil when absent
func (db *DB) RecentTicketIndex(limit int) ([]*TicketIndexEntry, error)
func (db *DB) UpdateTicketStatus(externalKey, status string) error

type InvestigationRecord struct {
    ID string; ThreadTS string; Channel string; Repo string
    FindingsJSON string; CreatedAt time.Time
}
func (db *DB) SaveInvestigation(rec *InvestigationRecord) error
func (db *DB) GetInvestigationByThread(threadTS string) (*InvestigationRecord, error) // newest, nil,nil absent
func (db *DB) GetInvestigation(id string) (*InvestigationRecord, error)
func (db *DB) FindInvestigationByTicket(issueID string) (*InvestigationRecord, error) // join via ticket_index
```

- [ ] **Step 1:** Failing tests: fresh `:memory:` DB migrates to version 10; `UpsertTicketIndex` twice with same key keeps one row and bumps `last_seen_at`; `GetTicketIndex` unknown key → `(nil, nil)`; `SaveInvestigation` + `GetInvestigationByThread` round-trip; `FindInvestigationByTicket` resolves through a ticket_index row.
- [ ] **Step 2:** Run — FAIL.
- [ ] **Step 3:** Append migration version 10 to the list at `db.go:208-221`:

```sql
CREATE TABLE IF NOT EXISTS ticket_index (
  external_key TEXT PRIMARY KEY, issue_id TEXT NOT NULL, issue_url TEXT,
  source TEXT DEFAULT '', created_at DATETIME NOT NULL, last_seen_at DATETIME NOT NULL,
  last_status TEXT DEFAULT '', status_checked_at DATETIME);
CREATE TABLE IF NOT EXISTS investigations (
  id TEXT PRIMARY KEY, thread_ts TEXT, channel TEXT, repo TEXT,
  findings_json TEXT NOT NULL, created_at DATETIME NOT NULL);
CREATE INDEX IF NOT EXISTS idx_invest_thread ON investigations(thread_ts);
```

  Implement accessors with the existing `dbRetry`/`dbTimeout` pattern (`db.go:19-40`). `FindInvestigationByTicket`: `SELECT i.* FROM investigations i JOIN ticket_index t ON t.external_key LIKE 'thread:%' AND i.thread_ts = substr(t.external_key, instr(substr(t.external_key,8), ':') + 8) WHERE t.issue_id = ?` is fragile — instead add column `investigation_id TEXT DEFAULT ''` to `ticket_index` in the same migration and join on it directly. Extend `TicketIndexEntry` with `InvestigationID string`.
- [ ] **Step 4:** `go test ./internal/state/` — PASS. Also verify migration is idempotent by opening the same file-backed temp DB twice in a test.

**Phase 1 checkpoint:** full suite green → operator `/release` (commit "v2 phase 1: contracts").

---

## Phase 2 — Excision (SERIAL, one agent; large mechanical change)

### Task 4: Decouple digest from tadpole + personality

**Files:**
- Modify: `internal/digest/digest.go`, `internal/digest/analyze.go`, `internal/digest/guardrails.go`, `internal/digest/*_test.go`

**Interfaces — produces exactly:**

```go
// replaces SpawnFunc (digest.go:74)
type ProposeFunc func(ctx context.Context, f investigation.Findings, msg Message) error
// InvestigateFunc (digest.go:71) now returns *investigation.Findings:
type InvestigateFunc func(ctx context.Context, opp Opportunity, msg Message, tickets []TicketContext) (*investigation.Findings, error)
```

- [ ] **Step 1:** Delete `InvestigateResult` (`digest.go:62-68`); import `internal/investigation`; replace every use with `investigation.Findings` (`TaskSpec` → compose from `Findings.Reasoning`; keep field mapping: `IssueID`→`IssueID`, `FilesFound`→`FilesFound`).
- [ ] **Step 2:** Remove `tadpole` import (`digest.go:19`): replace `SpawnFunc`/`EngineOpts.Spawn` with `Propose ProposeFunc`; in `processOpportunities` (`digest.go:779-819`) and `ResumeInvestigations` (`digest.go:392`) replace `tadpole.Task` construction + `e.spawn` with `e.propose(ctx, *findings, msg)`. Keep claim/unclaim, dedup, and gate logic untouched. Keep the `:crown:` notice but it is now sent by the propose implementation (Phase 4), so delete the direct `e.notify` spawn-announcement at `digest.go:793-803` and the `hatching_chick` react at `:817`.
- [ ] **Step 3:** Remove `personality` import from all three files: in `guardrails.go:15-25` and `analyze.go:106-122` the floor becomes exactly `minConf := e.cfg.MinConfidence; if minConf <= 0 { minConf = 0.95 }; if e.cfg.DryRun && e.cfg.CommentInvestigation && minConf > 0.85 { minConf = 0.85 }`. Delete `EngineOpts.Personality` (`digest.go:185`) and field (`digest.go:144`).
- [ ] **Step 4:** Update digest tests: replace mock spawn funcs with mock propose funcs; run `go test ./internal/digest/` — PASS. (`go build ./...` will still fail at `cmd/` — expected until Task 6.)

### Task 5: Decouple ribbit from personality

**Files:**
- Modify: `internal/ribbit/ribbit.go`, `internal/ribbit/ribbit_test.go`

- [ ] **Step 1:** Remove the `personality` import and field (`ribbit.go:15,35`); change ctor to `New(agentProvider agent.Provider, cfg *config.Config, tracker issuetracker.Tracker) *Engine` (drop `mgr`). Delete the personality prompt-fragment block (`ribbit.go:140-149`); `maxTurns` handling is deleted in Task 7, so for now keep the literal `10` local variable.
- [ ] **Step 2:** Fix tests; `go test ./internal/ribbit/` — PASS.

### Task 6: Delete packages and rewire `cmd/`

**Files:**
- Delete: `internal/tadpole/`, `internal/reviewer/`, `internal/personality/`, `cmd/run.go`, `cmd/investigation.go`
- Modify: `cmd/root.go`, `cmd/handlers.go`, `cmd/helpers.go`, `cmd/status.go`, `internal/state/recovery.go`, `internal/state/state.go`

- [ ] **Step 1:** `git rm -r internal/tadpole internal/reviewer internal/personality cmd/run.go` (operator note: staging via git rm is acceptable; the commit still goes through /release).
- [ ] **Step 2:** `cmd/root.go` (anchors from the v1 map): delete construction/wiring at `:138-161` (personality), `:183-214` (tadpole runner/pool, reviewer watcher, OnShip, OnPersonalityOutcome), `:386-399` (personality feedback), `:439` (prWatcher.Run), `:449` (CleanupStaleWorktrees), `:482-493` keeps only `messageWg.Wait()` (no pool shutdown). Semaphores (`:176-177`) become `investigateSem := make(chan struct{}, cfg.Limits.MaxConcurrent)` and `ribbitSem` unchanged. Digest opts: replace `Spawn:` with `Propose: nil // wired in Phase 4 Task 12`, delete `Personality:` line. `ribbit.New` call drops the personality arg.
- [ ] **Step 3:** `cmd/handlers.go`: delete the retry-tadpole path (`:166-213`), the tadpole route (`:257-368` — the whole `(a)` branch), and `handleTadpoleRequest` (`:472-590`). `msg.IsTadpoleRequest` branch at `:43` becomes `if msg.IsTicketRequest { handleTicketRequest(...) }` — **stub** `handleTicketRequest` for now: reply ":ticket: Ticket flow lands in phase 4." (compiles; replaced in Task 13). All routes fall through to ribbit. Keep the CTA attach sites but they now call `islack.TicketBlocks` — until W-C lands, temporarily keep `FixThisBlocks` compiling by leaving call sites unchanged (W-C renames both ends atomically in Task 10).
- [ ] **Step 4:** `cmd/status.go`: remove personality import and the endpoints at `:52-72,:117-125,:145,:153,:164`. `cmd/helpers.go`: delete `isRetryIntent`/`hasFailedTadpole` (`:69,:89`); **keep** `syncRepos` (`:169`), `enrichWithIssueDetails` (`:21`), `buildVCSResolver` (`:228`). Extract from `syncRepos` a new exported helper `SyncRepoNow(ctx context.Context, repo config.RepoConfig) error` (same fetch/reset logic, single repo) — W-A consumes it via a small interface, see Task 9.
- [ ] **Step 5:** `internal/state/recovery.go`: delete phase 2 worktree scan (`:53-82`) and `removeWorktreeDir` (`:104`); `RecoverResult.OrphanWorktrees` field removed. `state.go`: statuses comment (`state.go:13`) now `starting|investigating|done|failed`; no schema change needed.
- [ ] **Step 6:** `go build ./...` green; `go test ./...` green; `gofmt -l .` clean.

### Task 7: Agent layer — remove `--max-turns`

**Files:**
- Modify: `internal/agent/provider.go`, `internal/agent/claude.go`, `internal/agent/claude_test.go`, `internal/agent/mock.go`, plus the three surviving callers: `internal/triage/triage.go:117`, `internal/ribbit/ribbit.go`, `internal/digest/analyze.go:126`

- [ ] **Step 1:** Failing test update: `claude_test.go` asserts `--max-turns` never appears in `buildArgs` output for any opts.
- [ ] **Step 2:** Delete `RunOpts.MaxTurns` (`provider.go:25`), the emit at `claude.go:132-134`, the hardcoded `"--max-turns","1"` in `Resume` (`claude.go:90`), `RunResult.HitMaxTurns` + `error_max_turns` mapping (`claude.go:188`). Remove `MaxTurns` from all surviving callers (triage keeps `Timeout: 30*time.Second`; ribbit deletes maxTurns/retry-bump — retry-on-empty stays but without `+5`; digest analyze relies on timeout only). Delete `HitMaxTurns` branches in ribbit (`ribbit.go:196-199`).
- [ ] **Step 3:** `go test ./... && go vet ./...` green.

**Phase 2 checkpoint:** daemon builds and runs as "ribbit-only" toad (all triage routes answer in thread; buttons still say v1 text). Operator smoke-tests against a test channel, then `/release`.

---

## Phase 3 — Parallel workstreams (3 agents CONCURRENTLY; file ownership map applies)

### W-A · Task 8: MCP client support in `internal/agent`

**Files:**
- Create: `internal/agent/mcp.go`, `internal/agent/mcp_test.go`
- Modify: `internal/agent/provider.go`, `internal/agent/claude.go`, `internal/agent/claude_test.go`

**Interfaces — produces exactly:**

```go
// RunOpts gains:
//   MCPConfigPath   string   // emits --mcp-config <path> --strict-mcp-config
//   AllowedMCPTools []string // appended to --allowedTools, e.g. "mcp__sentry__*"
func WriteMCPConfig(dir string, servers map[string]config.MCPServerConfig) (path string, err error)
// Renders {"mcpServers": {name: {"type":"http","url":...,"headers":{"Authorization":"Bearer $TOKEN"}}}}
// or {"command": ...} for Command servers; token read from os.Getenv(s.AuthTokenEnv) at render time.
// Returns "" and nil error when servers is empty.
```

- [ ] **Step 1:** Failing tests: `WriteMCPConfig` renders an http server with bearer header from env; empty map → `("", nil)`; `buildArgs` with `MCPConfigPath` set includes `--mcp-config <path>` and `--strict-mcp-config`; `AllowedMCPTools` merge into the `--allowedTools` value for `PermissionReadOnly` (`Read,Glob,Grep,...,mcp__sentry__*`).
- [ ] **Step 2:** Run — FAIL. **Step 3:** Implement (extend the switch at `claude.go:137-146`; append MCP tools inside the `PermissionReadOnly` case after bash entries). **Step 4:** PASS.

### W-A · Task 9: Investigation runner

**Files:**
- Create: `internal/investigation/runner.go`, `internal/investigation/prompt.go`, `internal/investigation/runner_test.go`

**Interfaces — produces exactly:**

```go
type RepoSyncer func(ctx context.Context, repo config.RepoConfig) error // cmd wires SyncRepoNow
type Runner struct { /* agent agent.Provider; model string; mcpConfigPath string;
    allowedMCPTools []string; sync RepoSyncer; repoPaths map[string]string */ }
func NewRunner(p agent.Provider, model, mcpConfigPath string, allowedMCPTools []string,
    sync RepoSyncer, repoPaths map[string]string) *Runner
type Request struct {
    Text        string   // primary request / task description
    ThreadContext []string
    Category    string; Confidence float64; Summary string
    ChannelName string; Keywords []string; FilesHint []string
    SentryRefs  []string // from intake detection; may be empty
    TicketContext string // pre-formatted <linked_tickets> block or ""
    Repo        *config.RepoConfig // resolved; required
    Timeout     time.Duration      // FromMessage: 4m; FromOpportunity: 10m
}
func (r *Runner) Run(ctx context.Context, req Request) (*Findings, error)
```

- [ ] **Step 1:** Failing tests with `agent.MockProvider`: (a) `Run` calls the syncer before the agent; (b) `RunOpts` carries `PermissionReadOnly`, repo `WorkDir`, all `repoPaths` as `AdditionalDirs`, the runner's `MCPConfigPath`/`AllowedMCPTools`, and `req.Timeout`; (c) prompt contains the Sentry instruction block only when `SentryRefs` non-empty; (d) mock returning a valid findings JSON yields parsed `*Findings`.
- [ ] **Step 2:** Run — FAIL. **Step 3:** Implement. `prompt.go` merges the two v1 prompts (`investigatePrompt` cmd/investigation.go:54-92 and `triggeredInvestigatePrompt` :199-240 — both deleted in Phase 2; recreate from this spec) into one template demanding terminal JSON matching `Findings` exactly, with sections: role ("staff engineer investigating an intake report; produce evidence or say infeasible"), inputs (message, thread context, triage hints, ticket context, sentry refs), rules ("root_cause MUST be phrased as a hypothesis and cite evidence refs"; "every evidence file ref is repo-relative path:line"; "acceptance_criteria are independently checkable"; "scope lists what to change, non_goals what NOT to"; "if a Sentry ref is present, use the sentry MCP tools to pull the full issue and Seer analysis before concluding"), and the JSON schema with an inline example. **Step 4:** PASS.

### W-A · Task 10: API-key fallback in ClaudeProvider

**Files:**
- Modify: `internal/agent/claude.go`, `internal/agent/provider.go`, `internal/agent/claude_test.go`

**Interfaces:** `ClaudeProvider` gains `FallbackAPIKeyEnv string` (set from `cfg.Agent.FallbackAPIKeyEnv` in `NewProvider`, whose signature becomes `NewProvider(platform, fallbackEnv string)`).

- [ ] **Step 1:** Failing test: a run whose stderr/result matches `(?i)(usage limit|rate limit|out of extra usage)` and `FallbackAPIKeyEnv` set + env var present → command re-executed once with `ANTHROPIC_API_KEY=<value>` in `cmd.Env`; without the env var → original error returned unchanged. (Testable by extracting `runOnce(ctx, opts, extraEnv []string)` and asserting via a fake `execCommand` seam — add `var execCommand = exec.CommandContext` at top of claude.go and swap in tests.)
- [ ] **Step 2:** Run — FAIL. **Step 3:** Implement; log `slog.Warn("claude seat throttled, retrying via API key", "env", p.FallbackAPIKeyEnv)`. **Step 4:** PASS.

### W-B · Task 11: Issuetracker extensions

**Files:**
- Modify: `internal/issuetracker/tracker.go`, `internal/issuetracker/linear.go`, `internal/issuetracker/linear_test.go`

**Interfaces — produces exactly:**

```go
// CreateIssueOpts (tracker.go:110) gains:
//   StateID string   // optional Linear workflow state UUID
//   Labels  []string // extra label IDs beyond bug/feature mapping
// CreateIssue now returns IssueRef with InternalID populated (issueCreate → issue { id identifier url title }).
```

- [ ] **Step 1:** Failing tests (httptest GraphQL server, following existing linear_test.go pattern): mutation body includes `stateId` only when set; extra labels merged with category label; response `id` lands in `IssueRef.InternalID`. Concurrency test: two goroutines calling `CreateIssue` trigger exactly one `teams` resolution query (`sync.Once`).
- [ ] **Step 2:** Run — FAIL. **Step 3:** Implement in `linear.go:328-407`; wrap `resolveTeamID` (`linear.go:292`) in `sync.Once` stored on the tracker. **Step 4:** PASS.

### W-B · Task 12: Ticket engine — compose, gate, file

**Files:**
- Create: `internal/ticket/ticket.go`, `internal/ticket/compose.go`, `internal/ticket/ticket_test.go`, `internal/ticket/compose_test.go`

**Interfaces — produces exactly:**

```go
package ticket

type Decision int
const (
    DecisionPropose Decision = iota // Slack CTA
    DecisionAutoFile
)
type Source string // "auto" | "cta" | "digest" | "escalation"

type Store interface { // implemented by *state.DB (Task 3 accessors)
    UpsertTicketIndex(e *state.TicketIndexEntry) error
    GetTicketIndex(externalKey string) (*state.TicketIndexEntry, error)
}
type Engine struct { /* tracker issuetracker.Tracker; store Store; cfg config.TicketConfig; permalink func(channel, ts string) (string, error) */ }
func New(tr issuetracker.Tracker, store Store, cfg config.TicketConfig,
    permalink func(channel, ts string) (string, error)) *Engine

func (e *Engine) Decide(f investigation.Findings) Decision
// AutoFile iff cfg.AutoFile && len(f.SentryIssueIDs) > 0 && f.Confidence >= cfg.AutoFileConfidence && f.Feasible

func ExternalKey(f investigation.Findings, channel, threadTS string) string
// "sentry:<first id>" when present, else "thread:<channel>:<threadTS>"

type FileResult struct { Ref *issuetracker.IssueRef; AlreadyExisted bool }
func (e *Engine) FileOrUpdate(ctx context.Context, f investigation.Findings,
    channel, threadTS, investigationID string, src Source) (*FileResult, error)
// GetTicketIndex hit → PostComment("**Toad re-observed this issue** ...") + bump last_seen → AlreadyExisted:true
// miss → CreateIssue(Title, ComposeBody(...), Category from f, StateID: cfg.TriageStateID) → UpsertTicketIndex

func ComposeBody(f investigation.Findings, slackPermalink, investigationID string) string
```

- [ ] **Step 1:** Failing compose test: golden-string assert that `ComposeBody` renders, in order: `## Problem`, `## Root cause (hypothesis)` with evidence bullets rendered as `- `+"`ref`"+` — note`, `## Scope`, `## Non-goals`, `## Acceptance criteria` as `- [ ]` items, and a footer containing the Slack permalink, `sentry:<id>` per ref, and `toad:investigation <id>`. Failing gate tests: table over (AutoFile flag, sentry refs, confidence, feasible) → Decision. Failing idempotency test with a fake Store + `issuetracker.NoopTracker`-style fake: second `FileOrUpdate` for same key returns `AlreadyExisted: true` and posts a comment instead of creating.
- [ ] **Step 2:** Run — FAIL. **Step 3:** Implement. **Step 4:** PASS.

### W-C · Task 13: Slack CTA re-target + Sentry detection

**Files:**
- Modify: `internal/slack/responder.go`, `internal/slack/interactive.go`, `internal/slack/events.go`, `internal/slack/client.go`
- Create: `internal/slack/sentry.go`, `internal/slack/sentry_test.go`
- Test: extend `internal/slack/interactive_test.go` / `events_test.go` if present, else create

**Interfaces — produces exactly:**

```go
// responder.go: FixThisBlocks → renamed
func TicketBlocks(text, threadTS string) []slack.Block   // button "Create Linear ticket", action id "toad_ticket"
func TicketedByBlocks(orig slack.Blocks, userName string) []slack.Block // ":ticket: Ticket requested by <name>"
// client.go IncomingMessage: IsTadpoleRequest → IsTicketRequest; new field SentryRefs []string
// sentry.go:
func ExtractSentryRefs(fullText string) []string
// matches https://<anything>.sentry.io/.../issues/<id-or-shortid> and sentry.io/organizations/<org>/issues/<id>
```

- [ ] **Step 1:** Failing tests: `ExtractSentryRefs` over a realistic Sentry Slack attachment text (issue URL in attachment title link `<https://acme.sentry.io/issues/5566778899|BILLING-2291>`) returns the ID; plain text without sentry.io → empty. Interactive test: block-actions callback with action id `toad_ticket` produces a message via `c.handler` with `IsTicketRequest == true`.
- [ ] **Step 2:** Run — FAIL.
- [ ] **Step 3:** Implement: rename action const (`interactive.go:16`) to `actionIDTicket = "toad_ticket"`; update `parseFixAction`→`parseTicketAction`, optimistic-replace copy in `handleInteractive` (`interactive.go:46-52`) to ":ticket: Creating ticket..."; `SpawnedByBlocks`→`TicketedByBlocks` (`responder.go:29`). In `events.go`: populate `msg.SentryRefs = ExtractSentryRefs(fullText)` at both build sites (`events.go:74-75` region and `:145-146` region); reaction path (`events.go:191-192`) sets `IsTicketRequest`. Update `FetchMessage` (`client.go:427`) likewise.
- [ ] **Step 4:** PASS; `go build ./...` will fail at `cmd/handlers.go` call sites (`FixThisBlocks`, `IsTadpoleRequest`) — fix those two call-site names in the same task (they are mechanical renames; Phase-3 freeze on `cmd/` is waived for exactly these renames, nothing else).

### W-C · Task 14: Triage — escalate signal + Sentry awareness

**Files:**
- Modify: `internal/triage/triage.go`, `internal/triage/classify_test.go`

**Interfaces:** `Result` gains `Escalate bool \`json:"escalate"\``.

- [ ] **Step 1:** Failing test: prompt (the `triagePrompt` const) mentions both rules; `parseResult` round-trips `"escalate": true`.
- [ ] **Step 2:** Run — FAIL. **Step 3:** Prompt additions to `triage.go:45-93`: (a) "Messages posted by monitoring bots (Sentry) that contain an error/stack trace are category `bug`, actionable, with the error signature in `keywords`." (b) "`escalate` is true ONLY when the user explicitly asks to create/file a ticket or issue for something already discussed (e.g. 'make a ticket for this'). Otherwise false." Add `"escalate": false` to the JSON template in the prompt. **Step 4:** PASS.

**Phase 3 checkpoint:** merge the three workstreams (operator), full suite green, `/release`.

---

## Phase 4 — Wiring (SERIAL, one agent)

### Task 15: v2 routing in `cmd/handlers.go` + `cmd/root.go`

**Files:**
- Modify: `cmd/root.go`, `cmd/handlers.go`, `cmd/helpers.go`
- Create: `cmd/ticketflow.go`, `cmd/ticketflow_test.go`

**Interfaces — consumes:** `investigation.NewRunner/Run`, `ticket.New/Decide/FileOrUpdate/ComposeBody`, `agent.WriteMCPConfig`, `islack.TicketBlocks`, `state` accessors, `triage.Result.Escalate`, `msg.SentryRefs`, `SyncRepoNow`.

- [ ] **Step 1 (root.go):** At startup: `mcpPath, _ := agent.WriteMCPConfig(toadpath.Dir(), cfg.Agent.MCPServers)`; `allowedMCP := []string{"mcp__sentry__*"}` when a `sentry` server is configured, else nil; construct `investRunner := investigation.NewRunner(agentProvider, cfg.Agent.Model, mcpPath, allowedMCP, wrapSync(cfg), repoPaths)` and `ticketEngine := ticket.New(tracker, stateDB, cfg.Ticket, slackClient.GetPermalink)`. `NewProvider` call gains `cfg.Agent.FallbackAPIKeyEnv`. Wire `Propose:` in digest opts to `proposeFromDigest` (Task 16). Thread both engines through `handleMessage` params.
- [ ] **Step 2 (handlers, triggered route):** For `bug|feature && Confidence >= 0.5`: claim thread (existing `Claim` protocol), `SetStatus("Investigating...")`, build `investigation.Request` from msg/triage (Timeout 4m, `SentryRefs: msg.SentryRefs`, TicketContext via `enrichWithIssueDetails` block), **idempotency pre-check**: `key := ticket.ExternalKey`-style sentry key; if `GetTicketIndex(key)` hits → `PostComment` re-observation + Slack reply linking the existing ticket, unclaim, return. Else `investRunner.Run`; `!Feasible` → fall through to ribbit (unchanged v1 semantics). Save `InvestigationRecord` (id `invest-<ms>-<hex>`, reuse the hex helper pattern from v1 runner IDs). Then `switch ticketEngine.Decide(f)`: `DecisionAutoFile` → `FileOrUpdate(..., "auto")` → reply ":ticket: Filed <url> — *<title>*\n\n<Reasoning>" ; `DecisionPropose` → reply findings text with `TicketBlocks`.
- [ ] **Step 3 (handleTicketRequest, replacing the Task-6 stub):** the CTA/emoji/escalate entry: load `GetInvestigationByThread(threadTS)`; if present and fresh (< 24h) reuse; else run a fresh investigation over the thread (`FetchThreadMessages` context, Timeout 4m). Then `FileOrUpdate(..., src)` where src is `"cta"` or `"escalation"`; reply with the ticket link. Triage `Escalate == true` in `handleTriggered` routes here.
- [ ] **Step 4 (intake allowlist):** replace the bot drop at the v1 anchor (`cmd/handlers.go:81` region) with: `if msg.IsBot && !slices.Contains(cfg.Intake.BotAllowlist, msg.BotID) { return }` — allowlisted bots continue into `handleTriggered`-equivalent triage (treat as triggered: acquire `ribbitSem`… reuse the existing triggered path with `IsTriggered` forced true for allowlisted bots).
- [ ] **Step 5:** `ticketflow_test.go`: with `agent.MockProvider` returning a canned findings JSON and a fake tracker, assert end-to-end: sentry-corroborated high-confidence → auto-file + ticket_index row; low-confidence → propose blocks present; duplicate sentry key → comment path. Run — PASS. Full suite green.

### Task 16: Digest propose + notice re-label

**Files:**
- Modify: `cmd/root.go` (the `NotifyInvestigation` callback region and new `proposeFromDigest`), `cmd/ticketflow.go`

- [ ] **Step 1:** Implement `proposeFromDigest(ctx, f investigation.Findings, msg digest.Message) error`: same gate — `Decide`; AutoFile (sentry-corroborated digest finds) → `FileOrUpdate(..., "digest")` + thread notice; otherwise post findings with `TicketBlocks` (this replaces the v1 `:crown:` spawn announcement). `NotifyInvestigation` callback: keep contributor cc-mentions (`GetSuggestedReviewers` + `ResolveGitHubToSlack`) and Linear crossposting; buttons now `TicketBlocks`.
- [ ] **Step 2:** Full suite + `go vet` green.

### Task 17: MCP `investigations` tool; drop `watches`

**Files:**
- Modify: `internal/mcp/tools.go`, `internal/mcp/tools_test.go`, `cmd/root.go` (registration block)

**Interfaces — produces exactly:**

```go
type investigationsArgs struct {
    Ticket string `json:"ticket,omitempty"` // Linear ref e.g. SCL-1482
    Thread string `json:"thread,omitempty"` // Slack thread TS
}
func RegisterInvestigationsTool(srv *gomcp.Server, db *state.DB) // any valid token; returns findings_json as TextContent
```

- [ ] **Step 1:** Failing test: seeded DB row is returned by ticket ref (via `FindInvestigationByTicket`) and by thread; both empty args → error text "provide ticket or thread".
- [ ] **Step 2:** Implement; delete `RegisterWatchesTool` + `watchesArgs` + `formatWatches` + `PRWatchReader` (`tools.go:322-390` region) and its registration in `cmd/root.go`; update the `query` tool's table-list description text (drop `pr_watches`/`personality_adjustments`, add `ticket_index`/`investigations`). **Step 3:** PASS.

### Task 18: Outcome poller

**Files:**
- Create: `cmd/outcomes.go`, `cmd/outcomes_test.go`
- Modify: `cmd/root.go` (launch goroutine), `cmd/status.go` (surface counts)

**Interfaces:** `func runOutcomePoller(ctx context.Context, db *state.DB, tracker issuetracker.Tracker, interval time.Duration)` — hourly: `RecentTicketIndex(100)` → for rows with `status_checked_at` older than interval, `GetIssueStatus` → `UpdateTicketStatus`; log transitions (`slog.Info("ticket outcome", "issue", id, "from", old, "to", new)`). Skip when tracker is Noop.

- [ ] **Step 1:** Failing test with fake tracker: status change persists; unchanged status only bumps `status_checked_at`. **Step 2:** Implement + wire `go runOutcomePoller(ctx, stateDB, tracker, time.Hour)` in root.go. **Step 3:** PASS.

**Phase 4 checkpoint:** full suite green → operator smoke test in a sandbox Slack channel (mention with a fake bug; verify propose flow; click CTA; verify Linear ticket in a test team) → `/release`.

---

## Phase 5 — Validation & docs (SERIAL)

### Task 19: End-to-end validation matrix (operator + one agent)

- [ ] Question flow: "@toad how does X work" → ribbit answer, no button.
- [ ] Escalation: reply "make a ticket for this" → ticket created from thread, `source=escalation`.
- [ ] Bug report (no Sentry): triggered → propose with button; click → ticket; second click on same thread → links existing (idempotency by thread key).
- [ ] Sentry alert in allowlisted channel: allowlist admits it; investigation pulls Sentry MCP; auto-file when confidence ≥ 0.85; re-alert → comment, no duplicate.
- [ ] Digest: seeded chatter → propose comment with button (no auto-file without sentry refs).
- [ ] Kill daemon mid-investigation → restart → run marked failed, no orphan claims.
- [ ] `toad status` shows outcome counts; MCP `investigations` returns findings for a filed ticket.

### Task 20: Rewrite CLAUDE.md + config docs

**Files:**
- Modify: `CLAUDE.md`, `docs/` config examples, `.toad.yaml.example` if present

- [ ] Rewrite the Architecture section for v2 (message flow, packages list minus tadpole/reviewer/personality, plus investigation/ticket), Important Details (no worktrees; read-only; MCP config; fallback env; new tables), keep Git Policy and Build & Test sections verbatim.
- [ ] Full suite + `gofmt` green → operator `/release` (this is the v2.0.0 cut).

---

## Self-review notes (already applied)

- Spec §4.1–§4.9 each map to tasks: 4.1→T1/T9, 4.2→T12, 4.3→T13/T15, 4.4→T8/T10, 4.5→T3, 4.6→T14, 4.7→T4/T16/T17/T18, 4.8→T2, 4.9→T11; §5→T12 compose; §6→T10 + deployment is operational; §7→checkpoints; §3 removals→T4–T7.
- Type names cross-checked: `investigation.Findings`, `ticket.Engine.FileOrUpdate`, `state.TicketIndexEntry`, `TicketBlocks`, `IsTicketRequest`, `Result.Escalate` used consistently across tasks.
- Known intentional deviation: Phase 3 W-C touches two `cmd/` call-site renames (documented waiver in Task 13).
