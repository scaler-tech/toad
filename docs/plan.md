# Toad - Implementation Plan

## Phase 1: MVP (Slack listener + ribbit replies)

**Status: Complete**

- [x] Initialize Go project
- [x] Config loading (`internal/config/config.go`)
- [x] Slack client with Socket Mode (`internal/slack/`)
- [x] Event routing — mention, message, reaction (`internal/slack/events.go`)
- [x] Triage engine with Haiku (`internal/triage/triage.go`)
- [x] Ribbit responder with prefetch + Sonnet (`internal/ribbit/ribbit.go`)
- [x] Main daemon loop + message handler (`cmd/root.go`)
- [x] `toad init` interactive setup wizard (`cmd/init.go`)
- [x] Auto-init on missing config
- [x] Shared TUI theme (`internal/tui/theme.go`)

## Phase 1.1: First real-world fixes

**Status: Complete**

- [x] Fix self-reply loop — bot processes its own replies as new events
  - Store bot user ID via `AuthTest` on connect (`internal/slack/client.go`)
  - Reject `ev.User == botUserID` in all event handlers (`internal/slack/events.go`)
- [x] Improve ribbit quality — answers were generic instead of pointing to code
  - Rewrite prompt to be conversational, answer actual questions, reference files
- [x] Add debug logging — hard to diagnose issues without tracing message flow
  - Debug logs at every decision point: event routing, triage, ribbit, handler

## Phase 1.2: Load test fixes

**Status: Complete**

- [x] Message deduplication — same app_mention processed multiple times when Slack re-delivers events in threads
  - Track seen message timestamps in `Client.seen` map (`internal/slack/client.go`)
  - Check `markSeen()` before dispatching in `handleAppMention` and `handleMessage`
- [x] Skip triage gate for direct @mentions — triage was rejecting valid questions (actionable=false)
  - Triage is now informational for direct mentions (provides keywords/files for ribbit)
  - Always generate ribbit when someone explicitly @mentions toad
  - Only show "react :frog: to fix" CTA for bug/feature categories
- [x] Concurrency limit for Claude calls — rapid-fire messages spawned unbounded parallel processes
  - Semaphore based on `config.Limits.MaxConcurrent` gates triggered + passive handlers
  - Triggered messages block-wait for a slot; passive messages skip if at limit
- [x] Suppress `already_reacted` noise — downgraded to debug level in `React()` method
- [x] Fix triage JSON parsing — Haiku sometimes appends reasoning text after JSON
  - Extract `{...}` object from response using first `{` to last `}`
- [x] Skip duplicate message events for @mentions — `handleMessage` skips texts containing `<@botUserID>`

## Phase 2: Tadpole Runner

**Status: Complete**

- [x] Worktree management (`internal/tadpole/worktree.go`)
  - `CreateWorktree(ctx, repoPath, slug, defaultBranch)` — creates worktree under `~/.toad/worktrees/`, branch `toad/<slug>`
  - `RemoveWorktree(repoPath, wtPath)` — force remove + prune, 30s timeout
  - `Slugify(task)` — lowercase, hyphens, truncate to 40 chars
- [x] Claude CLI runner (`internal/tadpole/claude.go`)
  - `RunClaude(ctx, opts)` — spawns `claude --print --dangerously-skip-permissions --output-format json`
  - Parses JSON envelope for result, cost, session_id
  - Per-call timeout via context
- [x] Validation (`internal/tadpole/validate.go`)
  - `Validate(ctx, worktreePath, cfg)` — test + lint + file count check
  - 5-minute timeout, structured output for retry prompts
- [x] Tadpole runner lifecycle (`internal/tadpole/runner.go`)
  - Setup → implement → validate → retry loop → ship (push + PR) → cleanup
  - Slack status updates via single editable message throughout
  - Reaction lifecycle: :hatching_chick: → :white_check_mark: / :x:
