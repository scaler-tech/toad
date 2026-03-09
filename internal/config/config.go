// Package config loads and validates YAML configuration with cascading defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/scaler-tech/toad/internal/toadpath"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Slack        SlackConfig        `yaml:"slack"`
	Repos        ReposConfig        `yaml:"repos"`
	Limits       LimitsConfig       `yaml:"limits"`
	Triage       TriageConfig       `yaml:"triage"`
	Claude       ClaudeConfig       `yaml:"claude"` // Deprecated: use Agent.Model and Agent.AppendSystemPrompt
	Digest       DigestConfig       `yaml:"digest"`
	IssueTracker IssueTrackerConfig `yaml:"issue_tracker"`
	VCS          VCSConfig          `yaml:"vcs"`
	Agent        AgentConfig        `yaml:"agent"`
	Log          LogConfig          `yaml:"log"`
	MCP          MCPConfig          `yaml:"mcp"`
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

type ReposConfig struct {
	SyncMinutes int          `yaml:"sync_minutes"` // periodic git fetch interval; 0 = disabled (default)
	List        []RepoConfig `yaml:"list"`
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
	MaxConcurrent    int      `yaml:"max_concurrent"`
	MaxTurns         int      `yaml:"max_turns"`
	TimeoutMinutes   int      `yaml:"timeout_minutes"`
	MaxFilesChanged  int      `yaml:"max_files_changed"`
	MaxRetries       int      `yaml:"max_retries"`
	MaxReviewRounds  int      `yaml:"max_review_rounds"`
	MaxCIFixRounds   int      `yaml:"max_ci_fix_rounds"`
	HistorySize      int      `yaml:"history_size"`
	ReviewBots       []string `yaml:"review_bots"`        // bot usernames whose PR comments can trigger fixes (e.g. "greptile[bot]")
	WorktreeTTLHours int      `yaml:"worktree_ttl_hours"` // auto-remove worktrees older than this (0 = disabled)
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
	Enabled                bool     `yaml:"enabled"`                  // default: false (opt-in)
	DryRun                 bool     `yaml:"dry_run"`                  // collect + analyze but skip spawn/notify
	BatchMinutes           int      `yaml:"batch_minutes"`            // default: 5
	MinConfidence          float64  `yaml:"min_confidence"`           // default: 0.95
	MaxAutoSpawnHour       int      `yaml:"max_auto_spawn_hour"`      // default: 3
	AllowedCategories      []string `yaml:"allowed_categories"`       // default: ["bug"]
	MaxEstSize             string   `yaml:"max_est_size"`             // default: "medium"
	MaxChunkSize           int      `yaml:"max_chunk_size"`           // default: 50
	ChunkTimeoutSecs       int      `yaml:"chunk_timeout_secs"`       // default: 120
	InvestigateTimeoutSecs int      `yaml:"investigate_timeout_secs"` // default: 600 (10 min)
	InvestigateMaxTurns    int      `yaml:"investigate_max_turns"`    // default: 25
	CommentInvestigation   bool     `yaml:"comment_investigation"`    // dry_run only: post findings as Slack reply
}

type IssueTrackerConfig struct {
	Enabled          bool   `yaml:"enabled"`
	Provider         string `yaml:"provider"`
	APIToken         string `yaml:"api_token"`
	TeamID           string `yaml:"team_id"`
	CreateIssues     bool   `yaml:"create_issues"`
	BugLabelID       string `yaml:"bug_label_id"`
	FeatureLabelID   string `yaml:"feature_label_id"`
	RespectAssignees bool   `yaml:"respect_assignees"` // defer to ticket assignee instead of spawning
	StaleDays        int    `yaml:"stale_days"`        // assignments older than this are ignored (default: 7)
}

type VCSConfig struct {
	Platform     string   `yaml:"platform"`      // "github" (default), "gitlab"
	Host         string   `yaml:"host"`          // optional: self-hosted GitLab hostname
	BotUsernames []string `yaml:"bot_usernames"` // usernames to treat as bots (GitLab)
}

type AgentConfig struct {
	Platform           string `yaml:"platform"`             // "claude" (default)
	Model              string `yaml:"model"`                // default: "sonnet"
	AppendSystemPrompt string `yaml:"append_system_prompt"` // extra instructions for all agent runs
}

type LogConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

