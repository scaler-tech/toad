package config

import (
	"os"
	"testing"
)

// validTestCfg returns a defaults config with a valid repo for validation tests.
func validTestCfg() *Config {
	cfg := defaults()
	dir, _ := os.Getwd()
	cfg.Repos = []RepoConfig{{Name: "test", Path: dir, Primary: true}}
	return cfg
}

func TestDefaults(t *testing.T) {
	cfg := defaults()

	if cfg.Slack.Triggers.Emoji != "frog" {
		t.Errorf("default emoji should be 'frog', got %q", cfg.Slack.Triggers.Emoji)
	}
	if cfg.Limits.MaxConcurrent != 2 {
		t.Errorf("default max_concurrent should be 2, got %d", cfg.Limits.MaxConcurrent)
	}
	if cfg.Limits.MaxTurns != 30 {
		t.Errorf("default max_turns should be 30, got %d", cfg.Limits.MaxTurns)
	}
	if cfg.Limits.TimeoutMinutes != 10 {
		t.Errorf("default timeout should be 10, got %d", cfg.Limits.TimeoutMinutes)
	}
	if cfg.Limits.MaxFilesChanged != 5 {
		t.Errorf("default max_files should be 5, got %d", cfg.Limits.MaxFilesChanged)
	}
	if cfg.Limits.MaxRetries != 1 {
		t.Errorf("default max_retries should be 1, got %d", cfg.Limits.MaxRetries)
	}
	if cfg.Triage.Model != "haiku" {
		t.Errorf("default triage model should be 'haiku', got %q", cfg.Triage.Model)
	}
	if cfg.Claude.Model != "sonnet" {
		t.Errorf("default claude model should be 'sonnet', got %q", cfg.Claude.Model)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("default log level should be 'info', got %q", cfg.Log.Level)
	}
}

func TestValidate_MissingAppToken(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.BotToken = "xoxb-test"
	err := Validate(cfg)
	if err == nil {
		t.Error("expected error for missing app_token")
	}
}

func TestValidate_MissingBotToken(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.AppToken = "xapp-test"
	err := Validate(cfg)
	if err == nil {
		t.Error("expected error for missing bot_token")
	}
}

func TestValidate_NoChannels(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	cfg.Slack.Channels = nil
	err := Validate(cfg)
	if err != nil {
		t.Errorf("empty channels should be valid (auto-join mode): %v", err)
	}
}

func TestValidate_Valid(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	err := Validate(cfg)
	if err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}

func TestValidate_NoRepos(t *testing.T) {
	cfg := defaults()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	err := Validate(cfg)
	if err == nil {
		t.Error("expected error for missing repos")
	}
}

func TestApplyEnv(t *testing.T) {
	cfg := defaults()

	os.Setenv("TOAD_SLACK_APP_TOKEN", "xapp-from-env")
	os.Setenv("TOAD_SLACK_BOT_TOKEN", "xoxb-from-env")
	defer os.Unsetenv("TOAD_SLACK_APP_TOKEN")
	defer os.Unsetenv("TOAD_SLACK_BOT_TOKEN")

	applyEnv(cfg)

	if cfg.Slack.AppToken != "xapp-from-env" {
		t.Errorf("expected app_token from env, got %q", cfg.Slack.AppToken)
	}
	if cfg.Slack.BotToken != "xoxb-from-env" {
		t.Errorf("expected bot_token from env, got %q", cfg.Slack.BotToken)
	}
}

func TestApplyEnv_LinearToken(t *testing.T) {
	cfg := defaults()

	os.Setenv("TOAD_LINEAR_API_TOKEN", "lin_api_test123")
	defer os.Unsetenv("TOAD_LINEAR_API_TOKEN")

	applyEnv(cfg)

	if cfg.IssueTracker.APIToken != "lin_api_test123" {
		t.Errorf("expected linear API token from env, got %q", cfg.IssueTracker.APIToken)
	}
}

func TestValidate_IssueTrackerCreateMissingToken(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	cfg.IssueTracker.Enabled = true
	cfg.IssueTracker.CreateIssues = true
	cfg.IssueTracker.TeamID = "team-123"
	// No API token
	err := Validate(cfg)
	if err == nil {
		t.Error("expected error for missing api_token when create_issues enabled")
	}
}

func TestValidate_IssueTrackerCreateMissingTeamID(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	cfg.IssueTracker.Enabled = true
	cfg.IssueTracker.CreateIssues = true
	cfg.IssueTracker.APIToken = "lin_api_test"
	// No team ID
	err := Validate(cfg)
	if err == nil {
		t.Error("expected error for missing team_id when create_issues enabled")
	}
}

func TestValidate_IssueTrackerDetectOnlyNoValidation(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	cfg.IssueTracker.Enabled = true
	cfg.IssueTracker.CreateIssues = false // detect-only, no token needed
	err := Validate(cfg)
	if err != nil {
		t.Errorf("detect-only mode should not require token/team: %v", err)
	}
}

func TestDefaults_IssueTracker(t *testing.T) {
	cfg := defaults()

	if cfg.IssueTracker.Enabled {
		t.Error("issue tracker should be disabled by default")
	}
	if cfg.IssueTracker.Provider != "linear" {
		t.Errorf("default provider should be 'linear', got %q", cfg.IssueTracker.Provider)
	}
	if cfg.IssueTracker.CreateIssues {
		t.Error("create_issues should be false by default")
	}
}

func TestPrimaryRepo_Single(t *testing.T) {
	repos := []RepoConfig{{Name: "only", Path: "/tmp/only"}}
	p := PrimaryRepo(repos)
	if p == nil || p.Name != "only" {
		t.Error("single repo should be returned as primary")
	}
}

func TestPrimaryRepo_ExplicitPrimary(t *testing.T) {
	repos := []RepoConfig{
		{Name: "a", Path: "/tmp/a"},
		{Name: "b", Path: "/tmp/b", Primary: true},
	}
	p := PrimaryRepo(repos)
	if p == nil || p.Name != "b" {
		t.Error("should return the explicitly primary repo")
	}
}

func TestPrimaryRepo_NoPrimary(t *testing.T) {
	repos := []RepoConfig{
		{Name: "a", Path: "/tmp/a"},
		{Name: "b", Path: "/tmp/b"},
	}
	p := PrimaryRepo(repos)
	if p != nil {
		t.Error("should return nil when no primary and multiple repos")
	}
}

func TestValidateRepos_DuplicateNames(t *testing.T) {
	dir := t.TempDir()
	cfg := defaults()
	cfg.Repos = []RepoConfig{
		{Name: "dup", Path: dir},
		{Name: "dup", Path: dir},
	}
	err := ValidateRepos(cfg)
	if err == nil {
		t.Error("expected error for duplicate repo names")
	}
}

func TestValidateRepos_MultiplePrimary(t *testing.T) {
	dir := t.TempDir()
	cfg := defaults()
	cfg.Repos = []RepoConfig{
		{Name: "a", Path: dir, Primary: true},
		{Name: "b", Path: dir, Primary: true},
	}
	err := ValidateRepos(cfg)
	if err == nil {
		t.Error("expected error for multiple primary repos")
	}
}
