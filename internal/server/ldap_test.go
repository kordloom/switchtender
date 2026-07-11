package server

import (
	"testing"

	"github.com/dcadolph/yardmaster/internal/user"
)

func TestLDAPRoleFor(t *testing.T) {
	t.Parallel()
	l := &LDAPAuth{
		defaultRole: user.RoleViewer,
		roleMap: map[string]user.Role{
			"cn=admins,ou=groups,dc=x": user.RoleAdmin,
			"cn=ops,ou=groups,dc=x":    user.RoleOperator,
		},
	}
	// Test 0: A matching group sets the role, case-insensitively.
	if r := l.roleFor([]string{"CN=Admins,OU=Groups,DC=X"}); r != user.RoleAdmin {
		t.Errorf("admin group role = %q, want admin", r)
	}
	// Test 1: The first group that maps wins.
	if r := l.roleFor([]string{"cn=other,dc=x", "cn=ops,ou=groups,dc=x"}); r != user.RoleOperator {
		t.Errorf("ops group role = %q, want operator", r)
	}
	// Test 2: No mapped group falls back to the default role.
	if r := l.roleFor([]string{"cn=nobody,dc=x"}); r != user.RoleViewer {
		t.Errorf("unmatched role = %q, want viewer", r)
	}
	// Test 3: No groups at all falls back to the default role.
	if r := l.roleFor(nil); r != user.RoleViewer {
		t.Errorf("no-groups role = %q, want viewer", r)
	}
}
