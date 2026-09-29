// Package logutil provides shared logger plumbing for SwitchTender. All logs go to stderr in JSON.
package logutil

import "go.uber.org/zap"

// New returns a SwitchTender zap.Logger configured for production: JSON encoding to stderr.
func New() (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	return cfg.Build()
}
