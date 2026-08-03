# 🐸 toad

**A Slack-native investigation agent that turns bug reports and alerts into evidence-backed Linear tickets.**

Toad watches your Slack channels, triages what comes in with Haiku, and — for anything that looks like a bug or feature — runs a read-only investigation against your actual codebase before it ever touches a tracker. The output isn't "someone should look at this," it's a root-cause hypothesis with `file:line` evidence, a scope, explicit non-goals, and acceptance criteria. Toad no longer writes code, opens PRs, or creates git worktrees; that work now belongs to [Biome](#biome), a separate cloud coding platform that picks up filed tickets.

## Why this shape

Two systems, two kinds of confidence, and humans in the loop at both:

- **Toad's confidence is about intake** — is this really a bug, do I understand where it lives, is the evidence solid enough to act on?
- **Biome's confidence is about delivery** — can this be safely built, tested, and shipped?

Toad only auto-files a ticket without a human in between when a report is corroborated by an allowlisted monitoring bot (e.g. Sentry) *and* the investigation clears a confidence floor. Everything else — the common case — becomes a "Create Linear ticket" button in Slack. A human decides whether the ticket is worth filing; later, a human (or Biome's own gate) decides whether the fix is worth shipping. Two sign-offs, not zero and not four.

## How it works

```
Slack message (incl. allowlisted bots, e.g. Sentry)
  -> Triage (Haiku, ~1s): actionable? category? escalate?
  -> route:
       question        -> ribbit reply (Claude, read-only tools)
       bug / feature    -> read-only investigation (Claude, Sentry MCP support)
                             -> Findings: feasible?, root cause (hypothesis),
                                file:line evidence, scope, non-goals,
                                acceptance criteria, confidence
                             -> ticket gate:
                                  Sentry-corroborated + confident + feasible
                                    -> auto-file to Linear
                                  otherwise
                                    -> "Create Linear ticket" button in Slack
```

Escalation paths bypass the gate entirely, since a human already signed off:
- Triage returning `escalate: true` on an urgent message
- Clicking the "Create Linear ticket" CTA on any toad message

Passive coverage works the same way at batch scale: the digest engine (**Toad King**) collects untriggered channel messages, has Haiku propose opportunities across a batch, and feeds anything that survives its guardrails through the identical investigate-then-gate flow — it never spawns anything, only proposes or files.

Findings are persisted regardless of what the ticket gate decides, so a later CTA click, escalation, or an MCP `investigations` lookup can reuse an existing investigation instead of paying for a new one.

## Quick start

```bash
toad init    # setup wizard: Slack tokens, repo paths, Linear, optional Sentry MCP
toad         # start the daemon
```

See **[SETUP.md](SETUP.md)** for prerequisites, the Slack app scopes toad actually uses, environment variables, finding your Sentry bot ID, running as a daemon, and a smoke-test checklist.

## Features

- **Q&A (ribbit)** — `@toad` a question and get a codebase-grounded answer using read-only tools (Read, Glob, Grep) plus read-only VCS lookups (`gh`/`glab` issue and PR views). Thread memory keeps follow-ups coherent; ribbit retries once on an empty result.
- **Investigation** — a read-only Claude run per actionable report, scoped to every configured repo (`--add-dir`), optionally backed by a Sentry MCP server for pulling stack traces and issue context directly into the investigation.
- **Ticket filing + gate semantics** — `internal/ticket` is the single author of every filed ticket. It decides auto-file vs. propose, composes the ticket body (problem, root-cause hypothesis, evidence, scope, non-goals, acceptance criteria), and de-duplicates by an external key (`sentry:<issue-id>` or `thread:<channel>:<ts>`) so re-observing the same problem posts a comment instead of a duplicate ticket.
- **Escalation paths** — CTA button, or a triage `escalate` verdict — each files immediately, bypassing the auto-file gate since a human (or an urgent-enough signal) already made the call.
- **Digest (Toad King)** — passive, opt-in batch analysis of channel traffic with confidence/category/size/hourly-cap guardrails, so proactive ticket proposals stay conservative by default.
- **MCP server tools** — `ask` (ribbit-backed Q&A), `logs`, `investigations` (Biome's context bridge — look up a prior investigation's findings by thread or ticket), and `query` (read-only SQL against toad's state DB), all behind per-user bearer tokens issued via a Slack slash command.
- **Outcome tracking** — an hourly poller checks every filed ticket against Linear for status transitions and logs them. Visibility only; it never changes toad's filing behavior.

## Biome

Biome is the separate cloud platform that turns a signed-off Linear ticket into a shipped fix. Toad's job ends at "here's a ticket a human is willing to stand behind"; Biome's job is everything from there — coding, testing, and delivery confidence. The `investigations` MCP tool is the bridge: Biome can pull toad's original findings (evidence, scope, acceptance criteria) instead of re-deriving them from the ticket text alone.

## Architecture

```
cmd/            Cobra commands: toad (daemon), init, status, restart, update, version.
                Daemon logic: root.go (bootstrap), handlers.go (message routing,
                bot allowlist), ticketflow.go (investigate-and-file flow: triggered
                investigations, CTA/escalation requests, digest hooks),
                outcomes.go (hourly Linear status poller)

internal/
  slack/          Socket Mode client, event routing, dedup, reply tracking
  triage/         Haiku classification: actionable, category, size, keywords,
                  files, escalate
  ribbit/         Read-only Q&A engine, thread memory, VCS-aware Bash allowlist
  investigation/  Read-only investigation runner: prompt, agent invocation,
                  Findings parsing (feasibility, root cause, evidence, scope,
                  acceptance criteria, confidence)
  ticket/         Ticket Engine: auto-file/propose gate, idempotent filing,
                  ticket body composition
  state/          In-memory + SQLite state, crash recovery, ticket_index and
                  investigations tables
  digest/         Toad King: batching, Haiku analysis, guardrails, gated
                  propose/file — never spawns
  config/         YAML config with cascading defaults, multi-repo profiles,
                  repo resolver
  agent/          Agent CLI abstraction (Claude Code subprocess), MCP config
                  writer, API-key fallback, provider interface
  vcs/            VCS provider abstraction (GitHub via gh, GitLab via glab):
                  CLI availability checks + suggested-reviewer lookups;
                  ribbit's own read-only gh/glab PR/CI/issue lookups run via
                  its Bash allowlist, not through this package (investigations
                  get no gh/glab access at all — Read/Glob/Grep + MCP only)
  issuetracker/   Linear API client: issue creation/lookup, comments,
                  assignee gating (crossposting to an existing ticket is
                  orchestrated by cmd/root.go, not this package)
  mcp/            MCP server: ask, logs, investigations, query tools with
                  token auth
  tui/            Shared huh theme for the init wizard
  update/         Auto-update via Homebrew
  log/            Structured logging (slog, optional file output)
  preflight/      Pre-run validation checks
  toadpath/       Home directory resolution (~/.toad or $TOAD_HOME)
```

## Development

```bash
go build ./...    # Build
go test ./...     # Test
go vet ./...      # Lint
gofmt -l .        # Formatting check (CI-enforced)
```

## License

[Elastic License 2.0 (ELv2)](LICENSE) — free to use, modify, and distribute. You may not offer toad as a hosted/managed service.
