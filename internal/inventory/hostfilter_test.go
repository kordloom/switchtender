package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// filterHosts are the candidates every filter test is matched against.
func filterHosts() []*Candidate {
	return []*Candidate{{
		Name: "web1", Groups: []string{"prod", "web"},
		Vars: map[string]any{
			"env": "prod", "port": json.Number("8080"), "tags": []any{"edge", "tls"},
			"owner": map[string]any{"team": "payments"},
		},
		Facts: map[string]string{
			"distribution": "Ubuntu", "processor_vcpus": "8", "ip": "10.0.0.5",
		},
		InventoryID: "inv_a", InventoryName: "production",
	}, {
		Name: "web2-canary", Groups: []string{"web"},
		Vars:        map[string]any{"env": "staging", "port": json.Number("80"), "enabled_tls": true},
		Facts:       map[string]string{"distribution": "RedHat", "processor_vcpus": "2"},
		InventoryID: "inv_a", InventoryName: "production",
	}, {
		Name: "db1", Groups: []string{"db"},
		Vars:        map[string]any{"env": "prod"},
		InventoryID: "inv_b", InventoryName: "databases",
	}}
}

// TestHostFilterSemantics pins what each piece of AWX host_filter syntax selects. A smart inventory
// is only as trustworthy as its filter: one that read a lookup as an exact match, or not as and,
// would reach machines its author never meant it to.
func TestHostFilterSemantics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Filter    string
		WantHosts []string
	}{{ // Test 0: A bare name is an exact match.
		Filter: "name=web1", WantHosts: []string{"web1"},
	}, { // Test 1: A quoted value is read whole.
		Filter: `name="web2-canary"`, WantHosts: []string{"web2-canary"},
	}, { // Test 2: icontains ignores case.
		Filter: "name__icontains=WEB", WantHosts: []string{"web1", "web2-canary"},
	}, { // Test 3: startswith and endswith read the edges.
		Filter: "name__startswith=web and name__endswith=canary", WantHosts: []string{"web2-canary"},
	}, { // Test 4: Group membership matches a direct group.
		Filter: "groups__name=prod", WantHosts: []string{"web1"},
	}, { // Test 5: Two terms side by side mean and.
		Filter: "groups__name=web name__icontains=canary", WantHosts: []string{"web2-canary"},
	}, { // Test 6: or widens.
		Filter: "groups__name=db or name=web1", WantHosts: []string{"db1", "web1"},
	}, { // Test 7: not negates, and binds tighter than or.
		Filter:    "groups__name=web and not name__icontains=canary or groups__name=db",
		WantHosts: []string{"db1", "web1"},
	}, { // Test 8: Parentheses group.
		Filter:    "groups__name=web and (name=web1 or name=db1)",
		WantHosts: []string{"web1"},
	}, { // Test 9: A host variable is matched by key.
		Filter: "variables__env=prod", WantHosts: []string{"db1", "web1"},
	}, { // Test 10: A nested variable is matched by its path.
		Filter: "variables__owner__team=payments", WantHosts: []string{"web1"},
	}, { // Test 11: A list variable is matched through [].
		Filter: "variables__tags[]=tls", WantHosts: []string{"web1"},
	}, { // Test 12: A number compares numerically.
		Filter: "variables__port__gt=100", WantHosts: []string{"web1"},
	}, { // Test 13: A boolean variable matches true.
		Filter: "variables__enabled_tls=true", WantHosts: []string{"web2-canary"},
	}, { // Test 14: isnull selects hosts without the variable.
		Filter: "variables__tags__isnull=true", WantHosts: []string{"db1", "web2-canary"},
	}, { // Test 15: A fact is matched with the ansible_ prefix AWX writes.
		Filter: "ansible_facts__ansible_distribution=Ubuntu", WantHosts: []string{"web1"},
	}, { // Test 16: A fact is matched without the prefix too, and a host with no facts never matches.
		Filter: "ansible_facts__processor_vcpus=2", WantHosts: []string{"web2-canary"},
	}, { // Test 17: The default route address is the stored ip.
		Filter: "ansible_facts__ansible_default_ipv4__address=10.0.0.5", WantHosts: []string{"web1"},
	}, { // Test 18: The input inventory's name is a field.
		Filter: "inventory__name=databases", WantHosts: []string{"db1"},
	}, { // Test 19: in matches any listed value.
		Filter: "name__in=db1,web1", WantHosts: []string{"db1", "web1"},
	}, { // Test 20: A regex lookup is a regular expression.
		Filter: `name__regex=^web\d$`, WantHosts: []string{"web1"},
	}, { // Test 21: search is a case-insensitive name search.
		Filter: "search=CANARY", WantHosts: []string{"web2-canary"},
	}, { // Test 22: Keywords are read in any case.
		Filter: "NOT groups__name=web AND inventory=inv_b", WantHosts: []string{"db1"},
	}, { // Test 23: enabled=true selects every host and enabled=false none.
		Filter: "enabled=false", WantHosts: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f, err := ParseHostFilter(test.Filter)
			if err != nil {
				t.Fatalf("ParseHostFilter(%q) error = %v", test.Filter, err)
			}
			var got []string
			for _, h := range filterHosts() {
				if f.Match(h) {
					got = append(got, h.Name)
				}
			}
			if diff := cmp.Diff(test.WantHosts, got, cmpopts.EquateEmpty(),
				cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
				t.Errorf("%q selected (-want +got):\n%s", test.Filter, diff)
			}
		})
	}
}

