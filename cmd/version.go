package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/scaler-tech/toad/internal/update"
)

// Set via ldflags at build time.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

var verbose bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the toad version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("toad %s\n", Version)
		if verbose {
			fmt.Printf("  commit:  %s\n", Commit)
			fmt.Printf("  built:   %s\n", BuildDate)
		}

		// Check for newer version (silent on error or dev builds)
		info, err := update.Check(Version)
		if err == nil && info != nil && info.Available {
			fmt.Printf("\n  Update available: v%s → v%s\n", info.Current, info.Latest)
			fmt.Printf("  Run `toad update` to upgrade\n")
		}
	},
}

func init() {
	versionCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show commit and build date")
	rootCmd.AddCommand(versionCmd)
}
