package dispatch

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

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
			err := d.validateCredentials(context.Background(), test.Tool, test.IDs)
			if !errors.Is(err, test.Want) {
				t.Errorf("validateCredentials(%q) error = %v, want %v", test.Tool, err, test.Want)
			}
		})
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

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestMaterializeOpenStackMasksOnlyThePassword(t *testing.T) {
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
	// The password is masked; the non-secret constants, above all the common word "Default", are
	// not, so plain log text is never redacted for containing them.
	if !containsString(secrets, "os-secret") {
		t.Errorf("secrets = %v, want the password tracked for masking", secrets)
	}
	for _, leaked := range []string{"Default", "prod", "deploy", "https://keystone:5000/v3"} {
		if containsString(secrets, leaked) {
			t.Errorf("secrets = %v, want the non-secret value %q left out of the mask", secrets, leaked)
		}
	}
}
