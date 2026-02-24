package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/huh/v2"
	"github.com/slack-go/slack"
	"github.com/spf13/cobra"

	"github.com/hergen/toad/internal/tui"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Set up toad in the current directory",
	Long:  "Interactive setup wizard that creates a comprehensive .toad.yaml config file.",
	RunE:  runInit,
}

func init() {
	rootCmd.AddCommand(initCmd)
}

const toadASCII = `
      @..@
     (----)
    ( >__< )
    ^^ ~~ ^^
`

func runInit(cmd *cobra.Command, args []string) error {
	const configFile = ".toad.yaml"
	theme := tui.ToadTheme()

	// ── Overwrite check ─────────────────────────────

	if _, err := os.Stat(configFile); err == nil {
		var overwrite bool
		err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(".toad.yaml already exists").
					Description("Overwrite it with a new configuration?").
					Affirmative("Overwrite").
					Negative("Cancel").
					Value(&overwrite),
			),
		).WithTheme(theme).Run()
		if err != nil {
			return err
		}
		if !overwrite {
			fmt.Println("Canceled.")
			return nil
		}
	}

	// ── Essential setup (4 screens) ─────────────────

	var appToken, botToken string

	// Screen 1: Welcome + Screen 2: Slack instructions
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Welcome to Toad").
				Description(toadASCII+`
Toad is an AI-powered coding assistant that lives in Slack.
It monitors your channels, triages messages, answers questions,
and autonomously fixes bugs by creating pull requests.

This wizard will get you up and running in under 2 minutes.`).
				Next(true).
				NextLabel("Let's go"),
		),

		huh.NewGroup(
			huh.NewNote().
				Title("Step 1 · Slack App").
				Description(`Create a Slack app before continuing:

1. Go to https://api.slack.com/apps → "From scratch"
2. Enable Socket Mode (Settings → Socket Mode → toggle on)
3. Generate an App-Level Token with scope connections:write
   (Settings → Basic Information → App-Level Tokens)
   It starts with xapp-
4. Add Bot Token Scopes (OAuth & Permissions → Scopes):
   • app_mentions:read    • channels:history
   • channels:join        • channels:read
   • chat:write           • groups:history
   • groups:read          • reactions:read
   • reactions:write      • users:read
5. Subscribe to events (Event Subscriptions → toggle on):
   • app_mention          • message.channels
   • message.groups       • reaction_added
6. Install the app to your workspace
   Copy the Bot User OAuth Token (starts with xoxb-)`).
				Next(true).
				NextLabel("I've got my tokens"),
		),

		huh.NewGroup(
			huh.NewInput().
				Title("App-Level Token").
				Description("From Basic Information → App-Level Tokens").
				Placeholder("xapp-1-...").
				Value(&appToken).
				Validate(func(s string) error {
					if !strings.HasPrefix(s, "xapp-") {
						return fmt.Errorf("must start with xapp-")
					}
					return nil
				}),
			huh.NewInput().
				Title("Bot User OAuth Token").
				Description("From OAuth & Permissions → Bot User OAuth Token").
				Placeholder("xoxb-...").
				EchoMode(huh.EchoModePassword).
				Value(&botToken).
				Validate(func(s string) error {
					if !strings.HasPrefix(s, "xoxb-") {
						return fmt.Errorf("must start with xoxb-")
					}
					return nil
				}),
		),
	).WithTheme(theme).Run()
	if err != nil {
		return err
	}

	// Validate token immediately — don't waste the user's time
	fmt.Print(tui.StyledMessage("Validating bot token..."))
	if err := validateBotToken(botToken); err != nil {
		return fmt.Errorf("token validation failed: %w", err)
	}
	fmt.Print(tui.StyledMessage("Token valid!"))

	// ── Screen 3: Repo with auto-detection ──────────

	cwd, _ := os.Getwd()
	repoPath := cwd
	repoName := filepath.Base(cwd)

	err = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Step 2 · Repository").
				Description("Configure the repo Toad will work on.\nYou can add more repos later.").
				Next(true).
				NextLabel("Continue"),
			huh.NewInput().
				Title("Repo path").
				Description("Absolute path to the git repository").
				Value(&repoPath).
				Validate(validateRepoPath),
			huh.NewInput().
				Title("Repo name").
				Description("Short name to identify this repo").
				Value(&repoName).
				Validate(huh.ValidateNotEmpty()),
		),
	).WithTheme(theme).Run()
	if err != nil {
		return err
	}

	// Auto-detect stack, commands, branch
	absRepoPath, _ := filepath.Abs(repoPath)
	detected := detectRepoDefaults(absRepoPath)

	testCommand := detected.TestCommand
	lintCommand := detected.LintCommand
	defaultBranch := detected.DefaultBranch

	// Build the detection summary
	detectedInfo := "Confirm the commands Toad should use to validate changes."
	if detected.Stack != "" {
		detectedInfo = fmt.Sprintf("Detected %s project", detected.Stack)
		if detected.Module != "" {
			detectedInfo += fmt.Sprintf(" (%s)", detected.Module)
		}
		detectedInfo += ".\nConfirm or adjust the suggested commands."
	}

	// Build branch options — include detected branch if it's non-standard
	branchOptions := []huh.Option[string]{
		huh.NewOption("main", "main"),
		huh.NewOption("master", "master"),
		huh.NewOption("develop", "develop"),
	}
	if defaultBranch != "main" && defaultBranch != "master" && defaultBranch != "develop" {
		branchOptions = append([]huh.Option[string]{
			huh.NewOption(defaultBranch+" (detected)", defaultBranch),
		}, branchOptions...)
	}

	err = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Step 2 · "+strings.TrimSpace(repoName)).
				Description(detectedInfo).
				Next(true).
				NextLabel("Continue"),
			huh.NewInput().
				Title("Test command").
				Placeholder("e.g. go test ./...").
				Value(&testCommand),
			huh.NewInput().
				Title("Lint command").
				Placeholder("e.g. golangci-lint run").
				Value(&lintCommand),
			huh.NewSelect[string]().
				Title("Default branch").
				Options(branchOptions...).
				Value(&defaultBranch),
		),
	).WithTheme(theme).Run()
	if err != nil {
		return err
	}

	// ── Screen 4: Toad King ─────────────────────────

	toadKingMode := "dry-run"

	err = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Step 3 · Toad King").
				Description("Toad King passively monitors your Slack channels and\n"+
					"identifies bugs that could be fixed automatically.\n\n"+
					"Dry-run mode shows opportunities in the dashboard\n"+
					"without acting. Live mode auto-spawns fixes.").
				Options(
					huh.NewOption("Dry-run — monitor and report (recommended)", "dry-run"),
					huh.NewOption("Live — auto-fix high-confidence bugs", "live"),
					huh.NewOption("Off — disable passive monitoring", "off"),
				).
				Value(&toadKingMode),
		),
	).WithTheme(theme).Run()
	if err != nil {
		return err
	}

	// ── Advanced settings (optional) ────────────────

	var customize bool
	err = huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Customize advanced settings?").
				Description("Channels, triggers, AI models, limits, issue tracker.\nDefaults work great for most setups.").
				Affirmative("Yes, customize").
				Negative("No, use defaults").
				Value(&customize),
		),
	).WithTheme(theme).Run()
	if err != nil {
		return err
	}

	// Defaults
	var channelsRaw string
	triggerEmoji := "frog"
	triggerKeywordsRaw := "toad fix, toad help"
	claudeModel := "sonnet"
	triageModel := "haiku"
	var autoSpawn bool
	maxConcurrentStr := "2"
	maxTurnsStr := "30"
	timeoutStr := "10"
	maxFilesStr := "5"
	maxRetriesStr := "1"
	var autoMerge bool
	var prLabelsRaw string
	var issueEnabled bool
	issueProvider := "linear"
	var issueAPIToken, issueTeamID string
	var issueCreate bool
	logLevel := "info"

	if customize {
		err = runAdvancedSetup(theme,
			&channelsRaw, &triggerEmoji, &triggerKeywordsRaw,
			&claudeModel, &triageModel, &autoSpawn,
			&maxConcurrentStr, &maxTurnsStr, &timeoutStr, &maxFilesStr, &maxRetriesStr,
			&autoMerge, &prLabelsRaw,
			&issueEnabled, &issueProvider, &issueAPIToken, &issueTeamID, &issueCreate,
			&logLevel,
		)
		if err != nil {
			return err
		}
	}

	// ── Multi-repo loop ─────────────────────────────

	firstRepo := repoTemplateData{
		Name:          strings.TrimSpace(repoName),
		Path:          absRepoPath,
		TestCommand:   testCommand,
		LintCommand:   lintCommand,
		DefaultBranch: defaultBranch,
		AutoMerge:     autoMerge,
		PRLabels:      parseCSV(prLabelsRaw),
	}
	repos := []repoTemplateData{firstRepo}

	for {
		var addAnother bool
		err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Add another repository?").
					Description(fmt.Sprintf("Currently configured: %d repo(s)", len(repos))).
					Affirmative("Yes").
					Negative("No, finish setup").
					Value(&addAnother),
			),
		).WithTheme(theme).Run()
		if err != nil || !addAnother {
			break
		}

		repo, err := runRepoSubForm(theme)
		if err != nil {
			break
		}
		repos = append(repos, repo)
	}

	// Primary repo selection (if multiple)
	if len(repos) > 1 {
		options := make([]huh.Option[int], len(repos))
		for i, r := range repos {
			options[i] = huh.NewOption(r.Name, i)
		}
		var primaryIdx int
		err := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[int]().
					Title("Which repo is primary?").
					Description("Toad falls back to the primary repo when it can't determine which repo a message is about").
					Options(options...).
					Value(&primaryIdx),
			),
		).WithTheme(theme).Run()
		if err == nil {
			repos[primaryIdx].Primary = true
		}
	}

	// ── Build digest config from Toad King mode ─────

	digestEnabled := toadKingMode != "off"
	digestDryRun := toadKingMode != "live"

	// ── Render and write config ─────────────────────

	data := templateData{
		Slack: slackTemplateData{
			AppToken: appToken,
			BotToken: botToken,
			Channels: parseCSV(channelsRaw),
			Emoji:    triggerEmoji,
			Keywords: parseCSV(triggerKeywordsRaw),
		},
		Repos: repos,
		Limits: limitsTemplateData{
			MaxConcurrent:   parseIntOr(maxConcurrentStr, 2),
			MaxTurns:        parseIntOr(maxTurnsStr, 30),
			TimeoutMinutes:  parseIntOr(timeoutStr, 10),
			MaxFilesChanged: parseIntOr(maxFilesStr, 5),
			MaxRetries:      parseIntOr(maxRetriesStr, 1),
		},
		Triage: triageTemplateData{
			Model:     triageModel,
			AutoSpawn: autoSpawn,
		},
		Claude: claudeTemplateData{
			Model: claudeModel,
		},
		Digest: digestTemplateData{
			Enabled: digestEnabled,
			DryRun:  digestDryRun,
		},
		IssueTracker: issueTrackerTemplateData{
			Enabled:      issueEnabled,
			Provider:     issueProvider,
			APIToken:     issueAPIToken,
			TeamID:       issueTeamID,
			CreateIssues: issueCreate,
		},
		Log: logTemplateData{
			Level: logLevel,
		},
	}

	configData, err := renderConfig(data)
	if err != nil {
		return fmt.Errorf("rendering config: %w", err)
	}

	if err := os.WriteFile(configFile, configData, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", configFile, err)
	}

	// ── Summary ─────────────────────────────────────

	fmt.Println()
	fmt.Print(tui.StyledMessage("Setup complete!"))
	fmt.Printf("  Config written to %s\n", configFile)
	fmt.Printf("  Repo:      %s (%s)\n", firstRepo.Name, detected.Stack)
	if toadKingMode == "live" {
		fmt.Printf("  Toad King: live mode\n")
	} else if toadKingMode == "dry-run" {
		fmt.Printf("  Toad King: dry-run (monitor only)\n")
	} else {
		fmt.Printf("  Toad King: off\n")
	}
	if len(repos) > 1 {
		fmt.Printf("  Repos:     %d configured\n", len(repos))
	}
	fmt.Println()
	fmt.Println("  Start toad with: toad")
	fmt.Println()

	return nil
}

