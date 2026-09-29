package trigger_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/triggertest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	triggertest.Contract(t, func() trigger.Store { return trigger.NewMemStore() })
}

func TestNew(t *testing.T) {
	t.Parallel()
	plain, tg, err := trigger.New("deploy", "tpl_1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !strings.HasPrefix(plain, "whk_") {
		t.Errorf("token = %q, want whk_ prefix", plain)
	}
	if tg.TokenHash != trigger.HashToken(plain) || tg.TemplateID != "tpl_1" {
		t.Errorf("trigger = %+v, want hash of plaintext and template tpl_1", tg)
	}
}

func TestNewSigningSecret(t *testing.T) {
	t.Parallel()
	first, err := trigger.NewSigningSecret()
	if err != nil {
		t.Fatalf("NewSigningSecret() error = %v", err)
	}
	if !strings.HasPrefix(first, "whs_") {
		t.Errorf("secret = %q, want whs_ prefix", first)
	}
	second, err := trigger.NewSigningSecret()
	if err != nil {
		t.Fatalf("NewSigningSecret() error = %v", err)
	}
	if first == second {
		t.Errorf("two secrets are identical, want random values")
	}
}

func TestVerifySignature(t *testing.T) {
	t.Parallel()
	secret := "whs_deadbeef"
	body := []byte(`{"ref":"refs/heads/main"}`)
	sig := trigger.SignBody(secret, body)
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("SignBody() = %q, want sha256= prefix", sig)
	}
	tests := []struct {
		Secret    string
		Header    string
		Body      []byte
		WantValid bool
	}{
		{ // Test 0: Matching secret, body, and signature verify.
			Secret: secret, Header: sig, Body: body, WantValid: true,
		},
		{ // Test 1: A different secret does not verify.
			Secret: "whs_other", Header: sig, Body: body, WantValid: false,
		},
		{ // Test 2: A tampered body does not verify.
			Secret: secret, Header: sig, Body: []byte("changed"), WantValid: false,
		},
		{ // Test 3: A missing header does not verify.
			Secret: secret, Header: "", Body: body, WantValid: false,
		},
		{ // Test 4: An empty secret never verifies.
			Secret: "", Header: sig, Body: body, WantValid: false,
		},
		{ // Test 5: A malformed header does not verify.
			Secret: secret, Header: "sha256=zzzz", Body: body, WantValid: false,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := trigger.VerifySignature(test.Secret, test.Body, test.Header); got != test.WantValid {
				t.Errorf("VerifySignature() = %v, want %v", got, test.WantValid)
			}
		})
	}
}
