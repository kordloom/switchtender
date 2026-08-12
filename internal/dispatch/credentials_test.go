package dispatch

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// dynLease tracks whether the fake dynamic engine's lease was revoked, so the revoke-after-run test
// can assert the run cleanup ended the minted secret.
var dynLease struct {
	mu      sync.Mutex
	revoked bool
}

// init registers a fake dynamic secrets engine once before any test runs, so the minters map is not
// written while parallel tests read it. Its mint returns a value derived from the config and a lease
// whose revoke records that it fired.
func init() {
	secretsource.RegisterDynamic("test_dynamic", func(_ context.Context, config string) (string, *secretsource.Lease, error) {
		return "minted-" + config, secretsource.NewLease("test_dynamic", func(context.Context) error {
			dynLease.mu.Lock()
			dynLease.revoked = true
			dynLease.mu.Unlock()
			return nil
		}), nil
	})
	// A registered custom kind whose injector returns an extra var, so the default injection branch's
	// handling of Injection.ExtraVars can be exercised. Registered here because the injector registry
	// is written only at init, before any parallel test reads it.
	credential.RegisterInjector("test_extravars_kind", func(secret string) (credential.Injection, error) {
		return credential.Injection{
			ExtraVars: map[string]string{"api_token": secret},
			Secrets:   []string{secret},
		}, nil
	})
}

func TestMaterializeCommandCredential(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("echo API_TOKEN=secret123")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "cred_1", Name: "vault-token", Kind: credential.KindEnv,
		Source: credential.SourceCommand, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d := &Dispatcher{credentials: store, sealer: sealer}
	spec := &roundhouse.Spec{}
	cleanup, _, err := d.materializeCredentials(context.Background(), &run.Run{ID: "run_1", CredentialIDs: []string{"cred_1"}}, spec)
	defer cleanup()
	if err != nil {
		t.Fatalf("materializeCredentials() error = %v", err)
	}

	found := false
	for _, e := range spec.Env {
		if e == "API_TOKEN=secret123" {
			found = true
		}
	}
	if !found {
		t.Errorf("spec.Env = %v, want API_TOKEN=secret123 resolved from the command", spec.Env)
	}
}

// TestValidateCredentialsToolMismatch verifies an Ansible-only credential attached to a non-Ansible
// run is rejected at submit rather than silently ignored at execution, and that the same credential is
// accepted under Ansible and env-based kinds are accepted under any tool.
func TestValidateCredentialsToolMismatch(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	keySealed, err := sealer.Seal("PRIVATE KEY DATA")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	envSealed, err := sealer.Seal("API_TOKEN=abc123")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := credential.NewMemStore()
	for _, c := range []*credential.Credential{
		{ID: "ssh", Name: "deploy-key", Kind: credential.KindSSHKey, Secret: keySealed},
		{ID: "env", Name: "cloud", Kind: credential.KindEnv, Secret: envSealed},
	} {
		if err := store.Save(context.Background(), c); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	d := &Dispatcher{credentials: store, sealer: sealer}

	tests := []struct {
		Tool string
		IDs  []string
		Want error
	}{
		{Tool: run.ToolTerraform, IDs: []string{"ssh"}, Want: ErrToolCredential}, // Test 0: ssh key on terraform is rejected.
		{Tool: run.ToolBash, IDs: []string{"ssh"}, Want: ErrToolCredential},      // Test 1: ssh key on bash is rejected.
		{Tool: run.ToolAnsible, IDs: []string{"ssh"}, Want: nil},                 // Test 2: ssh key on ansible is fine.
		{Tool: "", IDs: []string{"ssh"}, Want: nil},                              // Test 3: empty tool means ansible.
		{Tool: run.ToolTerraform, IDs: []string{"env"}, Want: nil},               // Test 4: env applies to any tool.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := d.validateCredentials(context.Background(), test.Tool, test.IDs, true)
			if !errors.Is(err, test.Want) {
				t.Errorf("validateCredentials(%q) error = %v, want %v", test.Tool, err, test.Want)
			}
		})
	}
}