// runAdvancedSetup shows optional configuration screens.
func runAdvancedSetup(
	theme huh.Theme,
	channelsRaw *string, triggerEmoji *string, triggerKeywordsRaw *string,
	claudeModel *string, triageModel *string, autoSpawn *bool,
	maxConcurrentStr *string, maxTurnsStr *string, timeoutStr *string, maxFilesStr *string, maxRetriesStr *string,
	autoMerge *bool, prLabelsRaw *string,
	issueEnabled *bool, issueProvider *string, issueAPIToken *string, issueTeamID *string, issueCreate *bool,
	logLevel *string,
) error {
	return huh.NewForm(
		// Channels & triggers
		huh.NewGroup(
			huh.NewNote().
				Title("Channels & Triggers").
				Description("Configure which channels Toad monitors and what triggers it.").
				Next(true).
				NextLabel("Continue"),
			huh.NewInput().
				Title("Channel IDs").
				Description("Comma-separated Slack channel IDs (leave empty for all public channels)").
				Placeholder("C0123456789, C9876543210").
				Value(channelsRaw),
			huh.NewInput().
				Title("Trigger emoji").
				Description("React with this emoji to trigger Toad on a message").
				Value(triggerEmoji),
			huh.NewInput().
				Title("Trigger keywords").
				Description("Comma-separated phrases that trigger Toad").
				Value(triggerKeywordsRaw),
		),

		// AI models
		huh.NewGroup(
			huh.NewNote().
				Title("AI Models").
				Description("Configure which models Toad uses.\nAll models run on your existing subscription.").
				Next(true).
				NextLabel("Continue"),
			huh.NewSelect[string]().
				Title("Tadpole model (code generation)").
				Options(
					huh.NewOption("Sonnet — balanced speed & quality (recommended)", "sonnet"),
					huh.NewOption("Opus — most capable, slower", "opus"),
					huh.NewOption("Haiku — fastest, less capable", "haiku"),
				).
				Value(claudeModel),
			huh.NewSelect[string]().
				Title("Triage model (message classification)").
				Options(
					huh.NewOption("Haiku — fast, cheap (recommended)", "haiku"),
					huh.NewOption("Sonnet — more accurate", "sonnet"),
				).
				Value(triageModel),
			huh.NewConfirm().
				Title("Auto-spawn without trigger?").
				Description("Skip the emoji/keyword trigger — spawn tadpoles for any detected bug or feature request").
				Affirmative("Yes").
				Negative("No, require trigger").
				Value(autoSpawn),
		),

		// Repo options
		huh.NewGroup(
			huh.NewConfirm().
				Title("Enable auto-merge on PRs?").
				Description("Automatically merge Toad PRs when CI passes (requires GitHub repo setting)").
				Affirmative("Yes").
				Negative("No").
				Value(autoMerge),
			huh.NewInput().
				Title("PR labels").
				Description("Comma-separated labels to apply to Toad PRs (optional)").
				Placeholder("toad, automated").
				Value(prLabelsRaw),
		),

		// Safety limits
		huh.NewGroup(
			huh.NewNote().
				Title("Safety Limits").
				Description("Guard rails for autonomous runs. Defaults are conservative.").
				Next(true).
				NextLabel("Continue"),
			huh.NewInput().
				Title("Max concurrent tadpoles").
				Value(maxConcurrentStr).
				Validate(validatePositiveInt),
			huh.NewInput().
				Title("Max turns per tadpole").
				Value(maxTurnsStr).
				Validate(validatePositiveInt),
			huh.NewInput().
				Title("Timeout (minutes)").
				Value(timeoutStr).
				Validate(validatePositiveInt),
			huh.NewInput().
				Title("Max files changed").
				Description("Abort if a tadpole touches more files than this").
				Value(maxFilesStr).
				Validate(validatePositiveInt),
			huh.NewInput().
				Title("Max retries").
				Value(maxRetriesStr).
				Validate(validateNonNegativeInt),
		),

		// Issue tracker
		huh.NewGroup(
			huh.NewConfirm().
				Title("Enable issue tracker integration?").
				Description("Connect to Linear to track and create issues from Slack").
				Affirmative("Yes").
				Negative("No").
				Value(issueEnabled),
		),

		// Issue tracker config (hidden unless enabled)
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Issue tracker provider").
				Options(huh.NewOption("Linear", "linear")).
				Value(issueProvider),
			huh.NewInput().
				Title("API Token").
				Description("Linear API token (or set TOAD_LINEAR_API_TOKEN env var later)").
				EchoMode(huh.EchoModePassword).
				Placeholder("lin_api_...").
				Value(issueAPIToken),
			huh.NewInput().
				Title("Team ID").
				Description("Linear team ID for issue creation").
				Value(issueTeamID),
			huh.NewConfirm().
				Title("Auto-create issues?").
				Description("Create Linear issues for bugs/features discovered in Slack").
				Affirmative("Yes").
				Negative("No").
				Value(issueCreate),
		).WithHideFunc(func() bool { return !*issueEnabled }),

		// Log level
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Log level").
				Options(
					huh.NewOption("Debug", "debug"),
					huh.NewOption("Info (default)", "info"),
					huh.NewOption("Warn", "warn"),
					huh.NewOption("Error", "error"),
				).
				Value(logLevel),
		),
	).WithTheme(theme).Run()
}

