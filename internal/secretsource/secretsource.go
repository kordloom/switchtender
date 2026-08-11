// Package secretsource resolves a value from an external source at run time, so any resource, not
// just a credential, can be sourced from a secrets engine. A source is a kind and a config: local
// means the config is the value itself; command, vault, gsm, aws, azure, conjur, ccp, and onepassword
// fetch the value from an external store. New engines register a resolver for their kind, so the set
// is pluggable.
package secretsource

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	// KindAWS means the config is a JSON AWS Secrets Manager secret id, region, and credentials, read
	// with a Signature Version 4 signed request.
	KindAWS = "aws"
	// KindAzure means the config is a JSON Azure Key Vault name, secret, and authentication, read over
	// HTTP with a bearer token from the config, a service principal, or a managed identity.
	KindAzure = "azure"
	// KindConjur means the config is a JSON CyberArk Conjur URL, account, variable, and authentication,
	// read over HTTP with an access token from the config or an API-key exchange.
	KindConjur = "conjur"
	// KindCCP means the config is a JSON CyberArk Central Credential Provider URL, app id, and account
	// locator, read over the AIMWebService REST API with a client certificate or an allowed-machine rule.
	KindCCP = "ccp"
	// KindOnePassword means the config is a JSON 1Password Connect URL, token, vault, item, and field,
	// read over the Connect REST API at launch with no op CLI on the runner.
	KindOnePassword = "onepassword"
	// KindVaultDynamic means the config names a Vault dynamic secrets path that mints a short-lived
	// credential on each read, returned with a lease that revokes it after the run.
	KindVaultDynamic = "vault_dynamic"
	// KindAWSSTS means the config names an IAM role to assume through AWS STS, minting short-lived
	// role credentials for each run as an env block, next to Vault dynamic on the ephemeral-secret story.
	KindAWSSTS = "aws_sts"
)

// ErrResolve is returned when a source cannot produce its value.
var ErrResolve = errors.New("secret resolve failed")

// ResolverFunc fetches a value from a source's config at run time.
type ResolverFunc func(ctx context.Context, config string) (string, error)

// resolvers maps a source kind to its resolver. Register adds engines such as AWS Secrets Manager or
// 1Password without touching the core.
var resolvers = map[string]ResolverFunc{
	KindCommand:     resolveCommand,
	KindVault:       resolveVault,
	KindGSM:         resolveGSM,
	KindAWS:         resolveAWS,
	KindAzure:       resolveAzure,
	KindConjur:      resolveConjur,
	KindCCP:         resolveCCP,
	KindOnePassword: resolveOnePassword,
}

// Register adds a resolver for a source kind so a new secrets engine plugs in. It panics on a
// duplicate or reserved kind, which is a programming error.
func Register(kind string, fn ResolverFunc) {
	if kind == "" || kind == KindLocal {
		panic("secretsource: cannot register the local kind")
	}
	if _, exists := resolvers[kind]; exists {
		panic("secretsource: duplicate resolver for " + kind)
	}
	if _, exists := minters[kind]; exists {
		panic("secretsource: kind already registered as dynamic: " + kind)
	}
	resolvers[kind] = fn
}

// Lease is a handle to a minted short-lived secret. Revoking it tells the engine to end the secret
// early. A secret that is never revoked expires on the engine's own TTL, so a lost lease is not an
// orphan, only a secret that lives out its lifetime.
type Lease struct {
	// kind names the engine that minted the secret, for logging and audit.
	kind string
	// revoke ends the secret early, nil for a source that mints nothing revocable.
	revoke func(ctx context.Context) error
}

// NewLease builds a lease for a minted secret, naming the engine and capturing how to revoke it.
func NewLease(kind string, revoke func(ctx context.Context) error) *Lease {
	return &Lease{kind: kind, revoke: revoke}
}

