package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/runfiles"
	"github.com/kordloom/switchtender/internal/secretsource"
	"github.com/kordloom/switchtender/internal/util"
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
// is accepted, so a bad reference fails at submit time instead of execution time. When enforceTool
// is set, an Ansible-only credential attached to a non-Ansible run is also rejected. The tool check
// is skipped for credentials inherited from the target inventory: those are shared by every tool
// that targets the inventory and are inert under a tool they do not fit, so rejecting a terraform
// run because the inventory also carries an ssh key would couple inventory config to tool choice.
// Existence and decryptability, which do fail a run at execution, are checked in both cases.
func (d *Dispatcher) validateCredentials(ctx context.Context, tool string, ids []string, enforceTool bool) error {
	if len(ids) == 0 {
		return nil
	}
	if d.credentials == nil || d.sealer == nil || !d.sealer.Enabled() {
		return credential.ErrNoKey
	}
	ansible := run.NormalizeTool(tool) == run.ToolAnsible
	federated := map[credential.Kind]string{}
	for _, id := range ids {
		c, err := d.credentials.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: %s", err, id)
		}
		// A federated credential holds no secret to open. What it needs instead is an issuer on
		// this process and settings that will mint, checked now so a run that cannot get its token
		// is refused at submit rather than failing once it is running.
		if credential.Federated(c.Kind) {
			if _, err := d.checkFederated(c, federated); err != nil {
				return err
			}
			continue
		}
		if _, err := d.sealer.Open(c.Secret); err != nil {
			return unopenable(c, err)
		}
		if enforceTool && !ansible && credential.AnsibleOnly(c.Kind) {
			return fmt.Errorf("%w: credential %s of kind %s applies only to the ansible tool, not %s",
				ErrToolCredential, id, c.Kind, run.NormalizeTool(tool))
		}
	}
	return nil
}

// unopenable explains why a stored credential's secret did not open. A credential with no secret is
// one nobody has set yet, which every imported credential is until someone does, and one that does
// not open was sealed under a different key or salt, or was damaged. Both reached whoever launched as
// a bare 500 with "sealed value too short" in the server log, so each error names the credential and
// the fix.
func unopenable(c *credential.Credential, err error) error {
	if c.Secret == "" {
		return fmt.Errorf("%w: %q. Set its secret on the Credentials page or with "+
			"PUT /v1/credentials/%s, then launch again", credential.ErrNoSecret, c.Name, c.ID)
	}
	return fmt.Errorf("%w: %q (%v). It was sealed under a different SWITCHTENDER_ENCRYPTION_KEY or "+
		"SWITCHTENDER_ENCRYPTION_SALT, or its stored value is damaged. Restore the key and salt it was "+
		"sealed with, or set its secret again", credential.ErrUnreadable, c.Name, err)
}

// inventoryCredentialIDs returns the credentials the run's target inventory attaches, minus the
// run's own, so the caller can validate the inherited set without repeating the run's credentials.
func (d *Dispatcher) inventoryCredentialIDs(ctx context.Context, r *run.Run) []string {
	if r.InventoryID == "" || d.inventories == nil {
		return nil
	}
	inv, err := d.inventories.Get(ctx, r.InventoryID)
	if err != nil {
		return nil
	}
	own := make(map[string]bool, len(r.CredentialIDs))
	for _, id := range r.CredentialIDs {
		own[id] = true
	}
	var out []string
	for _, id := range inv.CredentialIDs {
		if id != "" && !own[id] {
			out = append(out, id)
		}
	}
	return out
}

