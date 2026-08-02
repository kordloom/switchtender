package credential

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestCredentialTypeValidate covers the rules a custom type must satisfy, since a malformed one is
// caught at definition rather than expanding wrong at run time.
func TestCredentialTypeValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Type CredentialType
		Want bool
	}{{
		Name: "good env type", Want: true,
		Type: CredentialType{Name: "Datadog", Fields: []Field{{Name: "api_key", Secret: true}},
			EnvInjectors: map[string]string{"DD_API_KEY": "{{api_key}}"}},
	}, {
		Name: "good extra-var type", Want: true,
		Type: CredentialType{Name: "Vault", Fields: []Field{{Name: "addr"}, {Name: "token", Secret: true}},
			ExtraVarInjectors: map[string]string{"vault_addr": "{{addr}}", "vault_token": "{{token}}"}},
	}, {
		Name: "literal with field spliced in", Want: true,
		Type: CredentialType{Name: "Bearer", Fields: []Field{{Name: "token", Secret: true}},
			EnvInjectors: map[string]string{"AUTH_HEADER": "Bearer {{token}}"}},
	}, {
		Name: "no name", Want: false,
		Type: CredentialType{Fields: []Field{{Name: "x"}}, EnvInjectors: map[string]string{"X": "{{x}}"}},
	}, {
		Name: "no fields", Want: false,
		Type: CredentialType{Name: "Empty", EnvInjectors: map[string]string{"X": "y"}},
	}, {
		Name: "injects nothing", Want: false,
		Type: CredentialType{Name: "Inert", Fields: []Field{{Name: "x"}}},
	}, {
		Name: "field name with a dash", Want: false,
		Type: CredentialType{Name: "T", Fields: []Field{{Name: "api-key"}},
			EnvInjectors: map[string]string{"K": "v"}},
	}, {
		Name: "field name uppercase", Want: false,
		Type: CredentialType{Name: "T", Fields: []Field{{Name: "APIKey"}},
			EnvInjectors: map[string]string{"K": "v"}},
	}, {
		Name: "duplicate field", Want: false,
		Type: CredentialType{Name: "T", Fields: []Field{{Name: "x"}, {Name: "x"}},
			EnvInjectors: map[string]string{"K": "{{x}}"}},
	}, {
		Name: "env name lowercase", Want: false,
		Type: CredentialType{Name: "T", Fields: []Field{{Name: "x"}},
			EnvInjectors: map[string]string{"my key": "{{x}}"}},
	}, {
		Name: "reference to an undeclared field", Want: false,
		Type: CredentialType{Name: "T", Fields: []Field{{Name: "x"}},
			EnvInjectors: map[string]string{"K": "{{y}}"}},
	}, {
		Name: "extra-var name with a dot", Want: false,
		Type: CredentialType{Name: "T", Fields: []Field{{Name: "x"}},
			ExtraVarInjectors: map[string]string{"a.b": "{{x}}"}},
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			err := test.Type.Validate()
			if test.Want && err != nil {
				t.Errorf("Validate() rejected a valid type: %v", err)
			}
			if !test.Want && err == nil {
				t.Error("Validate() accepted an invalid type")
			}
			if !test.Want && err != nil && !errors.Is(err, ErrBadType) {
				t.Errorf("Validate() error = %v, want ErrBadType", err)
			}
		})
	}
}

