package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderConfig(t *testing.T) {
	data := templateData{
		GeneratedAt: "2026-02-24",
		Slack: slackTemplateData{
			AppToken: "xapp-1-test",
			BotToken: "xoxb-test",
			Channels: []string{"C123", "C456"},
			Emoji:    "frog",
			Keywords: []string{"toad fix", "toad help"},
		},
		Repos: []repoTemplateData{
			{
				Name:          "my-app",
				Path:          "/home/dev/my-app",
				TestCommand:   "go test ./...",
				LintCommand:   "golangci-lint run",
				DefaultBranch: "main",
				AutoMerge:     false,
				PRLabels:      []string{"toad"},
			},
		},
		Limits: limitsTemplateData{
			MaxConcurrent:   2,
			MaxTurns:        30,
			TimeoutMinutes:  10,
			MaxFilesChanged: 5,
			MaxRetries:      1,
		},
		Triage:  triageTemplateData{Model: "haiku", AutoSpawn: false},
		Claude:  claudeTemplateData{Model: "sonnet"},
		Digest:  digestTemplateData{Enabled: true, DryRun: true},
		IssueTracker: issueTrackerTemplateData{
			Enabled:      true,
			Provider:     "linear",
			APIToken:     "lin_api_test",
			TeamID:       "TEAM-123",
			CreateIssues: true,
		},
		Log: logTemplateData{Level: "info"},
	}

	out, err := renderConfig(data)
	if err != nil {
		t.Fatalf("renderConfig failed: %v", err)
	}

	result := string(out)

	// Verify it's valid YAML (parseable)
	var parsed map[string]interface{}
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid YAML: %v\n\nOutput:\n%s", err, result)
	}

	// Check key sections exist
	checks := []string{
		`app_token: "xapp-1-test"`,
		`bot_token: "xoxb-test"`,
		`- "C123"`,
		`emoji: "frog"`,
		`- "toad fix"`,
		`name: "my-app"`,
		`path: "/home/dev/my-app"`,
		`test_command: "go test ./..."`,
		`lint_command: "golangci-lint run"`,
		`default_branch: "main"`,
		`auto_merge: false`,
		`- "toad"`,
		`max_concurrent: 2`,
		`max_turns: 30`,
		`timeout_minutes: 10`,
		`model: "haiku"`,
		`auto_spawn: false`,
		`model: "sonnet"`,
		`enabled: true`,
		`dry_run: true`,
		`provider: "linear"`,
		`api_token: "lin_api_test"`,
		`team_id: "TEAM-123"`,
		`create_issues: true`,
		`level: "info"`,
	}
	for _, check := range checks {
		if !strings.Contains(result, check) {
			t.Errorf("output missing expected content: %q", check)
		}
	}
}

func TestRenderConfig_NoChannels(t *testing.T) {
	data := templateData{
		Slack: slackTemplateData{
			AppToken: "xapp-1-test",
			BotToken: "xoxb-test",
			Emoji:    "frog",
			Keywords: []string{"toad fix"},
		},
		Repos: []repoTemplateData{
			{Name: "app", Path: "/app", DefaultBranch: "main"},
		},
		Limits:  limitsTemplateData{MaxConcurrent: 2, MaxTurns: 30, TimeoutMinutes: 10, MaxFilesChanged: 5, MaxRetries: 1},
		Triage:  triageTemplateData{Model: "haiku"},
		Claude:  claudeTemplateData{Model: "sonnet"},
		Digest:  digestTemplateData{Enabled: false},
		Log:     logTemplateData{Level: "info"},
	}

	out, err := renderConfig(data)
	if err != nil {
		t.Fatalf("renderConfig failed: %v", err)
	}

	result := string(out)

	// Channels should be commented out
	if strings.Contains(result, "channels:\n    - ") {
		t.Error("channels should be commented out when empty")
	}
	if !strings.Contains(result, "# channels:") {
		t.Error("should have commented-out channels section")
	}
}

func TestRenderConfig_IssueTrackerDisabled(t *testing.T) {
	data := templateData{
		Slack: slackTemplateData{
			AppToken: "xapp-1-test",
			BotToken: "xoxb-test",
			Emoji:    "frog",
			Keywords: []string{"toad fix"},
		},
		Repos: []repoTemplateData{
			{Name: "app", Path: "/app", DefaultBranch: "main"},
		},
		Limits:       limitsTemplateData{MaxConcurrent: 2, MaxTurns: 30, TimeoutMinutes: 10, MaxFilesChanged: 5, MaxRetries: 1},
		Triage:       triageTemplateData{Model: "haiku"},
		Claude:       claudeTemplateData{Model: "sonnet"},
		Digest:       digestTemplateData{Enabled: false},
		IssueTracker: issueTrackerTemplateData{Enabled: false},
		Log:          logTemplateData{Level: "info"},
	}

	out, err := renderConfig(data)
	if err != nil {
		t.Fatalf("renderConfig failed: %v", err)
	}

	result := string(out)

	// Issue tracker should be fully commented out
	if strings.Contains(result, "issue_tracker:\n  enabled: true") {
		t.Error("issue tracker should be commented out when disabled")
	}
	if !strings.Contains(result, "# issue_tracker:") {
		t.Error("should have commented-out issue_tracker section")
	}
}

