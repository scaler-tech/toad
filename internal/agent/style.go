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
