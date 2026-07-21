package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/grant"
	"github.com/dcadolph/switchtender/internal/project"
	"github.com/dcadolph/switchtender/internal/team"
	"github.com/dcadolph/switchtender/internal/user"
)

// fakeProjects is a project.Store that answers List from a fixed slice, leaving the rest unused.
type fakeProjects struct {
	project.Store
	list []*project.Project
}

// List returns the fixed project slice.
func (f *fakeProjects) List(context.Context) ([]*project.Project, error) { return f.list, nil }

// fakeGrants is a grant.Store that answers ForObject from a map, leaving the other methods unused.
type fakeGrants struct {
	grant.Store
	byObject map[string][]*grant.Grant
	err      error
}

// ForObject returns the configured grants for object, or the configured error.
func (f *fakeGrants) ForObject(_ context.Context, object string) ([]*grant.Grant, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byObject[object], nil
}

// List returns every configured grant across all objects, or the configured error. It stamps each
// grant's Object from its map key, since the real store persists the object on every grant row.
func (f *fakeGrants) List(_ context.Context) ([]*grant.Grant, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*grant.Grant
	for obj, gs := range f.byObject {
		for _, g := range gs {
			cp := *g
			cp.Object = obj
			out = append(out, &cp)
		}
	}
	return out, nil
}

// fakeTeams is a team.Store that answers TeamsForUser from a map, leaving the other methods unused.
type fakeTeams struct {
	team.Store
	byUser map[string][]string
}

// TeamsForUser returns the configured teams for userID.
func (f *fakeTeams) TeamsForUser(_ context.Context, userID string) ([]string, error) {
	return f.byUser[userID], nil
}

// testAuthz builds an authorizer whose grants and team memberships cover the delegation cases: a
// direct manage grant, a use-only grant, a team manage grant, and a credential manage grant.
func testAuthz() *authorizer {
	return &authorizer{
		grants: &fakeGrants{byObject: map[string][]*grant.Grant{
			"proj_1": {{Subject: "user_1", Access: grant.AccessManage}},
			"proj_2": {{Subject: "user_2", Access: grant.AccessUse}},
			"proj_3": {{Subject: "team_x", Access: grant.AccessManage}},
			"cred_1": {{Subject: "user_1", Access: grant.AccessManage}},
		}},
		teams: &fakeTeams{byUser: map[string][]string{"user_3": {"team_x"}}},
	}
}

// TestDelegatedObject checks that only an exact edit or delete of a single grantable object returns
// an id, and every other shape returns empty.
func TestDelegatedObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Method  string
		Path    string
		WantObj string
	}{ // Test 0: Edit a project.
		{http.MethodPut, "/v1/projects/proj_1", "proj_1"},
		// Test 1: Delete a template.
		{http.MethodDelete, "/v1/templates/tpl_1", "tpl_1"},
		// Test 2: Edit an inventory.
		{http.MethodPut, "/v1/inventories/inv_1", "inv_1"},
		// Test 3: Delete a credential.
		{http.MethodDelete, "/v1/credentials/cred_1", "cred_1"},
		// Test 4: The /v1 prefix is optional.
		{http.MethodPut, "/projects/proj_1", "proj_1"},
		// Test 5: Create is not a single-object mutation.
		{http.MethodPost, "/v1/projects", ""},
		// Test 6: A read never delegates.
		{http.MethodGet, "/v1/projects/proj_1", ""},
		// Test 7: A sub-resource never delegates.
		{http.MethodPut, "/v1/projects/proj_1/sub", ""},
		// Test 8: Launch is a sub-resource, not a delegable edit.
		{http.MethodPost, "/v1/templates/tpl_1/launch", ""},
		// Test 9: Inventory sources are not grantable objects.
		{http.MethodPut, "/v1/inventory-sources/src_1", ""},
		// Test 10: Grants are not grantable objects.
		{http.MethodDelete, "/v1/grants/grant_1", ""},
		// Test 11: Users are not grantable objects.
		{http.MethodDelete, "/v1/users/user_1", ""},
		// Test 12: A collection path has no id.
		{http.MethodPut, "/v1/projects", ""},
		// Test 13: PATCH is not an edit method the API uses here.
		{http.MethodPatch, "/v1/projects/proj_1", ""},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(test.Method, test.Path, nil)
			if got := delegatedObject(r); got != test.WantObj {
				t.Errorf("delegatedObject(%s %s) = %q, want %q", test.Method, test.Path, got, test.WantObj)
			}
		})
	}
}

