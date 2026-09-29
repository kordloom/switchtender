// Package cmd holds the SwitchTender CLI commands.
package cmd

// Process exit codes returned by the SwitchTender CLI.
const (
	// CodeOK indicates a successful run.
	CodeOK = 0
	// CodeError indicates an unrecovered runtime error.
	CodeError = 1
	// CodeUsage indicates a CLI usage error.
	CodeUsage = 2
)
