package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
)

func TestResolveCommandSecret(t *testing.T) {
	t.Parallel()
	got, err := resolveCommandSecret(context.Background(), "printf 'hunter2'")
	if err != nil || got != "hunter2" {
		t.Fatalf("resolveCommandSecret() = %q, %v; want hunter2, nil", got, err)
	}
	if _, err := resolveCommandSecret(context.Background(), "exit 7"); !errors.Is(err, ErrSecretResolve) {
		t.Errorf("failing command error = %v, want ErrSecretResolve", err)
	}
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
	cleanup, err := d.materializeCredentials(&run.Run{ID: "run_1", CredentialIDs: []string{"cred_1"}}, spec)
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
