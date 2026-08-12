package inventory

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestRedactRemovesSecretValues proves inventory content served to a reader keeps its hosts and
// variable names but not the credentials written into it, in every encoding one arrives in.
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
	}, { // Test 3: Names that merely contain "pass" are not secrets and keep their values. A key
		// file path is redacted, because one classifier decides for every encoding and it counts a
		// private_key name as secret. Blacking out a path costs a reader nothing; a second, looser
		// classifier for INI would serve a secret the strict one caught. The masker is a separate
		// question, covered by TestSecretsSkipsFileReferences.
		Name: "not secrets",
		In:   "web01 bypass=true passive_mode=false ansible_ssh_private_key_file=/k/id\n",
		Gone: []string{"/k/id"},
		Kept: []string{"bypass=true", "passive_mode=false", "ansible_ssh_private_key_file"},
	}, { // Test 4: A JSON document, the form a dynamic inventory is stored in and the form
		// ansible-inventory --list prints. The quoted variable name defeats a line pattern.
		Name: "json document",
		In: `{"all": {"hosts": {"web01": {"ansible_user": "deploy",` +
			` "ansible_password": "hunter2"}}}}`,
		Gone: []string{"hunter2"},
		Kept: []string{"web01", "deploy", "ansible_password"},
	}, { // Test 5: An unquoted YAML scalar with spaces goes whole, not just up to its first space.
		Name: "unquoted yaml with spaces",
		In:   "all:\n  vars:\n    ansible_password: horse battery staple\n    ansible_port: 22\n",
		Gone: []string{"horse battery staple", "battery", "staple"},
		Kept: []string{"ansible_password", "ansible_port", "22"},
	}, { // Test 6: A whole mapping under a secret-bearing key goes, not only its string leaves.
		Name: "nested secret mapping",
		In:   "all:\n  vars:\n    vault_secret:\n      user: root\n      value: s3kr1t\n",
		Gone: []string{"s3kr1t", "root"},
		Kept: []string{"vault_secret"},
	}, { // Test 7: A password written as a JSON number is still a password.
		Name: "numeric secret",
		In:   `{"all": {"vars": {"ansible_password": 8675309, "ansible_port": 22}}}`,
		Gone: []string{"8675309"},
		Kept: []string{"ansible_port", "22"},
	}, { // Test 8: An assignment embedded in an ordinary variable's value is caught too.
		Name: "assignment inside a leaf",
		In:   "all:\n  vars:\n    ansible_ssh_extra_args: -o Whatever=1 --password=hunter2\n",
		Gone: []string{"hunter2"},
		Kept: []string{"Whatever=1"},
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

// TestSecretsCollectsValues proves the run-log masker is handed the same values the API removes, in
// every encoding. A value the collector misses is never masked out of a run's output.
func TestSecretsCollectsValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
		Want []string
	}{{ // Test 0: INI.
		Name: "ini",
		In:   "web01 ansible_user=deploy ansible_password=hunter2\n",
		Want: []string{"hunter2"},
	}, { // Test 1: A JSON document, where the old line pattern collected nothing at all.
		Name: "json document",
		In: `{"all": {"hosts": {"web01": {"ansible_password": "hunter2",` +
			` "ansible_become_pass": "horse battery staple"}}}}`,
		Want: []string{"hunter2", "horse battery staple"},
	}, { // Test 2: An unquoted YAML scalar is collected whole, not just its first word.
		Name: "unquoted yaml with spaces",
		In:   "all:\n  vars:\n    ansible_password: horse battery staple\n",
		Want: []string{"horse battery staple"},
	}, { // Test 3: The same password on many hosts is handed over once.
		Name: "repeated value",
		In: "all:\n  hosts:\n    web01:\n      ansible_password: hunter2\n" +
			"    web02:\n      ansible_password: hunter2\n",
		Want: []string{"hunter2"},
	}, { // Test 4: Nothing secret, nothing collected.
		Name: "no secrets",
		In:   "all:\n  hosts:\n    web01:\n      ansible_port: 22\n",
		Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := Secrets(test.In)
			less := func(a, b string) bool { return a < b }
			if diff := cmp.Diff(test.Want, got, cmpopts.EquateEmpty(),
				cmpopts.SortSlices(less)); diff != "" {
				t.Errorf("Secrets() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSecretsSkipsFileReferences proves a key file's location is removed from what a reader is
// served but is never handed to the run-log masker, in each encoding. The path appears throughout
// ordinary run output and masking it would black out lines that carry nothing sensitive. Key
// material pasted inline under the same name is still collected, since that is the secret itself.
func TestSecretsSkipsFileReferences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
		Want []string
	}{{ // Test 0: INI.
		Name: "ini",
		In:   "web01 ansible_ssh_private_key_file=/home/deploy/.ssh/id_rsa\n",
		Want: nil,
	}, { // Test 1: YAML.
		Name: "yaml",
		In:   "all:\n  vars:\n    ansible_ssh_private_key_file: /home/deploy/.ssh/id_rsa\n",
		Want: nil,
	}, { // Test 2: JSON.
		Name: "json",
		In:   `{"all": {"vars": {"ansible_ssh_private_key_file": "/home/deploy/.ssh/id_rsa"}}}`,
		Want: nil,
	}, { // Test 3: Inline key material under a file-shaped name is the secret, not a location.
		Name: "inline material",
		In: "all:\n  vars:\n    ansible_ssh_private_key_file: |\n" +
			"      -----BEGIN OPENSSH PRIVATE KEY-----\n      b3BlbnNza\n",
		Want: []string{"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNza\n"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.Want, Secrets(test.In), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Secrets() mismatch (-want +got):\n%s", diff)
			}
			if got := Redact(test.In); strings.Contains(got, "id_rsa") ||
				strings.Contains(got, "b3BlbnNza") {
				t.Errorf("redaction served the key reference:\n%s", got)
			}
		})
	}
}

// TestSecretsAndRedactAgree proves the values the run-log masker holds are exactly the ones the API
// removes, so a secret cannot be masked in one place and served in the other.
func TestSecretsAndRedactAgree(t *testing.T) {
	t.Parallel()
	contents := []string{
		"web01 ansible_password=hunter2 ansible_user=deploy\n" +
			"db01 ansible_become_pass='sp aced'\n",
		"all:\n  vars:\n    token_id: abc123xyz\n    ansible_password: sp aced too\n",
		`{"all": {"hosts": {"web01": {"ansible_password": "hunter2"}},` +
			` "vars": {"vault_api_key": "sk-live-abcdefgh"}}}`,
	}
	for testNum, content := range contents {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			redacted := Redact(content)
			secrets := Secrets(content)
			if len(secrets) == 0 {
				t.Fatalf("no secrets collected from:\n%s", content)
			}
			for _, secret := range secrets {
				if strings.Contains(redacted, secret) {
					t.Errorf("the masker holds %q but the API would still serve it:\n%s",
						secret, redacted)
				}
			}
		})
	}
}

