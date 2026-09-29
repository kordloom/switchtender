package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

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

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
