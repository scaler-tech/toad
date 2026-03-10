# Investigation Outreach for Bot Messages

**Date:** 2026-03-10

## Problem

When toad investigates digest opportunities in dry-run/comment mode, the investigation reply often goes unnoticed — especially for bot-originated messages (error reporting bots, Linear bot, etc.). The Slack thread reply only notifies the OP, which for bots means nobody sees it. Linear ticket discussions also miss toad's findings since they only exist in Slack.

## Solution

Two outreach mechanisms for bot-originated messages:

1. **Slack user tagging** — @mention up to 2 relevant developers in the investigation reply, based on recent file contributors
2. **Linear crossposting** — post findings as a comment on the referenced Linear ticket with a link back to the Slack thread

For human-authored messages, add a gentle nudge: _"Tag a relevant dev if you'd like someone to take a look."_

## Bot Detection

The digest `Message` struct gains a `BotID` field (already available from Slack events).

- If `cfg.Digest.BotList` is configured and non-empty, only messages where `BotID` matches the list trigger active outreach
- If `BotList` is empty/unset (default), any message with a non-empty `BotID` triggers active outreach

## GitHub-to-Slack User Resolution

Three-tier resolution (in priority order):

1. **Explicit mapping** (DB) — user-registered via `/toad github add`. Always wins.
2. **Fuzzy match** — case-insensitive match of GitHub login against Slack users' display name, real name, and first/last name components. Fails silently if zero or multiple matches.
3. **Skip** — contributor omitted from tag list.

### Storage

New `github_slack_mappings` table in `state.db`:

| Column | Type |
|--------|------|
| slack_user_id | TEXT NOT NULL |
| github_login | TEXT NOT NULL (lowercase) |
| created_at | DATETIME |

Unique constraint on `github_login`.

### Slash Commands

- `/toad github add <login>` — links caller's Slack ID to a GitHub login
- `/toad github list` — shows caller's linked GitHub accounts
- `/toad github remove <login>` — unlinks a GitHub account

All ephemeral responses.

## Investigation Reply Format

### Bot messages

```
:mag: *Investigation findings:*

{reasoning}

cc @jamie @alex

[Let Toad fix this] (button)
```

The cc line uses Slack `<@U12345>` syntax for proper @mentions with notifications. Up to 2 users, resolved from `GetSuggestedReviewers` → GitHub-to-Slack mapping.

### Human messages

```
:mag: *Investigation findings:*

{reasoning}

_Tag a relevant dev if you'd like someone to take a look._

[Let Toad fix this] (button)
```

## Finding Relevant Users

1. Resolve repo path via `resolver.Resolve(opp.Repo, opp.FilesHint)`
2. Call `vcsProvider.GetSuggestedReviewers(ctx, repoPath, filesHint, botSet, 2)` — returns up to 2 GitHub logins
3. For each login, resolve to Slack user ID: DB mapping first, then fuzzy match
4. Filter out any login that doesn't resolve

Reuses the existing `GetSuggestedReviewers` on the `Provider` interface.

## Linear Crossposting

When the original message contains a Linear issue ref and is from a bot:

1. Get the Slack permalink for the investigation reply
2. Call `tracker.PostComment(ctx, issueRef, comment)` with:

```
**Toad investigation findings**

{reasoning}

Toad can fix this automatically — [go to the Slack thread](https://slack.com/...) and click the button to start.
```

Reuses the existing `PostComment` method on the `Tracker` interface.

## Data Flow

Replace the narrow `notifyInvestigation` callback with a richer input:

```go
type InvestigationNotice struct {
    Channel   string
    ThreadTS  string
    Text      string
    BotID     string
    IssueRefs []*issuetracker.IssueRef
    FilesHint []string
    Repo      string
}
```

The digest engine builds this from data already available in `processOpportunities` and `ResumeInvestigations`. The callback in `root.go` handles all outreach logic (Slack mentions, Linear crosspost, nudge text), keeping the digest package clean.

## Changes Summary

**New code:**
- `github_slack_mappings` table + DB methods (CRUD)
- `/toad github add|list|remove` slash commands
- `ResolveGitHubToSlack` function — DB lookup → fuzzy match → skip
- `InvestigationNotice` struct replacing the callback signature
- Outreach logic in `root.go` callback

**Modified code:**
- `digest.go` — investigation notification uses `InvestigationNotice`
- `digest.go` — `Message` struct gains `BotID` field
- `config.go` — `DigestConfig` gains `BotList []string`
- `root.go` — outreach logic in callback wiring
- `root.go` — passes `bot_id` through to digest `Collect`

**Reused as-is:**
- `GetSuggestedReviewers` on `Provider` interface
- `PostComment` on `Tracker` interface
- `FixThisBlocks` / button flow
- `getPermalink`
- Slash command infrastructure