func TestRenderConfig_CommentsPresent(t *testing.T) {
	data := templateData{
		Slack: slackTemplateData{
			AppToken: "xapp-1-test",
			BotToken: "xoxb-test",
			Emoji:    "frog",
			Keywords: []string{"toad fix"},
		},
		Repos: []repoTemplateData{
			{Name: "app", Path: "/app", DefaultBranch: "main"},
		},
		Limits:  limitsTemplateData{MaxConcurrent: 2, MaxTurns: 30, TimeoutMinutes: 10, MaxFilesChanged: 5, MaxRetries: 1},
		Triage:  triageTemplateData{Model: "haiku"},
		Claude:  claudeTemplateData{Model: "sonnet"},
		Digest:  digestTemplateData{Enabled: true, DryRun: true},
		Log:     logTemplateData{Level: "info"},
	}

	out, err := renderConfig(data)
	if err != nil {
		t.Fatalf("renderConfig failed: %v", err)
	}

	result := string(out)

	// Check advanced options are commented out
	commentedOptions := []string{
		"# max_review_rounds:",
		"# max_ci_fix_rounds:",
		"# history_size:",
		"# append_system_prompt:",
		"# batch_minutes:",
		"# min_confidence:",
		"# services:",
	}
	for _, opt := range commentedOptions {
		if !strings.Contains(result, opt) {
			t.Errorf("missing commented-out option: %q", opt)
		}
	}
}

func TestSuggestCommands(t *testing.T) {
	tests := []struct {
		stack    string
		wantTest string
		wantLint string
	}{
		{"Go", "go test ./...", "go vet ./..."},
		{"TypeScript", "npm test", "npm run lint"},
		{"Python", "pytest", "ruff check ."},
		{"Rust", "cargo test", "cargo clippy"},
		{"", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.stack, func(t *testing.T) {
			// Use a temp dir that won't have lock files
			tmpDir := t.TempDir()
			gotTest, gotLint := suggestCommands(tt.stack, tmpDir)
			if gotTest != tt.wantTest {
				t.Errorf("test command: got %q, want %q", gotTest, tt.wantTest)
			}
			if gotLint != tt.wantLint {
				t.Errorf("lint command: got %q, want %q", gotLint, tt.wantLint)
			}
		})
	}
}

func TestSuggestCommands_YarnLock(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "yarn.lock"), []byte(""), 0o644)

	testCmd, lintCmd := suggestCommands("TypeScript", tmpDir)
	if testCmd != "yarn test" {
		t.Errorf("test command: got %q, want %q", testCmd, "yarn test")
	}
	if lintCmd != "yarn lint" {
		t.Errorf("lint command: got %q, want %q", lintCmd, "yarn lint")
	}
}

func TestSuggestCommands_PHPMakefile(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "Makefile"), []byte("test:\n\tphpunit"), 0o644)

	testCmd, lintCmd := suggestCommands("PHP", tmpDir)
	if testCmd != "make test" {
		t.Errorf("test command: got %q, want %q", testCmd, "make test")
	}
	if lintCmd != "make stan && make cs" {
		t.Errorf("lint command: got %q, want %q", lintCmd, "make stan && make cs")
	}
}

func TestDetectDefaultBranch(t *testing.T) {
	// Create a temporary git repo
	tmpDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		cmd.Run()
	}

	run("init", "-b", "develop")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")

	// Create initial commit so branch exists
	os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test"), 0o644)
	run("add", ".")
	run("commit", "-m", "init")

	branch := detectDefaultBranch(tmpDir)
	if branch != "develop" {
		t.Errorf("got %q, want %q", branch, "develop")
	}
}

func TestDetectDefaultBranch_Fallback(t *testing.T) {
	// Non-git directory should return "main"
	tmpDir := t.TempDir()
	branch := detectDefaultBranch(tmpDir)
	if branch != "main" {
		t.Errorf("got %q, want %q", branch, "main")
	}
}

func TestParseIntOr(t *testing.T) {
	tests := []struct {
		input    string
		fallback int
		want     int
	}{
		{"5", 0, 5},
		{"0", 1, 0},
		{"", 10, 10},
		{"abc", 3, 3},
		{"-1", 5, 5},
		{"  42  ", 0, 42},
	}

	for _, tt := range tests {
		got := parseIntOr(tt.input, tt.fallback)
		if got != tt.want {
			t.Errorf("parseIntOr(%q, %d) = %d, want %d", tt.input, tt.fallback, got, tt.want)
		}
	}
}

func TestParseCSV(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"a, b, c", []string{"a", "b", "c"}},
		{"", nil},
		{"  ", nil},
		{"single", []string{"single"}},
		{"a,,b", []string{"a", "b"}},
	}

	for _, tt := range tests {
		got := parseCSV(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("parseCSV(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseCSV(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}
