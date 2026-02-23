package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Slack  SlackConfig  `yaml:"slack"`
	Repo   RepoConfig   `yaml:"repo"`
	Limits LimitsConfig `yaml:"limits"`
	Triage TriageConfig `yaml:"triage"`
	Claude ClaudeConfig `yaml:"claude"`
	Digest DigestConfig `yaml:"digest"`
	Log    LogConfig    `yaml:"log"`
}

type SlackConfig struct {
	AppToken string   `yaml:"app_token"`
	BotToken string   `yaml:"bot_token"`
	Channels []string `yaml:"channels"`
	Triggers Triggers `yaml:"triggers"`
}

type Triggers struct {
	Emoji    string   `yaml:"emoji"`
	Keywords []string `yaml:"keywords"`
}

type RepoConfig struct {
	Path          string          `yaml:"path"`
	TestCommand   string          `yaml:"test_command"`
	LintCommand   string          `yaml:"lint_command"`
	DefaultBranch string          `yaml:"default_branch"`
	AutoMerge     bool            `yaml:"auto_merge"`
	Services      []ServiceConfig `yaml:"services"`
}

// ServiceConfig maps a subdirectory to its specific lint/test commands.
// When a tadpole changes files under Path, these commands run instead of the root ones.
type ServiceConfig struct {
	Path        string `yaml:"path"`         // e.g. "web-app", "esg-api"
	TestCommand string `yaml:"test_command"` // e.g. "make test"
	LintCommand string `yaml:"lint_command"` // e.g. "make stan && make cs"
}

type LimitsConfig struct {
	MaxConcurrent  int     `yaml:"max_concurrent"`
	MaxTurns       int     `yaml:"max_turns"`
	TimeoutMinutes int     `yaml:"timeout_minutes"`
	MaxFilesChanged int    `yaml:"max_files_changed"`
	MaxBudgetUSD   float64 `yaml:"max_budget_usd"`
	MaxRetries     int     `yaml:"max_retries"`
}

type TriageConfig struct {
	Model     string `yaml:"model"`
	AutoSpawn bool   `yaml:"auto_spawn"`
}

type ClaudeConfig struct {
	Model              string `yaml:"model"`
	AppendSystemPrompt string `yaml:"append_system_prompt"`
}

type DigestConfig struct {
	Enabled           bool     `yaml:"enabled"`              // default: false (opt-in)
	DryRun            bool     `yaml:"dry_run"`              // collect + analyze but skip spawn/notify
	BatchMinutes      int      `yaml:"batch_minutes"`        // default: 5
	MinConfidence     float64  `yaml:"min_confidence"`       // default: 0.95
	MaxAutoSpawnHour  int      `yaml:"max_auto_spawn_hour"`  // default: 3
	AllowedCategories []string `yaml:"allowed_categories"`   // default: ["bug"]
	MaxEstSize        string   `yaml:"max_est_size"`         // default: "small"
	MaxChunkSize      int      `yaml:"max_chunk_size"`       // default: 50
	ChunkTimeoutSecs  int      `yaml:"chunk_timeout_secs"`   // default: 60
}

type LogConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

func defaults() *Config {
	cwd, _ := os.Getwd()
	homeDir, _ := os.UserHomeDir()

	return &Config{
		Slack: SlackConfig{
			Triggers: Triggers{
				Emoji:    "frog",
				Keywords: []string{"toad fix", "toad help"},
			},
		},
		Repo: RepoConfig{
			Path:          cwd,
			DefaultBranch: "main",
		},
		Limits: LimitsConfig{
			MaxConcurrent:  2,
			MaxTurns:       30,
			TimeoutMinutes: 10,
			MaxFilesChanged: 5,
			MaxBudgetUSD:   1.0,
			MaxRetries:     1,
		},
		Triage: TriageConfig{
			Model:     "haiku",
			AutoSpawn: false,
		},
		Claude: ClaudeConfig{
			Model: "sonnet",
		},
		Digest: DigestConfig{
			Enabled:           false,
			BatchMinutes:      5,
			MinConfidence:     0.95,
			MaxAutoSpawnHour:  3,
			AllowedCategories: []string{"bug"},
			MaxEstSize:        "small",
			MaxChunkSize:      50,
			ChunkTimeoutSecs:  60,
		},
		Log: LogConfig{
			Level: "info",
			File:  filepath.Join(homeDir, ".toad", "toad.log"),
		},
	}
}

// Load reads configuration from YAML files and environment variables.
// Loading order: defaults → ~/.toad/config.yaml → .toad.yaml → env vars
func Load() (*Config, error) {
	cfg := defaults()

	homeDir, _ := os.UserHomeDir()
	globalPath := filepath.Join(homeDir, ".toad", "config.yaml")
	if err := loadFile(cfg, globalPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("loading global config: %w", err)
	}

	if err := loadFile(cfg, ".toad.yaml"); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("loading project config: %w", err)
	}

	applyEnv(cfg)

	return cfg, nil
}

func loadFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, cfg)
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("TOAD_SLACK_APP_TOKEN"); v != "" {
		cfg.Slack.AppToken = v
	}
	if v := os.Getenv("TOAD_SLACK_BOT_TOKEN"); v != "" {
		cfg.Slack.BotToken = v
	}
}

// Validate checks that required configuration is present.
func Validate(cfg *Config) error {
	if cfg.Slack.AppToken == "" {
		return fmt.Errorf("slack app_token is required (set in .toad.yaml or TOAD_SLACK_APP_TOKEN env)")
	}
	if cfg.Slack.BotToken == "" {
		return fmt.Errorf("slack bot_token is required (set in .toad.yaml or TOAD_SLACK_BOT_TOKEN env)")
	}
	if cfg.Repo.Path == "" {
		return fmt.Errorf("repo path is required")
	}
	if _, err := os.Stat(cfg.Repo.Path); os.IsNotExist(err) {
		return fmt.Errorf("repo path does not exist: %s", cfg.Repo.Path)
	}
	return nil
}
