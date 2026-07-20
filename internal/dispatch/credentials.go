package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/credential"
	"github.com/dcadolph/switchtender/internal/roundhouse"
	"github.com/dcadolph/switchtender/internal/run"
	"github.com/dcadolph/switchtender/internal/secretsource"
)

// revokeTimeout bounds a single ephemeral secret revocation so a slow or unreachable secrets engine
// cannot hold up a finished run. A lease that is not revoked in time expires on its own TTL.
const revokeTimeout = 15 * time.Second

// WithCredentials lets runs materialize stored credentials at execution time.
func WithCredentials(store credential.Store, sealer *credential.Sealer) Option {
	return func(c *config) {
		c.credentials = store
		c.sealer = sealer
	}
}

// validateCredentials confirms every referenced credential exists and is decryptable before a run
// is accepted, so a bad reference fails at submit time instead of execution time.
func (d *Dispatcher) validateCredentials(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if d.credentials == nil || d.sealer == nil || !d.sealer.Enabled() {
		return credential.ErrNoKey
	}
	for _, id := range ids {
		c, err := d.credentials.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: %s", err, id)
		}
		if _, err := d.sealer.Open(c.Secret); err != nil {
			return fmt.Errorf("decrypt credential %s: %w", id, err)
		}
	}
	return nil
}

// materializeCredentials decrypts the run's credentials into files only the executing process can
// read and maps them onto the spec. It also returns every resolved plaintext secret so the caller
// can redact those values from the run's output. The returned cleanup removes every file.
func (d *Dispatcher) materializeCredentials(ctx context.Context, r *run.Run, spec *roundhouse.Spec) (func(), []string, error) {
	cleanup := func() {}
	ids := d.effectiveCredentialIDs(ctx, r)
	if len(ids) == 0 {
		return cleanup, nil, nil
	}
	if d.credentials == nil || d.sealer == nil {
		return cleanup, nil, credential.ErrNoKey
	}

	var paths []string
	var secrets []string
	var leases []*secretsource.Lease
	cleanup = func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
		for _, lease := range leases {
			revokeCtx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
			if err := lease.Revoke(revokeCtx); err != nil {
				d.log.Warn("dispatch: revoke ephemeral secret failed: "+err.Error(),
					zap.String("engine", lease.Kind()))
			}
			cancel()
		}
	}
	for _, id := range ids {
		c, plain, lease, err := d.openCredential(ctx, id)
		if err != nil {
			return cleanup, secrets, err
		}
		if lease != nil {
			leases = append(leases, lease)
		}
		secrets = append(secrets, plain)
		if c.Kind == credential.KindEnv {
			// The secret is each value, not the KEY=VALUE bundle, so mask the values a tool may echo.
			for _, line := range credential.EnvLines(plain) {
				if _, val, ok := strings.Cut(line, "="); ok {
					secrets = append(secrets, val)
				}
			}
		}
		f, err := os.CreateTemp("", "switchtender-cred-*")
		if err != nil {
			return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
		}
		paths = append(paths, f.Name())
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
		}
		if _, err := f.WriteString(plain); err != nil {
			_ = f.Close()
			return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
		}
		if err := f.Close(); err != nil {
			return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
		}

		switch c.Kind {
		case credential.KindSSHKey:
			spec.PrivateKeyPath = f.Name()
		case credential.KindVaultPassword:
			spec.VaultPasswordFile = f.Name()
		case credential.KindEnv:
			// Environment pairs go straight into the process; the temp file is not needed.
			paths = paths[:len(paths)-1]
			_ = os.Remove(f.Name())
			spec.Env = append(spec.Env, credential.EnvLines(plain)...)
		case credential.KindToken:
			// A token is exposed as one environment variable; the temp file is not needed.
			paths = paths[:len(paths)-1]
			_ = os.Remove(f.Name())
			spec.Env = append(spec.Env, credential.TokenEnvVar+"="+strings.TrimRight(plain, "\r\n"))
		case credential.KindBecomePassword:
			// The password reaches the play as a var through a file so it never lands on argv.
			vars, err := json.Marshal(map[string]string{"ansible_become_password": plain})
			if err != nil {
				return cleanup, secrets, fmt.Errorf("encode become credential %s: %w", id, err)
			}
			if err := os.WriteFile(f.Name(), vars, 0o600); err != nil {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
			spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, f.Name())
		case credential.KindRegistry:
			// Registry logins are consumed by the container runner for image pulls, not the play.
			paths = paths[:len(paths)-1]
			_ = os.Remove(f.Name())
		default:
			return cleanup, secrets, fmt.Errorf("%w: %s", credential.ErrBadKind, c.Kind)
		}
	}
	return cleanup, secrets, nil
}

// openCredential fetches a credential, decrypts its sealed secret, and resolves it through its
// source, returning the credential, its plain value, and, for a dynamic source, a lease that revokes
// the minted secret after the run. It is the shared path run materialization uses before applying the
// credential's kind.
func (d *Dispatcher) openCredential(ctx context.Context, id string) (*credential.Credential, string, *secretsource.Lease, error) {
	if d.credentials == nil || d.sealer == nil {
		return nil, "", nil, credential.ErrNoKey
	}
	c, err := d.credentials.Get(ctx, id)
	if err != nil {
		return nil, "", nil, fmt.Errorf("credential %s: %w", id, err)
	}
	plain, err := d.sealer.Open(c.Secret)
	if err != nil {
		return nil, "", nil, fmt.Errorf("decrypt credential %s: %w", id, err)
	}
	value, lease, err := secretsource.ResolveLeased(ctx, c.Source, plain)
	if err != nil {
		return nil, "", nil, fmt.Errorf("resolve credential %s: %w", id, err)
	}
	return c, value, lease, nil
}

// effectiveCredentialIDs returns the run's own credentials plus any attached to the stored inventory
// it targets, deduplicated and in order, so an inventory can carry secret variables that every run
// against it receives.
func (d *Dispatcher) effectiveCredentialIDs(ctx context.Context, r *run.Run) []string {
	ids := append([]string(nil), r.CredentialIDs...)
	if r.InventoryID != "" && d.inventories != nil {
		if inv, err := d.inventories.Get(ctx, r.InventoryID); err == nil {
			ids = append(ids, inv.CredentialIDs...)
		}
	}
	seen := make(map[string]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