// TestValidateRunInventoryCredentials proves validateRun checks credentials the target inventory
// attaches, not only the run's own: an undecryptable inventory credential fails at submit rather
// than at execution, while an Ansible-only inventory credential on a non-Ansible run is allowed,
// since it is inherited and inert rather than an explicit mistake.
func TestValidateRunInventoryCredentials(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	keySealed, err := sealer.Seal("PRIVATE KEY DATA")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	for _, c := range []*credential.Credential{
		{ID: "inv_ssh", Name: "deploy-key", Kind: credential.KindSSHKey, Secret: keySealed},
		// A credential whose stored secret is not valid ciphertext: decrypt fails.
		{ID: "inv_broken", Name: "broken", Kind: credential.KindEnv, Secret: "not-sealed-bytes"},
	} {
		if err := creds.Save(context.Background(), c); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	invs := inventory.NewMemStore()
	for _, inv := range []*inventory.Inventory{
		{ID: "inv_key", Name: "keyed", Content: "[all]\nlocalhost\n", CredentialIDs: []string{"inv_ssh"}},
		{ID: "inv_bad", Name: "bad", Content: "[all]\nlocalhost\n", CredentialIDs: []string{"inv_broken"}},
	} {
		if err := invs.Save(context.Background(), inv); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	d := &Dispatcher{credentials: creds, sealer: sealer, inventories: invs}

	// A terraform run against an inventory carrying an ssh key is accepted: the inherited cred is
	// inert under terraform, not a mistake to reject.
	if err := d.validateRun(context.Background(),
		&run.Run{ID: "r1", Tool: run.ToolTerraform, InventoryID: "inv_key"}); err != nil {
		t.Errorf("terraform run against a keyed inventory = %v, want accepted", err)
	}
	// An undecryptable inventory credential fails at submit rather than at execution.
	err = d.validateRun(context.Background(),
		&run.Run{ID: "r2", Tool: run.ToolAnsible, InventoryID: "inv_bad"})
	if err == nil {
		t.Error("a run whose inventory carries an undecryptable credential was accepted")
	}
}

func TestMaterializeTokenCredential(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("eyJhbGciOiJIUzI1NiJ9.token\n")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "cred_tok", Name: "api", Kind: credential.KindToken, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d := &Dispatcher{credentials: store, sealer: sealer}
	spec := &roundhouse.Spec{}
	cleanup, _, err := d.materializeCredentials(context.Background(), &run.Run{ID: "run_tok", CredentialIDs: []string{"cred_tok"}}, spec)
	defer cleanup()
	if err != nil {
		t.Fatalf("materializeCredentials() error = %v", err)
	}

	want := credential.TokenEnvVar + "=eyJhbGciOiJIUzI1NiJ9.token"
	found := false
	for _, e := range spec.Env {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Errorf("spec.Env = %v, want %q with the trailing newline trimmed", spec.Env, want)
	}
}

func TestMaterializeInventoryScopedCredential(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("REGION=us-east-1")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(context.Background(), &credential.Credential{
		ID: "cred_inv", Name: "cloud", Kind: credential.KindEnv, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	invs := inventory.NewMemStore()
	if err := invs.Save(context.Background(), &inventory.Inventory{
		ID: "inv_1", Name: "prod", Content: "[all]\nlocalhost\n", CredentialIDs: []string{"cred_inv"},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d := &Dispatcher{credentials: creds, sealer: sealer, inventories: invs}
	spec := &roundhouse.Spec{}
	// The run itself carries no credentials; the inventory it targets does.
	cleanup, _, err := d.materializeCredentials(context.Background(), &run.Run{ID: "run_1", InventoryID: "inv_1"}, spec)
	defer cleanup()
	if err != nil {
		t.Fatalf("materializeCredentials() error = %v", err)
	}

	found := false
	for _, e := range spec.Env {
		if e == "REGION=us-east-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("spec.Env = %v, want REGION=us-east-1 from the inventory-scoped credential", spec.Env)
	}
}

func TestMaterializeDynamicCredentialRevokes(t *testing.T) {
	t.Parallel()
	dynLease.mu.Lock()
	dynLease.revoked = false
	dynLease.mu.Unlock()

	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("app")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "cred_dyn", Name: "dynamic-db", Kind: credential.KindToken,
		Source: "test_dynamic", Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d := &Dispatcher{credentials: store, sealer: sealer, log: zap.NewNop()}
	spec := &roundhouse.Spec{}
	cleanup, secrets, err := d.materializeCredentials(context.Background(),
		&run.Run{ID: "run_dyn", CredentialIDs: []string{"cred_dyn"}}, spec)
	if err != nil {
		cleanup()
		t.Fatalf("materializeCredentials() error = %v", err)
	}

	// The minted value reaches the run as its token and is tracked for masking.
	wantEnv := credential.TokenEnvVar + "=minted-app"
	if !containsString(spec.Env, wantEnv) {
		t.Errorf("spec.Env = %v, want %q from the minted secret", spec.Env, wantEnv)
	}
	if !containsString(secrets, "minted-app") {
		t.Errorf("secrets = %v, want the minted value tracked for masking", secrets)
	}

	// The lease is live until the run cleanup revokes it.
	dynLease.mu.Lock()
	before := dynLease.revoked
	dynLease.mu.Unlock()
	if before {
		t.Error("lease revoked before cleanup, want revoke only after the run")
	}
	cleanup()
	dynLease.mu.Lock()
	after := dynLease.revoked
	dynLease.mu.Unlock()
	if !after {
		t.Error("lease not revoked after cleanup, want the run to end the minted secret")
	}
}

// TestMaterializeConnectionCredentials proves the machine, become, and network kinds each write the
// right Ansible connection or escalation vars to an extra-vars file and track the password for
// masking, so the secret reaches the play through a file and never on the command line.
func TestMaterializeConnectionCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Kind       credential.Kind
		Secret     string
		WantVars   map[string]string
		WantSecret string
	}{{ // Test 0: Machine password injects the user and password connection vars.
		Name:   "ssh_password",
		Kind:   credential.KindSSHPassword,
		Secret: "user=deploy\npassword=s3cret",
		WantVars: map[string]string{
			"ansible_user":     "deploy",
			"ansible_password": "s3cret",
		},
		WantSecret: "s3cret",
	}, { // Test 1: Become injects the full escalation subset.
		Name:   "become full",
		Kind:   credential.KindBecome,
		Secret: "method=sudo\nuser=root\npassword=rootpw",
		WantVars: map[string]string{
			"ansible_become_method":   "sudo",
			"ansible_become_user":     "root",
			"ansible_become_password": "rootpw",
		},
		WantSecret: "rootpw",
	}, { // Test 2: Become with only the required password omits the optional method and user.
		Name:   "become minimal",
		Kind:   credential.KindBecome,
		Secret: "password=justpw",
		WantVars: map[string]string{
			"ansible_become_password": "justpw",
		},
		WantSecret: "justpw",
	}, { // Test 3: Network injects the connection vars and defaults ansible_connection.
		Name:   "network default connection",
		Kind:   credential.KindNetwork,
		Secret: "user=admin\npassword=netpw\nnetwork_os=ios",
		WantVars: map[string]string{
			"ansible_user":       "admin",
			"ansible_password":   "netpw",
			"ansible_network_os": "ios",
			"ansible_connection": "network_cli",
		},
		WantSecret: "netpw",
	}, { // Test 4: Network honors an explicit connection and omits an unset network_os.
		Name:   "network explicit connection",
		Kind:   credential.KindNetwork,
		Secret: "user=admin\npassword=netpw\nconnection=netconf",
		WantVars: map[string]string{
			"ansible_user":       "admin",
			"ansible_password":   "netpw",
			"ansible_connection": "netconf",
		},
		WantSecret: "netpw",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			sealer := credential.NewSealer("pass", "salt")
			sealed, err := sealer.Seal(test.Secret)
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			store := credential.NewMemStore()
			if err := store.Save(context.Background(), &credential.Credential{
				ID: "cred_1", Name: test.Name, Kind: test.Kind, Secret: sealed,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			d := &Dispatcher{credentials: store, sealer: sealer}
			spec := &roundhouse.Spec{}
			cleanup, secrets, err := d.materializeCredentials(context.Background(),
				&run.Run{ID: "run_1", CredentialIDs: []string{"cred_1"}}, spec)
			defer cleanup()
			if err != nil {
				t.Fatalf("materializeCredentials() error = %v", err)
			}

			if len(spec.ExtraVarsFiles) != 1 {
				t.Fatalf("spec.ExtraVarsFiles = %v, want exactly one file", spec.ExtraVarsFiles)
			}
			data, err := os.ReadFile(spec.ExtraVarsFiles[0])
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			var gotVars map[string]string
			if err := json.Unmarshal(data, &gotVars); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if diff := cmp.Diff(test.WantVars, gotVars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ansible vars mismatch (-want +got):\n%s", diff)
			}
			if !containsString(secrets, test.WantSecret) {
				t.Errorf("secrets = %v, want the password %q tracked for masking", secrets, test.WantSecret)
			}
		})
	}
}

// TestMaterializeCredentialSettings proves the non-secret settings carrier: absent fields fill from
// settings, sealed fields win on conflict, become settings ride a machine credential, and settings
// values never enter the mask list, since masking a username would black out ordinary output.
func TestMaterializeCredentialSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name          string
		Kind          credential.Kind
		Secret        string
		Settings      map[string]string
		WantVars      map[string]string
		WantNotMasked []string
		WantMasked    []string
	}{{ // Test 0: Settings fill the user and become fields a password-only secret omits.
		Name:   "ssh_password fills from settings",
		Kind:   credential.KindSSHPassword,
		Secret: "user=\npassword=s3cret",
		Settings: map[string]string{
			"user": "deploy", "become_method": "sudo", "become_user": "root",
		},
		WantVars: map[string]string{
			"ansible_user":          "deploy",
			"ansible_password":      "s3cret",
			"ansible_become_method": "sudo",
			"ansible_become_user":   "root",
		},
		WantNotMasked: []string{"deploy", "sudo", "root"},
		WantMasked:    []string{"s3cret"},
	}, { // Test 1: A sealed field beats the settings value under the same name.
		Name:     "sealed wins",
		Kind:     credential.KindSSHPassword,
		Secret:   "user=alpha\npassword=s3cret",
		Settings: map[string]string{"user": "beta"},
		WantVars: map[string]string{
			"ansible_user":     "alpha",
			"ansible_password": "s3cret",
		},
		WantNotMasked: []string{"beta"},
		WantMasked:    []string{"s3cret"},
	}, { // Test 2: Become fills its optional method and user from settings.
		Name:     "become fills from settings",
		Kind:     credential.KindBecome,
		Secret:   "password=rootpw",
		Settings: map[string]string{"method": "doas", "user": "svc"},
		WantVars: map[string]string{
			"ansible_become_password": "rootpw",
			"ansible_become_method":   "doas",
			"ansible_become_user":     "svc",
		},
		WantNotMasked: []string{"doas"},
		WantMasked:    []string{"rootpw"},
	}, { // Test 3: Network fills its device fields from settings.
		Name:     "network fills from settings",
		Kind:     credential.KindNetwork,
		Secret:   "user=admin\npassword=netpw",
		Settings: map[string]string{"network_os": "ios", "connection": "netconf"},
		WantVars: map[string]string{
			"ansible_user":       "admin",
			"ansible_password":   "netpw",
			"ansible_network_os": "ios",
			"ansible_connection": "netconf",
		},
		WantNotMasked: []string{"ios"},
		WantMasked:    []string{"netpw"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			sealer := credential.NewSealer("pass", "salt")
			sealed, err := sealer.Seal(test.Secret)
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			store := credential.NewMemStore()
			if err := store.Save(context.Background(), &credential.Credential{
				ID: "cred_1", Name: test.Name, Kind: test.Kind, Secret: sealed,
				Settings: test.Settings,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			d := &Dispatcher{credentials: store, sealer: sealer}
			spec := &roundhouse.Spec{}
			cleanup, secrets, err := d.materializeCredentials(context.Background(),
				&run.Run{ID: "run_1", CredentialIDs: []string{"cred_1"}}, spec)
			defer cleanup()
			if err != nil {
				t.Fatalf("materializeCredentials() error = %v", err)
			}

			if len(spec.ExtraVarsFiles) != 1 {
				t.Fatalf("spec.ExtraVarsFiles = %v, want exactly one file", spec.ExtraVarsFiles)
			}
			data, err := os.ReadFile(spec.ExtraVarsFiles[0])
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			var gotVars map[string]string
			if err := json.Unmarshal(data, &gotVars); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if diff := cmp.Diff(test.WantVars, gotVars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ansible vars mismatch (-want +got):\n%s", diff)
			}
			for _, v := range test.WantMasked {
				if !containsString(secrets, v) {
					t.Errorf("secrets = %v, want %q tracked for masking", secrets, v)
				}
			}
			for _, v := range test.WantNotMasked {
				if containsString(secrets, v) {
					t.Errorf("secrets = %v carries non-secret setting %q, which would mask "+
						"ordinary output", secrets, v)
				}
			}
		})
	}
}

// TestMaterializeSSHKeySettings proves a key credential's settings reach the play as connection and
// become vars beside the key file, so an imported AWX machine credential lands runnable.
func TestMaterializeSSHKeySettings(t *testing.T) {
	t.Parallel()
	raw := "-----BEGIN OPENSSH PRIVATE KEY-----\nunencrypted-body\n-----END OPENSSH PRIVATE KEY-----\n"
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal(raw)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "cred_1", Name: "machine key", Kind: credential.KindSSHKey, Secret: sealed,
		Settings: map[string]string{"user": "keydeploy", "become_method": "sudo"},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d := &Dispatcher{credentials: store, sealer: sealer}
	spec := &roundhouse.Spec{}
	cleanup, secrets, err := d.materializeCredentials(context.Background(),
		&run.Run{ID: "run_1", CredentialIDs: []string{"cred_1"}}, spec)
	defer cleanup()
	if err != nil {
		t.Fatalf("materializeCredentials() error = %v", err)
	}
	if spec.PrivateKeyPath == "" {
		t.Fatal("spec.PrivateKeyPath is empty, want the materialized key path")
	}
	if len(spec.ExtraVarsFiles) != 1 {
		t.Fatalf("spec.ExtraVarsFiles = %v, want the settings vars file", spec.ExtraVarsFiles)
	}
	data, err := os.ReadFile(spec.ExtraVarsFiles[0])
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var gotVars map[string]string
	if err := json.Unmarshal(data, &gotVars); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	want := map[string]string{"ansible_user": "keydeploy", "ansible_become_method": "sudo"}
	if diff := cmp.Diff(want, gotVars); diff != "" {
		t.Errorf("ansible vars mismatch (-want +got):\n%s", diff)
	}
	if containsString(secrets, "keydeploy") {
		t.Errorf("secrets = %v carries the non-secret user, which would mask ordinary output", secrets)
	}
}

// TestMaterializeEnvSettingsNotInjected proves an env credential's settings are reference metadata
// only: the sealed pairs reach the environment, but a settings pair does not, so it can neither
// shadow another credential's sealed value of the same name nor spill an imported input into the run.
func TestMaterializeEnvSettingsNotInjected(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("FOO=sealedvalue")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "cred_1", Name: "cloud env", Kind: credential.KindEnv, Secret: sealed,
		Settings: map[string]string{"AWS_REGION": "us-east-1", "FOO": "clobber"},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d := &Dispatcher{credentials: store, sealer: sealer}
	spec := &roundhouse.Spec{}
	cleanup, secrets, err := d.materializeCredentials(context.Background(),
		&run.Run{ID: "run_1", CredentialIDs: []string{"cred_1"}}, spec)
	defer cleanup()
	if err != nil {
		t.Fatalf("materializeCredentials() error = %v", err)
	}
	// Only the sealed pair is injected. The settings never appear, so FOO is not clobbered and the
	// region does not spill into the environment.
	if diff := cmp.Diff([]string{"FOO=sealedvalue"}, spec.Env); diff != "" {
		t.Errorf("spec.Env mismatch (-want +got):\n%s", diff)
	}
	if !containsString(secrets, "sealedvalue") {
		t.Error("secrets does not track the sealed env value for masking")
	}
	if containsString(secrets, "us-east-1") {
		t.Errorf("secrets = %v carries the non-secret region, which would mask ordinary output", secrets)
	}
}

// TestMaterializeConnectionCredentialsMissingFields proves each connection kind rejects material that
// omits a required field, so a bad credential fails materialization instead of running with empty
// authentication vars.
func TestMaterializeConnectionCredentialsMissingFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Kind   credential.Kind
		Secret string
	}{
		{"ssh_password missing password", credential.KindSSHPassword, "user=deploy"}, // Test 0.
		{"become missing password", credential.KindBecome, "method=sudo\nuser=root"}, // Test 1.
		{"network missing user", credential.KindNetwork, "password=netpw"},           // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			sealer := credential.NewSealer("pass", "salt")
			sealed, err := sealer.Seal(test.Secret)
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			store := credential.NewMemStore()
			if err := store.Save(context.Background(), &credential.Credential{
				ID: "cred_1", Name: test.Name, Kind: test.Kind, Secret: sealed,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			d := &Dispatcher{credentials: store, sealer: sealer}
			spec := &roundhouse.Spec{}
			cleanup, _, err := d.materializeCredentials(context.Background(),
				&run.Run{ID: "run_1", CredentialIDs: []string{"cred_1"}}, spec)
			defer cleanup()
			if !errors.Is(err, credential.ErrBadField) {
				t.Fatalf("materializeCredentials() error = %v, want ErrBadField", err)
			}
		})
	}
}

// encryptedSSHKey generates an ed25519 key sealed under passphrase in the OpenSSH format and returns
// its PEM text, so a test exercises the real unlock path at run time.
func encryptedSSHKey(t *testing.T, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	if err != nil {
		t.Fatalf("marshal encrypted key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

// materializeSSHKey seals secretPlain as an ssh_key credential and materializes it, returning the
// resulting spec, the masking secrets, and the materialize error, with cleanup deferred by the caller.
func materializeSSHKey(t *testing.T, secretPlain string) (*roundhouse.Spec, []string, func(), error) {
	t.Helper()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal(secretPlain)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "cred_ssh", Name: "deploy-key", Kind: credential.KindSSHKey, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := &Dispatcher{credentials: store, sealer: sealer}
	spec := &roundhouse.Spec{}
	cleanup, secrets, err := d.materializeCredentials(context.Background(),
		&run.Run{ID: "run_ssh", CredentialIDs: []string{"cred_ssh"}}, spec)
	return spec, secrets, cleanup, err
}

// TestMaterializeSSHKeyPassphrase proves a passphrase protected key is unlocked in process to a key
// the tool consumes without prompting, that the passphrase never lands in the key file and is tracked
// for masking, that a wrong passphrase fails materialization, and that a bare key passes through.
func TestMaterializeSSHKeyPassphrase(t *testing.T) {
	t.Parallel()

	t.Run("with passphrase", func(t *testing.T) {
		t.Parallel()
		encrypted := encryptedSSHKey(t, "unlock-me")
		spec, secrets, cleanup, err := materializeSSHKey(t, credential.BuildSSHKeySecret(encrypted, "unlock-me"))
		defer cleanup()
		if err != nil {
			t.Fatalf("materializeCredentials() error = %v", err)
		}
		if spec.PrivateKeyPath == "" {
			t.Fatal("spec.PrivateKeyPath is empty, want the materialized key path")
		}
		data, err := os.ReadFile(spec.PrivateKeyPath)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if _, err := ssh.ParseRawPrivateKey(data); err != nil {
			t.Errorf("materialized key does not parse without a passphrase: %v", err)
		}
		if strings.Contains(string(data), "unlock-me") {
			t.Error("materialized key file contains the passphrase, want it kept off disk")
		}
		if !containsString(secrets, "unlock-me") {
			t.Error("secrets does not track the passphrase for masking")
		}
	})

	t.Run("wrong passphrase", func(t *testing.T) {
		t.Parallel()
		encrypted := encryptedSSHKey(t, "unlock-me")
		_, _, cleanup, err := materializeSSHKey(t, credential.BuildSSHKeySecret(encrypted, "wrong"))
		defer cleanup()
		if !errors.Is(err, credential.ErrUnlock) {
			t.Fatalf("materializeCredentials() error = %v, want ErrUnlock", err)
		}
	})

	t.Run("bare key passthrough", func(t *testing.T) {
		t.Parallel()
		raw := "-----BEGIN OPENSSH PRIVATE KEY-----\nunencrypted-body\n-----END OPENSSH PRIVATE KEY-----\n"
		spec, _, cleanup, err := materializeSSHKey(t, raw)
		defer cleanup()
		if err != nil {
			t.Fatalf("materializeCredentials() error = %v", err)
		}
		data, err := os.ReadFile(spec.PrivateKeyPath)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if string(data) != raw {
			t.Errorf("materialized bare key = %q, want the raw key unchanged", string(data))
		}
	})
}

// TestRegistrySecretsMasked proves the registry pull login is collected for masking. It resolves
// onto the spec outside materializeCredentials, so without registrySecrets a container runner that
// echoed a failed registry login would leak the password into the run's output, unmasked.
func TestRegistrySecretsMasked(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("reguser\nreg-p@ssw0rd-secret")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "pull_1", Name: "ghcr", Kind: credential.KindRegistry, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := &Dispatcher{credentials: store, sealer: sealer}
	spec := &roundhouse.Spec{}
	if err := d.resolvePullCredential("pull_1", spec); err != nil {
		t.Fatalf("resolvePullCredential() error = %v", err)
	}
	got := registrySecrets(spec)
	if !containsString(got, "reg-p@ssw0rd-secret") {
		t.Errorf("registrySecrets = %v, want the registry password tracked for masking", got)
	}
}

// TestRegistryPasswordMaskedInLog drives a full run whose image pull uses a registry credential and
// whose runner echoes the registry password, proving the password is masked in the stored log. This
// covers the wiring, not just registrySecrets: the pull login resolves onto the spec outside
// materializeCredentials, so streamSpec must add it to the masker or a failed-pull message leaks it.
func TestRegistryPasswordMaskedInLog(t *testing.T) {
	t.Parallel()
	const pw = "reg-p@ssw0rd-supersecret"
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("reguser\n" + pw)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := run.NewMemStore()
	creds := credential.NewMemStore()
	if err := creds.Save(context.Background(), &credential.Credential{
		ID: "pull_1", Name: "ghcr", Kind: credential.KindRegistry, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			// A container runner echoes the registry login on a failed pull; simulate that here.
			_, _ = io.WriteString(out, "pull failed for "+spec.RegistryUsername+" pw="+spec.RegistryPassword+"\n")
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil, WithCredentials(creds, sealer), WithNoJanitor())
	defer d.Close()
	ctx := context.Background()
	r := &run.Run{
		ID: "run_reg", Playbook: "site.yml", Image: "ghcr.io/acme/ee:1",
		PullCredentialID: "pull_1", Status: run.StatusPending, CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d.streamSpec(ctx, r.Clone(), false, nil,
		func(roundhouse.Result, error, *masker, *run.SummaryFold) run.Status { return run.StatusSucceeded })

	body, err := store.Log(ctx, r.ID)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if strings.Contains(string(body), pw) {
		t.Errorf("registry password leaked into the run log: %q", body)
	}
	if !strings.Contains(string(body), maskToken) {
		t.Errorf("expected the registry password masked with %q; log: %q", maskToken, body)
	}
}

// TestMaterializeTypedCredentialFileCleanedUp proves the typed-credential extra-vars temp file,
// which holds the credential's field values, is tracked by the run cleanup so it does not outlive
// the run. The same tracking, applied before the error is checked in the caller, is what keeps a
// file left by a failed injection from leaking secret material into TMPDIR.
func TestMaterializeTypedCredentialFileCleanedUp(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal(`{"tok":"s3cret-field"}`)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	types := credential.NewMemTypeStore()
	if err := types.Save(context.Background(), &credential.CredentialType{
		ID: "ct_1", Name: "custom",
		Fields:            []credential.Field{{Name: "tok", Label: "Token", Secret: true}},
		ExtraVarInjectors: map[string]string{"api_token": "{{tok}}"},
	}); err != nil {
		t.Fatalf("Save type error = %v", err)
	}
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "cred_1", Name: "c", TypeID: "ct_1", Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := &Dispatcher{credentials: store, sealer: sealer, credentialTypes: types}
	spec := &roundhouse.Spec{}
	cleanup, secrets, err := d.materializeCredentials(context.Background(),
		&run.Run{ID: "run_1", CredentialIDs: []string{"cred_1"}}, spec)
	if err != nil {
		cleanup()
		t.Fatalf("materializeCredentials() error = %v", err)
	}
	if len(spec.ExtraVarsFiles) != 1 {
		cleanup()
		t.Fatalf("spec.ExtraVarsFiles = %v, want the typed extra-vars file", spec.ExtraVarsFiles)
	}
	path := spec.ExtraVarsFiles[0]
	if _, err := os.Stat(path); err != nil {
		cleanup()
		t.Fatalf("extra-vars file not written: %v", err)
	}
	if !containsString(secrets, "s3cret-field") {
		t.Error("the typed credential's secret field was not tracked for masking")
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("extra-vars file survived cleanup (%v), a leak of the credential's field values", err)
	}
}

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestMaterializeOpenStackMasksTheLoginNotTheConfig(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	// No explicit domains, so the injector fabricates the constant "Default".
	sealed, err := sealer.Seal("auth_url=https://keystone:5000/v3\nusername=deploy\n" +
		"password=os-secret\nproject_name=prod")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "cred_1", Name: "os", Kind: credential.KindOpenStack, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d := &Dispatcher{credentials: store, sealer: sealer}
	spec := &roundhouse.Spec{}
	cleanup, secrets, err := d.materializeCredentials(context.Background(),
		&run.Run{ID: "run_1", CredentialIDs: []string{"cred_1"}}, spec)
	defer cleanup()
	if err != nil {
		t.Fatalf("materializeCredentials() error = %v", err)
	}
	if !containsString(spec.Env, "OS_PASSWORD=os-secret") ||
		!containsString(spec.Env, "OS_USER_DOMAIN_NAME=Default") {
		t.Fatalf("spec.Env = %v, want the OS_ variables including the fabricated domain", spec.Env)
	}
	// The login identity (password and username) is masked; the non-secret config, above all the
	// common word "Default", is not, so plain log text is never redacted for containing it.
	for _, want := range []string{"os-secret", "deploy"} {
		if !containsString(secrets, want) {
			t.Errorf("secrets = %v, want the login value %q tracked for masking", secrets, want)
		}
	}
	for _, leaked := range []string{"Default", "prod", "https://keystone:5000/v3"} {
		if containsString(secrets, leaked) {
			t.Errorf("secrets = %v, want the non-secret value %q left out of the mask", secrets, leaked)
		}
	}
}

func TestInjectedMaskValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// In is the injection.
		In credential.Injection
		// Want is the values to mask.
		Want []string
	}{{ // Test 0: A named secret set masks exactly those, not the non-secret env values.
		Name: "named secrets",
		In:   credential.Injection{Env: []string{"OS_USERNAME=u", "OS_PASSWORD=p"}, Secrets: []string{"p"}},
		Want: []string{"p"},
	}, { // Test 1: A nil secret set masks every produced value, the conservative default.
		Name: "nil masks all",
		In:   credential.Injection{Env: []string{"A=1", "B=2"}},
		Want: []string{"1", "2"},
	}, { // Test 2: An empty but non-nil set is treated like nil and masks all, not nothing. A
		// host injector returning an empty slice must not silently leak its values.
		Name: "empty masks all",
		In:   credential.Injection{Env: []string{"A=1", "B=2"}, Secrets: []string{}},
		Want: []string{"1", "2"},
	}, { // Test 3: With no named secrets, extra-vars values are masked too, not just env values. A
		// custom kind that injects a secret extra var must not leak it because the fallback ignored it.
		Name: "extra vars masked in fallback",
		In:   credential.Injection{ExtraVars: map[string]string{"api_token": "sekret"}},
		Want: []string{"sekret"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := injectedMaskValues(test.In)
			if diff := cmp.Diff(test.Want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("injectedMaskValues() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
