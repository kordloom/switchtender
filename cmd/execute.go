package cmd

import (
	"errors"
	"fmt"
	"os"
)

// Execute runs the Yardmaster root command and exits the process with the appropriate code.
func Execute() {
	err := rootCmd.Execute()
	if err == nil {
		os.Exit(CodeOK)
	}
	fmt.Fprintln(os.Stderr, err)
	if errors.Is(err, ErrUsage) {
		os.Exit(CodeUsage)
	}
	os.Exit(CodeError)
}
