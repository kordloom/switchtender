package util_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/util"
)

func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   []string
		Want string
	}{
		{In: nil, Want: ""},                    // Test 0: Nothing given.
		{In: []string{"", "", "c"}, Want: "c"}, // Test 1: Skips empties.
		{In: []string{"a", "b"}, Want: "a"},    // Test 2: First wins.
		{In: []string{"", ""}, Want: ""},       // Test 3: All empty.
	}
	for i, test := range tests {
		if got := util.FirstNonEmpty(test.In...); got != test.Want {
			t.Errorf("test %d: FirstNonEmpty(%v) = %q, want %q", i, test.In, got, test.Want)
		}
	}
}

// TestSecretKey pins the one classifier the audit digest and the inventory redactor both consult.
func TestSecretKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want bool
	}{
		{In: "ansible_password", Want: true},             // Test 0: The common host variable.
		{In: "ANSIBLE_BECOME_PASSWORD", Want: true},      // Test 1: Case does not matter.
		{In: "ansible_ssh_pass", Want: true},             // Test 2: A terminal _pass.
		{In: "pass", Want: true},                         // Test 3: The bare name.
		{In: "token_id", Want: true},                     // Test 4: A stem with a trailing part.
		{In: "vault_api_key", Want: true},                // Test 5: An api_key anywhere in the name.
		{In: "fields", Want: true},                       // Test 6: A custom type's secret bag.
		{In: "ansible_ssh_private_key_file", Want: true}, // Test 7: Private key material.
		{In: "bypass", Want: false},                      // Test 8: Contains pass, is not one.
		{In: "passive_mode", Want: false},                // Test 9: Same.
		{In: "passthrough", Want: false},                 // Test 10: Same.
		{In: "ansible_user", Want: false},                // Test 11: An ordinary variable.
		{In: "aws_access_key_id", Want: false},           // Test 12: An identifier, not a secret.
		{In: "", Want: false},                            // Test 13: Nothing.
	}
	for i, test := range tests {
		if got := util.SecretKey(test.In); got != test.Want {
			t.Errorf("test %d: SecretKey(%q) = %v, want %v", i, test.In, got, test.Want)
		}
	}
}

func TestClip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In    string
		Limit int
		Want  string
	}{
		{In: "short", Limit: 10, Want: "short"},      // Test 0: Under the limit passes through.
		{In: "abcdefgh", Limit: 5, Want: "abcde..."}, // Test 1: Byte cut plus ellipsis.
		{In: "héllo wörld", Limit: 2, Want: "h..."},  // Test 2: A cut landing inside a rune walks back.
	}
	for i, test := range tests {
		if got := util.Clip(test.In, test.Limit); got != test.Want {
			t.Errorf("test %d: Clip(%q, %d) = %q, want %q", i, test.In, test.Limit, got, test.Want)
		}
	}
}
