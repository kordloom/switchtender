package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/switchtender/internal/grant"
	"github.com/dcadolph/switchtender/internal/org"
	"github.com/dcadolph/switchtender/internal/project"
	"github.com/dcadolph/switchtender/internal/user"
)

// mustAddOrg saves an organization and its members, failing the test on any store error.
func mustAddOrg(t *testing.T, orgs org.Store, id string, members map[string]org.Role) {
	t.Helper()
	ctx := context.Background()
	if err := orgs.Save(ctx, &org.Org{ID: id, Name: id}); err != nil {
		t.Fatalf("Save org %s: %v", id, err)
	}
	for uid, role := range members {
		if err := orgs.AddMember(ctx, id, uid, role); err != nil {
			t.Fatalf("AddMember %s/%s: %v", id, uid, err)
		}
	}
}

// ctxActor returns a context carrying the given actor, as the auth gate would install it.
func ctxActor(userID string, role user.Role) context.Context {
	return context.WithValue(context.Background(), actorKey{}, Actor{UserID: userID, Role: role})
}

// orgProjectIDs extracts the ids of a project slice in order, for comparing list visibility.
func orgProjectIDs(ps []*project.Project) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

// orgOwnedFixture builds the shared object-tenancy fixture and returns a factory for an authorizer
// over it. Organization org_a owns proj_a and proj_solo, org_b owns proj_b. proj_a additionally holds
// an explicit use grant to a non-member, and the unowned proj_granted holds a read grant to a member,
// so the tests can prove ownership adds to grants rather than replacing them.
func orgOwnedFixture(t *testing.T) func(strict bool) *authorizer {
	t.Helper()
	orgs := org.NewMemStore()
	mustAddOrg(t, orgs, "org_a", map[string]org.Role{
		"user_admin_a": org.RoleAdmin, "user_member_a": org.RoleMember,
	})
	mustAddOrg(t, orgs, "org_b", map[string]org.Role{"user_b": org.RoleMember})

	owners := map[string]string{"proj_a": "org_a", "proj_solo": "org_a", "proj_b": "org_b"}
	resolver := OrgResolverFunc(func(_ context.Context, id string) (string, bool) {
		o, ok := owners[id]
		return o, ok
	})
	grants := &fakeGrants{byObject: map[string][]*grant.Grant{
		"proj_a":       {{Subject: "user_outsider", Access: grant.AccessUse}},
		"proj_granted": {{Subject: "user_member_a", Access: grant.AccessRead}},
	}}
	return func(strict bool) *authorizer {
		return &authorizer{grants: grants, orgs: orgs, orgOwners: resolver, strict: strict}
	}
}

// TestAuthorizeOrgOwnership checks the additive authorize decision: an org admin manages the org's
// objects, a plain member uses them but cannot manage, a non-member holding an explicit grant still
// gets in, a global admin bypasses, and an unowned object still follows the role.
func TestAuthorizeOrgOwnership(t *testing.T) {
	t.Parallel()
	newAuthz := orgOwnedFixture(t)
	tests := []struct {
		Name   string
		Actor  Actor
		Object string
		Want   grant.Access
		Strict bool
		Allow  bool
	}{ // Test 0: A global admin bypasses object checks entirely.
		{"admin bypass", Actor{UserID: "user_x", Role: user.RoleAdmin}, "proj_solo", grant.AccessManage, true, true},
		// Test 1: An admin of the owning org manages its objects.
		{"org admin manages", Actor{UserID: "user_admin_a", Role: user.RoleViewer}, "proj_solo", grant.AccessManage, true, true},
		// Test 2: A plain member of the owning org may use its objects.
		{"member uses", Actor{UserID: "user_member_a", Role: user.RoleViewer}, "proj_solo", grant.AccessUse, true, true},
		// Test 3: A plain member gets use, not manage.
		{"member cannot manage", Actor{UserID: "user_member_a", Role: user.RoleViewer}, "proj_solo", grant.AccessManage, true, false},
		// Test 4: A non-member holding an explicit grant still gets access, proving ownership is additive.
		{"non-member grant additive", Actor{UserID: "user_outsider", Role: user.RoleViewer}, "proj_a", grant.AccessUse, true, true},
		// Test 5: An unowned object defers to the role when grants are not strict.
		{"unowned defers to role", Actor{UserID: "user_stranger", Role: user.RoleViewer}, "proj_free", grant.AccessUse, false, true},
		// Test 6: An unowned object under strict grants is denied without a grant, the unchanged behavior.
		{"unowned strict denies", Actor{UserID: "user_stranger", Role: user.RoleViewer}, "proj_free", grant.AccessUse, true, false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			authz := newAuthz(test.Strict)
			err := authz.authorize(ctxActor(test.Actor.UserID, test.Actor.Role), test.Object, test.Want)
			if test.Allow && err != nil {
				t.Errorf("%s: authorize() = %v, want allow", test.Name, err)
			}
			if !test.Allow && !errors.Is(err, errForbiddenGrant) {
				t.Errorf("%s: authorize() = %v, want errForbiddenGrant", test.Name, err)
			}
		})
	}
}

