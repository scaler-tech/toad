# Smart Responder — Design

**Date:** 2026-08-11
**Status:** Approved design (pending spec review)

## Problem

Toad treats every substantive ask as an investigation. In Linear sessions, follow-up prompts
always re-run the full read-only investigation — even "update the ticket", which needs a
ticket edit and a one-line reply, produced six minutes of code research. Slack threads have
the same shape: a follow-up in an already-investigated thread re-triggers investigation, and
a first-touch bug *question* gets the same treatment as a bug *report*. Toad also barely uses
its own memory: prior findings sit in the `investigations` table but only narrow reuse paths
read them.

## Decisions (made during brainstorming)

1. **Safe ticket edits are allowed.** Sessions (and Slack asks) may update a ticket's
   description or title and post comments when a human asks. Never: status, assignee,
   labels, project, deletion. Enforced in toad's code, not by prompt.
2. **One smart responder, no pre-classifier.** Every conversational ask goes to a single
   Sonnet agent that sees the conversation, toad's prior findings, and ticket context, has
   read-only code tools, and decides for itself how deep to dig. No Haiku routing lane.
3. **Scope: everything at once.** Linear sessions (first-touch and follow-ups), Slack thread
   follow-ups, and Slack first-touch intent routing — one spec. The unattended pipelines
   (digest, Sentry auto-file, explicit CTA/escalation ticket requests) keep the structured
   investigation runner unchanged.

## Component: `internal/responder`

The conversational core. Ribbit's engine moves here; ribbit becomes a thin Slack adapter.

### Input — `Conversation`

Surface-neutral struct assembled by the caller:

