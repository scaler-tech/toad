package config

import (
	"os"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := defaults()

	if cfg.Slack.Triggers.Emoji != "frog" {
		t.Errorf("default emoji should be 'frog', got %q", cfg.Slack.Triggers.Emoji)
	}
	if cfg.Repo.DefaultBranch != "main" {
		t.Errorf("default branch should be 'main', got %q", cfg.Repo.DefaultBranch)
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
	cfg := defaults()
	cfg.Slack.BotToken = "xoxb-test"
	cfg.Slack.Channels = []string{"C123"}
	err := Validate(cfg)
	if err == nil {
		t.Error("expected error for missing app_token")
	}
}

func TestValidate_MissingBotToken(t *testing.T) {
	cfg := defaults()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.Channels = []string{"C123"}
	err := Validate(cfg)
	if err == nil {
		t.Error("expected error for missing bot_token")
	}
}

func TestValidate_NoChannels(t *testing.T) {
	cfg := defaults()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	cfg.Slack.Channels = nil
	err := Validate(cfg)
	if err != nil {
		t.Error("empty channels should be valid (auto-join mode)")
	}
}

func TestValidate_Valid(t *testing.T) {
	cfg := defaults()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	cfg.Slack.Channels = []string{"C123"}
	// Repo.Path defaults to cwd which exists
	err := Validate(cfg)
	if err != nil {
		t.Errorf("unexpected validation error: %v", err)
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