// Kind returns the engine that minted the lease, or an empty string for a nil lease.
func (l *Lease) Kind() string {
	if l == nil {
		return ""
	}
	return l.kind
}

// Revoke ends the minted secret early. A nil lease, or a lease with no revoke func, is a no-op, so a
// caller can revoke unconditionally.
func (l *Lease) Revoke(ctx context.Context) error {
	if l == nil || l.revoke == nil {
		return nil
	}
	return l.revoke(ctx)
}

// MintFunc mints a short-lived value from a dynamic engine's config, returning the value and a lease
// that revokes it.
type MintFunc func(ctx context.Context, config string) (string, *Lease, error)

// minters maps a dynamic source kind to its mint function. RegisterDynamic adds engines such as AWS
// STS without touching the core.
var minters = map[string]MintFunc{
	KindVaultDynamic: mintVaultDynamic,
	KindAWSSTS:       mintAWSSTS,
}

// RegisterDynamic adds a mint function for a dynamic source kind, so a new short-lived secrets engine
// plugs in. It panics on a duplicate or reserved kind, which is a programming error.
func RegisterDynamic(kind string, fn MintFunc) {
	if kind == "" || kind == KindLocal {
		panic("secretsource: cannot register the local kind")
	}
	if _, exists := minters[kind]; exists {
		panic("secretsource: duplicate dynamic engine for " + kind)
	}
	if _, exists := resolvers[kind]; exists {
		panic("secretsource: kind already registered as a resolver: " + kind)
	}
	minters[kind] = fn
}

// Registered reports whether kind is already claimed, by the local default, a resolver, or a minter.
// Resolvers and minters share one namespace, so a plugin declaring a kind already taken by either
// would panic the shared registry; the plugin loader checks this first and refuses the plugin.
func Registered(kind string) bool {
	if kind == "" || kind == KindLocal {
		return true
	}
	if _, ok := resolvers[kind]; ok {
		return true
	}
	_, ok := minters[kind]
	return ok
}

// NormalizeKind maps an empty kind to the local default and otherwise returns kind unchanged.
func NormalizeKind(kind string) string {
	if kind == "" {
		return KindLocal
	}
	return kind
}

// ValidKind reports whether kind names a supported source: local, a registered resolver, or a
// registered dynamic engine.
func ValidKind(kind string) bool {
	k := NormalizeKind(kind)
	if k == KindLocal {
		return true
	}
	if _, ok := resolvers[k]; ok {
		return true
	}
	_, ok := minters[k]
	return ok
}

// Kinds returns every supported source kind, sorted: local plus each registered resolver and dynamic
// engine. It is the exact set ValidKind accepts, so a user-facing hint built from it cannot drift
// from the resolver and minter tables.
func Kinds() []string {
	out := make([]string, 0, len(resolvers)+len(minters)+1)
	out = append(out, KindLocal)
	for k := range resolvers {
		out = append(out, k)
	}
	for k := range minters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ResolveLeased returns the value a source names and, for a dynamic engine, a lease that revokes the
// minted secret. A local source returns its config with no lease. A registered resolver returns its
// value with no lease. A dynamic engine mints a short-lived value and returns a lease for it.
func ResolveLeased(ctx context.Context, kind, config string) (string, *Lease, error) {
	k := NormalizeKind(kind)
	if k == KindLocal {
		return config, nil, nil
	}
	if mint, ok := minters[k]; ok {
		return mint(ctx, config)
	}
	fn, ok := resolvers[k]
	if !ok {
		return "", nil, fmt.Errorf("%w: unknown source %q", ErrResolve, kind)
	}
	value, err := fn(ctx, config)
	return value, nil, err
}

// Resolve returns the value a source names, fetching it at call time and discarding any lease. It
// suits callers, such as inventory content, that read a value but do not revoke it.
func Resolve(ctx context.Context, kind, config string) (string, error) {
	value, _, err := ResolveLeased(ctx, kind, config)
	return value, err
}
