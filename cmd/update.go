package cmd

import (
	"fmt"
	"os/exec"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/scaler-tech/toad/internal/update"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update toad to the latest version",
	RunE:  runUpdate,
}

func init() {
	rootCmd.AddCommand(updateCmd)
}

func runUpdate(cmd *cobra.Command, args []string) error {
	accentStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#F59E0B"))
	successStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981")).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280"))
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444"))

	fmt.Println()
	fmt.Println("  " + accentStyle.Render("Checking for updates..."))

	info, err := update.Check(Version)
	if err != nil {
		fmt.Println("  " + errorStyle.Render(fmt.Sprintf("Failed to check for updates: %v", err)))
		fmt.Println()
		return nil
	}

	if info == nil {
		fmt.Println("  " + dimStyle.Render("Running a dev build — update check skipped."))
		fmt.Println()
		return nil
	}

	if !info.Available {
		fmt.Println("  " + successStyle.Render(fmt.Sprintf("toad v%s is already the latest version.", info.Current)))
		fmt.Println()
		return nil
	}

	fmt.Println("  " + accentStyle.Render(fmt.Sprintf("Update available: v%s → v%s", info.Current, info.Latest)))
	fmt.Println()

	// Check if homebrew is available
	hasBrew := exec.Command("brew", "--version").Run() == nil //nolint:gosec // brew is a fixed binary
	if !hasBrew {
		fmt.Println("  " + dimStyle.Render("Homebrew not found. Update manually:"))
		fmt.Println()
		fmt.Println("  " + dimStyle.Render("  go install github.com/scaler-tech/toad@latest"))
		fmt.Println("  " + dimStyle.Render("  or download from: "+info.ReleaseURL))
		fmt.Println()
		return nil
	}

	fmt.Println("  " + dimStyle.Render("Refreshing tap..."))
	if refreshOut, err := exec.Command("brew", "update").CombinedOutput(); err != nil {
		fmt.Println("  " + errorStyle.Render(fmt.Sprintf("brew update failed: %s", strings.TrimSpace(string(refreshOut)))))
		fmt.Println()
		return err
	}

	fmt.Println("  " + dimStyle.Render("Upgrading toad..."))
	upgradeCmd := exec.Command("brew", "upgrade", "--cask", "toad")
	upgradeOutput, upgradeErr := upgradeCmd.CombinedOutput()
	if upgradeErr != nil {
		errMsg := strings.TrimSpace(string(upgradeOutput))
		if strings.Contains(errMsg, "already installed") {
			fmt.Println("  " + successStyle.Render(fmt.Sprintf("toad v%s is already installed.", info.Latest)))
		} else {
			fmt.Println("  " + errorStyle.Render(fmt.Sprintf("brew upgrade failed: %v", upgradeErr)))
		}
		fmt.Println()
		return nil
	}
	if out := strings.TrimSpace(string(upgradeOutput)); out != "" {
		fmt.Println(out)
	}

	fmt.Println()
	fmt.Println("  " + successStyle.Render(fmt.Sprintf("Successfully updated to toad v%s!", info.Latest)))
	fmt.Println()
	return nil
}
