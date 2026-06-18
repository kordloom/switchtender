package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// versionCmd prints the Yardmaster version to stdout.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the Yardmaster version.",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(Version)
	},
}
