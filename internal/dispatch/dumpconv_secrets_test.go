package dispatch

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/inventory"
)

// TestStaticFromDumpSecretsAreRedacted proves the JSON inventory document staticFromDump writes is
// redacted and collected like any other inventory content. The dynamic-inventory path stores its
// host list as JSON, and a JSON document names its variables in quotes, so a redactor that only
// understands bare name=value or name: value assignments sees nothing to remove and a viewer-role
// reader is served the plaintext ansible_password. The same document also feeds the run masker, so
// a secret the collector misses is never masked out of run output either.
func TestStaticFromDumpSecretsAreRedacted(t *testing.T) {
	t.Parallel()
	dump := []byte(`{
		"all": {"children": ["dyn"]},
		"dyn": {
			"hosts": ["web01"],
			"vars": {"vault_api_key": "sk-live-groupwide"}
		},
		"_meta": {"hostvars": {"web01": {
			"ansible_host": "10.0.0.1",
			"ansible_user": "deploy",
			"ansible_password": "hunter2",
			"ansible_become_pass": "horse battery staple"
		}}}
	}`)
	static, err := staticFromDump(dump)
	if err != nil {
		t.Fatalf("staticFromDump() error = %v", err)
	}
	content := string(static)

	secrets := []string{"hunter2", "horse battery staple", "sk-live-groupwide"}
	redacted := inventory.Redact(content)
	for _, secret := range secrets {
		if strings.Contains(redacted, secret) {
			t.Errorf("redacted inventory still serves %q:\n%s", secret, redacted)
		}
	}
	for _, kept := range []string{"web01", "10.0.0.1", "deploy", "ansible_password"} {
		if !strings.Contains(redacted, kept) {
			t.Errorf("redaction removed %q, which is not secret:\n%s", kept, redacted)
		}
	}

	collected := inventorySecrets(content)
	for _, secret := range secrets {
		found := false
		for _, got := range collected {
			if got == secret {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the run masker was never handed %q, collected %v", secret, collected)
		}
	}
}

// TestStaticInventorySecretsAreRedacted proves the same for a static inventory a user pastes in,
// in each encoding the file plugins accept: the JSON an ansible-inventory --list dump produces and
// the YAML form, including an unquoted YAML value with spaces, where a line pattern that stops at
// whitespace would leave everything after the first word in the clear.
func TestStaticInventorySecretsAreRedacted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Content string
		Secrets []string
		Kept    []string
	}{{ // Test 0: A pasted ansible-inventory --list dump.
		Name: "json dump",
		Content: `{"all": {"hosts": {"web01": {"ansible_user": "deploy",` +
			` "ansible_password": "hunter2"}}}}`,
		Secrets: []string{"hunter2"},
		Kept:    []string{"web01", "deploy", "ansible_password"},
	}, { // Test 1: An unquoted YAML value with spaces is removed whole, not just its first word.
		Name: "unquoted yaml with spaces",
		Content: "all:\n  hosts:\n    db01:\n" +
			"      ansible_password: horse battery staple\n      ansible_port: 22\n",
		Secrets: []string{"horse battery staple", "battery", "staple"},
		Kept:    []string{"db01", "ansible_password", "22"},
	}, { // Test 2: The INI form keeps working, one variable redacted and its neighbors intact.
		Name:    "ini",
		Content: "[web]\nweb01 ansible_user=deploy ansible_password=hunter2 ansible_port=22\n",
		Secrets: []string{"hunter2"},
		Kept:    []string{"[web]", "ansible_user=deploy", "ansible_port=22"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			redacted := inventory.Redact(test.Content)
			for _, secret := range test.Secrets {
				if strings.Contains(redacted, secret) {
					t.Errorf("redacted inventory still serves %q:\n%s", secret, redacted)
				}
			}
			for _, kept := range test.Kept {
				if !strings.Contains(redacted, kept) {
					t.Errorf("redaction removed %q, which is not secret:\n%s", kept, redacted)
				}
			}
			collected := inventorySecrets(test.Content)
			for _, secret := range test.Secrets {
				masked := false
				for _, got := range collected {
					if strings.Contains(got, secret) {
						masked = true
						break
					}
				}
				if !masked {
					t.Errorf("the run masker was never handed %q, collected %v", secret, collected)
				}
			}
		})
	}
}