// TestHostFilterRefusals pins that a filter this cannot read is refused rather than read as
// matching nothing, which would make a smart inventory empty without saying why.
func TestHostFilterRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Filter string
		Want   error
	}{
		{Filter: "", Want: ErrHostFilter},                                // Test 0: Empty.
		{Filter: "hostname=web1", Want: ErrHostFilter},                   // Test 1: Unknown field.
		{Filter: "name__resembles=web", Want: ErrHostFilter},             // Test 2: Unknown lookup.
		{Filter: "groups=web", Want: ErrHostFilter},                      // Test 3: Groups without name.
		{Filter: "name=web1 and", Want: ErrHostFilter},                   // Test 4: Dangling keyword.
		{Filter: "(name=web1", Want: ErrHostFilter},                      // Test 5: Unclosed parenthesis.
		{Filter: `name="web1`, Want: ErrHostFilter},                      // Test 6: Unclosed quote.
		{Filter: "name__regex=(", Want: ErrHostFilter},                   // Test 7: Bad pattern.
		{Filter: "web1", Want: ErrHostFilter},                            // Test 8: A bare word.
		{Filter: "ansible_facts=x", Want: ErrHostFilter},                 // Test 9: A fact with no name.
		{Filter: "enabled=yes", Want: ErrHostFilter},                     // Test 10: enabled is boolean.
		{Filter: "name=", Want: ErrHostFilter},                           // Test 11: An empty value.
		{Filter: "name=web1) or (name=db1", Want: ErrHostFilter},         // Test 12: Stray closing.
		{Filter: "groups__name=web or not name__icontains=x", Want: nil}, // Test 13: A valid filter.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, err := ParseHostFilter(test.Filter)
			if !errors.Is(err, test.Want) {
				t.Errorf("ParseHostFilter(%q) error = %v, want %v", test.Filter, err, test.Want)
			}
		})
	}
}

// TestHostFilterDepthBounded pins that a hostile filter cannot exhaust the parser's stack.
func TestHostFilterDepthBounded(t *testing.T) {
	t.Parallel()
	deep := ""
	for range 200 {
		deep += "not ("
	}
	deep += "name=x"
	for range 200 {
		deep += ")"
	}
	if _, err := ParseHostFilter(deep); !errors.Is(err, ErrHostFilter) {
		t.Errorf("a filter nested 200 deep parsed with error %v, want ErrHostFilter", err)
	}
}

// TestHostFilterUsesFacts pins that facts are only asked for when a term reads them, so a filter
// over names never costs a read of the whole estate.
func TestHostFilterUsesFacts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Filter    string
		WantFacts bool
	}{
		{Filter: "name=web1", WantFacts: false},                                      // Test 0.
		{Filter: "name=web1 or ansible_facts__distribution=Ubuntu", WantFacts: true}, // Test 1.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f, err := ParseHostFilter(test.Filter)
			if err != nil {
				t.Fatalf("ParseHostFilter() error = %v", err)
			}
			if got := f.UsesFacts(); got != test.WantFacts {
				t.Errorf("UsesFacts() = %v, want %v", got, test.WantFacts)
			}
		})
	}
}
