package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestResolveNative pins what the native engine resolves, what it leaves to Ansible, and what it
// refuses as a document Ansible would not read whole. The conformance corpus proves the resolved
// documents against real Ansible; this table pins the classification without needing Ansible.
func TestResolveNative(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want        error
		Content     string
		WantHosts   []string
		WantVar     string
		WantVarJSON string
		WantReason  string
	}{{ // Test 0: INI with a group variable folded into the host.
		Content: "[web]\nweb1\n[web:vars]\nport=8080\n", WantHosts: []string{"web1"},
		WantVar: "port", WantVarJSON: `8080`,
	}, { // Test 1: YAML read with YAML 1.1 booleans.
		Content: "all:\n  hosts:\n    h1:\n      flag: on\n", WantHosts: []string{"h1"},
		WantVar: "flag", WantVarJSON: `true`,
	}, { // Test 2: JSON read before YAML, so 1e3 is a number.
		Content: `{"web": {"hosts": {"h1": {"n": 1e3}}}}`, WantHosts: []string{"h1"},
		WantVar: "n", WantVarJSON: `1000.0`,
	}, { // Test 3: An empty document is an empty inventory.
		Content: "",
	}, { // Test 4: A plugin configuration needs Ansible, and says why.
		Content: "plugin: amazon.aws.aws_ec2\n", Want: ErrNeedsAnsible,
		WantReason: "inventory plugin configuration",
	}, { // Test 5: A vault-encrypted value needs Ansible.
		Content: "all:\n  vars:\n    s: !vault |\n      $ANSIBLE_VAULT;1.1;AES256\n      6162\n",
		Want:    ErrNeedsAnsible, WantReason: "vault-encrypted",
	}, { // Test 6: A Python complex number needs Ansible.
		Content: "[g]\nh1 c=1j\n", Want: ErrNeedsAnsible, WantReason: "complex number",
	}, { // Test 7: Invalid UTF-8 needs Ansible.
		Content: "[g]\nh\xff\n", Want: ErrNeedsAnsible, WantReason: "not valid UTF-8",
	}, { // Test 8: The --list JSON is refused, saying what to store instead.
		Content: `{"_meta": {"hostvars": {}}, "all": {"children": ["ungrouped"]}}`,
		Want:    ErrInvalidInventory, WantReason: "--list --yaml",
	}, { // Test 9: An unknown INI section is refused.
		Content: "[g:things]\nh1\n", Want: ErrInvalidInventory, WantReason: "unknown type",
	}, { // Test 10: A child group never declared is refused.
		Content: "[p:children]\nmissing\n", Want: ErrInvalidInventory,
		WantReason: "includes undefined group",
	}, { // Test 11: A YAML group whose hosts are a list is refused.
		Content: "web:\n  hosts:\n    - h1\n", Want: ErrInvalidInventory,
		WantReason: "requires a dictionary",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := ResolveNative(test.Content)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ResolveNative() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				if !strings.Contains(err.Error(), test.WantReason) {
					t.Errorf("error %q does not say %q", err, test.WantReason)
				}
				return
			}
			if diff := cmp.Diff(test.WantHosts, got.Hosts(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hosts mismatch (-want +got):\n%s", diff)
			}
			if test.WantVar == "" {
				return
			}
			b, err := json.Marshal(got.Vars(test.WantHosts[0])[test.WantVar])
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(test.WantVarJSON, string(b)); diff != "" {
				t.Errorf("variable %s mismatch (-want +got):\n%s", test.WantVar, diff)
			}
		})
	}
}

// TestNeedsAnsibleReason pins that the reason a document needs Ansible can be read back from the
// error, which is what the dispatcher quotes when Ansible is missing.
func TestNeedsAnsibleReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Err        error
		WantOK     bool
		WantReason string
	}{{ // Test 0: Direct.
		Err: needsAnsible("why"), WantOK: true, WantReason: "why",
	}, { // Test 1: Wrapped.
		Err: fmt.Errorf("wrapped: %w", needsAnsible("why")), WantOK: true, WantReason: "why",
	}, { // Test 2: Not one.
		Err: invalidf("bad"), WantOK: false,
	}, { // Test 3: No error.
		Err: nil, WantOK: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			reason, ok := NeedsAnsibleReason(test.Err)
			if ok != test.WantOK || reason != test.WantReason {
				t.Errorf("NeedsAnsibleReason() = %q, %v, want %q, %v", reason, ok, test.WantReason,
					test.WantOK)
			}
		})
	}
}

// TestParseAnsibleCoreVersion pins reading the version from an Ansible command's banner and the
// tested-range check doctor uses.
func TestParseAnsibleCoreVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Banner      string
		WantVersion string
		WantTested  bool
	}{{ // Test 0: A current banner.
		Banner: "ansible-inventory [core 2.18.1]\n  config file = None\n", WantVersion: "2.18.1",
		WantTested: true,
	}, { // Test 1: A release candidate.
		Banner: "ansible-inventory [core 2.21.0rc1]", WantVersion: "2.21.0rc1", WantTested: true,
	}, { // Test 2: An old release outside the range.
		Banner: "ansible-inventory [core 2.14.3]", WantVersion: "2.14.3",
	}, { // Test 3: A banner with no core version.
		Banner: "ansible-inventory 2.9.27",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := ParseAnsibleCoreVersion(test.Banner)
			if got != test.WantVersion {
				t.Errorf("ParseAnsibleCoreVersion() = %q, want %q", got, test.WantVersion)
			}
			if tested := AnsibleCoreTested(got); tested != test.WantTested {
				t.Errorf("AnsibleCoreTested(%q) = %v, want %v", got, tested, test.WantTested)
			}
		})
	}
}

// TestListingDigestAndListJSON pins that the digest of a listing does not depend on the order the
// document wrote things in, masks secrets, and survives a round trip through the --list JSON, which
// is how the cross-check compares the two engines' readings.
func TestListingDigestAndListJSON(t *testing.T) {
	t.Parallel()
	a, err := ResolveNative("[web]\nw1 x=1\nw2 password=hunter2\n[db]\nd1\n")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolveNative("[db]\nd1\n[web]\nw2 password=other\nw1 x=1\n")
	if err != nil {
		t.Fatal(err)
	}
	da, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Errorf("Digest() differs for the same inventory written in another order: %s, %s", da, db)
	}
	raw, err := a.ListJSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseListing(raw)
	if err != nil {
		t.Fatal(err)
	}
	if diff := DiffListings(a, back); len(diff) > 0 {
		t.Errorf("a listing does not survive its own --list JSON: %v", diff)
	}
}
