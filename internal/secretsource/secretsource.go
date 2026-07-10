// Package secretsource resolves a value from an external source at run time, so any resource, not
// just a credential, can be sourced from a secrets engine. A source is a kind and a config: local
// means the config is the value itself; command, vault, and gsm fetch the value from an external
// store. New engines register a resolver for their kind, so the set is pluggable.
package secretsource

import (
	"context"
	"errors"
	"fmt"
)

const (
	// KindLocal means the config is the value itself. It is the default.
	KindLocal = "local"
	// KindCommand means the config is a command whose stdout is the value.
	KindCommand = "command"
	// KindVault means the config is a JSON Vault address, path, and field read over HTTP.
	KindVault = "vault"
	// KindGSM means the config is a JSON Google Secret Manager project, secret, and version.
	KindGSM = "gsm"
)

// ErrResolve is returned when a source cannot produce its value.
var ErrResolve = errors.New("secret resolve failed")

// ResolverFunc fetches a value from a source's config at run time.
type ResolverFunc func(ctx context.Context, config string) (string, error)

// resolvers maps a source kind to its resolver. Register adds engines such as AWS Secrets Manager or
// 1Password without touching the core.
var resolvers = map[string]ResolverFunc{
	KindCommand: resolveCommand,
	KindVault:   resolveVault,
	KindGSM:     resolveGSM,
}

// Register adds a resolver for a source kind so a new secrets engine plugs in. It panics on a
// duplicate kind, which is a programming error.
func Register(kind string, fn ResolverFunc) {
	if kind == "" || kind == KindLocal {
		panic("secretsource: cannot register the local kind")
	}
	if _, exists := resolvers[kind]; exists {
		panic("secretsource: duplicate resolver for " + kind)
	}
	resolvers[kind] = fn
}

// NormalizeKind maps an empty kind to the local default and otherwise returns kind unchanged.
func NormalizeKind(kind string) string {
	if kind == "" {
		return KindLocal
	}
	return kind
}

// ValidKind reports whether kind names a supported source: local or a registered engine.
func ValidKind(kind string) bool {
	k := NormalizeKind(kind)
	if k == KindLocal {
		return true
	}
	_, ok := resolvers[k]
	return ok
}

// Resolve returns the value a source of the given kind names, fetching it at call time. A local
// source returns its config unchanged; a registered engine resolves the config to its value.
func Resolve(ctx context.Context, kind, config string) (string, error) {
	k := NormalizeKind(kind)
	if k == KindLocal {
		return config, nil
	}
	fn, ok := resolvers[k]
	if !ok {
		return "", fmt.Errorf("%w: unknown source %q", ErrResolve, kind)
	}
	return fn(ctx, config)
}
