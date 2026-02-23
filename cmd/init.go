package cmd

import (
	"fmt"
	"os"
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
// It omits repo.path and log.file so runtime defaults apply.
type outputConfig struct {
	Slack outputSlackConfig `yaml:"slack"`
}

type outputSlackConfig struct {
	AppToken string   `yaml:"app_token"`
	BotToken string   `yaml:"bot_token"`
	Channels []string `yaml:"channels,omitempty"`
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

	// Form 2: Slack setup guide → token inputs
	var appToken, botToken string

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

	// Write config (no channels — toad auto-joins all public channels)
	out := outputConfig{
		Slack: outputSlackConfig{
			AppToken: appToken,
			BotToken: botToken,
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
