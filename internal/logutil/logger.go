// Package logutil provides shared logger plumbing for Railwarden. All logs go to stderr in JSON.
package logutil

import (
	"context"

	"go.uber.org/zap"
)

// loggerCtxKey is the unexported context key used to attach a *zap.Logger to a context.
type loggerCtxKey struct{}

// New returns a Railwarden zap.Logger configured for production: JSON encoding to stderr.
func New() (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	return cfg.Build()
}

// WrapLogger returns a copy of ctx with log attached.
func WrapLogger(ctx context.Context, log *zap.Logger) context.Context {
	return context.WithValue(ctx, loggerCtxKey{}, log)
}

// UnwrapLogger returns the *zap.Logger attached to ctx. A no-op logger is returned when none is set.
func UnwrapLogger(ctx context.Context) *zap.Logger {
	log, ok := ctx.Value(loggerCtxKey{}).(*zap.Logger)
	if !ok {
		return zap.NewNop()
	}
	return log
}