// TestAuthorizeOrgManageDelegation checks that management delegation, the path the auth gate uses to
// allow an edit or delete beyond the global role, follows org ownership: an org admin manages the
// org's objects, a plain member does not, and a non-member does not.
func TestAuthorizeOrgManageDelegation(t *testing.T) {
	t.Parallel()
	authz := orgOwnedFixture(t)(true)
	tests := []struct {
		Name   string
		Actor  Actor
		Object string
		WantOK bool
	}{ // Test 0: An admin of the owning org may manage its object.
		{"org admin", Actor{UserID: "user_admin_a", Role: user.RoleViewer}, "proj_solo", true},
		// Test 1: A plain member may not manage, so the gate falls back to the role.
		{"org member", Actor{UserID: "user_member_a", Role: user.RoleViewer}, "proj_solo", false},
		// Test 2: A member of another org may not manage.
		{"other org", Actor{UserID: "user_b", Role: user.RoleViewer}, "proj_solo", false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := authz.manages(context.Background(), test.Actor, test.Object)
			if err != nil {
				t.Fatalf("manages() error = %v", err)
			}
			if got != test.WantOK {
				t.Errorf("%s: manages() = %v, want %v", test.Name, got, test.WantOK)
			}
		})
	}
}

// TestAuthorizeOrgStrictLeak is the isolation leak guard: under strict grants a member of another org
// and an ungranted stranger can neither authorize nor list an object owned by an org they do not
// belong to, proven through both the authorize decision and the read filter.
func TestAuthorizeOrgStrictLeak(t *testing.T) {
	t.Parallel()
	authz := orgOwnedFixture(t)(true)

	deny := []struct {
		Name   string
		Actor  Actor
		Object string
	}{ // Test 0: An org_b member cannot use an ungranted org_a object.
		{"org_b member on org_a object", Actor{UserID: "user_b", Role: user.RoleViewer}, "proj_solo"},
		// Test 1: An org_b member cannot use an org_a object even when it carries a grant to someone else.
		{"org_b member on granted org_a object", Actor{UserID: "user_b", Role: user.RoleViewer}, "proj_a"},
		// Test 2: A stranger in no org cannot use an org_a object.
		{"stranger on org_a object", Actor{UserID: "user_stranger", Role: user.RoleViewer}, "proj_solo"},
	}
	for testNum, d := range deny {
		t.Run(fmt.Sprintf("authorize test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := authz.authorize(ctxActor(d.Actor.UserID, d.Actor.Role), d.Object, grant.AccessUse)
			if !errors.Is(err, errForbiddenGrant) {
				t.Errorf("%s: authorize() = %v, want errForbiddenGrant", d.Name, err)
			}
		})
	}

	// The list path must not leak either: an org_b member sees only org_b's objects, never org_a's.
	t.Run("readFilter excludes other orgs", func(t *testing.T) {
		t.Parallel()
		list := []*project.Project{
			{ID: "proj_a", OrgID: "org_a"},
			{ID: "proj_solo", OrgID: "org_a"},
			{ID: "proj_b", OrgID: "org_b"},
		}
		visible, err := filterReadable(ctxActor("user_b", user.RoleViewer), authz, list,
			func(p *project.Project) string { return p.ID },
			func(p *project.Project) string { return p.OrgID })
		if err != nil {
			t.Fatalf("filterReadable() error = %v", err)
		}
		if diff := cmp.Diff([]string{"proj_b"}, orgProjectIDs(visible), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("visible mismatch (-want +got):\n%s", diff)
		}
	})
}

