# Multi-Issue Digest Claims

**Date**: 2026-03-12

## Problem

Toad's digest engine can identify multiple issues from a single Slack message, but the current thread claim mechanism (`threads map[string]string` in `state.Manager`) only allows one claim per thread. Only one of N identified issues gets acted on; the rest are silently dropped.

## Design: Scoped Claims

### State Manager Changes (`internal/state/`)

**Data model**: Change `threads` from `map[string]string` to `map[string]map[string]string` — `threadTS -> (scope -> runID)`.

**Run struct**: Add `ClaimScope string` field to `Run`. This lets `Track` place the run ID in the correct scope slot and `Complete` remove the right entry.

Two claim modes:

- **Exclusive (`scope = ""`)**: Used by triggered/:frog: paths. Returns false if any claim (scoped or exclusive) exists on the thread. When an exclusive claim exists, all other claims on that thread return false.
- **Scoped (e.g. `scope = "DAT-3199"`)**: Used by digest. Multiple different scopes coexist on the same thread. The same scope cannot be claimed twice. An exclusive claim blocks scoped claims.

**New API**:

```go
ClaimScoped(threadTS, scope string) bool  // digest uses this
Claim(threadTS string) bool               // unchanged, calls ClaimScoped(ts, "")
UnclaimScoped(threadTS, scope string)     // digest uses this; only removes placeholders (empty runID)
Unclaim(threadTS string)                  // unchanged, unclaims scope ""
GetByThread(threadTS string) []*Run       // returns ALL active runs for thread (was: single *Run)
```

`Claim(ts)` / `Unclaim(ts)` remain backwards-compatible for triggered and :frog: flows.

**Track/Complete changes**:
- `Track(run)` reads `run.ClaimScope` to place the run ID in `threads[threadTS][scope]`.
- `Complete(runID, ...)` reads the completed run's `ClaimScope` to delete only `threads[threadTS][scope]`. If the inner map becomes empty, delete the outer key too.

**Hydration**: `NewPersistentManager` hydration loop must populate the nested map using each run's `ClaimScope` field instead of the current flat assignment.

**SQLite**: Add a `claim_scope` column to the `runs` table (default `""`) so crash recovery can rebuild the scoped claims map. Update `SaveRun` to persist it, `ActiveRuns` scan to read it.

### Triggered/:frog: Path (unchanged behavior)

`handleTriggered` calls `GetByThread` — if it returns any runs, reply with status. Blocking behavior is the same as today, but can now report multiple active runs:

> ":frog: Already working on this thread (2 active: starting, shipping)"

All direct `stateManager.Claim`/`Unclaim` call sites in `cmd/root.go` (retry handler, triggered investigation, frog CTA) continue to use bare `Claim(threadTS)` which maps to `ClaimScoped(ts, "")`.

### Digest Path Changes (`internal/digest/digest.go`)

Replace `claim(threadTS)` with `claimScoped(threadTS, issueID)` where `issueID` is the scope key derived from the opportunity.

The `processOpportunities` loop no longer skips opportunities from the same thread — each gets its own scoped claim and spawns its own tadpole. Each tadpole posts its own reply in the thread with its PR link.

**`ResumeInvestigations`** also calls `e.claim`/`e.unclaim` and needs the same scoped treatment.

Same unclaim-on-error pattern, just scoped. `UnclaimScoped` only removes entries where the run ID is still empty (placeholder), matching current `Unclaim` behavior.

### Callback Signature Changes

Update the digest engine's function types:

```go
// Before
ClaimFunc  func(threadTS string) bool
UnclaimFunc func(threadTS string)

// After
ClaimFunc  func(threadTS, scope string) bool
UnclaimFunc func(threadTS, scope string)
```

Wiring in `cmd/root.go` passes `stateManager.ClaimScoped` and `stateManager.UnclaimScoped`.

### Scope Derivation in Digest

When digest finds an opportunity and needs a scope key:

1. If a Linear issue ref is extracted from the message, use the issue ID (e.g. `"DAT-3199"`).
2. Otherwise, use a hash of the opportunity summary for same-flush/same-thread dedup. Cross-batch dedup is already handled by the existing `HasRecentOpportunity` keyword-overlap mechanism.

### Robustness Guarantees

- **Same issue can't spawn twice**: scoped claim on issue ID prevents it.
- **Triggered path stays exclusive**: bare `Claim()` blocks if anything is active on that thread.
- **Crash recovery**: `NewPersistentManager` hydrates scoped claims from `claim_scope` column in DB.
- **Unclaim on error**: every error path (investigation dismissed, spawn failed, dry-run) unclaims the scoped key. Only removes placeholders.
- **Backwards compatible**: `Claim(ts)` / `Unclaim(ts)` work exactly as before for triggered and :frog: flows.
- **Pool semaphore**: concurrent digest tadpoles are bounded by the existing tadpole pool semaphore.
- **Exclusive lifecycle**: when an exclusive claim completes, scoped claims from digest are unaffected (they're independent entries in the inner map). Each claim scope has its own lifecycle.

## Files to Change

| File | Changes |
|------|---------|
| `internal/state/state.go` | Add `ClaimScope` to `Run` struct. `threads` map type change. `ClaimScoped`/`UnclaimScoped`/`GetByThread` (returns slice). Update `Track` and `Complete` to use scope. Update `NewPersistentManager` hydration loop. |
| `internal/state/state_test.go` | Tests for scoped claims, coexistence with exclusive claims, Complete removing correct scope |
| `internal/state/db.go` | Add `claim_scope` column migration, update `SaveRun` to persist scope, update `ActiveRuns` scan to read scope |
| `internal/state/db_test.go` | Test migration, scoped persistence/hydration |
| `internal/digest/digest.go` | `ClaimFunc`/`UnclaimFunc` signatures, `processOpportunities` loop, `ResumeInvestigations`, scope derivation |
| `internal/digest/digest_test.go` | Update tests for new claim signature |
| `cmd/root.go` | Wire `ClaimScoped`/`UnclaimScoped` to digest engine, update `GetByThread` callers to handle `[]*Run` return |
| `internal/reviewer/reviewer.go` | Uses `db.GetByThread` (DB-level) — verify it works correctly with multiple runs per thread (it checks "is anything running?" so `LIMIT 1` is fine, but should be acknowledged) |
