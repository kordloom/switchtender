package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/kordloom/switchtender/internal/user"
)

// TestReadRolesBoundManagementData checks that reading the configuration which decides who may do
// what, and what runs without a person, is not open to every viewer.
//
// Read filtering is wired into templates, projects, inventories, credentials, and the run list. It
// was wired into nothing else, and a GET defaults to the viewer role, so a viewer in any
// organization could read the entire access map, every approval gate, and the schedules, triggers,
// and inventory sources naming the credentials and projects they run with. The access map in
// particular is both a map of what is worth attacking and a list of which changes nobody is
// watching. These objects carry no organization to filter by, so the role is what bounds them, the
// same way the audit trail and the account list are already bounded.
func TestReadRolesBoundManagementData(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path string
		Want user.Role
	}{ // Test 0 to 3: Authorization configuration is admin ground, like the audit trail.
		{"/v1/grants", user.RoleAdmin},
		{"/v1/grants/gr_1", user.RoleAdmin},
		{"/v1/policies", user.RoleAdmin},
		{"/v1/policies/pol_1", user.RoleAdmin},
		// Test 4 to 7: What runs without a person is operator ground.
		{"/v1/schedules", user.RoleOperator},
		{"/v1/schedules/sch_1", user.RoleOperator},
		{"/v1/triggers", user.RoleOperator},
		{"/v1/inventory-sources", user.RoleOperator},
		// Test 8 to 11: Ordinary operational reads stay open to viewers.
		{"/v1/runs", user.RoleViewer},
		{"/v1/templates", user.RoleViewer},
		{"/v1/fleet", user.RoleViewer},
		{"/v1/projects", user.RoleViewer},
		// Test 12 and 13: The two that were already bounded stay bounded.
		{"/v1/audit", user.RoleAdmin},
		{"/v1/users", user.RoleAdmin},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Path), func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				"http://example.test"+test.Path, nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			if got := requiredRole(req); got != test.Want {
				t.Errorf("reading %s needs %q, want %q", test.Path, got, test.Want)
			}
		})
	}
}
