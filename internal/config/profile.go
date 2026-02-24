package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RepoProfile is a compact fingerprint of a repository used for triage routing.
type RepoProfile struct {
	Name        string   // from config
	Path        string   // absolute path
	Description string   // from config or README
	Stack       string   // "Go", "TypeScript/React", "Python", etc.
	Module      string   // go.mod module, package.json name, etc.
	TopDirs     []string // top-level directories (filtered)
	Summary     string   // pre-compiled ~200 char summary for triage prompt
}

// skipDirs are directories excluded from top-level dir listing.
var skipDirs = map[string]bool{
	".git": true, ".github": true, ".vscode": true, ".idea": true,
	"node_modules": true, "vendor": true, ".toad": true, "__pycache__": true,
	".mypy_cache": true, ".pytest_cache": true, "dist": true, "build": true,
	".next": true, ".claude": true, ".docs": true,
}

// BuildProfiles scans each repo's filesystem and builds compact profiles.
func BuildProfiles(repos []RepoConfig) []RepoProfile {
	profiles := make([]RepoProfile, 0, len(repos))
	for _, repo := range repos {
		profiles = append(profiles, buildProfile(repo))
	}
	return profiles
}

func buildProfile(repo RepoConfig) RepoProfile {
	p := RepoProfile{
		Name: repo.Name,
		Path: repo.Path,
	}

	// Detect stack and module from manifest files
	p.Stack, p.Module = detectStack(repo.Path)

	// Read top-level directories
	p.TopDirs = readTopDirs(repo.Path)

	// Description: prefer config, fall back to README first paragraph
	if repo.Description != "" {
		p.Description = repo.Description
	} else {
		p.Description = readREADMEFirstParagraph(repo.Path)
	}

	// Compile summary
	p.Summary = compileSummary(p)

	return p
}

// detectStack checks for manifest files and returns (stack, module).
func detectStack(repoPath string) (string, string) {
	// Go
	if mod := readFirstLine(filepath.Join(repoPath, "go.mod")); mod != "" {
		module := strings.TrimPrefix(mod, "module ")
		module = strings.TrimSpace(module)
		return "Go", module
	}

	// Node/TypeScript
	if data, err := os.ReadFile(filepath.Join(repoPath, "package.json")); err == nil {
		name := extractJSONString(data, "name")
		stack := "JavaScript"
		if _, err := os.Stat(filepath.Join(repoPath, "tsconfig.json")); err == nil {
			stack = "TypeScript"
			// Check for React
			if containsString(data, "react") {
				stack = "TypeScript/React"
			}
		} else if containsString(data, "react") {
			stack = "JavaScript/React"
		}
		return stack, name
	}

	// Python
	for _, f := range []string{"pyproject.toml", "setup.py", "setup.cfg"} {
		if _, err := os.Stat(filepath.Join(repoPath, f)); err == nil {
			name := ""
			if f == "pyproject.toml" {
				name = readPyprojectName(filepath.Join(repoPath, f))
			}
			return "Python", name
		}
	}

	// Rust
	if _, err := os.Stat(filepath.Join(repoPath, "Cargo.toml")); err == nil {
		return "Rust", ""
	}

	// PHP
	if _, err := os.Stat(filepath.Join(repoPath, "composer.json")); err == nil {
		if data, err := os.ReadFile(filepath.Join(repoPath, "composer.json")); err == nil {
			return "PHP", extractJSONString(data, "name")
		}
		return "PHP", ""
	}

	return "", ""
}

func readTopDirs(repoPath string) []string {
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || skipDirs[name] {
			continue
		}
		dirs = append(dirs, name)
	}
	return dirs
}

func readREADMEFirstParagraph(repoPath string) string {
	for _, name := range []string{"README.md", "readme.md", "README"} {
		f, err := os.Open(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		var paragraph strings.Builder
		inParagraph := false

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())

			// Skip headings and blank lines before the first paragraph
			if !inParagraph {
				if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "![") {
					continue
				}
				inParagraph = true
			}

			// End of paragraph
			if inParagraph && line == "" {
				break
			}

			if paragraph.Len() > 0 {
				paragraph.WriteString(" ")
			}
			paragraph.WriteString(line)
		}

		text := paragraph.String()
		if len(text) > 100 {
			text = text[:97] + "..."
		}
		return text
	}
	return ""
}

func compileSummary(p RepoProfile) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[%s]", p.Name))
	if p.Stack != "" {
		sb.WriteString(fmt.Sprintf(" %s", p.Stack))
	}
	if p.Module != "" {
		sb.WriteString(fmt.Sprintf(" (module: %s)", p.Module))
	}
	if p.Description != "" {
		sb.WriteString(fmt.Sprintf(" — %s", p.Description))
	}
	if len(p.TopDirs) > 0 {
		dirs := p.TopDirs
		if len(dirs) > 8 {
			dirs = dirs[:8]
		}
		sb.WriteString(fmt.Sprintf(". Dirs: %s", strings.Join(dirs, ", ")))
	}
	s := sb.String()
	if len(s) > 250 {
		s = s[:247] + "..."
	}
	return s
}

// FormatForPrompt returns a formatted string of repo profiles for triage prompts.
func FormatForPrompt(profiles []RepoProfile) string {
	var sb strings.Builder
	sb.WriteString("Available repositories:\n")
	for _, p := range profiles {
		sb.WriteString(p.Summary)
		sb.WriteString("\n")
	}
	return sb.String()
}

// --- helpers ---

func readFirstLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		return scanner.Text()
	}
	return ""
}

// extractJSONString does a quick regex-free extraction of a top-level string field.
// Good enough for "name": "value" in package.json / composer.json.
func extractJSONString(data []byte, key string) string {
	needle := fmt.Sprintf(`"%s"`, key)
	s := string(data)
	idx := strings.Index(s, needle)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(needle):]
	// Skip whitespace and colon
	rest = strings.TrimLeft(rest, " \t\n\r:")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func containsString(data []byte, s string) bool {
	return strings.Contains(string(data), s)
}

func readPyprojectName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				val = strings.Trim(val, `"'`)
				return val
			}
		}
	}
	return ""
}
