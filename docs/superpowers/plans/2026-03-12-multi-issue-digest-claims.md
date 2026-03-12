# Multi-Issue Digest Claims Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow digest to spawn multiple concurrent tadpoles from the same Slack thread when multiple issues are identified.

**Architecture:** Replace the flat `threads map[string]string` in state.Manager with a nested `map[string]map[string]string` (threadTS → scope → runID). Exclusive claims (triggered/:frog:) block all scopes; scoped claims (digest) coexist per-issue. Add `ClaimScope` to the Run struct and persist it in SQLite.

**Tech Stack:** Go, SQLite (modernc.org/sqlite), in-memory state with write-through persistence.

**Spec:** `docs/superpowers/specs/2026-03-12-multi-issue-digest-claims-design.md`

---

## Task 1: State Manager — Scoped Claims

**Files:**
- Modify: `internal/state/state.go`
- Modify: `internal/state/state_test.go`

### Steps

- [ ] **Step 1: Add ClaimScope to Run struct**

In `internal/state/state.go`, add `ClaimScope string` field to the `Run` struct (after `RepoName`):

```go
type Run struct {
	ID            string
	Status        string
	SlackChannel  string
	SlackThreadTS string
	Branch        string
	WorktreePath  string
	Task          string
	RepoName      string
	ClaimScope    string // "" for exclusive claims, issue ID for scoped claims
	StartedAt     time.Time
	Result        *RunResult
}
```

- [ ] **Step 2: Change threads map type**

Change `Manager.threads` from `map[string]string` to `map[string]map[string]string`:

```go
type Manager struct {
	mu          sync.RWMutex
	db          *DB
	runs        map[string]*Run
	threads     map[string]map[string]string // slackThreadTS → (scope → runID)
	history     []*Run
	historySize int
}
```

Update `NewManager()` and `NewPersistentManager()` to initialize the new map type.

- [ ] **Step 3: Implement ClaimScoped**

Add `ClaimScoped(threadTS, scope string) bool`:
- Empty threadTS → always true
- Scope `""` (exclusive): fail if inner map exists and is non-empty
- Scoped (non-empty): fail if inner map has scope `""` (exclusive lock) OR same scope already exists
- On success: create inner map if needed, set `threads[threadTS][scope] = ""`

- [ ] **Step 4: Update Claim to delegate to ClaimScoped**

```go
func (m *Manager) Claim(threadTS string) bool {
	return m.ClaimScoped(threadTS, "")
}
```

- [ ] **Step 5: Implement UnclaimScoped**

Add `UnclaimScoped(threadTS, scope string)`:
- Only remove if value is still `""` (placeholder)
- If inner map becomes empty after removal, delete the outer key

- [ ] **Step 6: Update Unclaim to delegate to UnclaimScoped**

```go
func (m *Manager) Unclaim(threadTS string) {
	m.UnclaimScoped(threadTS, "")
}
```

- [ ] **Step 7: Update Track to use ClaimScope**

```go
func (m *Manager) Track(run *Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[run.ID] = run
	if run.SlackThreadTS != "" {
		scope := run.ClaimScope
		if m.threads[run.SlackThreadTS] == nil {
			m.threads[run.SlackThreadTS] = make(map[string]string)
		}
		m.threads[run.SlackThreadTS][scope] = run.ID
	}
	// ... db persistence unchanged
}
```

- [ ] **Step 8: Update Complete to use ClaimScope**

In `Complete`, replace `delete(m.threads, run.SlackThreadTS)` with:
```go
if run.SlackThreadTS != "" {
	if inner, ok := m.threads[run.SlackThreadTS]; ok {
		delete(inner, run.ClaimScope)
		if len(inner) == 0 {
			delete(m.threads, run.SlackThreadTS)
		}
	}
}
```

- [ ] **Step 9: Update GetByThread to return slice**

