package cmd

import (
	"io/fs"

	"github.com/spf13/cobra"
)

// DocsFS holds the embedded documentation tree, set by main before Execute. The server renders it
// inside the web UI. It is nil in builds that do not wire it, which disables the in-app docs.
var DocsFS fs.FS

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
}
