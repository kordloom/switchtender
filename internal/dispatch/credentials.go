package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/secretsource"
)

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
// read and maps them onto the spec. The returned cleanup removes every file.
func (d *Dispatcher) materializeCredentials(r *run.Run, spec *roundhouse.Spec) (func(), error) {
	cleanup := func() {}
	ids := d.effectiveCredentialIDs(r)
	if len(ids) == 0 {
		return cleanup, nil
	}
	if d.credentials == nil || d.sealer == nil {
		return cleanup, credential.ErrNoKey
	}

	var paths []string
	cleanup = func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}
	for _, id := range ids {
		c, err := d.credentials.Get(context.Background(), id)
		if err != nil {
			return cleanup, fmt.Errorf("credential %s: %w", id, err)
		}
		plain, err := d.sealer.Open(c.Secret)
		if err != nil {
			return cleanup, fmt.Errorf("decrypt credential %s: %w", id, err)
		}
		plain, err = secretsource.Resolve(context.Background(), c.Source, plain)
		if err != nil {
			return cleanup, fmt.Errorf("resolve credential %s: %w", id, err)
		}
		f, err := os.CreateTemp("", "yardmaster-cred-*")
		if err != nil {
			return cleanup, fmt.Errorf("materialize credential %s: %w", id, err)
		}
		paths = append(paths, f.Name())
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return cleanup, fmt.Errorf("materialize credential %s: %w", id, err)
		}
		if _, err := f.WriteString(plain); err != nil {
			_ = f.Close()
			return cleanup, fmt.Errorf("materialize credential %s: %w", id, err)
		}
		if err := f.Close(); err != nil {
			return cleanup, fmt.Errorf("materialize credential %s: %w", id, err)
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
				return cleanup, fmt.Errorf("encode become credential %s: %w", id, err)
			}
			if err := os.WriteFile(f.Name(), vars, 0o600); err != nil {
				return cleanup, fmt.Errorf("materialize credential %s: %w", id, err)
			}
			spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, f.Name())
		case credential.KindRegistry:
			// Registry logins are consumed by the container runner for image pulls, not the play.
			paths = paths[:len(paths)-1]
			_ = os.Remove(f.Name())
		default:
			return cleanup, fmt.Errorf("%w: %s", credential.ErrBadKind, c.Kind)
		}
	}
	return cleanup, nil
}

// effectiveCredentialIDs returns the run's own credentials plus any attached to the stored inventory
// it targets, deduplicated and in order, so an inventory can carry secret variables that every run
// against it receives.
func (d *Dispatcher) effectiveCredentialIDs(r *run.Run) []string {
	ids := append([]string(nil), r.CredentialIDs...)
	if r.InventoryID != "" && d.inventories != nil {
		if inv, err := d.inventories.Get(context.Background(), r.InventoryID); err == nil {
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