- `Messages []Message` — the exchange so far ({role: user|toad, text}), newest last. For
  Linear: the session transcript (prompts + toad's activities). For Slack: thread context +
  prior toad replies.
- `PriorFindings string` — rendered from the newest relevant `investigations` row, with its
  age. Lookup for Linear: the session's own records (`thread_ts = "linear-session:<id>"`)
  first, then by ticket (`FindInvestigationByTicket`, which only covers filed tickets). For
  Slack: by thread. Empty when none.
- `TicketContext string` — the existing `<linked_tickets>` block (details + up to 20
  comments) when a ticket is in play.
- `Repo`/`RepoPaths` — as today (primary + `--add-dir` for all).
- `Surface` — `slack` or `linear`; selects formatting rules (Slack mrkdwn vs Linear
  markdown) and which capabilities the prompt advertises.

### Output — envelope

The agent's final message MUST be one JSON object:

```json
{
  "reply": "markdown/mrkdwn answer for the human",
  "ticket_update": {"title": "", "description": "", "comment": ""},
  "did_investigate": false,
  "findings_summary": ""
}
```

- `reply` — always required. STE style rules apply (ProseStyleRules).
- `ticket_update` — optional; only when the human asked for a ticket change. Any non-empty
  field is applied by TOAD's code (see Safety). The agent never mutates anything itself.
- `did_investigate` + `findings_summary` — when the agent actually dug into code, it says
  so and summarizes what it established. Toad persists this to `investigations` (same
  trigger-keyed ID scheme as sessions use today) so later asks and the CTA reuse path see
  it.

Parsing tolerates prose around the JSON (same salvage strategy as `ParseFindings`).
On unparseable output: retry once (ribbit's existing pattern), then fall back to treating
the whole text as `reply`, with no ticket update (never guess a mutation) and no
findings persistence.

### Prompt principles

- Answer from what you already know (conversation, prior findings, ticket) when that is
  enough; read code only when the question needs evidence you do not have. Say which you
  did.
- Prior findings carry their age; verify against the code before repeating claims older
  than the reported code change.
- Ticket updates only on explicit human request; `description` REPLACES the ticket body, so
  preserve anything the human wrote unless told otherwise (prefer `comment` when unsure).
- All existing injection guards (DATA-not-instructions, no secrets/paths, read-only VCS
  CLI rules) carry over.

## Safety: `issuetracker` ticket edits

New tracker method `UpdateIssue(ctx, ref, opts)` with `UpdateIssueOpts{Title, Description
string}` (empty = leave unchanged) via Linear's `issueUpdate` mutation — the ONLY mutation
added. Comments go through the existing `PostComment`. The safe-subset restriction lives
here: there is deliberately no way to express status/assignee/label changes. NoopTracker
returns an error. The responder callers log every applied update
(`issue`, which fields, requesting surface).

## Wiring: Linear sessions (`internal/linearagent`)

- Processor keeps: ack `thought` → claim (`linear-agent` scope) → post → handled-record
  written only after the reply posts → dedup, at-least-once semantics, ack-failure abort.
- Replaced: `findFindings` + `composeResponse` + the investigation bridge. Every prompt
  (first-touch AND follow-up) becomes one responder call via a `Respond` callback wired in
  `cmd/` (same callback style as today's `Investigate`).
- Envelope handling: apply `ticket_update` first (via tracker; on failure, prepend a plain
  one-line note to the reply — "I could not update the ticket: <reason>"), then post
  `reply` as the `response` activity, then write the handled record.
- `findings_summary` non-empty → persist an `InvestigationRecord` (ID = trigger-keyed as
  today, `ThreadTS: "linear-session:<id>"`, `Reasoning` = summary) before posting.
- The session no longer runs `investigation.Runner`. Timeout: keep `Timeout` from
  ProcessorOpts (investigation timeout) as the ceiling — quick answers return early, deep
  digs get the full budget.

## Wiring: Slack

### Thread follow-ups

A triggered message in a thread where toad has prior state (a ribbit reply in thread
memory, or an `investigations` row for the thread) routes to the responder — regardless of
triage category. Exceptions that keep their existing paths: explicit ticket requests
(CTA button, triage `Escalate`, "toad, create a ticket" — `handleTicketRequest` as today)
and digest-originated flows. The responder call replaces both the "re-ribbit" and the
"re-investigate" branches for follow-ups.

### First-touch intent

`triage.Result` gains `Intent string` — one of `report | question | action | chatter`,
added to the triage prompt with one-line definitions and few-shot cues:

- `report` — describes a problem/need for the team to handle ("X is broken", "we should
  add Y").
- `question` — asks how/why/where, even about a bug ("why is X slow?").
- `action` — asks toad to do something conversational/ticket-shaped ("summarize this",
  "update DAT-123").
- `chatter` — everything else.

Routing: `bug|feature + report` → investigate-and-gate exactly as today. Everything else
actionable → responder. Missing/unknown intent (old cached results, Haiku omission) →
falls back to `report` for bug/feature (today's behavior — misroute cost is an unnecessary
investigation, never a lost report).

### Ribbit

`internal/ribbit` keeps its public seam (`Engine.Respond`) but delegates to
`internal/responder` with `Surface: slack`; its Slack-specific prompt content (About-you
capabilities, mrkdwn rules, 2000-char cap) moves into the responder's Slack formatting
profile. Ribbit's retry-on-empty, staleness note, and thread-memory behavior are preserved
in the adapter.

## Testing

- `internal/responder`: envelope parsing (clean JSON, fenced, prose-wrapped, garbage →
  fallback-to-reply), conversation assembly (prior findings included with age, absent when
  none), prompt content (capability lines per surface, STE rules included), mock-provider
  flow tests.
- `internal/issuetracker`: `UpdateIssue` mutation shape (httptest), empty-field semantics
  (title-only update sends no description), NoopTracker error.
- `internal/linearagent`: processor tests rewritten to the responder callback — ack/claim/
  record ordering unchanged (existing tests keep passing with the new callback), plus:
  ticket_update applied before reply posts; update-failure prepends the note and still
  replies; findings_summary persists; no summary → no persistence.
- `cmd`/Slack routing: follow-up-with-prior-state routes to responder (not investigation);
  explicit ticket request still routes to `handleTicketRequest`; first-touch `bug+question`
  routes to responder, `bug+report` investigates; missing intent falls back to report.
- `internal/triage`: prompt includes the intent field + definitions; parse tolerates
  missing intent.

## Rollout

One release. Server needs no config change. Existing `agent_sessions`/`investigations`
rows stay valid (the trigger-keyed ID scheme is unchanged). Watch the first sessions'
activity transcripts for depth calibration (agent digging when it should answer, or
answering when it should dig) — prompt tuning, not code, is the expected knob.

## Non-goals

- No status/assignee/label/project mutations, no issue creation from sessions.
- No changes to digest, auto-file gating, CTA/escalation ticket filing, or the
  investigation runner/prompt (they keep producing structured Findings for tickets).
- No webhook endpoint work.
- No cross-ticket memory beyond what `investigations` already stores.
