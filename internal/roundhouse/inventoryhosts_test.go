package roundhouse

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestParseInventoryHostsReadsGroupsNotOnlyHostVars pins that enumeration reads the groups.
//
// Ansible writes a host into _meta.hostvars only when that host HAS variables. An ordinary inventory
// whose hosts carry none produces an empty hostvars with every host still listed under its group, so
// reading hostvars alone enumerated ZERO hosts for the most common inventory there is. A sharded
// submit then saw fewer than two hosts, quietly ran unsharded, and answered 202. The request was not
// refused and nothing recorded that the fan-out had not happened.
func TestParseInventoryHostsReadsGroupsNotOnlyHostVars(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
		Want []string
	}{{ // Test 0: The shape real ansible emits for hosts with no variables.
		Name: "groups only, hostvars empty",
		In: `{"_meta":{"hostvars":{}},"all":{"children":["ungrouped","web","db"]},
			"web":{"hosts":["web01","web02"]},"db":{"hosts":["db01"]}}`,
		Want: []string{"db01", "web01", "web02"},
	}, { // Test 1: Hosts carrying variables appear in both places and are not doubled.
		Name: "hostvars and groups agree",
		In: `{"_meta":{"hostvars":{"web01":{"ansible_user":"deploy"}}},
			"web":{"hosts":["web01"]}}`,
		Want: []string{"web01"},
	}, { // Test 2: A dynamic plugin may name a host in hostvars that belongs to no group.
		Name: "hostvars only",
		In:   `{"_meta":{"hostvars":{"i-0abc":{}}},"all":{"children":["ungrouped"]}}`,
		Want: []string{"i-0abc"},
	}, { // Test 3: A host in two groups is one host.
		Name: "overlapping groups",
		In: `{"_meta":{"hostvars":{}},"web":{"hosts":["web01"]},
			"canary":{"hosts":["web01"]}}`,
		Want: []string{"web01"},
	}, { // Test 4: An empty inventory is empty, not an error.
		Name: "no hosts anywhere",
		In:   `{"_meta":{"hostvars":{}},"all":{"children":["ungrouped"]}}`,
		Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, err := parseInventoryHosts([]byte(test.In))
			if err != nil {
				t.Fatalf("test %d: parseInventoryHosts() error = %v", testNum, err)
			}
			if diff := cmp.Diff(test.Want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("test %d: hosts mismatch (-want +got):\n%s", testNum, diff)
			}
		})
	}
}

// TestParseInventoryHostsAgainstRealAnsible asks the actual tool rather than trusting a fixture of
// what it emits, because the fixture is the thing that was wrong.
func TestParseInventoryHostsAgainstRealAnsible(t *testing.T) {
	t.Parallel()
	bin, err := exec.LookPath("ansible-inventory")
	if err != nil {
		t.Skip("no ansible-inventory to ask")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.ini")
	if err := os.WriteFile(path, []byte("[web]\nweb01\nweb02\n\n[db]\ndb01\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	out, err := exec.Command(bin, "-i", path, "--list").Output()
	if err != nil {
		t.Fatalf("ansible-inventory error = %v", err)
	}
	got, err := parseInventoryHosts(out)
	if err != nil {
		t.Fatalf("parseInventoryHosts() error = %v", err)
	}
	if diff := cmp.Diff([]string{"db01", "web01", "web02"}, got); diff != "" {
		t.Errorf("real ansible enumeration mismatch (-want +got):\n%s", diff)
	}
}
