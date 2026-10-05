package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gopkg.in/yaml.v3"
)

// listingDoc is an ansible-inventory --list document: web1 has variables, web2 has none and so
// appears only in its group, lb1 belongs to no group and appears only in hostvars, and the number
// is past what a float64 holds exactly.
const listingDoc = `{
  "_meta": {"hostvars": {
    "web1": {"env": "prod", "big": 9007199254740993},
    "lb1": {"role": "edge"}
  }},
  "all": {"children": ["ungrouped", "web", "prod"]},
  "web": {"hosts": ["web1", "web2"]},
  "prod": {"children": ["web"]},
  "ungrouped": {}
}`

// TestListingReadsWhatAnsiblePrints pins how a composition reads an input: every host whether or
// not it has variables, direct groups only, and numbers as they were written.
func TestListingReadsWhatAnsiblePrints(t *testing.T) {
	t.Parallel()
	l, err := ParseListing([]byte(listingDoc))
	if err != nil {
		t.Fatalf("ParseListing() error = %v", err)
	}
	if diff := cmp.Diff([]string{"lb1", "web1", "web2"}, l.Hosts()); diff != "" {
		t.Errorf("Hosts() mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"web"}, l.Groups("web1")); diff != "" {
		t.Errorf("Groups(web1) mismatch (-want +got):\n%s", diff)
	}
	if got := l.Vars("web1")["big"]; got != json.Number("9007199254740993") {
		t.Errorf("Vars(web1)[big] = %v, want the integer as written", got)
	}
	if _, err := ParseListing([]byte("not json")); !errors.Is(err, ErrListing) {
		t.Errorf("ParseListing(not json) error = %v, want ErrListing", err)
	}
}

// TestListingRestrictAndRender pins the inventory a run receives: only the hosts it was resolved
// to, each host's variables once, a host with no group kept under ungrouped, and empty groups gone.
func TestListingRestrictAndRender(t *testing.T) {
	t.Parallel()
	l, err := ParseListing([]byte(listingDoc))
	if err != nil {
		t.Fatalf("ParseListing() error = %v", err)
	}
	tests := []struct {
		Keep       map[string]bool
		WantHosts  []string
		WantGroups []string
	}{{ // Test 0: Everything kept renders every group and the ungrouped host.
		Keep:       map[string]bool{"web1": true, "web2": true, "lb1": true},
		WantHosts:  []string{"lb1", "web1", "web2"},
		WantGroups: []string{"prod", "ungrouped", "web"},
	}, { // Test 1: A host outside the set is gone from its group and its variables.
		Keep:       map[string]bool{"web2": true},
		WantHosts:  []string{"web2"},
		WantGroups: []string{"prod", "web"},
	}, { // Test 2: Nothing kept renders no host at all.
		Keep:       map[string]bool{},
		WantHosts:  nil,
		WantGroups: []string{"prod"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := l.Restrict(test.Keep)
			if diff := cmp.Diff(test.WantHosts, r.Hosts(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Restrict().Hosts() mismatch (-want +got):\n%s", diff)
			}
			body, err := r.Static()
			if err != nil {
				t.Fatalf("Static() error = %v", err)
			}
			var doc map[string]map[string]map[string]any
			if err := yaml.Unmarshal(body, &doc); err != nil {
				t.Fatalf("Static() is not YAML Ansible can read: %v", err)
			}
			var groups, hosts []string
			for g, entry := range doc {
				groups = append(groups, g)
				for h := range entry["hosts"] {
					hosts = append(hosts, h)
				}
			}
			sorted := cmpopts.SortSlices(func(a, b string) bool { return a < b })
			if diff := cmp.Diff(test.WantGroups, groups, sorted, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("rendered groups mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantHosts, hosts, sorted, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("rendered hosts mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestConstructedConfig pins which constructed plugin options are accepted. Anything else is
// refused, because an option nobody reviewed is an option that may reach outside the inventory.
func TestConstructedConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		Want       error
		WantPlugin bool
	}{{ // Test 0: Empty options merge the inputs and build nothing.
		In: "", WantPlugin: true,
	}, { // Test 1: The documented options are accepted and the plugin is named.
		In: "strict: true\ncompose:\n  state2: state | default('running')\n" +
			"groups:\n  off: state == 'shutdown'\n" +
			"keyed_groups:\n  - key: env\n    prefix: env\n    separator: _\n",
		WantPlugin: true,
	}, { // Test 2: The plugin may be named, as AWX writes it.
		In: "plugin: constructed\n", WantPlugin: true,
	}, { // Test 3: Another plugin is refused.
		In: "plugin: amazon.aws.aws_ec2\n", Want: ErrSourceVars,
	}, { // Test 4: An option outside the list is refused.
		In: "cache: true\n", Want: ErrSourceVars,
	}, { // Test 5: A lookup in an expression is refused.
		In: "compose:\n  x: lookup('pipe', 'id')\n", Want: ErrSourceVars,
	}, { // Test 6: A query in a group expression is refused, spacing aside.
		In: "groups:\n  g: query ('file', '/etc/passwd')\n", Want: ErrSourceVars,
	}, { // Test 7: A keyed group with no key is refused.
		In: "keyed_groups:\n  - prefix: env\n", Want: ErrSourceVars,
	}, { // Test 8: Options that are not a mapping are refused.
		In: "- a\n- b\n", Want: ErrSourceVars,
	}, { // Test 9: strict must be a boolean.
		In: "strict: sometimes\n", Want: ErrSourceVars,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			out, err := ConstructedConfig(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ConstructedConfig() error = %v, want %v", err, test.Want)
			}
			if test.WantPlugin && !strings.Contains(string(out), "plugin: constructed") {
				t.Errorf("ConstructedConfig() = %q, want the constructed plugin named", out)
			}
		})
	}
}

// TestValidate pins that an inventory's kind and its fields agree, so a smart inventory without a
// filter, or a static one with a stray filter, never reaches a store.
func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   *Inventory
		Want error
	}{
		// Test 0: A static inventory.
		{In: &Inventory{ID: "inv_1", Content: "[web]\nweb1\n"}, Want: nil},
		// Test 1: A static inventory carrying a filter.
		{In: &Inventory{ID: "inv_1", Content: "x", HostFilter: "name=a"}, Want: ErrComposition},
		// Test 2: A smart inventory.
		{In: &Inventory{ID: "inv_1", Kind: KindSmart, HostFilter: "groups__name=web"}, Want: nil},
		// Test 3: A smart inventory with a filter that does not parse.
		{In: &Inventory{ID: "inv_1", Kind: KindSmart, HostFilter: "hostname=web"}, Want: ErrHostFilter},
		// Test 4: A smart inventory with inputs.
		{In: &Inventory{ID: "inv_1", Kind: KindSmart, HostFilter: "name=a", InputIDs: []string{"inv_2"}},
			Want: ErrComposition},
		// Test 5: A smart inventory with content of its own.
		{In: &Inventory{ID: "inv_1", Kind: KindSmart, HostFilter: "name=a", Content: "[web]\nx\n"},
			Want: ErrComposition},
		// Test 6: A constructed inventory.
		{In: &Inventory{ID: "inv_1", Kind: KindConstructed, InputIDs: []string{"inv_2", "inv_3"},
			Limit: "web"}, Want: nil},
		// Test 7: A constructed inventory with no inputs.
		{In: &Inventory{ID: "inv_1", Kind: KindConstructed}, Want: ErrComposition},
		// Test 8: A constructed inventory naming itself.
		{In: &Inventory{ID: "inv_1", Kind: KindConstructed, InputIDs: []string{"inv_1"}},
			Want: ErrComposition},
		// Test 9: A constructed inventory naming an input twice.
		{In: &Inventory{ID: "inv_1", Kind: KindConstructed, InputIDs: []string{"inv_2", "inv_2"}},
			Want: ErrComposition},
		// Test 10: A constructed inventory with options the plugin may not take.
		{In: &Inventory{ID: "inv_1", Kind: KindConstructed, InputIDs: []string{"inv_2"},
			SourceVars: "plugin: script\n"}, Want: ErrSourceVars},
		// Test 11: An unknown kind.
		{In: &Inventory{ID: "inv_1", Kind: "dynamic"}, Want: ErrComposition},
		// Test 12: A constructed inventory reading content from a secret source.
		{In: &Inventory{ID: "inv_1", Kind: KindConstructed, InputIDs: []string{"inv_2"},
			ContentSource: "vault"}, Want: ErrComposition},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := Validate(test.In); !errors.Is(err, test.Want) {
				t.Errorf("Validate() error = %v, want %v", err, test.Want)
			}
		})
	}
}