// materializeCredentials decrypts the run's credentials into files only the executing process can
// read and maps them onto the spec. It also returns every resolved plaintext secret so the caller
// can redact those values from the run's output. The returned cleanup removes every file.
//
// Every file is written into one private directory for the run, mode 0700 with each file 0600, and
// the cleanup removes the directory whole. The caller defers the cleanup, so success, failure, a
// cancel, and a timeout all end there. A process killed mid-run runs no cleanup at all, which is
// why the directory is locked for the life of the run, its heartbeat counts while the process
// lives, and every dispatcher on the host sweeps the directories whose lock nobody holds.
//
// The directory is made for every run, credentials or not, and handed to the runner as the spec's
// RunDir, because a run carries secrets by other routes too: secret survey answers reach the extra
// vars file, an inline script can hold a token, and a containerized run's environment file holds
// every injected value. All of them now sit where the sweep reaches them.
func (d *Dispatcher) materializeCredentials(ctx context.Context, r *run.Run, spec *roundhouse.Spec) (func(), []string, error) {
	return d.materializeFrom(ctx, storeSource{d: d}, r, spec)
}

// materializeFrom is materializeCredentials with the secrets taken from src: this executor's own
// store, or what the control node delivered to a relay worker. Everything after the opening is the
// same either way, so a delivered credential lands in the same private run directory, in the same
// form, and reaches the masker the same way.
func (d *Dispatcher) materializeFrom(ctx context.Context, src secretSource, r *run.Run,
	spec *roundhouse.Spec) (func(), []string, error) {
	dir, err := runfiles.Create(d.runFiles(), r.ID)
	if err != nil {
		return func() {}, nil, fmt.Errorf("create the run's private directory: %w", err)
	}
	spec.RunDir, spec.RunFilesRoot = dir.Path(), d.runFiles()
	cleanup := func() {
		if err := dir.Remove(); err != nil && d.log != nil {
			d.log.Warn("dispatch: remove run credential directory: "+err.Error(),
				zap.String("run_id", r.ID))
		}
	}
	ids, err := src.credentialIDs(ctx, r)
	if err != nil {
		return cleanup, nil, err
	}
	if len(ids) == 0 {
		return cleanup, nil, nil
	}

	var paths []string
	var secrets []string
	var leases []*secretsource.Lease
	// runDir returns the run's private directory.
	runDir := func() (*runfiles.Dir, error) {
		return dir, nil
	}
	// newFile creates a private file in the run's directory.
	newFile := func() (*os.File, error) {
		rd, err := runDir()
		if err != nil {
			return nil, err
		}
		return rd.CreateTemp("cred-*")
	}
	cleanup = func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
		// Every file a run writes, federated token files included, lives in the run's one private
		// directory, removed whole, so no file written into it can outlive the run.
		if err := dir.Remove(); err != nil && d.log != nil {
			d.log.Warn("dispatch: remove run credential directory: "+err.Error(),
				zap.String("run_id", r.ID))
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
	federated := map[credential.Kind]string{}
	for _, id := range ids {
		c, plain, lease, err := src.credential(ctx, id)
		if lease != nil {
			leases = append(leases, lease)
		}
		if err != nil && !errors.Is(err, errFederatedCredential) {
			return cleanup, secrets, err
		}
		// A federated credential stores nothing and resolves nothing. It mints the run's identity
		// token now, into the run's private directory, created mode 0700 and registered for removal
		// before anything is written to it, so every return below, and the caller's deferred cleanup
		// on every exit after this, takes the token files with it. The directory is locked for the
		// life of the run, so the crash sweep reclaims token files a killed process left behind.
		if errors.Is(err, errFederatedCredential) {
			rd, err := runDir()
			if err != nil {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
			delivery, err := src.federated(ctx, r, c, spec.DryRun, rd.Path(), federated)
			// The token joins the mask list even when the delivery failed partway, such as an
			// exchange refused after the token was minted, so no reader of the list misses it.
			secrets = append(secrets, delivery.Secrets...)
			if err != nil {
				return cleanup, secrets, err
			}
			spec.Env = append(spec.Env, delivery.Env...)
			// The container plan mounts these, so the path a variable names resolves inside the
			// container as well as on the host.
			spec.CredentialFiles = append(spec.CredentialFiles, delivery.Files...)
			continue
		}
		// A kubeconfig is handed to the masker by its secret values, which its injector names, and
		// not whole. The masker redacts every line of a value, and a kubeconfig's lines include
		// "apiVersion: v1", which every kubectl get -o yaml prints.
		if c.Kind != credential.KindKubeconfig {
			secrets = append(secrets, plain)
		}
		if c.Kind == credential.KindEnv {
			// The secret is each value, not the KEY=VALUE bundle, so mask the values a tool may echo.
			for _, line := range credential.EnvLines(plain) {
				if _, val, ok := strings.Cut(line, "="); ok {
					secrets = append(secrets, val)
				}
			}
		}
		// A credential of a custom type carries its field values as a JSON object, not a single
		// secret, and its type decides the injection. It contributes environment variables, the
		// files its type renders, and, when the type declares them, an extra-vars file, so it takes
		// its own path and skips the per-kind switch below. Every file it creates is registered for
		// cleanup as it is created, before any error is checked, so a failure partway leaves nothing.
		if c.TypeID != "" {
			if err := d.injectTypedCredential(ctx, src, c, plain, spec, &secrets, newFile,
				&paths); err != nil {
				return cleanup, secrets, err
			}
			d.warnUnreferencedFiles(ctx, src, r, c)
			continue
		}
		f, err := newFile()
		if err != nil {
			return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
		}
		paths = append(paths, f.Name())
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
		}
		// Only a vault password is consumed as the raw file content, so only it writes plain here.
		// ssh_key writes its own unlocked key; the connection and become kinds overwrite the file
		// with a vars file; env, token, registry, and custom kinds remove it and inject by other
		// means. Writing plain for those wrote the decrypted secret to disk only to unlink it, a
		// needless exposure window the empty file below does not have.
		if c.Kind == credential.KindVaultPassword {
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
			// A key credential's settings can carry the connection user and become settings an AWX
			// machine credential bundles beside the key, so an import lands runnable. They reach the
			// play through a vars file like every other connection variable.
			if vars := connectionVars(c.Settings["user"], c.Settings["become_method"],
				c.Settings["become_user"]); len(vars) > 0 {
				vf, err := newFile()
				if err != nil {
					return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
				}
				paths = append(paths, vf.Name())
				if err := vf.Close(); err != nil {
					return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
				}
				if err := writeAnsibleVarsFile(vf.Name(), vars); err != nil {
					return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
				}
				spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, vf.Name())
			}
		case credential.KindVaultPassword:
			spec.VaultPasswords = append(spec.VaultPasswords,
				roundhouse.VaultPassword{Label: c.VaultID, Path: f.Name()})
		case credential.KindEnv:
			// Environment pairs go straight into the process; the temp file is not needed. Settings
			// on an env credential are reference metadata only, not injected: injecting them would
			// let a non-secret pair from one credential clobber a sealed env var of the same name on
			// another, and an unmatched import that fell back to this kind would spill its recorded
			// inputs into every run's environment.
			paths = paths[:len(paths)-1]
			_ = os.Remove(f.Name())
			pairs, err := credential.EnvPairs(plain)
			if err != nil {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
			spec.Env = append(spec.Env, pairs...)
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
			applySettings(fm, c.Settings, "user", "become_method", "become_user")
			user, pass := fm["user"], fm["password"]
			if user == "" || pass == "" {
				return cleanup, secrets, fmt.Errorf("%w: ssh_password needs user and password",
					credential.ErrBadField)
			}
			secrets = append(secrets, pass)
			vars := connectionVars(user, fm["become_method"], fm["become_user"])
			vars["ansible_password"] = pass
			if err := writeAnsibleVarsFile(f.Name(), vars); err != nil {
				return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
			}
			spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, f.Name())
		case credential.KindBecome:
			// Privilege escalation vars reach the play through a file so the password stays off argv.
			fm := credential.Fields(plain)
			applySettings(fm, c.Settings, "method", "user")
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
			applySettings(fm, c.Settings, "user", "network_os", "connection")
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
			secrets = append(secrets, injectedMaskValues(inj)...)
			for _, file := range inj.Files {
				ff, err := newFile()
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
				// A file whose injector named its secret values one by one is not handed over whole:
				// the masker redacts every line of a value, and a kubeconfig's ordinary lines would
				// vanish from the run's output wherever they appear.
				if !file.MaskByField {
					secrets = append(secrets, file.Content)
				}
				// The decoded fields are registered as well as the stored blob.
				//
				// A GCP service-account file is JSON, and its private_key is a PEM whose line breaks
				// are escaped inside that JSON. Registering only the stored form meant the masker
				// held the escaped spelling, so the moment a playbook decoded the file and printed
				// the key, the real PEM with real newlines matched nothing and went into the stored
				// log verbatim. The ssh_key path learned this already, for the same reason: the form
				// that reaches the output is not always the form that was stored.
				secrets = append(secrets, decodedJSONSecrets(file.Content)...)
				// The container plan mounts these, so the path the variable names resolves inside the
				// container as well as on the host.
				spec.CredentialFiles = append(spec.CredentialFiles, ff.Name())
				for _, ev := range file.EnvVars {
					spec.Env = append(spec.Env, ev+"="+ff.Name())
				}
			}
			// A registered kind can declare extra vars the same as a custom type. The typed path
			// honors them; the default branch dropped them silently, so a plugin credential's
			// extra vars never reached the play. Write them to a private vars file like every other
			// extra-vars carrier. Their values are masked through injectedMaskValues above.
			if len(inj.ExtraVars) > 0 {
				vf, err := newFile()
				if err != nil {
					return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
				}
				paths = append(paths, vf.Name())
				if err := vf.Close(); err != nil {
					return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
				}
				if err := writeAnsibleVarsFile(vf.Name(), inj.ExtraVars); err != nil {
					return cleanup, secrets, fmt.Errorf("materialize credential %s: %w", id, err)
				}
				spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, vf.Name())
			}
		}
	}
	return cleanup, secrets, nil
}