// TestRedactKeepsTheDocumentReadable proves redaction returns an inventory, not a wreck of one: a
// YAML document keeps its comments and its non-secret structure, and a multi-document stream keeps
// every document rather than being truncated to the first.
func TestRedactKeepsTheDocumentReadable(t *testing.T) {
	t.Parallel()
	content := "# production hosts\nall:\n  hosts:\n    web01:\n" +
		"      ansible_host: 10.0.0.1 # the vip\n      ansible_password: hunter2\n" +
		"---\nstaging:\n  hosts:\n    stg01:\n      ansible_password: second\n"
	got := Redact(content)
	for _, want := range []string{"# production hosts", "# the vip", "10.0.0.1", "staging", "stg01"} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction lost %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"hunter2", "second"} {
		if strings.Contains(got, gone) {
			t.Errorf("redacted content still carries %q:\n%s", gone, got)
		}
	}
}

// TestRedactLeavesUnparsableContentAlone proves content no parser accepts is still scanned line by
// line rather than handed back whole, and that the scan does not stop at the first non-secret
// assignment on a line.
func TestRedactLeavesUnparsableContentAlone(t *testing.T) {
	t.Parallel()
	content := "[web:vars]\nansible_user = deploy\nansible_password = hunter2\n" +
		"note: harmless then ansible_ssh_pass: horse battery staple\n"
	got := Redact(content)
	for _, gone := range []string{"hunter2", "horse battery staple", "battery"} {
		if strings.Contains(got, gone) {
			t.Errorf("redacted content still carries %q:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"[web:vars]", "ansible_user = deploy", "note: harmless"} {
		if !strings.Contains(got, kept) {
			t.Errorf("redaction removed %q, which is not secret:\n%s", kept, got)
		}
	}
}
