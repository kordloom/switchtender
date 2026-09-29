package backup

import (
	"context"
	"testing"

	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestCheckRefusesAPolicyTheAPIWouldRefuse asserts a restore refuses an approval policy that would
// be rejected if it were written through any other path.
//
// This is the failure this package names as the worst one it has: a restore that reports a healthy
// count of policies while the gates are gone. policy.Validate exists for exactly this and says a
// rule with a typo must be refused where it is written "rather than silently matching nothing".
// Both API handlers call it and the policy file loader calls it. A restore does not, and the check
// that is documented as applying "the validation every API path applies" looks at users and grants
// only.
//
// The consequence is not a cosmetic one. A policy naming an actor kind this build does not know
// matches no run at all, so the rule is inert: the summary counts the policy as restored, the
// operator reads a healthy recovery, and every change the rule was written to hold now runs
// unapproved.
func TestCheckRefusesAPolicyTheAPIWouldRefuse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := freshStores()
	// The API refuses this policy: the actor kind is in the wrong case.
	bad := &policy.Policy{
		ID: "pol_1", Name: "hold agent terraform", Tool: "terraform",
		ActorKind: "Agent", MaxDestroy: policy.DisabledMaxDestroy,
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("the fixture policy is valid, so this proves nothing")
	}

	p := &payload{Policies: []*policy.Policy{bad}}
	if err := check(ctx, stores, p); err != nil {
		// The refusal is the assertion. Everything below is the harm this test exists to name, and
		// it is only reachable on a build that accepted the policy.
		return
	}
	t.Error("a policy the API would refuse was accepted for restore")

	// What the accepted policy does once it is in: nothing.
	if _, err := apply(ctx, stores, p); err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	pols, err := stores.Policies.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	gated := &run.Run{
		ID: "run_1", Tool: "terraform", Command: "terraform destroy", ActorType: "agent",
	}
	if !policy.Requires(pols, gated) {
		t.Error("the restored gate holds nothing, so the change it was written to stop runs " +
			"unapproved while the restore reports the policy came back")
	}
}

// TestCheckRefusesAnOrgRoleTheAPIWouldRefuse asserts a restore refuses an organization membership
// whose role is not a role.
//
// The organization handler validates the role with org.ValidRole before adding a member. A restore
// writes the membership straight through, so a file can put a role in the database that no path
// would ever have accepted. Access does not escalate past member level, because the authorizer
// treats anything that is not admin as a plain member, but the value is stored, listed back, and
// shown as that account's role in the organization.
func TestCheckRefusesAnOrgRoleTheAPIWouldRefuse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := freshStores()
	if err := stores.Orgs.Save(ctx, &org.Org{ID: "org_1", Name: "acme"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	p := &payload{
		Orgs:       []*org.Org{{ID: "org_1", Name: "acme"}},
		OrgMembers: []orgMemberDTO{{OrgID: "org_1", UserID: "user_1", Role: org.Role("owner")}},
	}
	if err := check(ctx, stores, p); err != nil {
		// The refusal is the assertion. What follows reads back what apply stored, which is only
		// reachable on a build that accepted the membership.
		return
	}
	t.Error("an organization role the API would refuse was accepted for restore")

	if _, err := apply(ctx, stores, p); err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	members, err := stores.Orgs.Members(ctx, "org_1")
	if err != nil {
		t.Fatalf("Members() error = %v", err)
	}
	for _, m := range members {
		if !org.ValidRole(m.Role) {
			t.Errorf("%s holds organization role %q, which is not a role", m.UserID, m.Role)
		}
	}
}

// TestCheckRefusesTwoAccountsSharingOneNameInTheSameFile asserts a restore notices when the file
// itself holds two accounts under one username.
//
// The collision check compares each account in the file against the target install and never
// against the rest of the file. A username belongs to one account and both SQL backends enforce
// that with a unique index, so a file carrying a duplicate passes every check and then fails on the
// second account's insert, which is precisely the partway failure the check was added to prevent:
// organizations, teams, and the first accounts are already committed by then.
func TestCheckRefusesTwoAccountsSharingOneNameInTheSameFile(t *testing.T) {
	t.Parallel()
	err := check(context.Background(), freshStores(), &payload{Users: []userDTO{
		{User: user.User{ID: "user_a", Username: "bob", Role: user.RoleAdmin}},
		{User: user.User{ID: "user_b", Username: "bob", Role: user.RoleViewer}},
	}})
	if err == nil {
		t.Error("a file holding one username under two ids was accepted, so the restore fails " +
			"at the unique index after the identity tables are written")
	}
}
