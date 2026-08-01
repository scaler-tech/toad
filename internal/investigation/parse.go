package investigation

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

// ParseFindings extracts a Findings verdict from the raw text result of an
// investigation agent run. Agents are asked to end their output with a JSON
// object, but real output is messy (prose before/after, markdown fences,
// pretty-printing), so this tries three strategies in order:
//
//  1. scan for a literal `{"feasible"` prefix and take its matching brace span
//     — the common case where the agent output the object compactly.
//  2. strip markdown code fences, then parse the first JSON object found
//     — handles pretty-printed JSON wrapped in ```json ... ``` fences.
//  3. fall back to the last `"feasible"` occurrence and backscan for its
//     enclosing `{` — handles pretty-printed JSON preceded by stray braces
//     that would otherwise confuse strategy 2.
//
// It returns an error if none of the strategies yield a JSON object with a
// "feasible" field.
func ParseFindings(resultText string) (*Findings, error) {
	text := strings.TrimSpace(resultText)

	var findings Findings
	parsed := false

	// Strategy 1: look for {"feasible" directly — most reliable.
	if idx := strings.Index(text, `{"feasible"`); idx >= 0 {
		if end := findMatchingBrace(text, idx); end >= 0 {
			if err := json.Unmarshal([]byte(text[idx:end+1]), &findings); err == nil {
				parsed = true
			}
		}
	}

	// Strategy 2: strip markdown code fences, then parse the first JSON object.
	if !parsed {
		stripped := strings.TrimSpace(stripCodeFences(text))
		if idx := strings.Index(stripped, "{"); idx >= 0 {
			if end := findMatchingBrace(stripped, idx); end >= 0 {
				if err := json.Unmarshal([]byte(stripped[idx:end+1]), &findings); err == nil {
					parsed = true
				}
			}
		}
	}

	// Strategy 3: fall back to the last "feasible" occurrence (most likely the verdict).
	if !parsed {
		if idx := strings.LastIndex(text, `"feasible"`); idx >= 0 {
			for i := idx - 1; i >= 0; i-- {
				if text[i] == '{' {
					if end := findMatchingBrace(text, i); end >= 0 {
						if err := json.Unmarshal([]byte(text[i:end+1]), &findings); err == nil {
							parsed = true
						}
					}
					break
				}
			}
		}
	}

	if !parsed {
		return nil, errors.New("no valid JSON with a feasible field found in result text")
	}

	// FilesFound is always derived from the free-text fields, not read
	// directly off the JSON — the agent narrates file paths in prose far
	// more reliably than it lists them out as a structured array.
	findings.FilesFound = ExtractFilePaths(strings.Join([]string{findings.Problem, findings.RootCause, findings.Reasoning}, "\n"))

	return &findings, nil
}

// stripCodeFences removes markdown code fences (```json ... ``` or ``` ... ```)
// from text, returning the inner content. If no fences are found, returns the
// original text unchanged.
func stripCodeFences(text string) string {
	// Find opening fence
	fenceStart := strings.Index(text, "```")
	if fenceStart < 0 {
		return text
	}
	// Skip past the opening fence line (```json, ```, etc.)
	inner := text[fenceStart+3:]
	if nl := strings.Index(inner, "\n"); nl >= 0 {
		inner = inner[nl+1:]
	}
	// Find closing fence
	if fenceEnd := strings.Index(inner, "```"); fenceEnd >= 0 {
		inner = inner[:fenceEnd]
	}
	return inner
}

// findMatchingBrace finds the index of the '}' that matches the '{' at pos,
// accounting for nested braces and JSON strings.
func findMatchingBrace(s string, pos int) int {
	depth := 0
	inString := false
	escaped := false
	for i := pos; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		ch := s[i]
		if inString {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// knownExts lists file extensions that indicate a real source file path.
var knownExts = map[string]bool{
	".php": true, ".py": true, ".go": true, ".js": true, ".ts": true,
	".tsx": true, ".jsx": true, ".vue": true, ".rb": true, ".java": true,
	".rs": true, ".swift": true, ".kt": true, ".cs": true, ".c": true,
	".cpp": true, ".h": true, ".yaml": true, ".yml": true, ".json": true,
	".sql": true, ".sh": true, ".css": true, ".scss": true, ".html": true,
}

// ExtractFilePaths pulls file paths from free-form text like a findings
// narrative. It looks for tokens that contain a "/" and end with a known
// source file extension — this filters out vague keywords while keeping
// paths the investigation agent actually found in the repo.
func ExtractFilePaths(text string) []string {
	if text == "" {
		return nil
	}

	seen := make(map[string]bool)
	var paths []string

	// Split on whitespace and common delimiters
	for _, token := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == ',' || r == ';'
	}) {
		// Strip surrounding backticks, quotes, parens, brackets
		token = strings.Trim(token, "`\"'()[]{}:")

		// Must contain a directory separator to be a path (not just "Handler.php")
		if !strings.Contains(token, "/") {
			continue
		}

		// Must have a known file extension
		ext := strings.ToLower(filepath.Ext(token))
		if !knownExts[ext] {
			continue
		}

		// Skip URLs
		if strings.HasPrefix(token, "http://") || strings.HasPrefix(token, "https://") {
			continue
		}

		// Normalize: strip leading ./ or /
		token = strings.TrimPrefix(token, "./")
		token = strings.TrimLeft(token, "/")

		if !seen[token] {
			seen[token] = true
			paths = append(paths, token)
		}
	}

	return paths
}
