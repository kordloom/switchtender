package inventory

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestExpandHostPattern pins how a host entry becomes host names and a port, as Ansible's
// parse_address and expand_hostname_range make them.
func TestExpandHostPattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want      error
		Entry     string
		WantHosts []string
		WantPort  string
	}{{ // Test 0: A plain name.
		Entry: "web01", WantHosts: []string{"web01"},
	}, { // Test 1: A name with a port.
		Entry: "db.example.com:5309", WantHosts: []string{"db.example.com"}, WantPort: "5309",
	}, { // Test 2: An IPv4 address with a port.
		Entry: "10.0.0.5:2222", WantHosts: []string{"10.0.0.5"}, WantPort: "2222",
	}, { // Test 3: A bracketed IPv6 address with a port.
		Entry: "[2001:db8::1]:22", WantHosts: []string{"2001:db8::1"}, WantPort: "22",
	}, { // Test 4: A bare IPv6 address has no port.
		Entry: "fe80::1", WantHosts: []string{"fe80::1"},
	}, { // Test 5: A name Ansible will not take a port from stays whole.
		Entry: "web_:22", WantHosts: []string{"web_:22"},
	}, { // Test 6: A padded numeric range.
		Entry: "db[01:03]", WantHosts: []string{"db01", "db02", "db03"},
	}, { // Test 7: A stride.
		Entry: "app[1:10:3]", WantHosts: []string{"app1", "app4", "app7", "app10"},
	}, { // Test 8: An alphabetic stride.
		Entry: "x[a:k:5]", WantHosts: []string{"xa", "xf", "xk"},
	}, { // Test 9: Mixed case walks ASCII letters in order.
		Entry: "m[y:B]", WantHosts: []string{"my", "mz", "mA", "mB"},
	}, { // Test 10: Two ranges in one name.
		Entry: "r[1:2]-[a:b]", WantHosts: []string{"r1-a", "r1-b", "r2-a", "r2-b"},
	}, { // Test 11: A range with a port.
		Entry: "lb[1:2]:8443", WantHosts: []string{"lb1", "lb2"}, WantPort: "8443",
	}, { // Test 12: An empty begin is zero.
		Entry: "z[:2]", WantHosts: []string{"z0", "z1", "z2"},
	}, { // Test 13: A begin after the end names no host.
		Entry: "gone[5:1]",
	}, { // Test 14: An IPv4 address with ranges.
		Entry: "10.0.[0:1].[1:2]", WantHosts: []string{"10.0.0.1", "10.0.0.2", "10.0.1.1",
			"10.0.1.2"},
	}, { // Test 15: Unequal padding is refused, as Ansible refuses it.
		Entry: "h[01:100]", Want: errAny,
	}, { // Test 16: A negative step is left to Ansible.
		Entry: "h[5:1:-1]", Want: ErrNeedsAnsible,
	}, { // Test 17: A range past the bound is refused.
		Entry: "h[0:999]-[0:999]", Want: ErrTooManyHosts,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			hosts, port, err := expandHostPattern(test.Entry, MaxHosts)
			switch {
			case errors.Is(test.Want, errAny):
				if err == nil {
					t.Fatalf("expandHostPattern(%q) error = nil, want one", test.Entry)
				}
				return
			case !errors.Is(err, test.Want):
				t.Fatalf("expandHostPattern(%q) error = %v, want %v", test.Entry, err, test.Want)
			}
			if diff := cmp.Diff(test.WantHosts, hosts, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hosts mismatch (-want +got):\n%s", diff)
			}
			if port != test.WantPort {
				t.Errorf("port = %q, want %q", port, test.WantPort)
			}
		})
	}
}

// errAny marks a table entry that expects some error, whichever it is.
var errAny = errors.New("any error")
