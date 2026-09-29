package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
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
func (d *Dispatcher) validateCredentials(ctx context.Context, tool string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if d.credentials == nil || d.sealer == nil || !d.sealer.Enabled() {
		return credential.ErrNoKey
	}
	ansible := run.NormalizeTool(tool) == run.ToolAnsible
	for _, id := range ids {
		c, err := d.credentials.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: %s", err, id)
		}
		if _, err := d.sealer.Open(c.Secret); err != nil {
			return fmt.Errorf("decrypt credential %s: %w", id, err)
		}
		if !ansible && credential.AnsibleOnly(c.Kind) {
			return fmt.Errorf("%w: credential %s of kind %s applies only to the ansible tool, not %s",
				ErrToolCredential, id, c.Kind, run.NormalizeTool(tool))
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
		// An ssh_key writes its own unlocked key below, so the sealed plaintext, which may hold a
		// passphrase, never lands on disk. Every other kind writes its raw plaintext here.
		if c.Kind != credential.KindSSHKey {
			if _, err := f.WriteString(plain); err != nil {
				_ = f.Close()
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
		}
		if err := f.Close(); err != nil {
			return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
		}

		switch c.Kind {
		case credential.KindSSHKey:
			// Decrypt a passphrase protected key in process and write only the unlocked key, so the
			// passphrase never reaches disk or argv and the tool never prompts. A bare key passes through.
			material := credential.ParseSSHKey(plain)
			if material.Passphrase != "" {
				secrets = append(secrets, material.Passphrase)
			}
			unlocked, err := credential.UnlockSSHKey(material.PrivateKey, material.Passphrase)
			if err != nil {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
			if err := os.WriteFile(f.Name(), []byte(unlocked), 0o600); err != nil {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
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
		case credential.KindSSHPassword:
			// Machine password auth reaches the play as connection vars through a file, off argv.
			fm := credential.Fields(plain)
			user, pass := fm["user"], fm["password"]
			if user == "" || pass == "" {
				return cleanup, secrets, fmt.Errorf("%w: ssh_password needs user and password",
					credential.ErrBadField)
			}
			secrets = append(secrets, pass)
			if err := writeAnsibleVarsFile(f.Name(), map[string]string{
				"ansible_user":     user,
				"ansible_password": pass,
			}); err != nil {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
			spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, f.Name())
		case credential.KindBecome:
			// Privilege escalation vars reach the play through a file so the password stays off argv.
			fm := credential.Fields(plain)
			pass := fm["password"]
			if pass == "" {
				return cleanup, secrets, fmt.Errorf("%w: become needs password", credential.ErrBadField)
			}
			secrets = append(secrets, pass)
			become := map[string]string{"ansible_become_password": pass}
			if method := fm["method"]; method != "" {
				become["ansible_become_method"] = method
			}
			if user := fm["user"]; user != "" {
				become["ansible_become_user"] = user
			}
			if err := writeAnsibleVarsFile(f.Name(), become); err != nil {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
			spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, f.Name())
		case credential.KindNetwork:
			// Network device vars reach the play through a file so the password stays off argv.
			fm := credential.Fields(plain)
			user, pass := fm["user"], fm["password"]
			if user == "" || pass == "" {
				return cleanup, secrets, fmt.Errorf("%w: network needs user and password",
					credential.ErrBadField)
			}
			secrets = append(secrets, pass)
			connection := fm["connection"]
			if connection == "" {
				connection = "network_cli"
			}
			netVars := map[string]string{
				"ansible_user":       user,
				"ansible_password":   pass,
				"ansible_connection": connection,
			}
			if netOS := fm["network_os"]; netOS != "" {
				netVars["ansible_network_os"] = netOS
			}
			if err := writeAnsibleVarsFile(f.Name(), netVars); err != nil {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
			spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, f.Name())
		case credential.KindRegistry:
			// Registry logins are consumed by the container runner for image pulls, not the play.
			paths = paths[:len(paths)-1]
			_ = os.Remove(f.Name())
		default:
			// Typed and custom kinds contribute environment variables and files through a registered
			// injector. The raw temp file written above holds the unparsed material, so drop it.
			paths = paths[:len(paths)-1]
			_ = os.Remove(f.Name())
			inj, err := credential.Inject(c.Kind, plain)
			if err != nil {
				return cleanup, secrets, err
			}
			for _, line := range inj.Env {
				spec.Env = append(spec.Env, line)
				if _, val, ok := strings.Cut(line, "="); ok {
					secrets = append(secrets, val)
				}
			}
			for _, file := range inj.Files {
				ff, err := os.CreateTemp("", "switchtender-cred-*")
				if err != nil {
					return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
				}
				paths = append(paths, ff.Name())
				if err := ff.Chmod(0o600); err != nil {
					_ = ff.Close()
					return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
				}
				if _, err := ff.WriteString(file.Content); err != nil {
					_ = ff.Close()
					return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
				}
				if err := ff.Close(); err != nil {
					return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
				}
				secrets = append(secrets, file.Content)
				for _, ev := range file.EnvVars {
					spec.Env = append(spec.Env, ev+"="+ff.Name())
				}
			}
		}
	}
	return cleanup, secrets, nil
}

// writeAnsibleVarsFile encodes vars as JSON into the private file at path, so a connection or become
// credential passes its Ansible variables through an extra-vars file and keeps them off argv.
func writeAnsibleVarsFile(path string, vars map[string]string) error {
	data, err := json.Marshal(vars)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
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
