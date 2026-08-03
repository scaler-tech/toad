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
			Keywords: []string{"toad fix", "toad help"},
		},
		Repos: []repoTemplateData{
			{
				Name:          "my-app",
				Path:          "/home/dev/my-app",
				DefaultBranch: "main",
			},
		},
		Limits: limitsTemplateData{
			MaxConcurrent:  2,
			TimeoutMinutes: 10,
		},
		Triage: triageTemplateData{Model: "haiku"},
		Agent:  agentTemplateData{Model: "sonnet"},
		Digest: digestTemplateData{Enabled: true},
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
		`- "toad fix"`,
		`name: "my-app"`,
		`path: "/home/dev/my-app"`,
		`default_branch: "main"`,
		`max_concurrent: 2`,
		`timeout_minutes: 10`,
		`model: "haiku"`,
		`model: "sonnet"`,
		`enabled: true`,
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
			Keywords: []string{"toad fix"},
		},
		Repos: []repoTemplateData{
			{Name: "app", Path: "/app", DefaultBranch: "main"},
		},
		Limits: limitsTemplateData{MaxConcurrent: 2, TimeoutMinutes: 10},
		Triage: triageTemplateData{Model: "haiku"},
		Agent:  agentTemplateData{Model: "sonnet"},
		Digest: digestTemplateData{Enabled: false},
		Log:    logTemplateData{Level: "info"},
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
			Keywords: []string{"toad fix"},
		},
		Repos: []repoTemplateData{
			{Name: "app", Path: "/app", DefaultBranch: "main"},
		},
		Limits:       limitsTemplateData{MaxConcurrent: 2, TimeoutMinutes: 10},
		Triage:       triageTemplateData{Model: "haiku"},
		Agent:        agentTemplateData{Model: "sonnet"},
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
			Keywords: []string{"toad fix"},
		},
		Repos: []repoTemplateData{
			{Name: "app", Path: "/app", DefaultBranch: "main"},
		},
		Limits: limitsTemplateData{MaxConcurrent: 2, TimeoutMinutes: 10},
		Triage: triageTemplateData{Model: "haiku"},
		Agent:  agentTemplateData{Model: "sonnet"},
		Digest: digestTemplateData{Enabled: true},
		Log:    logTemplateData{Level: "info"},
	}

	out, err := renderConfig(data)
	if err != nil {
		t.Fatalf("renderConfig failed: %v", err)
	}

	result := string(out)

	// Check advanced options are commented out
	commentedOptions := []string{
		"# append_system_prompt:",
		"# batch_minutes:",
		"# min_confidence:",
	}
	for _, opt := range commentedOptions {
		if !strings.Contains(result, opt) {
			t.Errorf("missing commented-out option: %q", opt)
		}
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