// applySettings fills absent fields from the credential's non-secret settings, sealed values
// winning on conflict, so an imported credential's connection user or become method takes effect
// without living inside the sealed secret. Settings values are deliberately never added to the mask
// list: they are non-secret by contract, and masking a username like deploy would black out
// ordinary output everywhere it appears.
func applySettings(fm map[string]string, settings map[string]string, keys ...string) {
	for _, k := range keys {
		if fm[k] == "" && settings[k] != "" {
			fm[k] = settings[k]
		}
	}
}

// connectionVars renders the connection and become fields shared by the machine credential kinds
// into their ansible variable names, skipping any that are empty. The key path builds its vars from
// settings and the password path from the merged secret and settings, both through here, so the two
// kinds cannot drift on how a field maps to a variable.
func connectionVars(user, becomeMethod, becomeUser string) map[string]string {
	vars := map[string]string{}
	if user != "" {
		vars["ansible_user"] = user
	}
	if becomeMethod != "" {
		vars["ansible_become_method"] = becomeMethod
	}
	if becomeUser != "" {
		vars["ansible_become_user"] = becomeUser
	}
	return vars
}

// injectedMaskValues returns the values to redact from run output for one injection: the ones the
// injector named secret, or, when it named none, every value it produced, across both the
// environment and the extra vars. An empty Secrets slice is treated the same as a nil one, so an
// injector that names an empty set masks everything rather than nothing and cannot leak its own
// values by accident.
func injectedMaskValues(inj credential.Injection) []string {
	if len(inj.Secrets) > 0 {
		return inj.Secrets
	}
	var out []string
	for _, line := range inj.Env {
		if _, val, ok := strings.Cut(line, "="); ok {
			out = append(out, val)
		}
	}
	for _, val := range inj.ExtraVars {
		out = append(out, val)
	}
	return out
}

