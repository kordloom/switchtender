package importer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/inventory"
)

// TestApplyRefusesAnInventoryPastTheHostBound pins the check Apply makes before it writes anything:
// a planned inventory naming more hosts than inventory.MaxHosts is named, and a plan holding none
// is let through.
func TestApplyRefusesAnInventoryPastTheHostBound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Plan        *Plan
		Want        error
		WantMessage string
	}{{ // Test 0: One inventory one host past the bound.
		Plan: &Plan{Inventories: []*inventory.Inventory{
			{Name: "small", Content: "[web]\nweb01\n"},
			{Name: "fleet", Content: "[web]\nh[0:100000]\n"},
		}},
		Want: inventory.ErrTooManyHosts, WantMessage: `the inventory "fleet": inventory names more than 100000`,
	}, { // Test 1: An inventory at the bound.
		Plan: &Plan{Inventories: []*inventory.Inventory{{Name: "fleet", Content: "[web]\nh[1:100000]\n"}}},
	}, { // Test 2: A dynamic source's backing inventory, which holds no hosts until it refreshes.
		Plan: &Plan{Inventories: []*inventory.Inventory{{Name: "ec2"}}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.Plan.CheckHostBounds()
			if !errors.Is(err, test.Want) {
				t.Fatalf("CheckHostBounds() error = %v, want %v", err, test.Want)
			}
			if test.Want == nil {
				return
			}
			if !strings.Contains(err.Error(), test.WantMessage) {
				t.Errorf("CheckHostBounds() error = %q, want it to contain %q", err, test.WantMessage)
			}
			// ApplyStores is empty, so reaching any store would panic: the refusal comes first.
			if _, err := test.Plan.Apply(context.Background(), ApplyStores{}); !errors.Is(err, test.Want) {
				t.Errorf("Apply() error = %v, want %v before any store is touched", err, test.Want)
			}
		})
	}
}

// TestChefFleetPastTheHostBoundIsRefused maps a real export naming one node past the bound and
// checks the plan it produces is refused, so the mapping stage cannot hand Apply an inventory every
// launch would refuse.
func TestChefFleetPastTheHostBoundIsRefused(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i <= inventory.MaxHosts; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":"n%06d"}`, i)
	}
	b.WriteString("]")
	plan, err := FromChef([]byte(b.String()), time.Now())
	if err != nil {
		t.Fatalf("FromChef() error = %v", err)
	}
	if err := plan.CheckHostBounds(); !errors.Is(err, inventory.ErrTooManyHosts) {
		t.Errorf("CheckHostBounds() error = %v, want %v", err, inventory.ErrTooManyHosts)
	}
}
