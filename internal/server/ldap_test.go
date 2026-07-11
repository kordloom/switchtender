package server

import (
	"testing"

	"github.com/dcadolph/yardmaster/internal/user"
)

func TestRoleForGroups(t *testing.T) {
	t.Parallel()
	roleMap := map[string]user.Role{
		"cn=admins,ou=groups,dc=x": user.RoleAdmin,
		"cn=ops,ou=groups,dc=x":    user.RoleOperator,
	}
	// Test 0: A matching group sets the role, case-insensitively.
	if r := roleForGroups([]string{"CN=Admins,OU=Groups,DC=X"}, roleMap, user.RoleViewer); r != user.RoleAdmin {
		t.Errorf("admin group role = %q, want admin", r)
	}
	// Test 1: The first group that maps wins.
	if r := roleForGroups([]string{"cn=other,dc=x", "cn=ops,ou=groups,dc=x"}, roleMap, user.RoleViewer); r != user.RoleOperator {
		t.Errorf("ops group role = %q, want operator", r)
	}
	// Test 2: No mapped group falls back to the default role.
	if r := roleForGroups([]string{"cn=nobody,dc=x"}, roleMap, user.RoleViewer); r != user.RoleViewer {
		t.Errorf("unmatched role = %q, want viewer", r)
	}
	// Test 3: No groups at all falls back to the default role.
	if r := roleForGroups(nil, roleMap, user.RoleViewer); r != user.RoleViewer {
		t.Errorf("no-groups role = %q, want viewer", r)
	}
}

func TestClaimGroups(t *testing.T) {
	t.Parallel()
	// Test 0: A list of strings passes through.
	if g := claimGroups([]string{"a", "b"}); len(g) != 2 || g[0] != "a" {
		t.Errorf("[]string groups = %v, want [a b]", g)
	}
	// Test 1: A JSON list of any keeps only the strings.
	if g := claimGroups([]any{"a", 3, "b"}); len(g) != 2 || g[1] != "b" {
		t.Errorf("[]any groups = %v, want [a b]", g)
	}
	// Test 2: A single string becomes a one-element slice.
	if g := claimGroups("solo"); len(g) != 1 || g[0] != "solo" {
		t.Errorf("string groups = %v, want [solo]", g)
	}
	// Test 3: An absent claim is no groups.
	if g := claimGroups(nil); g != nil {
		t.Errorf("nil groups = %v, want nil", g)
	}
}
