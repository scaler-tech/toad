package config

import (
	"os"
	"strings"
	"testing"
)

// validTestCfg returns a defaults config with a valid repo for validation tests.
func validTestCfg() *Config {
	cfg := defaults()
	dir, _ := os.Getwd()
	cfg.Repos.List = []RepoConfig{{Name: "test", Path: dir, Primary: true}}
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
	if cfg.Agent.Model != "sonnet" {
		t.Errorf("default agent model should be 'sonnet', got %q", cfg.Agent.Model)
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
	cfg.Repos.List = []RepoConfig{
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
	cfg.Repos.List = []RepoConfig{
		{Name: "a", Path: dir, Primary: true},
		{Name: "b", Path: dir, Primary: true},
	}
	err := ValidateRepos(cfg)
	if err == nil {
		t.Error("expected error for multiple primary repos")
	}
}

func TestApplyEnv_LogLevel(t *testing.T) {
	cfg := defaults()

	os.Setenv("TOAD_LOG_LEVEL", "debug")
	defer os.Unsetenv("TOAD_LOG_LEVEL")

	applyEnv(cfg)

	if cfg.Log.Level != "debug" {
		t.Errorf("expected log level 'debug' from env, got %q", cfg.Log.Level)
	}
}

func TestValidate_UnsupportedGlobalVCSPlatform(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	cfg.VCS.Platform = "bitbucket"
	if err := Validate(cfg); err == nil {
		t.Error("expected error for unsupported global VCS platform")
	}
}

func TestMigrateClaudeToAgent_OnlyClaude(t *testing.T) {
	cfg := defaults()
	cfg.Claude.Model = "opus"
	cfg.Claude.AppendSystemPrompt = "be concise"
	// Agent stays at defaults — migration should apply
	cfg.Agent.Model = "sonnet"
	cfg.Agent.AppendSystemPrompt = ""

	// Simulate the migration logic from Load()
	agentDefaults := AgentConfig{Model: "sonnet"}
	if cfg.Claude.Model != "" && cfg.Agent.Model == agentDefaults.Model {
		cfg.Agent.Model = cfg.Claude.Model
	}
	if cfg.Claude.AppendSystemPrompt != "" && cfg.Agent.AppendSystemPrompt == "" {
		cfg.Agent.AppendSystemPrompt = cfg.Claude.AppendSystemPrompt
	}

	if cfg.Agent.Model != "opus" {
		t.Errorf("expected agent model migrated to 'opus', got %q", cfg.Agent.Model)
	}
	if cfg.Agent.AppendSystemPrompt != "be concise" {
		t.Errorf("expected agent system prompt migrated, got %q", cfg.Agent.AppendSystemPrompt)
	}
}

func TestMigrateClaudeToAgent_AgentWins(t *testing.T) {
	cfg := defaults()
	cfg.Claude.Model = "opus"
	cfg.Claude.AppendSystemPrompt = "old prompt"
	// Agent explicitly set — should NOT be overwritten by claude section
	cfg.Agent.Model = "haiku"
	cfg.Agent.AppendSystemPrompt = "new prompt"

	agentDefaults := AgentConfig{Model: "sonnet"}
	if cfg.Claude.Model != "" && cfg.Agent.Model == agentDefaults.Model {
		cfg.Agent.Model = cfg.Claude.Model
	}
	if cfg.Claude.AppendSystemPrompt != "" && cfg.Agent.AppendSystemPrompt == "" {
		cfg.Agent.AppendSystemPrompt = cfg.Claude.AppendSystemPrompt
	}

	if cfg.Agent.Model != "haiku" {
		t.Errorf("expected agent model to stay 'haiku', got %q", cfg.Agent.Model)
	}
	if cfg.Agent.AppendSystemPrompt != "new prompt" {
		t.Errorf("expected agent system prompt to stay 'new prompt', got %q", cfg.Agent.AppendSystemPrompt)
	}
}

func TestResolvedVCS_NilRepo(t *testing.T) {
	global := VCSConfig{Platform: "github"}
	got := ResolvedVCS(nil, global)
	if got.Platform != "github" {
		t.Errorf("nil repo should return global, got platform=%q", got.Platform)
	}
}

func TestResolvedVCS_NilOverride(t *testing.T) {
	global := VCSConfig{Platform: "github", Host: "gh.example.com"}
	repo := &RepoConfig{Name: "r", Path: "/tmp/r"} // VCS is nil
	got := ResolvedVCS(repo, global)
	if got.Platform != "github" || got.Host != "gh.example.com" {
		t.Errorf("nil VCS override should return global, got %+v", got)
	}
}

func TestResolvedVCS_PartialOverride(t *testing.T) {
	global := VCSConfig{Platform: "github", Host: "gh.example.com", BotUsernames: []string{"bot-a"}}
	repo := &RepoConfig{
		Name: "r", Path: "/tmp/r",
		VCS: &VCSConfig{Platform: "gitlab"},
	}
	got := ResolvedVCS(repo, global)
	if got.Platform != "gitlab" {
		t.Errorf("expected platform override to gitlab, got %q", got.Platform)
	}
	if got.Host != "gh.example.com" {
		t.Errorf("expected host inherited from global, got %q", got.Host)
	}
	if len(got.BotUsernames) != 1 || got.BotUsernames[0] != "bot-a" {
		t.Errorf("expected bot_usernames inherited from global, got %v", got.BotUsernames)
	}
}

func TestResolvedVCS_FullOverride(t *testing.T) {
	global := VCSConfig{Platform: "github", Host: "gh.example.com", BotUsernames: []string{"bot-a"}}
	repo := &RepoConfig{
		Name: "r", Path: "/tmp/r",
		VCS: &VCSConfig{Platform: "gitlab", Host: "gl.local", BotUsernames: []string{"renovate"}},
	}
	got := ResolvedVCS(repo, global)
	if got.Platform != "gitlab" {
		t.Errorf("expected gitlab, got %q", got.Platform)
	}
	if got.Host != "gl.local" {
		t.Errorf("expected gl.local, got %q", got.Host)
	}
	if len(got.BotUsernames) != 1 || got.BotUsernames[0] != "renovate" {
		t.Errorf("expected [renovate], got %v", got.BotUsernames)
	}
}

func TestYAMLOverlay_IntakeTicketAgentMCP(t *testing.T) {
	// Test that YAML overlay loads new intake, ticket, and agent mcp fields correctly.
	yamlContent := `
slack:
  app_token: xapp-test
  bot_token: xoxb-test
repos:
  list:
    - name: test-repo
      path: /tmp/test-repo
      primary: true
intake:
  bot_allowlist:
    - U123456
    - U789012
ticket:
  auto_file: true
  auto_file_confidence: 0.9
  triage_state_id: backlog-1
agent:
  platform: claude
  model: sonnet
  mcp_servers:
    sentry:
      url: https://sentry.example.com
      command: ""
      auth_token_env: SENTRY_API_TOKEN
    linear:
      url: ""
      command: /usr/local/bin/linear-mcp
      auth_token_env: LINEAR_API_TOKEN
  fallback_api_key_env: ANTHROPIC_API_KEY
`
	// Create temp directory and write YAML
	dir := t.TempDir()
	yamlPath := dir + "/test.yaml"
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write test YAML: %v", err)
	}

	cfg := defaults()
	if err := loadFile(cfg, yamlPath); err != nil {
		t.Fatalf("failed to load YAML: %v", err)
	}

	// Verify intake.bot_allowlist loaded
	if len(cfg.Intake.BotAllowlist) != 2 {
		t.Errorf("expected 2 bot allowlist entries, got %d", len(cfg.Intake.BotAllowlist))
	}
	if cfg.Intake.BotAllowlist[0] != "U123456" {
		t.Errorf("expected bot_allowlist[0]='U123456', got %q", cfg.Intake.BotAllowlist[0])
	}

	// Verify ticket fields loaded
	if !cfg.Ticket.AutoFile {
		t.Error("expected ticket.auto_file=true")
	}
	if cfg.Ticket.AutoFileConfidence != 0.9 {
		t.Errorf("expected ticket.auto_file_confidence=0.9, got %v", cfg.Ticket.AutoFileConfidence)
	}
	if cfg.Ticket.TriageStateID != "backlog-1" {
		t.Errorf("expected ticket.triage_state_id='backlog-1', got %q", cfg.Ticket.TriageStateID)
	}

	// Verify agent.mcp_servers loaded
	if cfg.Agent.MCPServers == nil {
		t.Fatal("expected agent.mcp_servers to be initialized")
	}
	if len(cfg.Agent.MCPServers) != 2 {
		t.Errorf("expected 2 MCP servers, got %d", len(cfg.Agent.MCPServers))
	}

	sentry, hasSentry := cfg.Agent.MCPServers["sentry"]
	if !hasSentry {
		t.Fatal("expected 'sentry' MCP server config")
	}
	if sentry.URL != "https://sentry.example.com" {
		t.Errorf("expected sentry.url='https://sentry.example.com', got %q", sentry.URL)
	}
	if sentry.AuthTokenEnv != "SENTRY_API_TOKEN" {
		t.Errorf("expected sentry.auth_token_env='SENTRY_API_TOKEN', got %q", sentry.AuthTokenEnv)
	}

	linear, hasLinear := cfg.Agent.MCPServers["linear"]
	if !hasLinear {
		t.Fatal("expected 'linear' MCP server config")
	}
	if linear.Command != "/usr/local/bin/linear-mcp" {
		t.Errorf("expected linear.command='/usr/local/bin/linear-mcp', got %q", linear.Command)
	}

	// Verify agent.fallback_api_key_env loaded
	if cfg.Agent.FallbackAPIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("expected agent.fallback_api_key_env='ANTHROPIC_API_KEY', got %q", cfg.Agent.FallbackAPIKeyEnv)
	}
}

