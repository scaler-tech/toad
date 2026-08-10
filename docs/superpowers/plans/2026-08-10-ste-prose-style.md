# STE Prose Style Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace toad's anti-AI-slop prose constants with an explicit ASD-STE100 (Simplified Technical English) style block, and drop the investigation prompt's four-block reasoning template in favor of free-form conclusion-first prose.

**Architecture:** All user-facing prose style lives in two shared constants in `internal/agent/style.go` (`ProseStyleRules`, `ProseStyleRulesSlim`), injected via `%s`/`%[3]s` into four prompts (ribbit, investigation, release notes, digest). This plan rewrites those two constants in place and removes the investigation prompt's separate "Explanation format" section. No call-site plumbing changes.

**Tech Stack:** Go, stdlib `testing` (no external test infra).

**Spec:** `docs/superpowers/specs/2026-08-10-ste-prose-style-design.md`

## Global Constraints

- **NEVER run `git commit`, `git add`, or `git push`** — this repo's CLAUDE.md routes all commits through the `/release` skill. Plan tasks therefore have NO commit steps; leave the working tree dirty.
- Neither style constant may contain a literal `%` character (they are injected into `fmt.Sprintf` format strings via `%s` arguments; `style_test.go` guards this).
- Run `gofmt -l .` after each task; fix any output with `gofmt -w <file>`.
- Full gate after the last task: `go build ./... && go test ./... && go vet ./... && gofmt -l .` (gofmt must print nothing).

---

### Task 1: Rewrite the shared style constants as ASD-STE100 rules

**Files:**
- Modify: `internal/agent/style.go` (whole file: both constants and their doc comments)
- Test: `internal/agent/style_test.go` (add one test; existing tests unchanged)

**Interfaces:**
- Produces: `agent.ProseStyleRules` and `agent.ProseStyleRulesSlim` (same exported names and types as today — `const`, `string`). Both now contain the marker text `Simplified Technical English (ASD-STE100)`. Tasks 2 and 3 rely on the names being unchanged and on the `ASD-STE100` marker.

- [ ] **Step 1: Write the failing test**

Append to `internal/agent/style_test.go`:

```go
// TestProseStyleRules_NamesTheStandard pins both constants to ASD-STE100:
// callers advertise Simplified Technical English to the model by name, and
// the swap away from the old anti-slop rules is complete only when both
// blocks say so.
func TestProseStyleRules_NamesTheStandard(t *testing.T) {
	for name, s := range map[string]string{
		"ProseStyleRules":     ProseStyleRules,
		"ProseStyleRulesSlim": ProseStyleRulesSlim,
	} {
		if !strings.Contains(s, "Simplified Technical English (ASD-STE100)") {
			t.Errorf("%s does not name ASD-STE100", name)
		}
	}
}
```

If `strings` is not yet imported in `style_test.go`, add it to the import block.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestProseStyleRules_NamesTheStandard -v`
Expected: FAIL — both constants currently hold the anti-slop text, which never mentions ASD-STE100.

- [ ] **Step 3: Replace both constants**

Replace the entire contents of `internal/agent/style.go` with:

```go
package agent

// ProseStyleRules is a shared writing-style block folded into prompts that
// generate user-facing prose (Slack replies, Linear ticket text, release
// notes). It instructs the model to write Simplified Technical English
// (ASD-STE100) — short single-idea sentences, active voice, one word one
// meaning — with a lean toward compact answers. A final tone line permits a
// conversational register in chat replies; STE controls the sentences, not
// the personality.
//
// Callers own the surrounding heading (each prompt has its own formatting
// convention) and, where the template is a fmt.Sprintf format string, MUST
// inject this via a %s argument rather than concatenating it into the
// format string — that keeps any future edits here from having to worry
// about escaping literal '%' characters.
const ProseStyleRules = `Write in Simplified Technical English (ASD-STE100):
- Write one idea per sentence.
- Keep sentences to at most 20 words (25 in descriptive text).
- Use the active voice. Use only simple tenses (past, present, future) — never perfect or progressive forms.
- Use one word with one meaning, and use the same word for the same thing every time. Do not cycle synonyms.
- Choose the simplest common word that is correct.
- Do not stack more than three nouns in a row.
- Keep the articles "the" and "a" — do not write in telegraph style.
- Keep paragraphs to at most six sentences, with one topic per paragraph.
Be compact:
- Give the answer or conclusion first, then the support.
- Write the shortest complete answer. Cut every sentence that does not change what the reader knows or does.
Be accurate:
- Be concrete: names, numbers, and repo-relative path:line references — never vague importance language like "improved efficiency".
- Never invent facts, numbers, or sources to satisfy these rules. If something is uncertain, say so plainly.
Tone: in conversation replies, contractions and a friendly register are welcome — these rules control the sentences, not the personality.`

