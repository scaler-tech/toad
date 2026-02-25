// Package config loads and validates YAML configuration with cascading defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Slack        SlackConfig        `yaml:"slack"`
	Repos        []RepoConfig       `yaml:"repos"`
	Limits       LimitsConfig       `yaml:"limits"`
	Triage       TriageConfig       `yaml:"triage"`
	Claude       ClaudeConfig       `yaml:"claude"`
	Digest       DigestConfig       `yaml:"digest"`
	IssueTracker IssueTrackerConfig `yaml:"issue_tracker"`
	VCS          VCSConfig          `yaml:"vcs"`
	Log          LogConfig          `yaml:"log"`
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
	Name           string          `yaml:"name"`
	Path           string          `yaml:"path"`
	Description    string          `yaml:"description"`
	Primary        bool            `yaml:"primary"`
	TestCommand    string          `yaml:"test_command"`
	LintCommand    string          `yaml:"lint_command"`
	DefaultBranch  string          `yaml:"default_branch"`
	AutoMerge      bool            `yaml:"auto_merge"`
	MergeBotFixups bool            `yaml:"merge_bot_fixups"`
	PRLabels       []string        `yaml:"pr_labels"`
	Services       []ServiceConfig `yaml:"services"`
	VCS            *VCSConfig      `yaml:"vcs"` // optional, overrides global VCS config
}

// ServiceConfig maps a subdirectory to its specific lint/test commands.
// When a tadpole changes files under Path, these commands run instead of the root ones.
type ServiceConfig struct {
	Path        string `yaml:"path"`         // e.g. "web-app", "esg-api"
	TestCommand string `yaml:"test_command"` // e.g. "make test"
	LintCommand string `yaml:"lint_command"` // e.g. "make stan && make cs"
}

type LimitsConfig struct {
	MaxConcurrent   int `yaml:"max_concurrent"`
	MaxTurns        int `yaml:"max_turns"`
	TimeoutMinutes  int `yaml:"timeout_minutes"`
	MaxFilesChanged int `yaml:"max_files_changed"`
	MaxRetries      int `yaml:"max_retries"`
	MaxReviewRounds int `yaml:"max_review_rounds"`
	MaxCIFixRounds  int `yaml:"max_ci_fix_rounds"`
	HistorySize     int `yaml:"history_size"`
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
	Enabled           bool     `yaml:"enabled"`             // default: false (opt-in)
	DryRun            bool     `yaml:"dry_run"`             // collect + analyze but skip spawn/notify
	BatchMinutes      int      `yaml:"batch_minutes"`       // default: 5
	MinConfidence     float64  `yaml:"min_confidence"`      // default: 0.95
	MaxAutoSpawnHour  int      `yaml:"max_auto_spawn_hour"` // default: 3
	AllowedCategories []string `yaml:"allowed_categories"`  // default: ["bug"]
	MaxEstSize        string   `yaml:"max_est_size"`        // default: "small"
	MaxChunkSize      int      `yaml:"max_chunk_size"`      // default: 50
	ChunkTimeoutSecs  int      `yaml:"chunk_timeout_secs"`  // default: 60
}

type IssueTrackerConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Provider       string `yaml:"provider"`
	APIToken       string `yaml:"api_token"`
	TeamID         string `yaml:"team_id"`
	CreateIssues   bool   `yaml:"create_issues"`
	BugLabelID     string `yaml:"bug_label_id"`
	FeatureLabelID string `yaml:"feature_label_id"`
}

type VCSConfig struct {
	Platform     string   `yaml:"platform"`      // "github" (default), "gitlab"
	Host         string   `yaml:"host"`           // optional: self-hosted GitLab hostname
	BotUsernames []string `yaml:"bot_usernames"`  // usernames to treat as bots (GitLab)
}

type LogConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

func defaults() *Config {
	homeDir, _ := os.UserHomeDir()

	return &Config{
		Slack: SlackConfig{
			Triggers: Triggers{
				Emoji:    "frog",
				Keywords: []string{"toad fix", "toad help"},
			},
		},
		Limits: LimitsConfig{
			MaxConcurrent:   2,
			MaxTurns:        30,
			TimeoutMinutes:  10,
			MaxFilesChanged: 5,
			MaxRetries:      1,
			MaxReviewRounds: 3,
			MaxCIFixRounds:  2,
			HistorySize:     50,
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
			ChunkTimeoutSecs:  120,
		},
		IssueTracker: IssueTrackerConfig{
			Enabled:  false,
			Provider: "linear",
		},
		VCS: VCSConfig{
			Platform: "github",
		},
		Log: LogConfig{
			Level: "info",
			File:  filepath.Join(homeDir, ".toad", "toad.log"),
		},
	}
}

