package server

import (
	"context"
	"errors"
	"testing"

	"github.com/dcadolph/switchtender/internal/grant"
	"github.com/dcadolph/switchtender/internal/org"
	"github.com/dcadolph/switchtender/internal/user"
)

// TestAuthorizeOrgGrant proves a grant to an organization reaches its members: a member of an org
// that holds a use grant on an object is authorized, and a non-member is denied.
func TestAuthorizeOrgGrant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	orgs := org.NewMemStore()
	if err := orgs.Save(ctx, &org.Org{ID: "org_1", Name: "acme"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := orgs.AddMember(ctx, "org_1", "user_5", org.RoleMember); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	authz := &authorizer{
		grants: &fakeGrants{byObject: map[string][]*grant.Grant{
			"proj_9": {{Subject: "org_1", Access: grant.AccessUse}},
		}},
		orgs: orgs,
	}
	withActor := func(id string) context.Context {
		return context.WithValue(context.Background(), actorKey{}, Actor{UserID: id, Role: user.RoleViewer})
	}

	// The member of the granted org may use the object.
	if err := authz.authorize(withActor("user_5"), "proj_9", grant.AccessUse); err != nil {
		t.Errorf("org member denied use: %v", err)
	}
	// A non-member is denied.
	if err := authz.authorize(withActor("user_6"), "proj_9", grant.AccessUse); !errors.Is(err, errForbiddenGrant) {
		t.Errorf("non-member error = %v, want errForbiddenGrant", err)
	}
}
