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

// TestMaterializeCustomTypeWithNoSecretFieldMasksEverything covers the case the test above does not:
// a type whose fields nobody marked secret.
//
// The built-in kinds route their values through injectedMaskValues, which masks every value an
// injector produced when the injector named none, deliberately, so an injector that names an empty
// set cannot leak its own values by accident. The custom-type path took the named set raw, so
// forgetting the flag produced a credential whose value reached the run environment with nothing in
// the mask list: the only thing the masker held was the sealed JSON blob, a string that never
// appears in tool output. An ansible-playbook -vvv, a bash step running env, or a provider debug
// line then wrote the key into the stored log and the live stream in the clear. Nothing in the API
// requires a field to be marked, so this is a one-word mistake with no warning attached.
func TestMaterializeCustomTypeWithNoSecretFieldMasksEverything(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	const value = "dd-live-APIKEYVALUE-9999"

	types := credential.NewMemTypeStore()
	typ := &credential.CredentialType{
		ID:   "ctype_datadog",
		Name: "Datadog API",
		// No field is marked secret, which is the mistake this covers.
		Fields:       []credential.Field{{Name: "api_key"}},
		EnvInjectors: map[string]string{"DATADOG_API_KEY": "{{api_key}}"},
	}
	if err := typ.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if err := types.Save(ctx, typ); err != nil {
		t.Fatalf("Save() type error = %v", err)
	}

	fields, err := json.Marshal(map[string]string{"api_key": value})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	sealed, err := sealer.Seal(string(fields))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(ctx, &credential.Credential{
		ID: "cred_dd", Name: "datadog", TypeID: "ctype_datadog", Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() credential error = %v", err)
	}

	d := &Dispatcher{credentials: creds, credentialTypes: types, sealer: sealer}
	spec := &roundhouse.Spec{}
	cleanup, secrets, err := d.materializeCredentials(ctx,
		&run.Run{ID: "run_1", CredentialIDs: []string{"cred_dd"}}, spec)
	defer cleanup()
	if err != nil {
		t.Fatalf("materializeCredentials() error = %v", err)
	}

	if !slices.Contains(spec.Env, "DATADOG_API_KEY="+value) {
		t.Fatalf("env = %v, want the key injected", spec.Env)
	}
	if !slices.Contains(secrets, value) {
		t.Errorf("the injected value is not in the mask list %v, so a run that echoes its "+
			"environment writes %q into the stored log unredacted", secrets, value)
	}
	// The masker must actually redact it, which is the property the mask list exists for.
	m := &masker{}
	m.set(secrets)
	if got := string(m.redact([]byte("TASK output: DATADOG_API_KEY=" + value))); strings.Contains(got, value) {
		t.Errorf("the masker left the value in the output: %q", got)
	}
}