// ProseStyleRulesSlim is a shorter distillation of ProseStyleRules for
// high-frequency, low-token prompts (digest's batch analysis, one Haiku call
// per chunk) where the full block would meaningfully bloat every call. It
// keeps only what matters for a single one-line summary: STE sentence
// discipline, concrete wording, compactness.
const ProseStyleRulesSlim = `Write in Simplified Technical English (ASD-STE100): short sentences with one idea each, active voice, the simplest common word, and the same word for the same thing every time. Be concrete — names, numbers, files — never vague importance language. State the point first and cut everything that does not change what the reader knows.`
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/agent/ -v`
Expected: PASS — the new test passes, and the pre-existing `TestProseStyleRules_NoStrayPercent` and `TestProseStyleRules_SprintfSafe` still pass (the new text contains no `%`).

- [ ] **Step 5: Format check**

Run: `gofmt -l internal/agent/`
Expected: no output. If a file prints, run `gofmt -w` on it. Do NOT commit.

---

### Task 2: Drop the investigation reasoning template, free-form STE reasoning

**Files:**
- Modify: `internal/investigation/prompt.go` (the `promptTemplate` const: remove the "Explanation format" section, rewrite the example `reasoning` value)
- Test: `internal/investigation/parse_test.go` (replace `TestPrompt_IncludesExplanationFormat`, update `TestPrompt_IncludesProseStyleRules`)

**Interfaces:**
- Consumes: `agent.ProseStyleRules` from Task 1 (unchanged name, injected as `%[3]s` — no code change at the injection site).
- Produces: nothing new — `buildPrompt(req Request) string` keeps its signature; only the template text changes.

- [ ] **Step 1: Write the failing tests**

In `internal/investigation/parse_test.go`, replace the whole `TestPrompt_IncludesExplanationFormat` function with:

```go
func TestPrompt_ReasoningInstruction(t *testing.T) {
	p := buildPrompt(Request{Text: "investigate this"})
	for _, want := range []string{
		"Lead with the conclusion",
		"Describe the code, not your search",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing reasoning instruction %q", want)
		}
	}
	for _, gone := range []string{"*Verdict.*", "Explanation format", "*Not checked.*"} {
		if strings.Contains(p, gone) {
			t.Errorf("prompt still contains dropped template element %q", gone)
		}
	}
}
```

And replace the body of `TestPrompt_IncludesProseStyleRules` (currently asserting the old snippet `"cut deploy time from 40 to 4 minutes"`) with a containment check on the constant itself, so it never breaks on wording edits again:

```go
func TestPrompt_IncludesProseStyleRules(t *testing.T) {
	p := buildPrompt(Request{Text: "the export job double-counts refunds"})
	if !strings.Contains(p, agent.ProseStyleRules) {
		t.Error("prompt missing the shared prose style rules")
	}
}
```

Add `"github.com/scaler-tech/toad/internal/agent"` to the test file's imports if absent.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/investigation/ -run 'TestPrompt_ReasoningInstruction|TestPrompt_IncludesProseStyleRules' -v`
Expected: `TestPrompt_ReasoningInstruction` FAILS (the template still contains `*Verdict.*` and "Explanation format", and lacks "Lead with the conclusion"). `TestPrompt_IncludesProseStyleRules` PASSES already (the prompt embeds the constant verbatim) — that is fine; it is a robustness rewrite, not a behavior change.

- [ ] **Step 3: Edit the prompt template**

In `internal/investigation/prompt.go`, replace this block of `promptTemplate` (everything from "Explanation format" through the banned-filler line — currently between the `%[3]s` line and the "Your final message MUST" paragraph):

```
Explanation format — the "reasoning" field is what a human reads in Slack. Write it for an engineer who knows the codebase but has not looked at this area today. Lead with the conclusion. Describe the code, not your search. Use up to four short blocks, dropping any that are empty (never pad):
- *Verdict.* One sentence: what is wrong or missing, and how big the fix is.
- *Today.* Two to four sentences of plain description of how the code behaves now.
- *Change.* One line per edit, each naming the layer or file.
- *Not checked.* Name each gap and what it could affect.
Explanation rules:
- One idea per sentence, around 20 words. Never chain findings with commas.
- Say what a symbol is before naming it: "the poller (ghfeedback)", not "the ghfeedback poller".
- Cut the trace. A file you read matters only if reading it changed the answer — never narrate what you searched, verified, or delegated.
- No confidence adjectives in prose ("confidence is high but not maximal"). The numeric confidence field carries your certainty; the "Not checked" block states the open questions causing any doubt.
- Banned filler: "matches the ask precisely", "bounded", "well scoped", "at every layer", "confirmed at every layer".
```

