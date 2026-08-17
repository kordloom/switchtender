package relay

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// gatedPlan stands up a control node holding a live terraform plan a worker has claimed, under the
// given policies, and returns a client and the store.
func gatedPlan(t *testing.T, policies ...*policy.Policy) (*Client, run.Store) {
	t.Helper()
	ctx := context.Background()
	store := run.NewMemStore()
	rules := policy.NewMemStore()
	for _, p := range policies {
		if err := rules.Save(ctx, p); err != nil {
			t.Fatalf("Save policy: %v", err)
		}
	}
	now := time.Now()
	plan := &run.Run{
		ID: "run_plan", Status: run.StatusPending, CreatedAt: now,
		Queue: "default", Tool: run.ToolTerraform, Command: "infra/prod",
		DryRun: true, Actor: "casey", ActorType: "session", OrgID: "org_1",
	}
	if err := store.Save(ctx, plan); err != nil {
		t.Fatalf("Save run: %v", err)
	}
	srv := httptest.NewServer(NewHandler(store, SinglePool("ymt_worker"), zap.NewNop(), rules, nil))
	t.Cleanup(srv.Close)
	tr := NewHTTPTransport(srv.URL, "ymt_worker", nil)
	if _, err := tr.Claim(ctx, "worker-1", []string{"default"}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return NewClient(tr), store
}

// applyOf returns the run the plan proposed, or nil when none was created.
func applyOf(t *testing.T, store run.Store) *run.Run {
	t.Helper()
	list, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, got := range list {
		if got.ID != "run_plan" {
			return got
		}
	}
	return nil
}

// TestAProposedApplyFacesTheSameRulesAsAnySubmission covers a hole that opened only on the deployment
// shape the product recommends, in the feature the product is most careful about.
//
// Every other way to start a run passes the same gate: a deny rule refuses it, an approval rule holds
// it, and the rule set in force is recorded on the run so the evidence can later show what did and did
// not stop it. The apply a relay worker's plan proposes was written straight to the store instead, so
// none of that ran. The identical install refused the apply when the control node happened to claim the
// plan and executed it when a worker did, and the run that actually destroys infrastructure carried no
// record of the rules it was submitted under, which reads afterward as a run from before rules were
// recorded at all.
func TestAProposedApplyFacesTheSameRulesAsAnySubmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Test 0: A deny rule refuses the apply, and no run is created.
	t.Run("a deny rule refuses it", func(t *testing.T) {
		t.Parallel()
		client, store := gatedPlan(t, &policy.Policy{
			ID: "pol_deny", Name: "no terraform from the relay", Tool: run.ToolTerraform,
			Effect: policy.EffectDeny, MaxDestroy: -1,
		})
		_, err := client.ProposeApply(ctx, "run_plan", 0, true)
		if err == nil {
			t.Fatal("a denied apply was created anyway")
		}
		// The reason has to reach the worker, which records it on the plan run: an operator reading a
		// failed plan must be able to see that a rule refused the apply, not that something broke.
		if !strings.Contains(err.Error(), "no terraform from the relay") {
			t.Errorf("the refusal does not name the rule: %v", err)
		}
		if got := applyOf(t, store); got != nil {
			t.Errorf("a deny rule was skipped and the apply exists: %+v", got.Status)
		}
	})

	// Test 1: A blanket approval rule holds the apply, even with a destroy count no threshold objects
	// to, and the apply says which rule held it.
	t.Run("an approval rule holds it", func(t *testing.T) {
		t.Parallel()
		client, store := gatedPlan(t, &policy.Policy{
			ID: "pol_hold", Name: "terraform needs a person", Tool: run.ToolTerraform,
			MaxDestroy: -1,
		})
		proposal, err := client.ProposeApply(ctx, "run_plan", 0, true)
		if err != nil {
			t.Fatalf("ProposeApply: %v", err)
		}
		if proposal.Status != run.StatusPendingApproval {
			t.Errorf("the proposed apply is %q, so an approval rule that refuses to let terraform run "+
				"unattended was skipped and the change is queued to execute", proposal.Status)
		}
		if !strings.Contains(proposal.HeldByPolicy, "terraform needs a person") {
			t.Errorf("held by %q, want the rule that held it named", proposal.HeldByPolicy)
		}
		if got := applyOf(t, store); got == nil || got.Status != run.StatusPendingApproval {
			t.Errorf("the stored apply does not match what was returned: %+v", got)
		}
	})

	// Test 2: A rule demanding a second person carries onto the apply, or the approval it is held for
	// could be given by the person who asked for it.
	t.Run("a distinct approver is carried", func(t *testing.T) {
		t.Parallel()
		client, _ := gatedPlan(t, &policy.Policy{
			ID: "pol_two", Name: "two people for terraform", Tool: run.ToolTerraform,
			MaxDestroy: -1, RequireDistinctApprover: true,
		})
		proposal, err := client.ProposeApply(ctx, "run_plan", 0, true)
		if err != nil {
			t.Fatalf("ProposeApply: %v", err)
		}
		if !proposal.RequireDistinctApprover {
			t.Error("the apply does not require a distinct approver, so the person who asked for the " +
				"change could release it themselves")
		}
	})

	// Test 3: The rule set in force is recorded on the apply. Without it the evidence cannot show that
	// nothing should have stopped this run, only that nothing did.
	t.Run("the rule set is recorded", func(t *testing.T) {
		t.Parallel()
		client, _ := gatedPlan(t, &policy.Policy{
			ID: "pol_other", Name: "ansible needs a person", Tool: run.ToolAnsible, MaxDestroy: -1,
		})
		proposal, err := client.ProposeApply(ctx, "run_plan", 0, true)
		if err != nil {
			t.Fatalf("ProposeApply: %v", err)
		}
		if proposal.PolicySet == nil || proposal.PolicySet.Digest == "" {
			t.Fatal("the apply records no rule set, so its evidence reads as a run submitted before " +
				"rules were captured")
		}
		if proposal.PolicySet.Count != 1 {
			t.Errorf("the apply records %d rules in force, want 1", proposal.PolicySet.Count)
		}
		// A rule that does not match still has to be in the recorded set, since the point is what was
		// in force, not what fired.
		if proposal.Status != run.StatusPending {
			t.Errorf("an ansible rule held a terraform apply: %q", proposal.Status)
		}
	})
}