// TestInjectSubstitutesAndMasks proves the core: fields are spliced literally into env and extra
// vars, and every secret field's value comes back for masking.
func TestInjectSubstitutesAndMasks(t *testing.T) {
	t.Parallel()
	typ := CredentialType{
		Name:   "Registry",
		Fields: []Field{{Name: "host"}, {Name: "user"}, {Name: "token", Secret: true}},
		EnvInjectors: map[string]string{
			"REGISTRY_HOST": "{{host}}",
			"REGISTRY_AUTH": "Bearer {{token}}",
		},
		ExtraVarInjectors: map[string]string{"registry_user": "{{user}}"},
	}
	if err := typ.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	inj, err := typ.Inject(map[string]string{
		"host": "registry.example.com", "user": "deploy", "token": "s3cr3t-value",
	})
	if err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	if !slices.Contains(inj.Env, "REGISTRY_HOST=registry.example.com") {
		t.Errorf("env = %v, want the host spliced in", inj.Env)
	}
	if !slices.Contains(inj.Env, "REGISTRY_AUTH=Bearer s3cr3t-value") {
		t.Errorf("env = %v, want the token spliced into the header", inj.Env)
	}
	if inj.ExtraVars["registry_user"] != "deploy" {
		t.Errorf("extra vars = %v, want the user", inj.ExtraVars)
	}
	// Only the secret field is returned for masking, and its raw value, so the masker redacts it
	// wherever it lands including inside the header.
	if !slices.Contains(inj.Secrets, "s3cr3t-value") {
		t.Errorf("secrets = %v, want the token", inj.Secrets)
	}
	if slices.Contains(inj.Secrets, "deploy") || slices.Contains(inj.Secrets, "registry.example.com") {
		t.Errorf("secrets = %v, want only the field marked secret", inj.Secrets)
	}
}

// TestInjectDoesNotReinterpretFieldValues is the injection-safety property. A field value that looks
// like a template reference must not be expanded, or a credential's own value becomes a way to reach
// another field.
func TestInjectDoesNotReinterpretFieldValues(t *testing.T) {
	t.Parallel()
	typ := CredentialType{
		Name:         "Two",
		Fields:       []Field{{Name: "a"}, {Name: "b", Secret: true}},
		EnvInjectors: map[string]string{"OUT": "{{a}}"},
	}
	if err := typ.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	// Field a's value references b. A single literal pass must leave it as text, not expand it.
	inj, err := typ.Inject(map[string]string{"a": "{{b}}", "b": "the-secret"})
	if err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	if !slices.Contains(inj.Env, "OUT={{b}}") {
		t.Errorf("env = %v: a field value that looks like a reference was re-expanded, so one "+
			"field's value reached another", inj.Env)
	}
	if strings.Contains(strings.Join(inj.Env, " "), "the-secret") {
		t.Error("the secret field leaked into the output through a value that looked like a reference")
	}
}

// TestInjectRefusesAMultilineValue pins that a value with a newline is refused, since an env file
// writes one entry per line and a line break would become a second variable.
func TestInjectRefusesAMultilineValue(t *testing.T) {
	t.Parallel()
	typ := CredentialType{
		Name: "T", Fields: []Field{{Name: "v", Secret: true}},
		EnvInjectors: map[string]string{"V": "{{v}}"},
	}
	if _, err := typ.Inject(map[string]string{"v": "abc\nLD_PRELOAD=/tmp/evil.so"}); err == nil {
		t.Error("a value spanning two lines was injected, so it becomes a second variable")
	}
}

// FuzzInject throws arbitrary field values at a fixed type and asserts the two invariants that
// matter: injection never panics, and every secret field's value appears verbatim in the returned
// secrets so the masker can redact it.
func FuzzInject(f *testing.F) {
	typ := CredentialType{
		Name:   "Fuzz",
		Fields: []Field{{Name: "a"}, {Name: "b", Secret: true}, {Name: "c", Secret: true}},
		EnvInjectors: map[string]string{
			"A": "{{a}}", "AB": "x{{a}}y{{b}}z", "B": "{{b}}",
		},
		ExtraVarInjectors: map[string]string{"cc": "{{c}}"},
	}
	f.Add("plain", "s3cret", "other")
	f.Add("{{b}}", "{{a}}", "{{c}}")
	f.Add("", "", "")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		inj, err := typ.Inject(map[string]string{"a": a, "b": b, "c": c})
		if err != nil {
			return // A multiline value is a legitimate refusal.
		}
		for name, val := range map[string]string{"b": b, "c": c} {
			if val == "" {
				continue
			}
			if strings.ContainsAny(val, "\n\r") {
				continue
			}
			if !slices.Contains(inj.Secrets, val) {
				t.Errorf("secret field %q value %q was not returned for masking, so it would reach "+
					"run output unredacted", name, val)
			}
		}
	})
}
