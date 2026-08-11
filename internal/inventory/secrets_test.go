package inventory

import (
	"fmt"
	"strings"
	"testing"
)

// TestRedactRemovesSecretValues proves inventory content served to a reader keeps its hosts and
// variable names but not the credentials written into it.
func TestRedactRemovesSecretValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
		Gone []string
		Kept []string
	}{{ // Test 0: An INI host line with a password beside ordinary variables.
		Name: "ini host vars",
		In:   "web01 ansible_user=deploy ansible_password=hunter2 ansible_port=22\n",
		Gone: []string{"hunter2"},
		Kept: []string{"web01", "ansible_user=deploy", "ansible_port=22", "ansible_password"},
	}, { // Test 1: A quoted password containing spaces is removed whole.
		Name: "quoted with spaces",
		In:   "db01 ansible_become_pass=\"two words here\"\n",
		Gone: []string{"two words here"},
		Kept: []string{"db01", "ansible_become_pass"},
	}, { // Test 2: YAML form.
		Name: "yaml",
		In:   "all:\n  vars:\n    api_key: sk-live-abcdefgh\n",
		Gone: []string{"sk-live-abcdefgh"},
		Kept: []string{"api_key"},
	}, { // Test 3: Names that merely contain "pass" are not secrets and keep their values.
		Name: "not secrets",
		In:   "web01 bypass=true passive_mode=false ansible_ssh_private_key_file=/k/id\n",
		Gone: nil,
		Kept: []string{"bypass=true", "passive_mode=false", "/k/id"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := Redact(test.In)
			for _, gone := range test.Gone {
				if strings.Contains(got, gone) {
					t.Errorf("redacted content still carries the secret %q:\n%s", gone, got)
				}
			}
			for _, kept := range test.Kept {
				if !strings.Contains(got, kept) {
					t.Errorf("redaction removed %q, which is not secret:\n%s", kept, got)
				}
			}
		})
	}
}

// TestSecretsAndRedactAgree proves the values the run-log masker holds are exactly the ones the API
// removes, so a secret cannot be masked in one place and served in the other.
func TestSecretsAndRedactAgree(t *testing.T) {
	t.Parallel()
	content := "web01 ansible_password=hunter2 ansible_user=deploy\n" +
		"db01 ansible_become_pass='sp aced'\n" +
		"all:\n  vars:\n    token_id: abc123xyz\n"
	redacted := Redact(content)
	for _, secret := range Secrets(content) {
		if strings.Contains(redacted, secret) {
			t.Errorf("the masker holds %q but the API would still serve it:\n%s", secret, redacted)
		}
	}
}
