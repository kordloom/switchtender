package cmd

import (
	"github.com/spf13/cobra"
)

// workerCmd runs a Yardmaster job worker (a yardgoat).
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Run a Yardmaster worker process.",
	RunE: func(cmd *cobra.Command, args []string) error {
		return nil
	},
}
