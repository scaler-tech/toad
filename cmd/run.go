package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hergen/toad/internal/config"
	toadlog "github.com/hergen/toad/internal/log"
	"github.com/hergen/toad/internal/state"
	"github.com/hergen/toad/internal/tadpole"
)

var runCmd = &cobra.Command{
	Use:   "run [task description]",
	Short: "Run a tadpole to fix an issue (CLI mode, no Slack)",
	Long:  "Manually spawn a tadpole that creates a worktree, runs Claude Code, validates, and opens a PR.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runTadpole,
}

func init() {
	rootCmd.AddCommand(runCmd)
}

func runTadpole(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	toadlog.Setup(cfg.Log.Level, cfg.Log.File)

	// CLI mode only requires repo path and CLI tools, not Slack tokens
	if err := validateForCLI(cfg); err != nil {
		return err
	}

	if err := checkClaude(); err != nil {
		return err
	}
	if err := checkGH(); err != nil {
		return err
	}

	taskDesc := strings.Join(args, " ")
	fmt.Printf(":frog: Starting tadpole: %s\n\n", taskDesc)

	sm := state.NewManager()
	runner := tadpole.NewRunner(cfg, nil, sm)

	task := tadpole.Task{
		Description: taskDesc,
		Summary:     taskDesc,
		Category:    "manual",
		EstSize:     "unknown",
	}

	ctx := context.Background()
	if err := runner.Execute(ctx, task); err != nil {
		return fmt.Errorf("tadpole failed: %w", err)
	}

	return nil
}

func validateForCLI(cfg *config.Config) error {
	if cfg.Repo.Path == "" {
		return fmt.Errorf("repo path is required")
	}
	return nil
}
