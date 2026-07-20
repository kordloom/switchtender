package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// versionCmd prints the SwitchTender version to stdout.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the SwitchTender version.",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(resolveVersion())
	},
}
