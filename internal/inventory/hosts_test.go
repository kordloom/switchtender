package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestHosts pins the host list read from each encoding a stored inventory is held in, including the
// address a provisioning callback is matched against. The list is the one Ansible reads: entries
// Ansible would not read are refused rather than guessed at.
func TestHosts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want          error
		Content       string
		WantNames     []string
		WantAddresses []string
	}{{ // Test 0: INI with groups, inline variables, a port, quoting, and sections without hosts.
		Content: "bastion\n[web]\nweb01 ansible_host=10.0.0.11 note=\"two words\"\n" +
			"db.example.com:5309\n# a comment\n[web:vars]\nhttp_port=80\n[all:children]\nweb\n",
		WantNames:     []string{"bastion", "db.example.com", "web01"},
		WantAddresses: []string{"bastion", "db.example.com", "10.0.0.11"},
	}, { // Test 1: INI ranges, numeric with padding and alphabetic.
		Content: "[web]\nweb[01:03].example.com\ndb-[a:b]\n",
		WantNames: []string{"db-a", "db-b", "web01.example.com", "web02.example.com",
			"web03.example.com"},
		WantAddresses: []string{"db-a", "db-b", "web01.example.com", "web02.example.com",
			"web03.example.com"},
	}, { // Test 2: The YAML static form with nested children and host variables.
		Content: "all:\n  hosts:\n    edge:\n  children:\n    web:\n      hosts:\n" +
			"        web01:\n          ansible_host: 192.0.2.10\n        web02:\n",
		WantNames:     []string{"edge", "web01", "web02"},
		WantAddresses: []string{"edge", "192.0.2.10", "web02"},
	}, { // Test 3: The JSON ansible-inventory --list form is refused: no Ansible plugin reads it.
		Content: `{"all":{"children":["ungrouped","web"]},"web":{"hosts":["web01","web02"]},` +
			`"_meta":{"hostvars":{"web01":{"ansible_host":"198.51.100.4"}}}}`,
		Want: ErrInvalidInventory,
	}, { // Test 4: A plugin configuration needs Ansible to name any host.
		Content: "plugin: amazon.aws.aws_ec2\nregions:\n  - us-east-1\n",
		Want:    ErrNeedsAnsible,
	}, { // Test 5: A range that expands past the bound is refused.
		Content: "[huge]\nh[0:999]-[0:999]\n",
		Want:    ErrTooManyHosts,
	}, { // Test 6: A # inside a word ends the line, as shlex reads it, so the host is not hidden.
		Content:   "[web]\nweb01 ansible_host=10.0.0.11#old ansible_host=10.0.0.99\n",
		WantNames: []string{"web01"}, WantAddresses: []string{"10.0.0.11"},
	}, { // Test 7: A stride, and a range whose start is after its end, which names nothing.
		Content: "[web]\napp[1:10:4]\ngone[5:1]\n", WantNames: []string{"app1", "app5", "app9"},
		WantAddresses: []string{"app1", "app5", "app9"},
	}, { // Test 8: A group ansible_host is not the host's own, which is what AWX matches.
		Content: "[web]\nweb01\n[web:vars]\nansible_host=10.9.9.9\n", WantNames: []string{"web01"},
		WantAddresses: []string{"web01"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := Hosts(test.Content)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Hosts() error = %v, want %v", err, test.Want)
			}
			var names, addrs []string
			for _, h := range got {
				names = append(names, h.Name)
				addrs = append(addrs, h.Address())
			}
			if diff := cmp.Diff(test.WantNames, names, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Hosts() names mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantAddresses, addrs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Hosts() addresses mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHostsPort pins that a host:port entry sets the host's own ansible_port the way Ansible's Host
// does, as an int, and that a zero port sets nothing.
func TestHostsPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Content  string
		WantVars map[string]any
	}{{ // Test 0: A port becomes ansible_port.
		Content: "db.example.com:5309\n", WantVars: map[string]any{"ansible_port": json.Number("5309")},
	}, { // Test 1: A zero port sets nothing.
		Content: "db.example.com:0\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := Hosts(test.Content)
			if err != nil {
				t.Fatalf("Hosts() error = %v", err)
			}
			if len(got) != 1 || got[0].Name != "db.example.com" {
				t.Fatalf("Hosts() = %+v, want db.example.com", got)
			}
			if diff := cmp.Diff(test.WantVars, got[0].Vars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("vars mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