// injectTypedCredential applies a custom-typed credential to the spec. newFile creates a private
// file in the run's directory, and every file created is appended to paths as soon as it exists, so
// the caller's cleanup removes it even when a later step fails.
//
// The field values are the sealed JSON object; the type's injectors turn them into files,
// environment variables, and extra vars. Files are rendered and written first, because an env or
// extra-var template may hand a file's path to the tool, which is how a kubeconfig reaches kubectl.
// Every value the type marks secret is added to the mask, so a field a tool echoes is redacted the
// same as any built-in secret. Extra vars go through a private file so they never land on argv, the
// way a become or network credential's variables do.
func (d *Dispatcher) injectTypedCredential(ctx context.Context, src secretSource, c *credential.Credential,
	plain string, spec *roundhouse.Spec, secrets *[]string, newFile func() (*os.File, error),
	paths *[]string) error {
	typ, err := src.credentialType(ctx, c)
	if err != nil {
		return err
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(plain), &values); err != nil {
		return fmt.Errorf("materialize credential %s: decode field values: %w", c.ID, err)
	}
	// With no field marked secret, every value is masked, the same fail-safe injectedMaskValues
	// applies to the env and extra vars below. A value that only reaches a file is not in either of
	// those, so without this a type nobody marked wrote its values into a file a tool can print.
	if len(typ.SecretFields()) == 0 {
		for _, v := range values {
			*secrets = append(*secrets, v)
		}
	}
	files, err := typ.RenderFiles(values)
	if err != nil {
		return fmt.Errorf("materialize credential %s: %w", c.ID, err)
	}
	written := make(map[string]string, len(files))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f, err := newFile()
		if err != nil {
			return fmt.Errorf("materialize credential %s: %w", c.ID, err)
		}
		*paths = append(*paths, f.Name())
		_, werr := f.WriteString(files[name])
		if err := errors.Join(werr, f.Close()); err != nil {
			return fmt.Errorf("materialize credential %s: write file %s: %w", c.ID, name, err)
		}
		written[name] = f.Name()
		// The container plan mounts these, so the path an injector hands over resolves inside the
		// container as well as on the host.
		spec.CredentialFiles = append(spec.CredentialFiles, f.Name())
	}
	inj, err := typ.Inject(values, written)
	if err != nil {
		return fmt.Errorf("materialize credential %s: %w", c.ID, err)
	}
	spec.Env = append(spec.Env, inj.Env...)
	// The same fail-safe the built-in kinds use. Taking inj.Secrets raw meant a custom type whose
	// fields nobody marked secret injected its values with nothing in the mask list: the sealed JSON
	// blob is what reached the masker, and that string never appears in tool output, so an
	// ansible-playbook -vvv, a bash step running env, or a provider debug line wrote the value into
	// the stored log and the live stream unredacted. Nothing requires a field to be marked, so
	// forgetting the flag is a one-word mistake with no warning attached to it.
	*secrets = append(*secrets, injectedMaskValues(inj)...)
	if len(inj.ExtraVars) == 0 {
		return nil
	}
	vf, err := newFile()
	if err != nil {
		return fmt.Errorf("materialize credential %s: %w", c.ID, err)
	}
	*paths = append(*paths, vf.Name())
	if err := vf.Close(); err != nil {
		return fmt.Errorf("materialize credential %s: %w", c.ID, err)
	}
	if err := writeAnsibleVarsFile(vf.Name(), inj.ExtraVars); err != nil {
		return fmt.Errorf("materialize credential %s: %w", c.ID, err)
	}
	spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, vf.Name())
	return nil
}

