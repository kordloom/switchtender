package cmd

import (
	"github.com/spf13/cobra"
)

// rootCmd is the SwitchTender top-level CLI command.
var rootCmd = &cobra.Command{
	// Execute prints the error itself and chooses the exit code, so cobra is asked not to print it a
	// second time.
	SilenceErrors: true,
	// Usage is suppressed once the command's own body starts, not before. Cobra parses flags and
	// validates arguments first, so a bad flag or a wrong argument count still prints usage, which is
	// the case where the manual is the answer. Everything after that is a runtime failure, where it
	// is not: refusing to serve with no tokens used to print the reason, then ninety lines of flag
	// help, then the reason again, burying the one line an operator needed inside its own manual.
	PersistentPreRun: func(cmd *cobra.Command, _ []string) { cmd.SilenceUsage = true },
	Use:              "switchtender",
	Short:            "Automation execution and fleet orchestration platform.",
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