with:

```
The "reasoning" field is what a human reads in Slack. Write it for an engineer who knows the codebase but has not looked at this area today. Lead with the conclusion. Describe the code, not your search — never narrate what you searched or verified. Keep it complete but compact. The numeric confidence field carries your certainty — do not put confidence language in the prose; state open questions plainly instead.
```

Then, in the example JSON at the bottom of the template, replace the `"reasoning"` value (the string starting `"*Verdict.* The export double-counts…"`) with:

```
"reasoning": "The export double-counts partial refunds, and the fix is one file. The nightly job sums every refund row for an order (billing/export/aggregate.py:118). A partial refund keeps both the original row and the adjustment row, so the job sums both. The fix filters superseded rows in the aggregation loop. Not checked: whether other order shapes use the same loop — spot-check totals after the fix."
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/investigation/ -v`
Expected: PASS, including the two tests from Step 1 and all pre-existing parse/prompt tests.

- [ ] **Step 5: Format check**

Run: `gofmt -l internal/investigation/`
Expected: no output. Fix with `gofmt -w` if not. Do NOT commit.

---

### Task 3: Point the remaining call-site tests at the new constants

**Files:**
- Modify: `internal/ribbit/ribbit_test.go` (one assertion in `TestPrompt_IncludesProseStyleRules`, ~line 447)
- Modify: `cmd/releasenotes_test.go` (one assertion in `TestPrompt_IncludesProseStyleRules`, ~line 333)
- Modify: `internal/digest/analyze_test.go` (one assertion in `TestPrompt_IncludesProseStyleRules`, ~line 71)

**Interfaces:**
- Consumes: `agent.ProseStyleRules` / `agent.ProseStyleRulesSlim` from Task 1.
- Produces: nothing — test-only changes.

These three tests assert snippets of the OLD anti-slop text (`"cut deploy time from 40 to 4 minutes"` in ribbit and releasenotes, `"never vague importance language"` in digest) and are broken (or brittle) after Task 1. Rewrite each to assert containment of the constant itself.

- [ ] **Step 1: See the current failures**

Run: `go test ./internal/ribbit/ ./cmd/ ./internal/digest/ -run TestPrompt_IncludesProseStyleRules -v`
Expected: ribbit and releasenotes FAIL (their snippet is gone). digest PASSES only by luck — its snippet "never vague importance language" survived into the new slim text; rewrite it anyway so it cannot silently drift.

- [ ] **Step 2: Rewrite the assertions**

`internal/ribbit/ribbit_test.go` — replace:

```go
	if !strings.Contains(prompt, "cut deploy time from 40 to 4 minutes") {
		t.Error("expected the shared prose style rules in the ribbit prompt")
	}
```

with:

```go
	if !strings.Contains(prompt, agent.ProseStyleRules) {
		t.Error("expected the shared prose style rules in the ribbit prompt")
	}
```

(`ribbit_test.go` already imports the `agent` package for `agent.MockProvider`.)

`cmd/releasenotes_test.go` — replace:

```go
	if !strings.Contains(p, "cut deploy time from 40 to 4 minutes") {
		t.Error("prompt missing the shared prose style rules")
	}
```

with:

```go
	if !strings.Contains(p, agent.ProseStyleRules) {
		t.Error("prompt missing the shared prose style rules")
	}
```

(`releasenotes_test.go` uses `agent.MockProvider` elsewhere; add the `"github.com/scaler-tech/toad/internal/agent"` import only if the compiler asks.)

`internal/digest/analyze_test.go` — replace:

```go
	if !strings.Contains(prompt, "never vague importance language") {
		t.Error("expected the shared (slim) prose style rules in the digest prompt")
	}
```

with:

```go
	if !strings.Contains(prompt, agent.ProseStyleRulesSlim) {
		t.Error("expected the shared (slim) prose style rules in the digest prompt")
	}
```

(same import note as above, for the `agent` package.)

- [ ] **Step 3: Run the three packages**

Run: `go test ./internal/ribbit/ ./cmd/ ./internal/digest/ -v -run TestPrompt_IncludesProseStyleRules`
Expected: all three PASS.

- [ ] **Step 4: Full gate**

Run: `go build ./... && go test ./... && go vet ./... && gofmt -l .`
Expected: build succeeds, all tests pass, vet clean, gofmt prints nothing. Do NOT commit — hand back to the user for `/release`.