- [x] Concurrency pool (`internal/tadpole/pool.go`)
  - Semaphore-gated, WaitGroup tracked, panic recovery
  - 30s graceful shutdown
- [x] Wire tadpole pool into message handler (`cmd/root.go`)
  - :frog: reaction on toad reply → fetch thread → triage → spawn tadpole
  - Atomic Claim/Unclaim to prevent duplicate spawns (TOCTOU fix)
  - IsTadpoleRequest check moved above bot filter
- [x] `toad run "task"` command (`cmd/run.go`) for manual one-shots

## Phase 2.1: Post-launch hardening

**Status: Complete**

- [x] Fix IsBot blocking tadpole dispatch — :frog: reaction fetches toad's own reply (IsBot=true), bot filter killed it before IsTadpoleRequest check
  - Moved IsTadpoleRequest check above bot filter in handleMessage
- [x] Fix git lock contention — CreateWorktree's `git fetch` on main repo blocked ribbit's prefetchContext
  - Create worktree from local ref first, then fetch inside the worktree
- [x] Context propagation — gitRun in worktree.go had no context, couldn't be cancelled
  - Added `gitRunCtx` using `exec.CommandContext`
- [x] TOCTOU race prevention — 10-30s window between GetByThread and Track allowed duplicate tadpoles
  - Added atomic `Claim`/`Unclaim` to state.Manager
- [x] Validation timeout — test/lint commands inherited unbounded signal context
  - Added 5-minute `context.WithTimeout`
- [x] RemoveWorktree timeout — git cleanup could hang on shutdown
  - Added 30-second timeout context
- [x] Empty ribbit fix — `--max-turns 1` caused `error_max_turns`, empty result, Slack `no_text` error
  - Bumped to `--max-turns 10` with `--allowedTools Read,Glob,Grep`
  - Added empty result check with subtype error reporting
- [x] Reaction UX — added RemoveReaction, SwapReaction to slack client
  - Ribbit: :eyes: → :speech_balloon: (success) / :warning: (failure)
  - Tadpole: :hatching_chick: → :white_check_mark: (success) / :x: (failure)
