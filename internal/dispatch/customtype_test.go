package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestMaterializeCustomTypedCredential proves the whole path: a credential of an operator-defined
// type has its fields injected as environment variables and extra vars, and every secret field is
// masked, exactly as a built-in kind would be.
func TestMaterializeCustomTypedCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")

	types := credential.NewMemTypeStore()
	typ := &credential.CredentialType{
		ID:   "ctype_registry",
		Name: "Container Registry",
		Fields: []credential.Field{
			{Name: "host"},
			{Name: "username"},
			{Name: "token", Secret: true},
		},
		EnvInjectors: map[string]string{
			"REGISTRY_HOST": "{{host}}",
			"REGISTRY_AUTH": "Bearer {{token}}",
		},
		ExtraVarInjectors: map[string]string{"registry_user": "{{username}}"},
	}
	if err := typ.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if err := types.Save(ctx, typ); err != nil {
		t.Fatalf("Save() type error = %v", err)
	}

	// The credential holds its field values as a sealed JSON object.
	fields, err := json.Marshal(map[string]string{
		"host": "registry.example.com", "username": "deploy", "token": "s3cr3t-token",
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	sealed, err := sealer.Seal(string(fields))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(ctx, &credential.Credential{
		ID: "cred_reg", Name: "prod-registry", TypeID: "ctype_registry", Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() credential error = %v", err)
	}

	d := &Dispatcher{credentials: creds, credentialTypes: types, sealer: sealer}
	spec := &roundhouse.Spec{}
	cleanup, secrets, err := d.materializeCredentials(ctx,
		&run.Run{ID: "run_1", CredentialIDs: []string{"cred_reg"}}, spec)
	defer cleanup()
	if err != nil {
		t.Fatalf("materializeCredentials() error = %v", err)
	}

	if !slices.Contains(spec.Env, "REGISTRY_HOST=registry.example.com") {
		t.Errorf("env = %v, want the host", spec.Env)
	}
	if !slices.Contains(spec.Env, "REGISTRY_AUTH=Bearer s3cr3t-token") {
		t.Errorf("env = %v, want the token spliced into the auth header", spec.Env)
	}
	// The token is masked, and the non-secret fields are not.
	if !slices.Contains(secrets, "s3cr3t-token") {
		t.Errorf("secrets = %v, want the token so run output redacts it", secrets)
	}
	// The extra var reached a private vars file rather than argv.
	if len(spec.ExtraVarsFiles) != 1 {
		t.Fatalf("extra vars files = %d, want 1", len(spec.ExtraVarsFiles))
	}
	content, err := os.ReadFile(spec.ExtraVarsFiles[0])
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(content), `"registry_user":"deploy"`) {
		t.Errorf("extra vars file = %s, want the user", content)
	}
}

// TestTypedCredentialWithoutTypesConfiguredFails checks that a credential naming a type the
// dispatcher cannot resolve fails the run rather than silently injecting nothing.
func TestTypedCredentialWithoutTypesConfiguredFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal(`{"token":"x"}`)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(ctx, &credential.Credential{
		ID: "cred_x", TypeID: "ctype_gone", Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// Credentials configured, but no type store.
	d := &Dispatcher{credentials: creds, sealer: sealer}
	_, _, err = d.materializeCredentials(ctx,
		&run.Run{ID: "run_1", CredentialIDs: []string{"cred_x"}}, &roundhouse.Spec{})
	if err == nil {
		t.Error("a credential naming an unresolvable type materialized with no error, so it would " +
			"run authenticating with nothing")
	}
}
