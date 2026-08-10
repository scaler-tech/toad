# STE Prose Style — Design

**Date:** 2026-08-10
**Status:** Approved (pending spec review)

## Problem

Toad's user-facing prose is currently steered by two recent additions: the shared anti-AI-slop
constants `agent.ProseStyleRules` / `ProseStyleRulesSlim` (commits `68f61fb`, `421d419`,
`a0dafe0`) and the four-block reasoning template (`*Verdict / Today / Change / Not checked*`)
in the investigation prompt (commit `eb810ed`). The results are better than before but still
not right. Both are to be replaced with explicit instructions to write in Simplified Technical
English (ASD-STE100), with a lean toward compact answers — especially in conversation replies.
Tickets stay complete, but concise.

## Decisions (made during brainstorming)

1. **STE strictness:** STE core rules everywhere, human tone allowed in conversation.
   Slack replies apply STE's sentence discipline but may use contractions and a friendly
   register. Ticket/findings prose applies the rules fully.
2. **Reasoning template:** dropped entirely. The `reasoning` field becomes free-form prose
   governed by the STE rules plus "lead with the conclusion, stay compact" — no fixed block
   structure.
3. **Structure:** one shared STE block with a tone carve-out line (approach A). No per-surface
   constant split; style stays in one place, all four call sites keep their `%s` injection.

## Changes

### 1. `internal/agent/style.go` — replace both constants

`ProseStyleRules` becomes an ASD-STE100 instruction block containing:

- The standard by name: "Write in Simplified Technical English (ASD-STE100)."
- Core STE rules:
  - One idea per sentence.
  - Sentences max ~20 words (25 for descriptive text).
  - Active voice; simple tenses only (past, present, future — no perfect or progressive).
  - One word = one meaning; use the same word for the same thing every time (no synonym
    cycling).
  - Choose the simplest common word.
  - No noun clusters longer than 3 nouns.
  - Keep articles ("the", "a") — no telegraph style.
  - Paragraphs max 6 sentences, one topic per paragraph.
- Compactness lean:
  - Answer first.
  - Write the shortest complete answer.
  - Cut any sentence that does not change what the reader knows or does.
- Two keepers from the old block (correctness rules, not humanizer):
  - Be concrete: names, `path:line` references, numbers — never vague importance language.
  - Never invent facts, numbers, or sources to satisfy these rules; if something is
    uncertain, say so plainly.
- Tone carve-out (final line): in conversation replies, contractions and a friendly register
  are allowed — STE controls the sentences, not the personality.

The banned-word list and anti-pattern list from the old block are removed; STE's
simplest-word / one-meaning rules subsume them.

`ProseStyleRulesSlim` becomes a 2–3 sentence STE distillation for the digest's per-chunk
Haiku calls: STE by name, short single-idea sentences, active voice, simplest word, concrete
references, compact.

Unchanged contract (keep the doc comment): callers own the surrounding heading, and
`fmt.Sprintf`-based templates must inject via a `%s` argument, never concatenate into the
format string. Neither constant may contain a literal `%`.

### 2. `internal/investigation/prompt.go` — drop the reasoning template

Remove the "Explanation format" four-block template and its "Explanation rules" list
(currently lines 127–137). Replace with a short instruction paragraph:

- `reasoning` is what a human reads in Slack.
- Lead with the conclusion.
- Describe the code, not your search.
- The numeric `confidence` field carries certainty — no confidence prose.

Ticket completeness is unaffected: the JSON schema (`problem`, `root_cause`, `evidence`,
`scope`, `non_goals`, `acceptance_criteria`) already enforces complete tickets, so
"complete but concise" needs no extra instruction beyond the shared style block.

Update the example JSON's `reasoning` value from the `*Verdict.* … *Not checked.*` block
format to free-form STE-style prose demonstrating conclusion-first compact writing.

### 3. `internal/ribbit/ribbit.go` — no change

The existing rules ("Keep it short (3–5 lines for questions, up to 10 for bugs)",
"Be conversational") already carry the compact-conversation lean and are consistent with the
tone carve-out. The new STE block flows in through the existing `%s`.

### 4. Call sites — no change

`cmd/releasenotes.go`, `internal/digest/analyze.go`, and `internal/investigation/prompt.go`
keep injecting the same constant names.

## Testing

- `internal/agent/style_test.go` — existing percent-guard and Sprintf-safety tests pass
  against the new constants by construction (the new text must contain no literal `%`).
- The four `TestPrompt_IncludesProseStyleRules` tests (ribbit, digest, releasenotes,
  investigation) currently assert snippets of the old anti-slop text; rewrite each to
  assert containment of the constant itself (`agent.ProseStyleRules` /
  `agent.ProseStyleRulesSlim`) so they stop breaking on wording edits.
- `internal/investigation/parse_test.go:350` — the assertion on `*Verdict.*` changes to
  assert the new reasoning instruction (e.g. "Lead with the conclusion").
- Full check: `go build ./... && go test ./... && go vet ./... && gofmt -l .`

## Non-goals

- No per-surface constant split (approach B rejected).
- No changes to prompt plumbing, triage, or the ticket engine.
- No new config; the style is not operator-tunable.
- This spec and the implementation are not committed directly — all commits go through
  `/release` (repo git policy).
