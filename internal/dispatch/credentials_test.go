package dispatch

import (
	"context"
	"testing"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
)

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
	cleanup, _, err := d.materializeCredentials(&run.Run{ID: "run_1", CredentialIDs: []string{"cred_1"}}, spec)
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
	cleanup, _, err := d.materializeCredentials(&run.Run{ID: "run_tok", CredentialIDs: []string{"cred_tok"}}, spec)
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
	cleanup, _, err := d.materializeCredentials(&run.Run{ID: "run_1", InventoryID: "inv_1"}, spec)
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