// TestAuthorizerManages checks that management authority follows an explicit manage grant to the user
// or a team, that a use grant does not confer it, and that an admin always has it.
func TestAuthorizerManages(t *testing.T) {
	t.Parallel()
	authz := testAuthz()
	tests := []struct {
		Name   string
		Actor  Actor
		Object string
		WantOK bool
	}{ // Test 0: An admin manages everything without a grant.
		{"admin", Actor{UserID: "user_9", Role: user.RoleAdmin}, "proj_1", true},
		// Test 1: A direct manage grant confers management.
		{"direct manage", Actor{UserID: "user_1", Role: user.RoleViewer}, "proj_1", true},
		// Test 2: A use grant does not confer management.
		{"use only", Actor{UserID: "user_2", Role: user.RoleOperator}, "proj_2", false},
		// Test 3: No grant, no management.
		{"no grant", Actor{UserID: "user_9", Role: user.RoleOperator}, "proj_1", false},
		// Test 4: A team manage grant reaches its members.
		{"team manage", Actor{UserID: "user_3", Role: user.RoleViewer}, "proj_3", true},
		// Test 5: A grant on another object does not carry over.
		{"wrong object", Actor{UserID: "user_1", Role: user.RoleViewer}, "proj_2", false},
		// Test 6: Credentials delegate the same way.
		{"credential manage", Actor{UserID: "user_1", Role: user.RoleViewer}, "cred_1", true},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, err := authz.manages(context.Background(), test.Actor, test.Object)
			if err != nil {
				t.Fatalf("manages() error = %v", err)
			}
			if got != test.WantOK {
				t.Errorf("manages(%s, %s) = %v, want %v", test.Actor.UserID, test.Object, got, test.WantOK)
			}
		})
	}

	// A nil grant store confers no management and is not an error.
	if ok, err := (&authorizer{}).manages(context.Background(), Actor{UserID: "user_1"}, "proj_1"); ok || err != nil {
		t.Errorf("nil-store manages() = %v, %v; want false, nil", ok, err)
	}
	// A store error surfaces rather than silently allowing.
	errBoom := errors.New("boom")
	bad := &authorizer{grants: &fakeGrants{err: errBoom}}
	if _, err := bad.manages(context.Background(), Actor{UserID: "user_1", Role: user.RoleViewer}, "proj_1"); !errors.Is(err, errBoom) {
		t.Errorf("store-error manages() err = %v, want boom", err)
	}
}

// TestReadFilter checks read-grant list filtering: without strict grants the role governs and all is
// visible, and under strict grants a non-admin sees only objects a grant lets them read, where use and
// manage confer read too, while an admin sees all. A grant-store error fails closed.
func TestReadFilter(t *testing.T) {
	t.Parallel()
	authz := &authorizer{
		strict: true,
		grants: &fakeGrants{byObject: map[string][]*grant.Grant{
			"proj_1": {{Subject: "user_1", Access: grant.AccessRead}},
			"proj_2": {{Subject: "user_2", Access: grant.AccessUse}},
			"proj_3": {{Subject: "team_x", Access: grant.AccessManage}},
		}},
		teams: &fakeTeams{byUser: map[string][]string{"user_3": {"team_x"}}},
	}
	all := []string{"proj_1", "proj_2", "proj_3"}
	withActor := func(a Actor) context.Context {
		return context.WithValue(context.Background(), actorKey{}, a)
	}
	kept := func(t *testing.T, ctx context.Context) []string {
		t.Helper()
		keep, err := authz.readFilter(ctx)
		if err != nil {
			t.Fatalf("readFilter() error = %v", err)
		}
		var out []string
		for _, id := range all {
			if keep(id, "") {
				out = append(out, id)
			}
		}
		return out
	}

	tests := []struct {
		Name  string
		Actor Actor
		Want  []string
	}{
		{"read grant sees only its object", Actor{UserID: "user_1", Role: user.RoleViewer}, []string{"proj_1"}},
		{"use grant confers read", Actor{UserID: "user_2", Role: user.RoleOperator}, []string{"proj_2"}},
		{"team manage grant confers read to a member", Actor{UserID: "user_3", Role: user.RoleViewer}, []string{"proj_3"}},
		{"no grant sees nothing", Actor{UserID: "user_9", Role: user.RoleViewer}, nil},
		{"admin sees all", Actor{UserID: "user_9", Role: user.RoleAdmin}, all},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.Want, kept(t, withActor(test.Actor)), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("kept mismatch (-want +got):\n%s", diff)
			}
		})
	}

	// Without strict grants the role governs, so everything is visible regardless of grants.
	nonStrict := &authorizer{grants: authz.grants, teams: authz.teams}
	keep, err := nonStrict.readFilter(withActor(Actor{UserID: "user_9", Role: user.RoleViewer}))
	if err != nil {
		t.Fatalf("readFilter() non-strict error = %v", err)
	}
	for _, id := range all {
		if !keep(id, "") {
			t.Errorf("non-strict dropped %s, want all kept", id)
		}
	}

	// A grant-store error surfaces so a list handler fails closed rather than leaking everything.
	bad := &authorizer{strict: true, grants: &fakeGrants{err: errors.New("boom")}}
	if _, err := bad.readFilter(withActor(Actor{UserID: "user_1", Role: user.RoleViewer})); err == nil {
		t.Error("readFilter with a failing store returned no error, want one")
	}
}