type MCPConfig struct {
	Enabled bool     `yaml:"enabled"`
	Host    string   `yaml:"host"`
	Port    int      `yaml:"port"`
	Devs    []string `yaml:"devs"`
	Message string   `yaml:"message"` // optional message included in the connect DM
}

func defaults() *Config {
	home, _ := toadpath.Home()

	return &Config{
		Slack: SlackConfig{
			Triggers: Triggers{
				Emoji:    "frog",
				Keywords: []string{"toad fix", "toad help"},
			},
		},
		Limits: LimitsConfig{
			MaxConcurrent:    2,
			MaxTurns:         30,
			TimeoutMinutes:   10,
			MaxFilesChanged:  5,
			MaxRetries:       1,
			MaxReviewRounds:  3,
			MaxCIFixRounds:   2,
			HistorySize:      50,
			WorktreeTTLHours: 24,
		},
		Triage: TriageConfig{
			Model:     "haiku",
			AutoSpawn: false,
		},
		Claude: ClaudeConfig{}, // deprecated, kept for YAML backward compat
		Digest: DigestConfig{
			Enabled:                false,
			BatchMinutes:           5,
			MinConfidence:          0.95,
			MaxAutoSpawnHour:       3,
			AllowedCategories:      []string{"bug"},
			MaxEstSize:             "medium",
			MaxChunkSize:           50,
			ChunkTimeoutSecs:       120,
			InvestigateTimeoutSecs: 600,
			InvestigateMaxTurns:    25,
		},
		IssueTracker: IssueTrackerConfig{
			Enabled:   false,
			Provider:  "linear",
			StaleDays: 7,
		},
		VCS: VCSConfig{
			Platform: "github",
		},
		Agent: AgentConfig{
			Platform: "claude",
			Model:    "sonnet",
		},
		Log: LogConfig{
			Level: "info",
			File:  filepath.Join(home, "toad.log"),
		},
		MCP: MCPConfig{
			Enabled: false,
			Host:    "localhost",
			Port:    8099,
		},
	}
}

// Load reads configuration from YAML files and environment variables.
// Loading order: defaults → ~/.toad/config.yaml → .toad.yaml → env vars → apply repo defaults
func Load() (*Config, error) {
	cfg := defaults()

	home, _ := toadpath.Home()
	globalPath := filepath.Join(home, "config.yaml")
	if err := loadFile(cfg, globalPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("loading global config: %w", err)
	}

	if err := loadFile(cfg, ".toad.yaml"); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("loading project config: %w", err)
	}

	applyEnv(cfg)

	// Migrate deprecated claude: section to agent: — only apply when the
	// agent field is still at its default so an explicit agent: block wins.
	agentDefaults := AgentConfig{Model: "sonnet"}
	if cfg.Claude.Model != "" && cfg.Agent.Model == agentDefaults.Model {
		cfg.Agent.Model = cfg.Claude.Model
	}
	if cfg.Claude.AppendSystemPrompt != "" && cfg.Agent.AppendSystemPrompt == "" {
		cfg.Agent.AppendSystemPrompt = cfg.Claude.AppendSystemPrompt
	}

	// Apply defaults and normalize paths for individual repos
	for i := range cfg.Repos.List {
		if cfg.Repos.List[i].DefaultBranch == "" {
			cfg.Repos.List[i].DefaultBranch = "main"
		}
		if cfg.Repos.List[i].Path != "" {
			cfg.Repos.List[i].Path, _ = filepath.Abs(cfg.Repos.List[i].Path)
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
	if v := os.Getenv("TOAD_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
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
	cfg.VCS.Platform = strings.ToLower(cfg.VCS.Platform)
	validAgentPlatforms := map[string]bool{"claude": true}
	agentPlatform := strings.ToLower(cfg.Agent.Platform)
	if agentPlatform == "" {
		agentPlatform = "claude"
	}
	if !validAgentPlatforms[agentPlatform] {
		return fmt.Errorf("unsupported agent platform %q — supported: claude", cfg.Agent.Platform)
	}
	cfg.Agent.Platform = agentPlatform
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
	if len(cfg.Repos.List) == 0 {
		return fmt.Errorf("at least one repo is required")
	}
	names := make(map[string]bool)
	primaryCount := 0
	validPlatforms := map[string]bool{"github": true, "gitlab": true}
	for _, r := range cfg.Repos.List {
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