func TestDefaults_TicketConfig(t *testing.T) {
	cfg := defaults()

	if !cfg.Ticket.AutoFile {
		t.Error("expected ticket.auto_file to default to true")
	}
	if cfg.Ticket.AutoFileConfidence != 0.85 {
		t.Errorf("expected ticket.auto_file_confidence to default to 0.85, got %v", cfg.Ticket.AutoFileConfidence)
	}
}

func TestValidate_MCPServerMissingBothURLAndCommand(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	cfg.Agent.MCPServers = map[string]MCPServerConfig{
		"invalid": {
			URL:          "",
			Command:      "",
			AuthTokenEnv: "SOME_TOKEN",
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Error("expected error when MCP server has both URL and Command empty")
	}
	if !strings.Contains(err.Error(), "agent.mcp_servers.invalid") {
		t.Errorf("expected error to mention 'agent.mcp_servers.invalid', got: %v", err)
	}
}

func TestValidate_MCPServerValidWithURL(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	cfg.Agent.MCPServers = map[string]MCPServerConfig{
		"valid": {
			URL:          "https://example.com",
			Command:      "",
			AuthTokenEnv: "SOME_TOKEN",
		},
	}
	err := Validate(cfg)
	if err != nil {
		t.Errorf("expected no error for valid MCP server with URL, got: %v", err)
	}
}

func TestValidate_MCPServerValidWithCommand(t *testing.T) {
	cfg := validTestCfg()
	cfg.Slack.AppToken = "xapp-test"
	cfg.Slack.BotToken = "xoxb-test"
	cfg.Agent.MCPServers = map[string]MCPServerConfig{
		"valid": {
			URL:          "",
			Command:      "/usr/bin/cmd",
			AuthTokenEnv: "SOME_TOKEN",
		},
	}
	err := Validate(cfg)
	if err != nil {
		t.Errorf("expected no error for valid MCP server with Command, got: %v", err)
	}
}
