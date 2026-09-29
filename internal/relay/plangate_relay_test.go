package relay_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAWorkerPlanGatesOverTheRelay drives the plan gate across the worker boundary. A worker reads
// the rules from the control node, plans the apply it claimed, and asks the control node to build
// the apply, because a worker cannot create a run. Every piece of that was tested alone while the
// pieces disagreed about what a gated run is: the worker plans the real apply it was given, and the
// control node would only propose from a dry run, so every worker-run plan gate failed its plan and
// proposed nothing. Only a test that runs the whole path can see the two halves agree.
func TestAWorkerPlanGatesOverTheRelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Plan is the change summary the worker's plan prints.
		Plan string
		// Rules are the policies in force, the destroy limit when nil.
		Rules func(*testing.T) policy.Store
		// WantStatus is where the proposed apply must end up.
		WantStatus run.Status
		// WantHeldBy is the reason a held apply must carry, empty for one that ran.
		WantHeldBy string
	}{{ // Test 0: A plan that destroys past the limit proposes an apply held for a person.
		Plan: "Plan: 0 to add, 0 to change, 3 to destroy.", WantStatus: run.StatusPendingApproval,
		WantHeldBy: "terraform destroys need a person (plan destroys 3, limit 0)",
	}, { // Test 1: A plan that destroys nothing proposes an apply the worker then runs.
		Plan: "Plan: 1 to add, 0 to change, 0 to destroy.", WantStatus: run.StatusSucceeded,
	}, { // Test 2: A rule on irreversible changes sends the apply to be planned, and the control node
		// grades the apply from the count the worker's plan reported, so it holds.
		Plan: "Plan: 0 to add, 0 to change, 3 to destroy.", Rules: irreversibleRules,
		WantStatus: run.StatusPendingApproval, WantHeldBy: "irreversible changes need a person",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			backing := run.NewMemStore()
			rules := planGateRules
			if test.Rules != nil {
				rules = test.Rules
			}
			ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil,
				rules(t), nil))
			t.Cleanup(ts.Close)

			transport := relay.NewHTTPTransport(ts.URL, testWorkerToken, nil)
			planner := roundhouse.RunnerFunc(
				func(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
					_, err := io.WriteString(out, test.Plan+"\n")
					return roundhouse.Result{ExitCode: 0}, err
				})
			worker := dispatch.New(relay.NewClient(transport), planner, zaptest.NewLogger(t),
				dispatch.WithPolicies(relay.NewPolicyClient(transport)), dispatch.WithWorkers(1),
				dispatch.WithNoJanitor(), dispatch.WithOwner("worker-a"),
				dispatch.WithClaimInterval(10*time.Millisecond))
			t.Cleanup(worker.Close)

			id := fmt.Sprintf("run_worker_plan_%d", testNum)
			if err := backing.Save(ctx, &run.Run{
				ID: id, Tool: run.ToolTerraform, Command: "infra/prod", Status: run.StatusPending,
				Actor: "casey", ActorType: "session", CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("seed Save() error = %v", err)
			}

			plan := waitTerminal(t, backing, id)
			if plan.Status != run.StatusSucceeded {
				t.Fatalf("plan status = %q (error %q), want succeeded: the worker planned and the "+
					"control node refused to build the apply", plan.Status, plan.Error)
			}
			apply := waitProposal(t, backing, id, test.WantStatus)
			if apply.DryRun {
				t.Error("the proposed apply is a dry run, so approving it changes nothing")
			}
			if apply.HeldByPolicy != test.WantHeldBy {
				t.Errorf("HeldByPolicy = %q, want %q", apply.HeldByPolicy, test.WantHeldBy)
			}
		})
	}
}

// irreversibleRules returns a policy store holding irreversible terraform changes for a person, a
// rule with a grade floor rather than a destroy limit.
func irreversibleRules(t *testing.T) policy.Store {
	t.Helper()
	rules := policy.NewMemStore()
	floor := policy.NewPolicy("irreversible changes need a person")
	floor.Tool, floor.Reversibility = run.ToolTerraform, run.Irreversible
	if err := rules.Save(context.Background(), floor); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	return rules
}

// waitProposal polls the control node's store until the apply proposed from planID reaches want, and
// fails the test naming the status it last saw when it never does.
func waitProposal(t *testing.T, store run.Store, planID string, want run.Status) *run.Run {
	t.Helper()
	seen := "no proposed apply"
	deadline := time.Now().Add(15 * time.Second)
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
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the apply proposed from %s never reached %q, last seen: %s", planID, want, seen)
	return nil
}