// runFiles returns the directory run credential directories are created under.
func (d *Dispatcher) runFiles() string {
	if d.runFilesRoot != "" {
		return d.runFilesRoot
	}
	return runfiles.DefaultRoot()
}

// sweepRunFiles sweeps the run-files root with s until the dispatcher closes: once as it starts,
// which on a restarted server or worker is the first moment anything can, and about every five
// minutes after, so a host whose only worker crashed for good is still cleaned by any server or
// worker that shares its root. A directory is removed only after two sweeps a gap apart find its
// lock free and its heartbeat still, so a restart costs one gap of waiting and never a live run's
// files.
func (d *Dispatcher) sweepRunFiles(s *runfiles.Sweeper) {
	defer d.wg.Done()
	s.Run(d.ctx, func(res runfiles.PassResult, err error) {
		if err != nil {
			d.log.Warn("dispatch: sweep run directories: "+err.Error(), zap.String("root", s.Root()))
		}
		if res.Removed > 0 {
			d.log.Info("dispatch: removed run directories left by a process that stopped",
				zap.Int("count", res.Removed), zap.String("root", s.Root()))
		}
	})
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
	// A federated credential has no value to open. Its token is minted for a run and belongs to the
	// run's environment, so a project key, a registry login, or an inventory source naming one is
	// refused rather than handed an empty secret it would use as a blank login. The credential is
	// still returned, so run materialization can tell this refusal apart and mint instead.
	if credential.Federated(c.Kind) {
		return c, "", nil, fmt.Errorf("%w: credential %q is %s, which mints a token for a run's "+
			"environment and cannot stand in for a stored secret here", errFederatedCredential, c.Name,
			c.Kind)
	}
	plain, err := d.sealer.Open(c.Secret)
	if err != nil {
		return nil, "", nil, unopenable(c, err)
	}
	value, lease, err := secretsource.ResolveLeased(ctx, c.Source, plain)
	if err != nil {
		return nil, "", nil, fmt.Errorf("resolve credential %s: %w", id, err)
	}
	return c, value, lease, nil
}

