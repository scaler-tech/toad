package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
	toadlog "github.com/scaler-tech/toad/internal/log"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/tadpole"
)

var repoFlag string

var runCmd = &cobra.Command{
	Use:   "run [task description]",
	Short: "Run a tadpole to fix an issue (CLI mode, no Slack)",
	Long:  "Manually spawn a tadpole that creates a worktree, runs the coding agent, validates, and opens a PR.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runTadpole,
}

func init() {
	runCmd.Flags().StringVar(&repoFlag, "repo", "", "repo name to target (required when multiple repos configured)")
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

	agentProvider, err := agent.NewProvider(cfg.Agent.Platform)
	if err != nil {
		return fmt.Errorf("agent config: %w", err)
	}
	if err := agentProvider.Check(); err != nil {
		return err
	}
	vcsResolver, err := buildVCSResolver(cfg)
	if err != nil {
		return fmt.Errorf("vcs config: %w", err)
	}

	// Resolve target repo
	repo, err := resolveRepoForCLI(cfg, repoFlag)
	if err != nil {
		return err
	}

	taskDesc := strings.Join(args, " ")
	fmt.Printf(":frog: Starting tadpole: %s\n\n", taskDesc)

	sm := state.NewManager()
	runner := tadpole.NewRunner(cfg, agentProvider, nil, sm, vcsResolver)

	repoPaths := make(map[string]string, len(cfg.Repos.List))
	for _, r := range cfg.Repos.List {
		repoPaths[r.Path] = r.Name
	}

	task := tadpole.Task{
		Description: taskDesc,
		Summary:     taskDesc,
		Category:    "manual",
		EstSize:     "unknown",
		Repo:        repo,
		RepoPaths:   repoPaths,
	}

	ctx := context.Background()
	if err := runner.Execute(ctx, task); err != nil {
		return fmt.Errorf("tadpole failed: %w", err)
	}

	return nil
}

func validateForCLI(cfg *config.Config) error {
	if len(cfg.Repos.List) == 0 {
		return fmt.Errorf("at least one repo is required")
	}
	return nil
}

func resolveRepoForCLI(cfg *config.Config, name string) (*config.RepoConfig, error) {
	if len(cfg.Repos.List) == 1 {
		return &cfg.Repos.List[0], nil
	}
	if name != "" {
		repo := config.RepoByName(cfg.Repos.List, name)
		if repo == nil {
			names := make([]string, len(cfg.Repos.List))
			for i, r := range cfg.Repos.List {
				names[i] = r.Name
			}
			return nil, fmt.Errorf("unknown repo %q — available: %s", name, strings.Join(names, ", "))
		}
		return repo, nil
	}
	// Multiple repos, no flag
	names := make([]string, len(cfg.Repos.List))
	for i, r := range cfg.Repos.List {
		names[i] = r.Name
	}
	return nil, fmt.Errorf("multiple repos configured — use --repo to specify: %s", strings.Join(names, ", "))
}