// TestListProjectsAppliesReadGrants proves the wiring: the list handler filters its results through the
// read grant, so under strict grants a non-admin viewer sees only granted projects and an admin sees all.
func TestListProjectsAppliesReadGrants(t *testing.T) {
	t.Parallel()
	store := &fakeProjects{list: []*project.Project{
		{ID: "proj_1", Name: "one"}, {ID: "proj_2", Name: "two"}, {ID: "proj_3", Name: "three"},
	}}
	authz := &authorizer{strict: true, grants: &fakeGrants{byObject: map[string][]*grant.Grant{
		"proj_2": {{Subject: "user_1", Access: grant.AccessRead}},
	}}}
	handler := listProjectsHandler(store, authz, zap.NewNop())

	ids := func(actor Actor) []string {
		req := httptest.NewRequest(http.MethodGet, "/v1/projects", nil).
			WithContext(context.WithValue(context.Background(), actorKey{}, actor))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Projects []*project.Project `json:"projects"`
			Count    int                `json:"count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := make([]string, len(resp.Projects))
		for i, p := range resp.Projects {
			out[i] = p.ID
		}
		if resp.Count != len(out) {
			t.Errorf("count = %d, want %d", resp.Count, len(out))
		}
		return out
	}

	if got := ids(Actor{UserID: "user_1", Role: user.RoleViewer}); cmp.Diff([]string{"proj_2"}, got) != "" {
		t.Errorf("viewer with a read grant saw %v, want [proj_2]", got)
	}
	if got := ids(Actor{UserID: "user_9", Role: user.RoleAdmin}); cmp.Diff([]string{"proj_1", "proj_2", "proj_3"}, got) != "" {
		t.Errorf("admin saw %v, want all three", got)
	}
}

// TestAuthGateAllowed checks the full decision: the global role gate first, then object-scoped manage
// delegation only for an exact grantable-object edit or delete. Delegation is additive, so it never
// allows anything the role gate already denies except a manage-granted mutation.
func TestAuthGateAllowed(t *testing.T) {
	t.Parallel()
	g := &authGate{authz: testAuthz()}
	tests := []struct {
		Name      string
		Actor     Actor
		Method    string
		Path      string
		WantAllow bool
	}{ // Test 0: An admin edits any project via the role.
		{"admin edit", Actor{UserID: "user_9", Role: user.RoleAdmin}, http.MethodPut, "/v1/projects/proj_1", true},
		// Test 1: An operator launches work via the role.
		{"operator run", Actor{UserID: "user_9", Role: user.RoleOperator}, http.MethodPost, "/v1/runs", true},
		// Test 2: A viewer reads via the role.
		{"viewer read", Actor{UserID: "user_9", Role: user.RoleViewer}, http.MethodGet, "/v1/projects/proj_1", true},
		// Test 3: An operator without a grant cannot edit a project.
		{"operator no grant", Actor{UserID: "user_9", Role: user.RoleOperator}, http.MethodPut, "/v1/projects/proj_1", false},
		// Test 4: A manage grant delegates the edit to a viewer.
		{"manage edit", Actor{UserID: "user_1", Role: user.RoleViewer}, http.MethodPut, "/v1/projects/proj_1", true},
		// Test 5: A manage grant delegates the delete too.
		{"manage delete", Actor{UserID: "user_1", Role: user.RoleViewer}, http.MethodDelete, "/v1/projects/proj_1", true},
		// Test 6: The manage grant is scoped to its object.
		{"manage other object", Actor{UserID: "user_1", Role: user.RoleViewer}, http.MethodPut, "/v1/projects/proj_2", false},
		// Test 7: A use grant does not delegate management.
		{"use no manage", Actor{UserID: "user_2", Role: user.RoleOperator}, http.MethodPut, "/v1/projects/proj_2", false},
		// Test 8: A team manage grant delegates to a member.
		{"team manage", Actor{UserID: "user_3", Role: user.RoleViewer}, http.MethodPut, "/v1/projects/proj_3", true},
		// Test 9: A non-grantable path is never delegated.
		{"non-grantable path", Actor{UserID: "user_1", Role: user.RoleViewer}, http.MethodPut, "/v1/inventory-sources/src_1", false},
		// Test 10: Creating an object is never delegated.
		{"create", Actor{UserID: "user_1", Role: user.RoleViewer}, http.MethodPost, "/v1/projects", false},
		// Test 11: A sub-resource is never delegated.
		{"subresource", Actor{UserID: "user_1", Role: user.RoleViewer}, http.MethodPut, "/v1/projects/proj_1/sub", false},
		// Test 12: Credentials delegate through the same path.
		{"credential delegate", Actor{UserID: "user_1", Role: user.RoleViewer}, http.MethodDelete, "/v1/credentials/cred_1", true},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(test.Method, test.Path, nil)
			got, err := g.allowed(context.Background(), test.Actor, r)
			if err != nil {
				t.Fatalf("allowed() error = %v", err)
			}
			if got != test.WantAllow {
				t.Errorf("allowed(%s, %s %s) = %v, want %v", test.Actor.UserID, test.Method, test.Path, got, test.WantAllow)
			}
		})
	}
}
