package cmd

import (
	"github.com/spf13/cobra"
)

// rootCmd is the Railwarden top-level CLI command.
var rootCmd = &cobra.Command{
	Use:   "railwarden",
	Short: "Automation execution and fleet orchestration platform.",
	Long: "Railwarden runs and governs automation across a fleet of hosts: Ansible, Terraform, " +
		"Bash, Python, and Go from one binary, with a provable audit trail over every change.",
}

// init registers the Railwarden subcommands on the root command.
func init() {
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(desktopCmd)
	rootCmd.AddCommand(workerCmd)
	rootCmd.AddCommand(tokenCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(importCmd)
	rootCmd.AddCommand(demoCmd)
}
