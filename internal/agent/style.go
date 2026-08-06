package agent

// ProseStyleRules is a shared writing-style block folded into prompts that
// generate user-facing prose (Slack replies, Linear ticket text, release
// notes) — distilled from github.com/petergyang/no-ai-slop's SKILL.md. It
// steers the model away from generic "AI slop" phrasing toward prose that's
// concrete and grounded in the actual input, not the shape of importance.
//
// Callers own the surrounding heading (each prompt has its own formatting
// convention) and, where the template is a fmt.Sprintf format string, MUST
// inject this via a %s argument rather than concatenating it into the
// format string — that keeps any future edits here from having to worry
// about escaping literal '%' characters.
const ProseStyleRules = `- Be concrete and specific: names, numbers, file paths, mechanisms, consequences — never abstract importance. "Improved efficiency" is banned thinking; "cut deploy time from 40 to 4 minutes" is the shape to aim for.
- Use active voice; let verbs do the work ("decided", not "made a decision"). A plain "is" or "has" beats a fake-strong verb like "serves as a centralized hub".
- Never use these words: delve, foster, leverage, utilize, facilitate, empower, streamline, robust, cutting-edge, game changer, tapestry, realm, beacon, multifaceted, meticulous, intricate, paramount, transformative, elevate, embark, supercharge, harness, ever-evolving, pivotal, testament, showcase, underscore.
- Avoid these patterns: binary contrasts ("It's not X, it's Y" — just state Y); throat-clearing ("Here's the thing"); faux-insight setups ("what nobody tells you"); colon-reveals for drama; trailing "-ing" analysis tacked onto a sentence ("..., highlighting the team's commitment"); importance puffery ("marks a pivotal moment"); telling the reader what to notice ("The key point is"); weasel attribution ("experts agree" — name the source or drop the claim); cycling through synonyms instead of repeating the clear word; dramatic sentence fragments; rhetorical setups ("What if I told you"); fake-profound or summary-recap endings — end on the last concrete point, not a moral.
- Formatting: no emoji in headings, no bold sprinkled in for emphasis, bullets only for genuine lists, minimal em dashes.
- Never invent facts, numbers, or sources to satisfy these rules — concreteness comes from the actual input; if something is uncertain, say so plainly instead of dressing it up.`

// ProseStyleRulesSlim is a shorter distillation of ProseStyleRules for
// high-frequency, low-token prompts (digest's batch analysis, one Haiku call
// per chunk) where the full block would meaningfully bloat every call. It
// keeps only the rules that matter for a single one-line summary: concrete
// wording and the banned-word list.
const ProseStyleRulesSlim = `Be concrete and specific — names, numbers, files, mechanisms, never vague importance language like "improved efficiency". Use active voice. Never use these words: delve, foster, leverage, utilize, facilitate, empower, streamline, robust, cutting-edge, game changer, tapestry, realm, beacon, multifaceted, meticulous, intricate, paramount, transformative, elevate, embark, supercharge, harness, ever-evolving, pivotal, testament, showcase, underscore.`
