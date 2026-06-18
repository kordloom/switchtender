package cmd

import (
	"github.com/spf13/cobra"
)

// serveCmd runs the Yardmaster HTTP server (the dispatcher).
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the Yardmaster server.",
	RunE: func(cmd *cobra.Command, args []string) error {
		return nil
	},
}