Change `GetByThread(threadTS string) *Run` to `GetByThread(threadTS string) []*Run`:
```go
func (m *Manager) GetByThread(threadTS string) []*Run {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inner, ok := m.threads[threadTS]
	if !ok {
		return nil
	}
	var runs []*Run
	for _, runID := range inner {
		if run, ok := m.runs[runID]; ok {
			runs = append(runs, run)
		}
	}
	return runs
}
```

- [ ] **Step 10: Update hydration in NewPersistentManager**

Replace the flat assignment with nested map population:
```go
for _, run := range active {
	m.runs[run.ID] = run
	if run.SlackThreadTS != "" {
		if m.threads[run.SlackThreadTS] == nil {
			m.threads[run.SlackThreadTS] = make(map[string]string)
		}
		m.threads[run.SlackThreadTS][run.ClaimScope] = run.ID
	}
}
```

- [ ] **Step 11: Write tests for scoped claims**

Add to `state_test.go`:
- `TestClaimScoped_MultipleScopesSameThread`: Two different scopes on same thread both succeed
- `TestClaimScoped_SameScopeFails`: Same scope on same thread fails
- `TestClaimScoped_ExclusiveBlocksScoped`: Exclusive claim blocks scoped claim
- `TestClaimScoped_ScopedBlocksExclusive`: Existing scoped claim blocks exclusive claim
- `TestUnclaimScoped_OnlyRemovesPlaceholder`: Doesn't remove tracked runs
- `TestComplete_ScopedReleasesOnlyItsScope`: Other scopes survive
- `TestGetByThread_ReturnsMultiple`: Returns all active runs for a thread

- [ ] **Step 12: Run tests**

Run: `go test ./internal/state/ -v`
Expected: All tests pass.

- [ ] **Step 13: Run build and vet**

Run: `go build ./... && go vet ./...`
Expected: Clean.

---

## Task 2: SQLite Persistence — claim_scope column

**Files:**
- Modify: `internal/state/db.go`

### Steps

- [ ] **Step 1: Add migration 9 for claim_scope column**

Append to the `migrations` slice:
```go
{9, `ALTER TABLE runs ADD COLUMN claim_scope TEXT DEFAULT ''`},
```

- [ ] **Step 2: Update SaveRun to persist ClaimScope**

Add `claim_scope` to the INSERT/REPLACE statement in `SaveRun`:
```sql
INSERT OR REPLACE INTO runs (id, status, slack_channel, slack_thread, branch, worktree_path, task, repo_name, claim_scope, started_at, result_json, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
```
Add `run.ClaimScope` to the args.

- [ ] **Step 3: Update scanRun and scanRuns to read ClaimScope**

