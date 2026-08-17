package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDistinctApproverIsEnforced covers separation of duties, the control a change-management review
// asks about first. An approval gate that the requester can release themselves is a formality: the
// person who wants the change made is the person who decides whether it is allowed. Nothing stopped
// that, while the compliance documentation described the control as if the product enforced it and
// the sample evidence pack advertised it.
//
// The requirement is recorded on the run when the rule holds it, not looked up when the decision is
// made, for the same reason the rule's name is: an admin editing or deleting the policy afterward must
// not be able to weaken a decision that is already pending, and an admin is exactly who this control
// constrains.
func TestDistinctApproverIsEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	policies := policy.NewMemStore()
	if err := policies.Save(ctx, &policy.Policy{
		ID: "pol_1", Name: "production apply", MaxDestroy: -1,
		RequireDistinctApprover: true,
	}); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	store := run.NewMemStore()
	d := New(store, okRunner(), zap.NewNop(), WithPolicies(policies))

	submit := func(actor string) *run.Run {
		r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
			run.WithActor(actor), run.WithActorType("session"))
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if r.Status != run.StatusPendingApproval {
			t.Fatalf("submitted status = %s, want pending_approval", r.Status)
		}
		if !r.RequireDistinctApprover {
			t.Fatalf("the run does not carry the distinct-approver requirement its rule set, so a "+
				"later policy edit decides whether the control applies (run %s)", r.ID)
		}
		return r
	}

	// Test 0: The requester cannot release their own held run.
	own := submit("casey")
	if _, err := d.Approve(ctx, own.ID, "casey", "session"); !errors.Is(err, ErrSelfApproval) {
		t.Errorf("self approval error = %v, want ErrSelfApproval: the requester released their own "+
			"change", err)
	}
	got, err := store.Get(ctx, own.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("status after refused self approval = %s, want pending_approval", got.Status)
	}

	// Test 1: Anyone else may. The control is separation of duties, not a second signature from the
	// same person under a different name.
	if _, err := d.Approve(ctx, own.ID, "dana", "session"); err != nil {
		t.Errorf("approval by another person = %v, want release", err)
	}

	// Test 2: Rejecting your own run is fine. Refusing a change you asked for needs no second person,
	// and blocking it would leave a requester unable to withdraw their own request.
	mine := submit("casey")
	if _, err := d.Reject(ctx, mine.ID, "changed my mind", "casey", "session"); err != nil {
		t.Errorf("self rejection = %v, want accepted", err)
	}

	// Test 3: A rule that does not ask for a distinct approver still lets one person do both, which is
	// the behavior a small team relies on.
	if err := policies.Save(ctx, &policy.Policy{
		ID: "pol_1", Name: "production apply", MaxDestroy: -1,
	}); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	relaxed := submit2(t, ctx, d, "casey")
	if _, err := d.Approve(ctx, relaxed.ID, "casey", "session"); err != nil {
		t.Errorf("self approval under a rule that permits it = %v, want release", err)
	}
}

// submit2 submits a held run without asserting the distinct-approver flag, for the relaxed case.
func submit2(t *testing.T, ctx context.Context, d *Dispatcher, actor string) *run.Run {
	t.Helper()
	r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
		run.WithActor(actor), run.WithActorType("session"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if r.RequireDistinctApprover {
		t.Fatalf("a rule that does not ask for a distinct approver marked the run as needing one")
	}
	return r
}

// TestEveryRunRecordsTheRulesInForce covers the evidence gap on the far side of the gate. A held run
// names the rule that held it, and its decision is signed, so a change somebody questioned has a strong
// record. A change that sailed through had none: nothing recorded what the rules were, so "no rule
// applied to this run" and "there were no rules" left the same trace, and a gate deleted an hour before
// a change was invisible afterward.
func TestEveryRunRecordsTheRulesInForce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	policies := policy.NewMemStore()
	store := run.NewMemStore()
	d := New(store, okRunner(), zap.NewNop(), WithPolicies(policies))

	submit := func() *run.Run {
		r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("echo hi"),
			run.WithActor("casey"), run.WithActorType("session"))
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		return r
	}

	// Test 0: With no rules at all, the run says so rather than saying nothing.
	bare := submit()
	if bare.PolicySet == nil || bare.PolicySet.Digest == "" {
		t.Fatalf("a run submitted under no rules recorded no rule set: %+v", bare.PolicySet)
	}
	if bare.PolicySet.Count != 0 {
		t.Errorf("count = %d, want 0", bare.PolicySet.Count)
	}

	// Test 1: With a rule in place, the run records the set including that rule by name.
	if err := policies.Save(ctx, &policy.Policy{
		ID: "pol_1", Name: "prod destroys need a person", Tool: run.ToolTerraform, MaxDestroy: 2,
	}); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	gated := submit()
	if gated.PolicySet == nil || gated.PolicySet.Count != 1 {
		t.Fatalf("run under one rule recorded %+v, want a set of one", gated.PolicySet)
	}
	if gated.PolicySet.Digest == bare.PolicySet.Digest {
		t.Error("adding a rule did not change the recorded set, so a gate added or removed between " +
			"two runs is invisible")
	}
	var named bool
	for _, r := range gated.PolicySet.Rules {
		if strings.Contains(r, "prod destroys need a person") {
			named = true
		}
	}
	if !named {
		t.Errorf("rules = %v, want the rule named so the evidence reads without the server",
			gated.PolicySet.Rules)
	}

	// Test 2: The set survives the store, since the evidence is read long after the submit.
	stored, err := store.Get(ctx, gated.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.PolicySet == nil || stored.PolicySet.Digest != gated.PolicySet.Digest {
		t.Errorf("stored set = %+v, want the digest recorded at submit", stored.PolicySet)
	}
}
