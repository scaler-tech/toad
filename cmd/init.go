package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/slack-go/slack"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/hergen/toad/internal/tui"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Set up toad in the current directory",
	Long:  "Interactive setup wizard that creates a .toad.yaml config file with Slack credentials.",
	RunE:  runInit,
}

func init() {
	rootCmd.AddCommand(initCmd)
}

// outputConfig is a purpose-built struct for marshaling .toad.yaml.
// It omits log.file so runtime defaults apply.
type outputConfig struct {
	Slack outputSlackConfig `yaml:"slack"`
	Repos []outputRepoConfig `yaml:"repos"`
}

type outputSlackConfig struct {
	AppToken string   `yaml:"app_token"`
	BotToken string   `yaml:"bot_token"`
	Channels []string `yaml:"channels,omitempty"`
}

type outputRepoConfig struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

func runInit(cmd *cobra.Command, args []string) error {
	const configFile = ".toad.yaml"

	formOpts := []tea.ProgramOption{tea.WithAltScreen()}
	theme := tui.ToadTheme()

	// Form 1 (conditional): overwrite confirmation
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
		).
			WithProgramOptions(formOpts...).
			WithTheme(theme).
			Run()
		if err != nil {
			return err
		}
		if !overwrite {
			fmt.Println("Canceled.")
			return nil
		}
	}

	// Form 2: Slack setup guide → token inputs → repo config
	var appToken, botToken string

	// Default repo path to cwd
	cwd, _ := os.Getwd()
	repoPath := cwd
	repoName := filepath.Base(cwd)

	err := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Slack App Setup").
				Description(`Before continuing, set up a Slack app:

1. Create an app at https://api.slack.com/apps → "From scratch"
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
   (OAuth & Permissions → Install to Workspace)
   Copy the Bot User OAuth Token — it starts with xoxb-

Toad will auto-join all public channels on startup.
For private channels, use /invite @YourBot.`).
				Next(true).
				NextLabel("I'm ready"),
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
		huh.NewGroup(
			huh.NewNote().
				Title("Repository").
				Description("Configure the repo toad will monitor and fix.\nYou can add more repos later in .toad.yaml.").
				Next(true).
				NextLabel("Continue"),
			huh.NewInput().
				Title("Repo name").
				Description("A short name to identify this repo").
				Value(&repoName).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("name is required")
					}
					return nil
				}),
			huh.NewInput().
				Title("Repo path").
				Description("Absolute path to the git repository").
				Value(&repoPath).
				Validate(func(s string) error {
					abs, err := filepath.Abs(s)
					if err != nil {
						return fmt.Errorf("invalid path: %w", err)
					}
					if _, err := os.Stat(abs); os.IsNotExist(err) {
						return fmt.Errorf("path does not exist: %s", abs)
					}
					return nil
				}),
		),
	).
		WithProgramOptions(formOpts...).
		WithTheme(theme).
		Run()
	if err != nil {
		return err
	}

	// Validate the bot token
	fmt.Print(tui.StyledMessage("Validating tokens..."))

	if err := validateBotToken(botToken); err != nil {
		return fmt.Errorf("token validation failed: %w", err)
	}

	// Normalize repo path to absolute
	absRepoPath, _ := filepath.Abs(repoPath)

	// Write config
	out := outputConfig{
		Slack: outputSlackConfig{
			AppToken: appToken,
			BotToken: botToken,
		},
		Repos: []outputRepoConfig{
			{Name: strings.TrimSpace(repoName), Path: absRepoPath},
		},
	}

	data, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(configFile, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", configFile, err)
	}

	fmt.Print(tui.StyledMessage(fmt.Sprintf("Wrote %s — toad will auto-join all public channels on startup.", configFile)))
	fmt.Println("  Start toad with: toad")
	return nil
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