// runRepoSubForm runs a standalone form to configure an additional repo.
func runRepoSubForm(theme huh.Theme) (repoTemplateData, error) {
	var repoPath, repoName string

	// Step 1: Path & name (empty default for additional repos)
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Repo path").
				Description("Absolute path to the git repository").
				Placeholder("/path/to/repo").
				Value(&repoPath).
				Validate(validateRepoPath),
			huh.NewInput().
				Title("Repo name").
				Description("Short name to identify this repo").
				Value(&repoName).
				Validate(huh.ValidateNotEmpty()),
		),
	).WithTheme(theme).Run()
	if err != nil {
		return repoTemplateData{}, err
	}

	// Auto-detect
	absPath, _ := filepath.Abs(repoPath)
	detected := detectRepoDefaults(absPath)

	testCommand := detected.TestCommand
	lintCommand := detected.LintCommand
	defaultBranch := detected.DefaultBranch

	detectedInfo := "Configure commands for this repo."
	if detected.Stack != "" {
		detectedInfo = fmt.Sprintf("Detected %s project", detected.Stack)
		if detected.Module != "" {
			detectedInfo += fmt.Sprintf(" (%s)", detected.Module)
		}
	}

	branchOptions := []huh.Option[string]{
		huh.NewOption("main", "main"),
		huh.NewOption("master", "master"),
		huh.NewOption("develop", "develop"),
	}
	if defaultBranch != "main" && defaultBranch != "master" && defaultBranch != "develop" {
		branchOptions = append([]huh.Option[string]{
			huh.NewOption(defaultBranch+" (detected)", defaultBranch),
		}, branchOptions...)
	}

	// Step 2: Commands
	err = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Repository: "+strings.TrimSpace(repoName)).
				Description(detectedInfo).
				Next(true).
				NextLabel("Continue"),
			huh.NewInput().
				Title("Test command").
				Placeholder("e.g. go test ./...").
				Value(&testCommand),
			huh.NewInput().
				Title("Lint command").
				Placeholder("e.g. golangci-lint run").
				Value(&lintCommand),
			huh.NewSelect[string]().
				Title("Default branch").
				Options(branchOptions...).
				Value(&defaultBranch),
		),
	).WithTheme(theme).Run()
	if err != nil {
		return repoTemplateData{}, err
	}

	return repoTemplateData{
		Name:          strings.TrimSpace(repoName),
		Path:          absPath,
		TestCommand:   testCommand,
		LintCommand:   lintCommand,
		DefaultBranch: defaultBranch,
	}, nil
}