// TestAuthorizeOrgStrictWrongfulDeny is the isolation over-reach guard: under strict grants a member
// still sees and uses their own org's objects and any object explicitly granted to them, and without
// strict grants an unowned object stays visible per the role. A partial gate that over-denied would
// fail here.
func TestAuthorizeOrgStrictWrongfulDeny(t *testing.T) {
	t.Parallel()
	newAuthz := orgOwnedFixture(t)
	strict := newAuthz(true)

	// A member is not wrongly denied use of their own org's object.
	t.Run("member uses own org object", func(t *testing.T) {
		t.Parallel()
		if err := strict.authorize(ctxActor("user_member_a", user.RoleViewer), "proj_solo", grant.AccessUse); err != nil {
			t.Errorf("member denied own org object: %v", err)
		}
	})

	// The list keeps the member's own org objects and anything granted to them, and hides only what
	// belongs to another org or is unowned and ungranted.
	t.Run("readFilter keeps own org and granted objects", func(t *testing.T) {
		t.Parallel()
		list := []*project.Project{
			{ID: "proj_a", OrgID: "org_a"},    // Owned by org_a; visible via membership.
			{ID: "proj_solo", OrgID: "org_a"}, // Owned by org_a; visible via membership.
			{ID: "proj_b", OrgID: "org_b"},    // Another org; hidden.
			{ID: "proj_granted", OrgID: ""},   // Unowned but read-granted; visible via the grant.
			{ID: "proj_free", OrgID: ""},      // Unowned and ungranted; hidden under strict, unchanged.
		}
		visible, err := filterReadable(ctxActor("user_member_a", user.RoleViewer), strict, list,
			func(p *project.Project) string { return p.ID },
			func(p *project.Project) string { return p.OrgID })
		if err != nil {
			t.Fatalf("filterReadable() error = %v", err)
		}
		want := []string{"proj_a", "proj_solo", "proj_granted"}
		if diff := cmp.Diff(want, orgProjectIDs(visible), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("visible mismatch (-want +got):\n%s", diff)
		}
	})

	// Without strict grants an unowned object stays visible per the role, both to use and in a list.
	t.Run("unowned visible per role when not strict", func(t *testing.T) {
		t.Parallel()
		nonStrict := newAuthz(false)
		if err := nonStrict.authorize(ctxActor("user_stranger", user.RoleViewer), "proj_free", grant.AccessUse); err != nil {
			t.Errorf("unowned object denied under role deferral: %v", err)
		}
		list := []*project.Project{{ID: "proj_free", OrgID: ""}}
		visible, err := filterReadable(ctxActor("user_stranger", user.RoleViewer), nonStrict, list,
			func(p *project.Project) string { return p.ID },
			func(p *project.Project) string { return p.OrgID })
		if err != nil {
			t.Fatalf("filterReadable() error = %v", err)
		}
		if diff := cmp.Diff([]string{"proj_free"}, orgProjectIDs(visible), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("visible mismatch (-want +got):\n%s", diff)
		}
	})
}
