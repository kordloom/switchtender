package cmd

import (
	"github.com/spf13/cobra"
)

// rootCmd is the Yardmaster top-level CLI command.
var rootCmd = &cobra.Command{
	Use:   "yardmaster",
	Short: "Playbook execution and fleet orchestration platform.",
	Long:  "Yardmaster orchestrates playbook execution across a fleet of hosts. Alternative to AWX and Semaphore.",
}

// init registers the Yardmaster subcommands on the root command.
func init() {
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(workerCmd)
	rootCmd.AddCommand(tokenCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(importCmd)
	rootCmd.AddCommand(demoCmd)
}
