# Toad as a Linear Agent — Design

**Date:** 2026-08-10
**Status:** Approved design (pending spec review)

## Problem

Toad talks to Linear with the operator's personal API key (`TOAD_LINEAR_API_TOKEN`), so every
ticket and comment it files appears to come from a human. And toad is invisible inside Linear:
nobody can @-mention it on a ticket to ask for a codebase-backed investigation.

Linear's agent platform fixes both: an OAuth2 app installed with `actor=app` gets its own
workspace identity, and the `app:mentionable` / `app:assignable` scopes make it taggable and
delegable. Mentions and delegations create Agent Sessions; agents reply through the
`agentActivityCreate` mutation (activity types: `thought`, `action`, `elicitation`,
`response`, `error`).

## Decisions (made during brainstorming)

1. **Delivery: polling, not webhooks — for now.** Toad has no inbound surface (Slack is
   Socket Mode) and stays that way. The `agentSessions` GraphQL query is pollable (verified
   via introspection). The session intake sits behind a small interface so a webhook
   listener can be added later without touching the session processor. Accepted trade-off:
   Linear may show the agent as briefly unresponsive until the next poll tick (its UI
   expects an acknowledgment within ~10 s of session creation).
2. **Triggers: mentions AND delegation** (`app:mentionable` + `app:assignable`).
3. **One spec, two phases.** Phase A (app identity) ships first; Phase B (agent sessions)
   builds on it.
4. **No ticket gate on session replies.** A mention or delegation is an explicit human ask —
   the same trust level as the "Create Linear ticket" CTA. Session replies are
   comment-shaped activities on an existing ticket; this path never files a new ticket.
5. **Package boundary:** new `internal/linearagent` package owns sessions/activities;
   `internal/issuetracker` stays ticket CRUD and only gains the token-source change.

## Phase A — App identity

### OAuth app + connect flow

- The operator creates the OAuth app once in Linear workspace settings (requires admin),
  enables agent capabilities, and sets the redirect URL to `http://localhost:9482/callback`.
- Client credentials come from env only: `TOAD_LINEAR_CLIENT_ID`, `TOAD_LINEAR_CLIENT_SECRET`.
- New command **`toad linear connect`** (Cobra, in `cmd/`):
  1. Starts a temporary HTTP listener on `localhost:9482`.
  2. Opens `https://linear.app/oauth/authorize` with `actor=app`, scopes
     `read,write,app:assignable,app:mentionable`, and a random `state` value it verifies on
     callback.
  3. Exchanges the code at `https://api.linear.app/oauth/token`.
  4. Stores the access token in the state DB `settings` table (key `linear_oauth_token`);
     if the response carries a refresh token, stores it as `linear_oauth_refresh_token`.
  5. Queries `viewer` with the new token and prints the app identity it connected as.
- `toad status` shows whether an app token is present (never the token itself).

### Token source in the tracker

- `LinearTracker` gains a token-source abstraction: prefer the stored OAuth token
  (`Authorization: Bearer <token>`), fall back to the personal API key (current raw-header
  behavior) when no OAuth token exists. All existing call sites are unchanged.
- On a 401 with an OAuth token: if a refresh token is stored, refresh once and retry; on
  refresh failure, log loudly and fall back to the API key for that call so filing keeps
  working. The `settings` read is cheap (SQLite); cache the token in memory and re-read only
  on 401.
- Config: no new YAML keys for Phase A. Presence of the stored OAuth token selects app
  identity; `issue_tracker.api_token` / `TOAD_LINEAR_API_TOKEN` remains the fallback and the
  validation error for missing credentials now mentions both options.

## Phase B — Agent sessions

### Package `internal/linearagent`

Three units, each independently testable:

- **`Intake`** (interface): delivers `SessionEvent`s (session snapshot + what changed).
  Phase B ships one implementation, **`Poller`**; a webhook handler is a later second
  implementation. The processor consumes the interface only.
