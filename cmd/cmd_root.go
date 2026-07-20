package cmd

import (
	"github.com/spf13/cobra"
)

// rootCmd is the SwitchTender top-level CLI command.
var rootCmd = &cobra.Command{
	Use:   "switchtender",
	Short: "Automation execution and fleet orchestration platform.",
	Long: "SwitchTender runs and governs automation across a fleet of hosts: Ansible, Terraform, " +
		"Bash, Python, and Go from one binary, with a provable audit trail over every change.",
}

// init registers the SwitchTender subcommands on the root command.
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