// revokeLease hands a dynamic source's minted secret back, on its own timeout so a slow engine
// cannot hold a run open. Every caller that opens a credential outside run materialization uses it,
// since a lease that is never revoked leaves live credentials behind on the engine.
func (d *Dispatcher) revokeLease(lease *secretsource.Lease) {
	if lease == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
	defer cancel()
	if err := lease.Revoke(ctx); err != nil {
		d.log.Warn("dispatch: revoke ephemeral secret failed: "+err.Error(),
			zap.String("engine", lease.Kind()))
	}
}

// sshKeyFrom turns a credential's resolved value into a usable private key, decrypting a
// passphrase-protected one in process so the passphrase never reaches disk or argv.
//
// It is shared with run materialization rather than repeated, because a caller that skips it hands
// the SSH parser the stored JSON wrapper instead of a key, and the failure reads as a bad key rather
// than as a step that was left out.
func sshKeyFrom(plain string) (key, passphrase string, err error) {
	material := credential.ParseSSHKey(plain)
	unlocked, err := credential.UnlockSSHKey(material.PrivateKey, material.Passphrase)
	if err != nil {
		return "", material.Passphrase, err
	}
	return unlocked, material.Passphrase, nil
}

// effectiveCredentialIDs returns the run's own credentials plus any attached to the stored inventory
// it targets, deduplicated and in order, so an inventory can carry secret variables that every run
// against it receives.
//
// A run executing an inventory snapshot receives the credentials the inventory attached when the run
// was submitted, which its approval covers, never the ones the inventory attaches now.
func (d *Dispatcher) effectiveCredentialIDs(ctx context.Context, r *run.Run) []string {
	ids := append([]string(nil), r.CredentialIDs...)
	switch {
	case r.InventorySnapshot != nil:
		ids = append(ids, r.InventorySnapshot.CredentialIDs...)
	case r.InventoryID != "" && d.inventories != nil:
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

// decodedJSONSecrets returns the string values of a credential file that is a JSON object, so the
// masker holds each field in the form a run will actually print rather than only the escaped form
// they were stored in. A file that is not a JSON object yields nothing.
//
// A field is registered when its NAME classifies as a secret, by the one classifier the rest of the
// product uses, or when its value is plainly key material. Registering every long string instead
// would hand the masker ordinary identifiers, such as a service account's type or project, and turn
// redaction into a search for common words that scribbles over legitimate output.
func decodedJSONSecrets(content string) []string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return nil
	}
	out := make([]string, 0, len(obj))
	for name, v := range obj {
		text, ok := v.(string)
		if !ok || text == "" || text == content {
			continue
		}
		if util.SecretKey(name) || strings.Contains(text, "PRIVATE KEY-----") {
			out = append(out, text)
		}
	}
	return out
}