- [x] Performance: remove prefetchContext (redundant with Claude's --allowedTools)
- [x] UX: remove cost from Slack status messages (kept in state/logs for analytics)

## Phase 2.2: Triage-based routing + auto-join

**Status: Complete**

- [x] Restore triage for all paths — skipping triage for @mentions caused feature requests to default to "question" category, missing auto-spawn
- [x] Auto-spawn for bugs/features — triage routes bugs and features directly to tadpole pool (~1s triage → immediate spawn), questions go to ribbit
  - Removed :frog: CTA for auto-spawn path (PR is the review gate, not the spawn decision)
- [x] Auto-join public channels — bot calls `conversations.join` for all public channels on startup
  - Added `channels:join` OAuth scope
  - Channel config is now optional (empty = monitor all joined channels)
- [x] Simplified init wizard — removed channel selection step, just asks for tokens

## Phase 3: Hardening, Memory & Toad King

**Status: Complete**

Inspired by [Stripe Minions](https://stripe.dev/blog/minions-stripes-one-shot-end-to-end-coding-agents-part-2): one-shot philosophy, bounded iterations, human review mandatory.

### Sprint 1: Tests + Persistent State

- [x] Unit tests for existing code (46 tests across 8 files)
  - `internal/state/state_test.go` — Manager: Track, GetByThread, Claim/Unclaim, Complete, History cap, concurrency
  - `internal/state/db_test.go` — DB: save/retrieve, update status, complete, active runs, thread memory, PR watches
  - `internal/triage/triage_test.go` — parseResult: valid JSON, code-fenced, trailing text, empty/invalid
  - `internal/tadpole/worktree_test.go` — Slugify: normal, unicode, empty, long, hyphens
  - `internal/tadpole/runner_test.go` — buildTadpolePrompt, buildRetryPrompt with/without hints
  - `internal/tadpole/validate_test.go` — truncate: under/at/over limit
  - `internal/config/config_test.go` — Defaults, Validate missing tokens/channels, ApplyEnv
- [x] SQLite persistence layer (`internal/state/db.go`)
  - `modernc.org/sqlite` (pure Go, no CGo)
  - `~/.toad/state.db` with WAL mode
  - `runs` table: id, status, slack_channel, slack_thread, branch, worktree_path, task, started_at, result_json
  - Write-through cache: in-memory maps for fast reads, DB for durability
  - `NewPersistentManager(db)` hydrates from DB; `NewManager()` stays in-memory for tests
- [x] Crash recovery (`internal/state/recovery.go`)
  - On startup: find active runs, mark as failed, clean up orphaned worktrees
  - Scan `~/.toad/worktrees/` for leaked directories not in DB
- [x] DB tests (`internal/state/db_test.go`) using `:memory:` SQLite
- [x] Wire into `cmd/root.go`: OpenDB → RecoverOnStartup → NewPersistentManager

### Sprint 2: Thread Memory

- [x] `thread_memory` SQLite table (thread_ts, channel, triage_json, response, created_at)
  - Store triage summary as raw string
  - 24-hour TTL
- [x] `PriorContext` in ribbit — prior summary + prior response prepended to prompt
  - "Previous conversation in this thread" section for follow-ups
- [x] Wire into handleTriggered: lookup before ribbit, save after ribbit
- [x] Hourly pruning goroutine for expired thread memories

### Sprint 3: PR Review Feedback Loop

- [x] `pr_watches` SQLite table (pr_number, pr_url, branch, run_id, slack_channel, slack_thread, last_comment_id, fix_count, closed)
- [x] PR review watcher (`internal/reviewer/reviewer.go`)
  - Polls `gh api repos/{owner}/{repo}/pulls/{N}/comments` every 2 minutes
  - Detects new comments by `id > last_comment_id`
  - Aggregates review comments into fix task, spawns tadpole on same branch
  - Max 3 fix rounds per PR to prevent infinite loops
  - Marks PR watch as closed when merged/closed
- [x] `ExistingBranch` field on `tadpole.Task`
  - `CheckoutWorktree(ctx, repoPath, branch)` — fetch + checkout existing branch
  - Ship mode: just `git push` (no `gh pr create`) for follow-up commits
- [x] `ShipCallback` via `OnShip()` — runner notifies reviewer after successful PR creation
- [x] Wire into `cmd/root.go`: start watcher, register PRs after successful tadpole ship

### Sprint 4: Channel Digest / Toad King

- [x] Digest config (`internal/config/config.go`)
  - `digest.enabled` (default: false — opt-in)
  - `digest.batch_minutes` (default: 5)
  - `digest.min_confidence` (default: 0.95)
  - `digest.max_auto_spawn_hour` (default: 3)
  - `digest.allowed_categories` (default: ["bug"])
  - `digest.max_est_size` (default: "small")
- [x] Digest engine (`internal/digest/digest.go`)
  - `Collect(msg)` — buffer non-bot messages in memory
  - `Run(ctx)` — ticker loop, flush buffer every N minutes
  - Single Haiku call per batch (~$0.001 regardless of batch size)
  - Conservative prompt: most batches should have zero actionable items
  - Returns `[]Opportunity` with message index, summary, confidence, category, size
- [x] 6 layers of guardrails:
  1. Disabled by default
  2. 0.95 confidence threshold
  3. Category restriction (bug only)
  4. Size restriction (tiny/small only)
  5. Hourly spawn cap (3/hour)
  6. Existing tadpole guardrails (max_files, test/lint, human PR review)
- [x] Wire into `cmd/root.go`: collect in handleMessage, start digest loop
- [x] `:crown: Toad King detected...` notification before auto-spawning

## Phase 3.5: Robustness & Bug Fixes

**Status: Planned**

Quick wins from the Feb 2026 full review. All are targeted, small changes.

### Sprint 1: Critical fixes

- [ ] Fix Unicode-unsafe PR title truncation
  - **File:** `internal/tadpole/runner.go` — `ship()` function
  - **Bug:** `len(title) > 70` counts bytes, not characters. `title[:67]` can split a multibyte character (emoji, accented chars), producing corrupted UTF-8 in the PR title.
  - **Fix:** Convert to runes for length check and truncation:
    ```go
    runes := []rune(title)
    if len(runes) > 70 {
        title = string(runes[:67]) + "..."
    }
    ```
  - **Test:** Add test case in `runner_test.go`: title with emoji, title with accented chars, title at exact boundary.

- [x] Remove `--dangerously-skip-permissions` from digest analyze call *(done in v0.1.6)*

- [ ] Log ALTER TABLE migration errors
  - **File:** `internal/state/db.go` — `migrate()` function, lines 131-134
  - **Bug:** `db.Exec(ALTER TABLE ...)` return values are discarded. If the ALTER fails (disk full, permissions, schema corruption), toad continues and later crashes with "no such column: dismissed".
  - **Fix:**
    ```go
    if count == 0 {
        if _, err := db.Exec(`ALTER TABLE ... ADD COLUMN dismissed ...`); err != nil {
            slog.Warn("migration: failed to add dismissed column", "error", err)
        }
        if _, err := db.Exec(`ALTER TABLE ... ADD COLUMN reasoning ...`); err != nil {
            slog.Warn("migration: failed to add reasoning column", "error", err)
        }
    }
    ```

- [ ] Add `QueryContext` with timeouts to critical DB paths
  - **File:** `internal/state/db.go`
  - **Problem:** All DB methods use `db.Query`/`db.Exec` without context. If SQLite is blocked (WAL contention, disk I/O stall), the calling goroutine blocks indefinitely, which can freeze the event loop.
  - **Fix:** Add a helper and use it in the hot-path methods:
    ```go
    func (d *DB) ctx() (context.Context, context.CancelFunc) {
        return context.WithTimeout(context.Background(), 10*time.Second)
    }
    ```
    Apply to: `SaveRun`, `UpdateStatus`, `CompleteRun`, `ActiveRuns`, `GetByThread`, `Stats`, `SaveDigestOpportunity`, `HasRecentOpportunity`. These are all called from the event loop or tadpole lifecycle.
  - **Skip for:** `migrate()` (runs once at startup, can take longer), `OpenPRWatches` (runs in its own goroutine on a 2-min timer).

### Sprint 2: Robustness

- [ ] Retry on thread context fetch failure
  - **File:** `cmd/root.go` — `handleTriggered()` lines 357-373, `handleTadpoleRequest()` lines 569-576
  - **Problem:** When `FetchThreadMessages` fails (network blip, Slack API timeout), toad logs a warning but proceeds with empty `ThreadContext`. Tadpoles get spawned with just "@toad fix!" — Claude has no idea what to fix.
  - **Fix:** Add a single retry with 2s backoff in both locations. Pattern:
    ```go
    threadMsgs, err := slackClient.FetchThreadMessages(msg.Channel, msg.ThreadTimestamp)
    if err != nil {
        slog.Warn("thread context fetch failed, retrying", "error", err)
        time.Sleep(2 * time.Second)
        threadMsgs, err = slackClient.FetchThreadMessages(msg.Channel, msg.ThreadTimestamp)
    }
    if err != nil {
        slog.Error("thread context fetch failed after retry", "error", err)
    }
    ```
  - For `handleTadpoleRequest`: if both attempts fail, reply with a warning instead of spawning blind:
    ```go
    slackClient.ReplyInThread(msg.Channel, threadTS,
        ":warning: Couldn't fetch thread context — spawning with limited context")
    ```
  - For `handleTriggered`: same retry, but still proceed (questions can work with just the trigger text).

- [ ] Sanitize Slack context in PR bodies
  - **File:** `internal/tadpole/runner.go` — `ship()` function, line 310
  - **Problem:** `task.Description` (raw Slack thread text) is embedded verbatim in the PR body inside a `<details>` block. Can leak API keys, tokens, internal URLs, PII that appeared in error messages or Sentry alerts.
  - **Fix:** Add a `sanitizeForPR(text string, maxLen int) string` function:
    1. Truncate to `maxLen` (4000 chars — GitHub PR body limit is 65536, but keep it reasonable)
    2. Redact common secret patterns: `xoxb-`, `xapp-`, `sk-`, `ghp_`, `glpat-`, `AKIA`, `Bearer `, `token=`, API key formats
    3. Replace matches with `[REDACTED]`
  - Apply in `ship()`:
    ```go
    body := fmt.Sprintf("...<details>\n<summary>Slack context</summary>\n\n%s\n\n</details>...",
        sanitizeForPR(task.Description, 4000))
    ```
  - **Test:** Add test cases in `runner_test.go` for each redaction pattern and truncation.

- [ ] Make PR review fix round limit configurable
  - **Files:** `internal/config/config.go`, `internal/state/db.go`, `internal/reviewer/reviewer.go`
  - **Problem:** `fix_count < 3` is hardcoded in the SQL query in `db.go:288` and the max round count in `reviewer.go`.
  - **Fix:**
    1. Add `MaxReviewRounds int` to `LimitsConfig` (default: 3)
    2. Change `OpenPRWatches()` to accept the limit parameter:
       ```go
       func (d *DB) OpenPRWatches(maxFixCount int) ([]*PRWatch, error) {
           rows, err := d.db.Query(
               "SELECT ... FROM pr_watches WHERE closed = FALSE AND fix_count < ?",
               maxFixCount,
           )
       ```
    3. Thread the config value through `reviewer.NewWatcher` → `poll()` → `OpenPRWatches()`

- [ ] Make history cap configurable
  - **Files:** `internal/config/config.go`, `internal/state/state.go`
  - **Problem:** `state.go:161` hardcodes `if len(m.history) > 50`. High-throughput teams want to see more history in the dashboard.
  - **Fix:**
    1. Add `HistorySize int` to `LimitsConfig` (default: 50)
    2. Pass it through to `Manager` — add a `historySize` field set in `NewPersistentManager`
    3. Use `m.historySize` instead of hardcoded 50 in `Complete()` and `NewPersistentManager` hydration call to `db.History()`

### Sprint 3: Test coverage gaps

- [ ] Add tests for `internal/reviewer/`
  - **File:** new `internal/reviewer/reviewer_test.go`
  - Currently zero tests. Cover:
    - `ExtractPRNumber`: valid PR URLs, trailing slash, non-PR URLs, malformed URLs
    - Comment filtering logic: extract a helper or test via `checkPR` with mocked `gh` output. Verify: bot comments skipped, comments with `id <= lastCommentID` skipped, new human comments included.
    - Fix task construction: verify the task description format includes file paths and comment bodies.
  - **Approach:** `ExtractPRNumber` is pure and testable as-is. For `checkPR`, either extract the filtering logic into a testable function or use `exec.Command` override pattern (test helper that sets PATH to a fake `gh` script).

- [ ] Add tests for digest investigation gate
  - **File:** new test cases in `internal/digest/digest_test.go`
  - Test `processOpportunities` with mock functions:
    - `investigate` returns feasible → verify `spawn` is called with the refined task spec
    - `investigate` returns not feasible → verify `spawn` is NOT called, opportunity persisted as dismissed
    - `investigate` returns error → verify `spawn` is NOT called, opportunity persisted as dismissed
    - Spawn limit: call `trySpawn()` until limit, verify next opportunity is skipped
    - Cross-batch dedup: persist an opportunity, then call again with same keywords, verify skipped
  - **Setup:** Create engine with mock spawn/notify/investigate/react funcs and in-memory DB (`:memory:` SQLite).

- [ ] Add tests for Slack event routing
  - **File:** new `internal/slack/events_test.go` (or extend `client_test.go`)
  - The event handlers in `events.go` have several filtering rules that should be tested:
    - Self-message rejection: message with `User == botUserID` is skipped
    - Dedup: same timestamp dispatched twice, second is skipped (`markSeen`)
    - Channel filtering: when `channels` is configured, events from other channels are dropped
    - Tadpole request detection: `:frog:` reaction on a toad reply sets `IsTadpoleRequest=true`
    - Bot message filtering: messages with `BotID` or subtypes are skipped (except tadpole requests)
  - **Approach:** Create a `Client` with mock Slack API, simulate incoming events, verify which messages reach the `OnMessage` callback.

- [ ] Add end-to-end tadpole runner test
  - **File:** new test cases in `internal/tadpole/runner_test.go`
  - Test the full `Execute` lifecycle with mocked externals:
    - Mock `CreateWorktree` / `RemoveWorktree` (use temp dirs)
    - Mock `RunClaude` (return canned output)
    - Mock `Validate` (return pass/fail)
    - Mock Slack client (collect posted messages and reactions)
    - Mock `ship` (return fake PR URL)
  - **Scenarios:**
    - Happy path: worktree → claude → validate passes → ship → done
    - Validation failure + retry: first validate fails, retry claude + validate passes → ship
    - Validation failure exhausts retries: verify fail status, correct Slack reactions
    - Claude error: verify fail status, worktree cleanup
  - **Approach:** This requires making `CreateWorktree`, `RunClaude`, `Validate`, `ship` injectable (interfaces or function fields on `Runner`). Currently they're direct function calls. Consider adding a `runnerDeps` struct or making them fields on `Runner` with production defaults.

## Phase 4: Toad King as Default Experience

**Status: Planned**

The competitive landscape has shifted (Feb 2026). Copilot Coding Agent is GA, Devin supports Slack triggers, Builder.io has Slack+Jira triggers. "Slack bot that creates PRs" is becoming table stakes. Toad King (proactive passive monitoring) remains unique — **lean into it as the headline feature**.

### Sprint 1: Make Toad King visible from day one

- [ ] Enable digest in dry-run mode by default — change `toad init` to write `digest.enabled: true` and `digest.dry_run: true` in generated config. New users immediately see Toad King identifying opportunities in the dashboard without any spawns.
- [ ] Add Toad King onboarding to init wizard — after token setup, explain what Toad King does and ask if they want to enable live mode (dry-run is on by default).
- [ ] Dashboard: add Toad King section with investigation results — show opportunities found, approved vs dismissed, feasibility reasoning. Make the value visible even in dry-run.

### Sprint 2: Adoption metrics

- [ ] Track merge rate — add `merged` boolean to `runs` table. Poll `gh pr view --json state` periodically (piggyback on reviewer poll). Dashboard shows: PRs created, PRs merged, merge rate.
- [ ] Track review round effectiveness — how often does a review fix round result in approval? Store per-round outcomes.
- [ ] Dashboard: ROI summary — "Toad has shipped X PRs, Y were merged (Z% merge rate), saving ~N hours of developer time."

## Phase 5: Multi-repo, Code Owners & MCP

**Status: Planned**

### Multi-repo support

- [ ] Support multiple repos in a single toad instance
  - Config: `repos:` list with per-repo path, default_branch, services, test/lint commands
  - Triage/digest must resolve which repo a message refers to (keywords, file hints, channel mapping)
  - Worktrees scoped per repo (`~/.toad/worktrees/<repo-slug>/`)
  - State DB: add `repo` column to runs table

### Code owner tagging

- [ ] After opening a PR, identify relevant code owners and request review
  - Parse CODEOWNERS file or use `git log --format='%ae' -- <changed-files>` to find recent contributors
  - `gh pr edit --add-reviewer` to request review from top contributors to changed files
  - Configurable: opt-in, max reviewers, exclude list

### MCP tool use

- [ ] Integrate MCP servers into tadpole and ribbit for richer context and actions
- [ ] **Linear**: create Linear issues/tasks linked to PRs, update issue status on PR merge, pull issue context into tadpole prompts
- [ ] **Laravel Boost** (or similar framework-specific MCP): give tadpoles framework-aware tools for better fixes in Laravel codebases
- [ ] Configurable MCP server list in `.toad.yaml` — tadpoles and ribbit get `--mcp-config` flag pointing to project MCP config

## Phase 6: Future Considerations

**Status: Ideas**

- [ ] Proper DB migration versioning — replace ad-hoc ALTER TABLE checks with a migration table tracking schema version numbers.
- [ ] Slack API rate limiting — add explicit backoff/retry for Slack API calls. Under high load (many concurrent tadpoles + ribbits), toad can hit rate limits.
- [ ] Diff review step — after Claude makes changes, run a fast Haiku call: "does this diff address the task?" to catch off-topic or destructive changes (e.g. deleting the test instead of fixing the bug).
- [ ] `toad init` auto-detect test/lint commands — detect language/framework from repo contents and suggest appropriate test/lint commands during setup.

## Verification

### MVP (Phase 1)

- [x] `go build` compiles
- [x] Slack client connects and receives events
- [x] @toad mention triggers triage + ribbit reply
- [x] Passive messages get triaged silently
- [x] Reaction trigger fetches and processes message

### Post-fix (Phase 1.1)

- [x] Bot's own replies show "skipping: self-message" in debug logs
- [x] No self-reply loop on @toad mention
- [x] Ribbit answers point to specific files, not generic advice
- [x] Debug logs trace full message flow end-to-end

### Load test (Phase 1.2)

- [x] Duplicate messages are skipped ("skipping: duplicate message" in debug logs)
- [x] Direct @mentions always get an answer (no more "can't help with this")
- [x] Rapid-fire messages queue instead of spawning unbounded processes
- [x] No `already_reacted` ERROR lines in logs

### Tadpole (Phase 2)

- [x] Tadpole creates worktree, runs Claude, validates, opens PR
- [x] Timeout kills tadpole after configured minutes
- [x] Concurrent tadpoles run in parallel up to limit
- [x] Test failure triggers retry, then reports failure
- [x] `toad run` works without Slack

### Phase 3

- [x] `go test ./...` — all 46 unit tests pass
- [x] Restart toad mid-tadpole → stale run marked failed, worktree cleaned up
- [x] Follow-up in thread → toad references its prior answer via thread memory
- [x] Tadpole PR gets review comment → fix tadpole spawns, pushes follow-up commit
- [x] Digest enabled → post clear bug description → Toad King spawns tadpole within batch window
- [x] Digest guardrails: vague messages ignored, hourly cap respected, non-bug categories skipped

### Phase 3.5

- [ ] PR titles with emoji/unicode characters render correctly (no byte truncation)
- [x] Digest analyze does not use `--dangerously-skip-permissions`
- [ ] DB migration errors are logged, not silently swallowed
- [ ] Thread context failure retries or warns user before spawning with empty context
- [ ] Reviewer fix round limit is configurable via `limits.max_review_rounds`
- [ ] `internal/reviewer/` has unit tests covering comment filtering and fix task construction
- [ ] Digest investigation gate has tests covering feasible/not-feasible/error paths

### Phase 4

- [ ] `toad init` enables digest in dry-run mode by default
- [ ] Dashboard shows Toad King investigation results (approved/dismissed with reasoning)
- [ ] Dashboard shows merge rate and ROI summary