// ── Validators ──────────────────────────────────────

func validateRepoPath(s string) error {
	abs, err := filepath.Abs(s)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}
	info, err := os.Stat(abs)
	if os.IsNotExist(err) {
		return fmt.Errorf("path does not exist: %s", abs)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", abs)
	}
	if _, err := os.Stat(filepath.Join(abs, ".git")); os.IsNotExist(err) {
		return fmt.Errorf("not a git repository: %s", abs)
	}
	return nil
}

func validatePositiveInt(s string) error {
	n := parseIntOr(s, -1)
	if n <= 0 {
		return fmt.Errorf("must be a positive integer")
	}
	return nil
}

func validateNonNegativeInt(s string) error {
	n := parseIntOr(s, -1)
	if n < 0 {
		return fmt.Errorf("must be a non-negative integer")
	}
	return nil
}

// ── Helpers ─────────────────────────────────────────

// parseCSV splits a comma-separated string into trimmed non-empty parts.
func parseCSV(s string) []string {
	var result []string
	for _, part := range strings.Split(s, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// validateBotToken checks the bot token works with a simple auth test.
func validateBotToken(botToken string) error {
	api := slack.New(botToken)
	_, err := api.AuthTest()
	if err != nil {
		return fmt.Errorf("auth test failed (is your bot token correct?): %w", err)
	}
	return nil
}
