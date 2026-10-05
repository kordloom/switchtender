package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// limitInventory is the stored inventory every limit below is evaluated against: two groups, and a
// third made of both as children, so a host is reached through more than one group.
const limitInventory = "[web]\nweb01 ansible_host=10.0.0.11\nweb02 ansible_host=10.0.0.12\n" +
	"canary ansible_host=10.0.0.13\n\n[db]\ndb01\n\n[prod:children]\nweb\ndb\n"

// TestLimitHostsIsAnsiblesOwnReading pins that the hosts a callback's limit check admits are the
// hosts Ansible itself selects with the same pattern, through real ansible-inventory. Each case is
// a pattern shape a template's limit is written in, including the ones whose rules are easiest to
// get wrong by imitation: group children, exclusion, intersection, a subscript, and a regular
// expression.
func TestLimitHostsIsAnsiblesOwnReading(t *testing.T) {
	t.Parallel()
	requireAnsibleInventory(t)
	d := New(run.NewMemStore(), roundhouse.NewAnsibleRunner(), zap.NewNop())
	t.Cleanup(d.Close)
	tests := []struct {
		Limit     string
		WantHosts []string
	}{{ // Test 0: A group.
		Limit: "web", WantHosts: []string{"canary", "web01", "web02"},
	}, { // Test 1: A group made of child groups.
		Limit: "prod", WantHosts: []string{"canary", "db01", "web01", "web02"},
	}, { // Test 2: An exclusion.
		Limit: "web:!canary", WantHosts: []string{"web01", "web02"},
	}, { // Test 3: An intersection.
		Limit: "prod:&db", WantHosts: []string{"db01"},
	}, { // Test 4: A list of hosts.
		Limit: "web02,db01", WantHosts: []string{"db01", "web02"},
	}, { // Test 5: A regular expression.
		Limit: "~web0[12]", WantHosts: []string{"web01", "web02"},
	}, { // Test 6: A pattern that matches nothing selects nothing.
		Limit: "nothing-here",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := d.LimitHosts(context.Background(), limitInventory, test.Limit)
			if err != nil {
				t.Fatalf("LimitHosts() error = %v", err)
			}
			if diff := cmp.Diff(test.WantHosts, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hosts mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLimitHostsNeedsAnsibleInventory pins that a dispatcher with no way to run ansible-inventory
// says so rather than answering with no hosts, which a caller would read as a host outside the
// limit.
func TestLimitHostsNeedsAnsibleInventory(t *testing.T) {
	t.Parallel()
	d := New(run.NewMemStore(), okRunner(), zap.NewNop())
	t.Cleanup(d.Close)
	if _, err := d.LimitHosts(context.Background(), limitInventory, "web"); !errors.Is(err,
		inventory.ErrResolve) {
		t.Errorf("LimitHosts() error = %v, want ErrResolve", err)
	}
}
