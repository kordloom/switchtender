package secretsource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// vaultDynamicConfig is the JSON a vault_dynamic source stores: the dynamic secrets path to read and
// which field of the minted secret to expose.
type vaultDynamicConfig struct {
	// Addr is the Vault address, for example https://vault.example.com:8200.
	Addr string `json:"addr"`
	// Path is the dynamic secrets read path, for example database/creds/app or aws/creds/deploy.
	Path string `json:"path"`
	// Field is the field of the minted secret to return, for example password.
	Field string `json:"field"`
	// Token authenticates the read and the later revoke. When empty, VAULT_TOKEN is used.
	Token string `json:"token,omitempty"`
}

// mintVaultDynamic reads a dynamic secret from Vault, returning the requested field and a lease that
// revokes it. A dynamic engine such as database or aws mints fresh credentials on each read, so the
// value is short-lived and unique to this run. The lease revokes the Vault lease over HTTP, so the
// credential is destroyed when the run ends rather than waiting out its TTL.
func mintVaultDynamic(ctx context.Context, config string) (string, *Lease, error) {
	var cfg vaultDynamicConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", nil, fmt.Errorf("%w: vault_dynamic config is not valid JSON", ErrResolve)
	}
	if cfg.Addr == "" || cfg.Path == "" || cfg.Field == "" {
		return "", nil, fmt.Errorf("%w: vault_dynamic config needs addr, path, and field", ErrResolve)
	}
	token, err := vaultResolveToken(cfg.Token, cfg.Addr)
	if err != nil {
		return "", nil, err
	}

	addr := strings.TrimRight(cfg.Addr, "/")
	url := addr + "/v1/" + strings.TrimLeft(cfg.Path, "/")
	if err := checkResolveURL(url); err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("X-Vault-Token", token)
	resp, err := safeClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("%w: vault request failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("%w: vault returned %s", ErrResolve, resp.Status)
	}

	value, leaseID, err := vaultDynamicSecret(body, cfg.Field)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrResolve, err)
	}
	return value, vaultHandleLease(addr, token, leaseID, vaultLeaseLifetime(body), time.Now()), nil
}

// maxLeaseLifetime bounds the lifetime read from a Vault response, so an absurd lease_duration
// cannot overflow the expiry computed from it.
const maxLeaseLifetime = 100 * 365 * 24 * time.Hour

// vaultLeaseLifetime returns how long Vault said a dynamic secret lives, from the lease_duration of
// its read response, or zero when the response did not say.
func vaultLeaseLifetime(body []byte) time.Duration {
	var resp struct {
		LeaseDuration int64 `json:"lease_duration"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.LeaseDuration <= 0 {
		return 0
	}
	if resp.LeaseDuration > int64(maxLeaseLifetime/time.Second) {
		return maxLeaseLifetime
	}
	return time.Duration(resp.LeaseDuration) * time.Second
}

// vaultDynamicSecret extracts the requested field and the lease id from a Vault dynamic secret read
// response, where the credential sits under data and the lease id is a top-level field.
func vaultDynamicSecret(body []byte, field string) (string, string, error) {
	var resp struct {
		LeaseID string                     `json:"lease_id"`
		Data    map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", fmt.Errorf("vault response is not valid JSON")
	}
	raw, ok := resp.Data[field]
	if !ok {
		return "", "", fmt.Errorf("field %q not found in the vault secret", field)
	}
	return rawToString(raw), resp.LeaseID, nil
}

// revokeVaultLease returns a revoke func that ends a Vault lease over HTTP, so a minted secret is
// destroyed when the run ends. An empty lease id, from a read that returned none, is a no-op.
func revokeVaultLease(addr, token, leaseID string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if leaseID == "" {
			return nil
		}
		payload, err := json.Marshal(map[string]string{"lease_id": leaseID})
		if err != nil {
			return err
		}
		url := addr + "/v1/sys/leases/revoke"
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("X-Vault-Token", token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := safeClient.Do(req)
		if err != nil {
			return fmt.Errorf("%w: vault revoke request failed: %s", ErrResolve, err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, httpMaxBody))
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("%w: vault revoke returned %s", ErrResolve, resp.Status)
		}
		return nil
	}
}