Add `claim_scope` to the SELECT column lists and scan it into `run.ClaimScope`. Update the column list in `ActiveRuns`, `History`, `GetByThread` (DB-level), and the `scanRun`/`scanRuns` helpers.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/state/ -v`
Expected: All tests pass including DB tests.

- [ ] **Step 5: Run build**

Run: `go build ./...`
Expected: Clean.

---

## Task 3: Digest Engine — Scoped Claims

**Files:**
- Modify: `internal/digest/digest.go`
- Modify: `internal/digest/digest_test.go`

### Steps

- [ ] **Step 1: Update ClaimFunc and UnclaimFunc signatures**

```go
type ClaimFunc func(threadTS, scope string) bool
type UnclaimFunc func(threadTS, scope string)
```

- [ ] **Step 2: Add scopeKey helper function**

```go
func scopeKey(opp Opportunity, tracker issuetracker.Tracker, msgText string) string {
	if tracker != nil {
		if ref := tracker.ExtractIssueRef(msgText); ref != nil {
			return ref.ID
		}
	}
	// Fall back to summary hash for same-flush dedup
	h := fnv.New32a()
	h.Write([]byte(opp.Summary))
	return fmt.Sprintf("digest-%x", h.Sum32())
}
```

Add `"hash/fnv"` to imports.

- [ ] **Step 3: Update processOpportunities — claim with scope**

At the claim call (line ~518), compute and use scope:
```go
scope := scopeKey(opp, e.tracker, msg.Text)
if e.claim != nil && !e.claim(threadTS, scope) {
	slog.Info("digest skipping: thread+scope already claimed",
		"summary", opp.Summary, "thread", threadTS, "scope", scope)
	continue
}
```

- [ ] **Step 4: Update all unclaim calls in processOpportunities to pass scope**

Replace every `e.unclaim(threadTS)` with `e.unclaim(threadTS, scope)` in the processOpportunities method. There are 6 call sites (lines ~606, ~622, ~647, ~700, ~723, ~764). The `scope` variable is already in scope from step 3.

- [ ] **Step 5: Update ResumeInvestigations — claim with scope**

In `ResumeInvestigations` (~line 358), compute scope and pass it:
```go
scope := scopeKey(Opportunity{Summary: opp.Summary}, e.tracker, msg.Text)
if !e.claim(threadTS, scope) {
	// ...
}
```
And update the unclaim at ~line 377:
```go
e.unclaim(threadTS, scope)
```

- [ ] **Step 6: Update digest tests**

Update any tests that mock `ClaimFunc`/`UnclaimFunc` to match the new 2-arg signatures.

- [ ] **Step 7: Run tests**

Run: `go test ./internal/digest/ -v`
Expected: All tests pass.

---

## Task 4: Root Wiring — GetByThread and Claim callbacks

**Files:**
- Modify: `cmd/root.go`

### Steps

- [ ] **Step 1: Update digest engine wiring**

At the `digest.New()` call (~line 342-343), change:
```go
Claim:   stateManager.ClaimScoped,
Unclaim: stateManager.UnclaimScoped,
```

- [ ] **Step 2: Update handleTriggered — GetByThread returns slice**

At ~line 672, change from:
```go
if existing := stateManager.GetByThread(threadTS); existing != nil {
	slackClient.ReplyInThread(msg.Channel, threadTS,
		fmt.Sprintf(":frog: Already working on this (status: %s)", existing.Status))
	return
}
```
To:
```go
if existing := stateManager.GetByThread(threadTS); len(existing) > 0 {
	statuses := make([]string, len(existing))
	for i, r := range existing {
		statuses[i] = r.Status
	}
	slackClient.ReplyInThread(msg.Channel, threadTS,
		fmt.Sprintf(":frog: Already working on this thread (%d active: %s)", len(existing), strings.Join(statuses, ", ")))
	return
}
```

- [ ] **Step 3: Update handleTadpoleRequest — GetByThread returns slice**

At ~line 1048 (in `handleTadpoleRequest`), find the `GetByThread` check and update similarly. The :frog: CTA handler should also check for existing runs.

Search for any other `GetByThread` call sites in `cmd/root.go` and update them all.

- [ ] **Step 4: Run build and vet**

Run: `go build ./... && go vet ./... && gofmt -l .`
Expected: Clean.

- [ ] **Step 5: Run all tests**

Run: `go test ./...`
Expected: All tests pass.

---

## Task 5: Reviewer — DB-level GetByThread

**Files:**
- Modify: `internal/reviewer/reviewer.go` (verify only)

### Steps

- [ ] **Step 1: Verify reviewer's DB.GetByThread usage**

The reviewer at ~line 136 calls `w.db.GetByThread(watch.SlackThread)` which is the *DB-level* method (returns `(*Run, error)`). This is separate from the state manager's `GetByThread`. It uses `LIMIT 1` and only checks "is anything running?" — this is correct behavior even with multiple runs per thread. No code change needed, just verify it still compiles.

- [ ] **Step 2: Run full test suite**

Run: `go test ./...`
Expected: All tests pass.

- [ ] **Step 3: Run formatting check**

Run: `gofmt -l .`
Expected: No output (all files formatted).