// Load reads configuration from YAML files and environment variables.
// Loading order: defaults → ~/.toad/config.yaml → .toad.yaml → env vars → apply repo defaults
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

	// Apply defaults and normalize paths for individual repos
	for i := range cfg.Repos {
		if cfg.Repos[i].DefaultBranch == "" {
			cfg.Repos[i].DefaultBranch = "main"
		}
		if cfg.Repos[i].Path != "" {
			cfg.Repos[i].Path, _ = filepath.Abs(cfg.Repos[i].Path)
		}
	}

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
	if v := os.Getenv("TOAD_LINEAR_API_TOKEN"); v != "" {
		cfg.IssueTracker.APIToken = v
	}
	// TOAD_GITLAB_HOST sets the global VCS host for self-hosted GitLab.
	// Per-repo host overrides must be set via YAML (repos[].vcs.host).
	if v := os.Getenv("TOAD_GITLAB_HOST"); v != "" {
		cfg.VCS.Host = v
	}
}

// Validate checks that required configuration is present.
// It ensures repos are normalized before validation.
func Validate(cfg *Config) error {
	if cfg.Slack.AppToken == "" {
		return fmt.Errorf("slack app_token is required (set in .toad.yaml or TOAD_SLACK_APP_TOKEN env)")
	}
	if cfg.Slack.BotToken == "" {
		return fmt.Errorf("slack bot_token is required (set in .toad.yaml or TOAD_SLACK_BOT_TOKEN env)")
	}
	validPlatforms := map[string]bool{"github": true, "gitlab": true}
	if cfg.VCS.Platform != "" && !validPlatforms[strings.ToLower(cfg.VCS.Platform)] {
		return fmt.Errorf("unsupported VCS platform %q — supported: github, gitlab", cfg.VCS.Platform)
	}
	if err := ValidateRepos(cfg); err != nil {
		return err
	}
	if cfg.IssueTracker.Enabled && cfg.IssueTracker.CreateIssues {
		if cfg.IssueTracker.APIToken == "" {
			return fmt.Errorf("issue_tracker.api_token is required when create_issues is enabled (set in config or TOAD_LINEAR_API_TOKEN env)")
		}
		if cfg.IssueTracker.TeamID == "" {
			return fmt.Errorf("issue_tracker.team_id is required when create_issues is enabled")
		}
	}
	return nil
}

// ValidateRepos checks that repos configuration is valid.
func ValidateRepos(cfg *Config) error {
	if len(cfg.Repos) == 0 {
		return fmt.Errorf("at least one repo is required")
	}
	names := make(map[string]bool)
	primaryCount := 0
	validPlatforms := map[string]bool{"github": true, "gitlab": true}
	for _, r := range cfg.Repos {
		if r.Path == "" {
			return fmt.Errorf("repo %q: path is required", r.Name)
		}
		if _, err := os.Stat(r.Path); os.IsNotExist(err) {
			return fmt.Errorf("repo path does not exist: %s", r.Path)
		}
		if r.Name == "" {
			return fmt.Errorf("repo at %s: name is required", r.Path)
		}
		if names[r.Name] {
			return fmt.Errorf("duplicate repo name: %q", r.Name)
		}
		names[r.Name] = true
		if r.Primary {
			primaryCount++
		}
		if r.VCS != nil && r.VCS.Platform != "" && !validPlatforms[strings.ToLower(r.VCS.Platform)] {
			return fmt.Errorf("repo %q: unsupported VCS platform %q — supported: github, gitlab", r.Name, r.VCS.Platform)
		}
	}
	if primaryCount > 1 {
		return fmt.Errorf("at most one repo can be marked primary")
	}
	return nil
}

// PrimaryRepo returns the primary repo, or the single repo if there's only one,
// or nil if there's no primary and multiple repos.
func PrimaryRepo(repos []RepoConfig) *RepoConfig {
	if len(repos) == 1 {
		return &repos[0]
	}
	for i := range repos {
		if repos[i].Primary {
			return &repos[i]
		}
	}
	return nil
}

// RepoByName returns the repo with the given name, or nil if not found.
func RepoByName(repos []RepoConfig, name string) *RepoConfig {
	for i := range repos {
		if repos[i].Name == name {
			return &repos[i]
		}
	}
	return nil
}

// RepoByPath returns the repo with the given path, or nil if not found.
func RepoByPath(repos []RepoConfig, path string) *RepoConfig {
	for i := range repos {
		if repos[i].Path == path {
			return &repos[i]
		}
	}
	return nil
}

// ResolvedVCS merges a repo's optional VCS override with the global VCS config.
// Per-repo fields take precedence when set; unset fields inherit from global.
func ResolvedVCS(repo *RepoConfig, global VCSConfig) VCSConfig {
	if repo == nil || repo.VCS == nil {
		return global
	}
	result := global
	if repo.VCS.Platform != "" {
		result.Platform = repo.VCS.Platform
	}
	if repo.VCS.Host != "" {
		result.Host = repo.VCS.Host
	}
	if len(repo.VCS.BotUsernames) > 0 {
		result.BotUsernames = repo.VCS.BotUsernames
	}
	return result
}
