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

// WithCredentialTypes gives the dispatcher the operator-defined credential types, so a credential
// that names one is injected per its type rather than by a built-in kind.
func WithCredentialTypes(types credential.TypeStore) Option {
	return func(c *config) {
		c.credentialTypes = types
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
		// A credential of a custom type carries its field values as a JSON object, not a single
		// secret, and its type decides the injection. It contributes environment variables and, when
		// the type declares them, an extra-vars file, and never a raw credential file, so it takes
		// its own path and skips the per-kind switch below.
		if c.TypeID != "" {
			vf, err := d.injectTypedCredential(ctx, c, plain, spec, &secrets)
			if err != nil {
				return cleanup, secrets, err
			}
			if vf != "" {
				paths = append(paths, vf)
			}
			continue
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
			// The decrypted key is masked as well as the passphrase. Only the stored form was
			// registered, and for a passphrase-protected key that form is JSON whose PEM line
			// breaks are escaped, so the masker never saw a line it could match. Unlocking also
			// re-encodes the key, so the bytes written to disk are not the bytes anyone registered.
			// A playbook that reads the key file back wrote it verbatim into the stored log.
			secrets = append(secrets, unlocked)
			if err := os.WriteFile(f.Name(), []byte(unlocked), 0o600); err != nil {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
			spec.PrivateKeyPath = f.Name()
		case credential.KindVaultPassword:
			spec.VaultPasswords = append(spec.VaultPasswords,
				roundhouse.VaultPassword{Label: c.VaultID, Path: f.Name()})
		case credential.KindEnv:
			// Environment pairs go straight into the process; the temp file is not needed.
			paths = paths[:len(paths)-1]
			_ = os.Remove(f.Name())
			spec.Env = append(spec.Env, credential.EnvLines(plain)...)
		case credential.KindToken:
			// A token is exposed as one environment variable; the temp file is not needed.
			paths = paths[:len(paths)-1]
			_ = os.Remove(f.Name())
			// One value, one variable. Only trailing newlines were trimmed, and a container run
			// writes these entries into an env file one per line, so a value with a newline in the
			// middle became several variables: a token ending in "\nLD_PRELOAD=/proj/evil.so" set
			// LD_PRELOAD for the run. Values arrive from a secret source's stdout, where an
			// unintended newline is ordinary.
			token := strings.TrimRight(plain, "\r\n")
			if strings.ContainsAny(token, "\n\r") {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: the token spans "+
					"more than one line, and a token is one value", id)
			}
			spec.Env = append(spec.Env, credential.TokenEnvVar+"="+token)
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
			spec.Env = append(spec.Env, inj.Env...)
			if inj.Secrets != nil {
				// The injector named exactly which of its values are secret, so a non-secret
				// constant it sets, an OpenStack domain of "Default" or a region, is not redacted
				// from every occurrence of that word in the run's output.
				secrets = append(secrets, inj.Secrets...)
			} else {
				// An injector that names no secrets has every value it produced masked, the
				// conservative default for a credential whose fields are all sensitive.
				for _, line := range inj.Env {
					if _, val, ok := strings.Cut(line, "="); ok {
						secrets = append(secrets, val)
					}
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

// injectTypedCredential applies a custom-typed credential to the spec, returning the path of any
// extra-vars file it wrote so the caller can clean it up.
//
// The field values are the sealed JSON object; the type's injectors turn them into environment
// variables and extra vars. Every value the type marks secret is added to the mask, so a field a
// tool echoes is redacted the same as any built-in secret. Extra vars go through a private file so
// they never land on argv, the way a become or network credential's variables do.
func (d *Dispatcher) injectTypedCredential(ctx context.Context, c *credential.Credential, plain string,
	spec *roundhouse.Spec, secrets *[]string) (string, error) {
	if d.credentialTypes == nil {
		return "", fmt.Errorf("materialize credential %s: it names a custom type but none are "+
			"configured", c.ID)
	}
	typ, err := d.credentialTypes.Get(ctx, c.TypeID)
	if err != nil {
		return "", fmt.Errorf("materialize credential %s: read type %s: %w", c.ID, c.TypeID, err)
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(plain), &values); err != nil {
		return "", fmt.Errorf("materialize credential %s: decode field values: %w", c.ID, err)
	}
	inj, err := typ.Inject(values)
	if err != nil {
		return "", fmt.Errorf("materialize credential %s: %w", c.ID, err)
	}
	spec.Env = append(spec.Env, inj.Env...)
	*secrets = append(*secrets, inj.Secrets...)
	if len(inj.ExtraVars) == 0 {
		return "", nil
	}
	vf, err := os.CreateTemp("", "switchtender-cred-*")
	if err != nil {
		return "", fmt.Errorf("materialize credential %s: %w", c.ID, err)
	}
	if err := vf.Chmod(0o600); err != nil {
		_ = vf.Close()
		return vf.Name(), fmt.Errorf("materialize credential %s: %w", c.ID, err)
	}
	if err := vf.Close(); err != nil {
		return vf.Name(), fmt.Errorf("materialize credential %s: %w", c.ID, err)
	}
	if err := writeAnsibleVarsFile(vf.Name(), inj.ExtraVars); err != nil {
		return vf.Name(), fmt.Errorf("materialize credential %s: %w", c.ID, err)
	}
	spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, vf.Name())
	return vf.Name(), nil
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
