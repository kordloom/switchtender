package secretsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrLeaseHandle is returned when a lease handle can never be revoked from here: its engine gives
// no handles, the handle is not one the engine wrote, or the source no longer names the engine that
// issued it. Retrying cannot help, so a caller drops the handle and the secret lives out its own
// lifetime.
var ErrLeaseHandle = errors.New("lease handle cannot be revoked")

// UnknownLifetime is how long a secret is assumed to live when its engine did not say. It is
// Vault's default maximum lease lifetime, so a handle is kept for as long as the secret can last.
const UnknownLifetime = 768 * time.Hour

// HandleRevokeFunc ends a minted secret from the handle its lease gave, reading config, the
// source's configuration as the credential now stores it, for anything else the engine needs, such
// as a token. The handle names the secret and config authorizes the revoke, so neither is stored
// with the other.
type HandleRevokeFunc func(ctx context.Context, handle, config string) error

// handleRevokers maps each source kind whose leases carry a handle to the function that revokes
// one. A dynamic engine a plugin registers gives no handle, so a secret it minted for a run expires
// on its own lifetime when the process that minted it stops before the run ends.
var handleRevokers = map[string]HandleRevokeFunc{
	KindVaultDynamic: revokeVaultHandle,
}

// RevokeHandle ends the secret a lease's handle names, for a source of kind configured by config.
// It returns an error wrapping ErrLeaseHandle when the handle can never be revoked from here.
func RevokeHandle(ctx context.Context, kind, handle, config string) error {
	fn, ok := handleRevokers[NormalizeKind(kind)]
	if !ok {
		return fmt.Errorf("%w: source %q gives no lease handles", ErrLeaseHandle, kind)
	}
	return fn(ctx, handle, config)
}

// vaultLeaseHandle is the handle of a Vault dynamic secret: the lease and the Vault that issued it.
type vaultLeaseHandle struct {
	// Addr is the issuing Vault's address, with no trailing slash.
	Addr string `json:"addr"`
	// LeaseID is the lease Vault issued the secret under.
	LeaseID string `json:"lease_id"`
}

// vaultHandleLease returns the lease of a Vault dynamic secret: the revoke func, and the handle and
// expiry another process needs to revoke it. A read that returned no lease id gives no handle,
// since there is nothing to revoke. A lifetime of zero means Vault did not say, and UnknownLifetime
// is assumed.
func vaultHandleLease(addr, token, leaseID string, lifetime time.Duration, now time.Time) *Lease {
	lease := NewLease(KindVaultDynamic, revokeVaultLease(addr, token, leaseID))
	if leaseID == "" {
		return lease
	}
	handle, err := json.Marshal(vaultLeaseHandle{Addr: addr, LeaseID: leaseID})
	if err != nil {
		return lease
	}
	if lifetime <= 0 {
		lifetime = UnknownLifetime
	}
	lease.handle, lease.expires = string(handle), now.Add(lifetime)
	return lease
}

// revokeVaultHandle revokes a Vault dynamic secret from its handle, with the token the source's
// configuration resolves to. The token is sent only to the Vault that issued the lease: a
// credential edited since to name another Vault holds that Vault's token, so the handle is refused
// rather than handing one server's token to another.
func revokeVaultHandle(ctx context.Context, handle, config string) error {
	var h vaultLeaseHandle
	if err := json.Unmarshal([]byte(handle), &h); err != nil || h.Addr == "" || h.LeaseID == "" {
		return fmt.Errorf("%w: not a vault_dynamic lease handle", ErrLeaseHandle)
	}
	var cfg vaultDynamicConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return fmt.Errorf("%w: the vault_dynamic config is not valid JSON", ErrLeaseHandle)
	}
	if strings.TrimRight(cfg.Addr, "/") != h.Addr {
		return fmt.Errorf("%w: the credential now names another Vault than the one that issued "+
			"the lease", ErrLeaseHandle)
	}
	token, err := vaultResolveToken(cfg.Token, cfg.Addr)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrLeaseHandle, err)
	}
	return revokeVaultLease(h.Addr, token, h.LeaseID)(ctx)
}