- **`Poller`**: every `linear_agent.poll_seconds` (default 15, min 5), runs one GraphQL
  query for the app's agent sessions in states `pending`, `active`, or `awaitingInput`,
  ordered by `updatedAt` since the stored cursor. For each session it decides what is
  unhandled:
  - a session with no toad activity yet → new session (mention or delegation),
  - a user prompt newer than toad's latest activity → follow-up.
  The exact field shapes (`agentSessions` filter args, activity source discrimination) are
  verified against live schema introspection during implementation — the design assumes
  only: sessions are queryable, carry their issue + activities, and activities distinguish
  agent-authored from user-authored.
- **`Processor`**: handles one `SessionEvent` end to end (one goroutine per session, bounded
  by the shared investigation semaphore):
  1. Post a `thought` acknowledgment immediately ("Reading the ticket and the code.").
  2. `ClaimScoped` on the issue identifier, scope `linear-agent` — coexists with digest or
     Slack-thread claims on related threads, blocks duplicate concurrent sessions on the
     same issue. Claim released via `defer` on success and failure.
  3. Build investigation input: issue details + up to 20 comments (existing
     `issuetracker` fetchers), plus the session's prompt text; resolve the repo with the
     existing multi-repo `Resolver` (fallback: primary).
  4. Reuse: if the `investigations` table has findings for this issue fresher than the
     issue's latest human activity, answer from them instead of re-running.
  5. Otherwise run the standard read-only `investigation.Runner.Run` and persist findings
     (same table, keyed by issue).
  6. Post the findings as a `response` activity — composed with the same body-composition
     helpers the ticket engine uses, full `ProseStyleRules` register (complete but
     concise). Failures post an `error` activity with a plain one-line reason.
  - Follow-ups re-enter the same path with the new prompt plus the prior findings as
    context.

### State (schema v13)

- New table **`agent_sessions`**: `session_id` PK, `issue_id`, `issue_identifier`,
  `last_handled_activity_at`, `status`, `updated_at`. This is the dedup record — the poller
  compares Linear's session state against it to find unhandled work; the processor updates
  it after each handled event.
- New `settings` key `linear_agent_cursor` (the `updatedAt` high-water mark for polling).
- `RecoverOnStartup`: sessions left mid-processing by a crash are re-marked unhandled so the
  next poll retries them.

### Daemon wiring

- `cmd/`: start the poller alongside the digest engine and outcome poller iff an OAuth
  token is stored AND `linear_agent.enabled` (new config key, default true) — no token, no
  poller, no noise.
- Config block:

  ```yaml
  linear_agent:
    enabled: true        # requires a connected app token to actually start
    poll_seconds: 15
  ```

### Guardrail interplay

- "Toad never comments on tickets delegated to an agent" governs toad-initiated comments
  (outcome tracking, repeat observations). An explicit @-mention or delegation **to toad**
  overrides it — a human asked. The rule is otherwise unchanged.
- Session replies never create tickets, never mutate issue state (status, assignee), and
  the investigation stays read-only (`PermissionReadOnly`, same allowlists as today).

## Testing

- Phase A: token-source unit tests (OAuth preferred, API-key fallback, 401→refresh→retry,
  refresh-failure fallback) against a mock HTTP transport; `linear connect` callback
  handler tested with `httptest` (state mismatch rejected, code exchange, settings write).
- Phase B: poller unhandled-work detection against fixture session payloads (new session,
  follow-up prompt, already-handled, toad-activity-last); processor flow with mocked
  tracker/runner (ack posted first, claim/release on success and failure, reuse-vs-rerun
  decision, error activity on runner failure); schema v13 migration test; recovery test.
- Existing suites must stay green; in-memory SQLite as today.

## Rollout

- Phase A release: `toad linear connect`, token source, status line. Operator connects; all
  filing flips to app identity with zero config change.
- Phase B release: poller + processor + schema v13. Operator re-runs nothing — scopes were
  granted in Phase A.

## Non-goals

- No webhook endpoint, no tunnel, no inbound HTTP (future work; the `Intake` seam is the
  preparation).
- No proactive session creation (`agentSessionCreateOnIssue`) — toad only answers when
  asked.
- No issue mutations from sessions (no status changes, no self-unassignment).
- No Slack notification of Linear-session activity.
- The MCP server, digest, ribbit, and Slack flows are unchanged.
