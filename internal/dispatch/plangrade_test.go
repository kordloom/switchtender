package dispatch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAFloorRuleDecidesADestroyingApplyFromItsPlan follows a terraform apply under a rule that
// holds irreversible changes for a person. Before its plan the apply grades costly, the rule cannot
// see what it destroys, and it used to run straight through. Now the apply is planned first, the
// apply the plan proposes carries the destroy count, and the rule decides on the grade that count
// gives: a plan that destroys something is held, and one that destroys nothing runs.
func TestAFloorRuleDecidesADestroyingApplyFromItsPlan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says what the plan found.
		Name string
		// Summary is the plan's change summary.
		Summary string
		// WantStatus is where the proposed apply must end up.
		WantStatus run.Status
		// WantHeldBy is the rule the apply must name as holding it, empty when it ran.
		WantHeldBy string
		// WantDestroys is the destroy count the apply must carry.
		WantDestroys int
		// WantApplies is how many real applies the runner may have executed.
		WantApplies int64
	}{{ // Test 0: The plan destroys three resources, so the apply is irreversible and held.
		Name: "destroys three", Summary: "Plan: 0 to add, 0 to change, 3 to destroy.\n",
		WantStatus: run.StatusPendingApproval, WantHeldBy: "irreversible changes need a person",
		WantDestroys: 3,
	}, { // Test 1: The plan destroys nothing, so the apply is below the floor and runs.
		Name: "destroys none", Summary: "Plan: 2 to add, 0 to change, 0 to destroy.\n",
		WantStatus: run.StatusSucceeded, WantApplies: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			policies := policy.NewMemStore()
			floor := policy.NewPolicy("irreversible changes need a person")
			floor.Tool, floor.Reversibility = run.ToolTerraform, run.Irreversible
			if err := policies.Save(ctx, floor); err != nil {
				t.Fatalf("policies.Save() error = %v", err)
			}
			store := run.NewMemStore()
			runner := &planGateRunner{summary: test.Summary}
			d := New(store, runner, nil, WithPolicies(policies), WithNoJanitor())
			defer d.Close()

			submitted, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
				run.WithCommand("infra/legacy"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if submitted.Status != run.StatusPending {
				t.Fatalf("the submission is %s, want pending: before its plan an apply grades "+
					"costly, below the floor", submitted.Status)
			}
			apply := proposalOf(t, store, submitted.ID, test.WantStatus)
			if apply.HeldByPolicy != test.WantHeldBy {
				t.Errorf("HeldByPolicy = %q, want %q", apply.HeldByPolicy, test.WantHeldBy)
			}
			if apply.PlanDestroys == nil || *apply.PlanDestroys != test.WantDestroys {
				t.Errorf("PlanDestroys = %v, want %d", apply.PlanDestroys, test.WantDestroys)
			}
			if got := runner.applies.Load(); got != test.WantApplies {
				t.Errorf("the runner applied %d times, want %d", got, test.WantApplies)
			}
		})
	}
}

// proposalOf waits for the apply proposed from planID to reach want, failing the test with the
// status it was last seen in when it never does.
func proposalOf(t *testing.T, store run.Store, planID string, want run.Status) *run.Run {
	t.Helper()
	seen := "no proposed apply"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := store.List(context.Background())
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		for _, r := range runs {
			if r.ProposedFrom != planID {
				continue
			}
			if r.Status == want {
				return r
			}
			seen = fmt.Sprintf("apply %s in status %q", r.ID, r.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the apply proposed from %s never reached %q, last seen: %s", planID, want, seen)
	return nil
}

// TestADestroyLimitWithAFloorIsWeighedOnThePlannedGrade pins how a rule that combines a destroy
// limit with a grade floor is weighed. The limit was checked against the plan run's own grade, which
// carries no destroy count, so the floor always read costly and the rule never held anything: a
// plan destroying three resources past a limit of two, irreversible by any reading, went straight
// to apply. The rules weigh the apply the plan proposes, with its count, which is what decides the
// release too.
func TestADestroyLimitWithAFloorIsWeighedOnThePlannedGrade(t *testing.T) {
	t.Parallel()
	policies := []*policy.Policy{{
		ID: "pol_1", Name: "big irreversible destroys", Tool: run.ToolTerraform, MaxDestroy: 2,
		Reversibility: run.Irreversible, RequireDistinctApprover: true,
	}}
	tests := []struct {
		// Name says what the plan found.
		Name string
		// Destroys is the plan's destroy count.
		Destroys int
		// WantHeldBy is the reason the apply must be held under, empty when it is not held.
		WantHeldBy string
	}{{ // Test 0: Three destroys pass the limit of two, and a destroy is irreversible.
		Name: "past the limit", Destroys: 3,
		WantHeldBy: "big irreversible destroys (plan destroys 3, limit 2)",
	}, { // Test 1: Two destroys are within the limit.
		Name: "within the limit", Destroys: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan := &run.Run{ID: "run_plan", Tool: run.ToolTerraform, Command: "infra/prod"}
			proposal := &run.Run{Status: run.StatusPending}
			run.ApplyOptions(proposal, applyOptions(plan, policies, test.Destroys, true))
			if proposal.HeldByPolicy != test.WantHeldBy {
				t.Errorf("HeldByPolicy = %q, want %q", proposal.HeldByPolicy, test.WantHeldBy)
			}
			wantHeld := test.WantHeldBy != ""
			if held := proposal.Status == run.StatusPendingApproval; held != wantHeld {
				t.Errorf("held = %v, want %v", held, wantHeld)
			}
			if proposal.RequireDistinctApprover != wantHeld {
				t.Errorf("RequireDistinctApprover = %v, want %v: the rule that held it demands a "+
					"second person", proposal.RequireDistinctApprover, wantHeld)
			}
			if plan.PlanDestroys != nil {
				t.Error("weighing the apply wrote the destroy count onto the plan run itself")
			}
		})
	}
}
