package cmd

import (
	"github.com/spf13/cobra"
)

// rootCmd is the Yardmaster top-level CLI command.
var rootCmd = &cobra.Command{
	Use:   "yardmaster",
	Short: "Automation execution and fleet orchestration platform.",
	Long: "Yardmaster runs and governs automation across a fleet of hosts: Ansible, Terraform, " +
		"Bash, Python, and Go from one binary, with a provable audit trail over every change.",
}

// init registers the Yardmaster subcommands on the root command.
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
