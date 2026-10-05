package relay_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/plantest"
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
			// The control node seals the plan file the worker hands it and, when the apply is claimed,
			// opens it for the claiming pool, which registered a delivery key for exactly that.
			control := dispatch.New(backing, roundhouse.RunnerFunc(nil), zaptest.NewLogger(t),
				dispatch.WithNoJanitor(),
				dispatch.WithClaimGate(func() error { return errors.New("closed for this test") }))
			t.Cleanup(control.Close)
			poolKey, err := handoff.GenerateKey()
			if err != nil {
				t.Fatalf("GenerateKey() error = %v", err)
			}
			pools, err := relay.LoadPools(writePoolFile(t, "workers:\n  - name: plans\n    "+
				"token_sha256: "+relay.HashToken(testWorkerToken)+"\n    queues: [plans]\n    "+
				"delivery_key: "+poolKey.Public().String()+"\n"))
			if err != nil {
				t.Fatalf("LoadPools() error = %v", err)
			}
			// Delivery records which worker received which secrets, so it needs the audit trail.
			ts := httptest.NewServer(relay.NewHandler(backing, pools, nil, rules(t), audit.NewMemStore(),
				relay.WithSecretOpener(control), relay.WithPlanSealer(control)))
			t.Cleanup(ts.Close)
			ring, err := handoff.NewKeyRing(poolKey)
			if err != nil {
				t.Fatalf("NewKeyRing() error = %v", err)
			}

			transport := relay.NewHTTPTransport(ts.URL, testWorkerToken, nil)
			var applied atomic.Value
			planner := roundhouse.RunnerFunc(
				func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
					_, err := io.WriteString(out, test.Plan+"\n")
					if spec.DryRun {
						return plantest.Result(test.Plan), err
					}
					// The apply carries out the plan file the plan saved, delivered to this worker.
					plan, rerr := os.ReadFile(spec.PlanFile)
					if rerr != nil {
						return roundhouse.Result{ExitCode: 1}, rerr
					}
					applied.Store(string(plan))
					return roundhouse.Result{ExitCode: 0}, err
				})
			worker := dispatch.New(relay.NewClient(transport), planner, zaptest.NewLogger(t),
				dispatch.WithPolicies(relay.NewPolicyClient(transport)), dispatch.WithWorkers(1),
				dispatch.WithNoJanitor(), dispatch.WithOwner("worker-a"),
				dispatch.WithClaimInterval(10*time.Millisecond), dispatch.WithQueues([]string{"plans"}),
				dispatch.WithSecretDelivery(relay.NewReceiver(transport, ring)),
				dispatch.WithRunFilesRoot(t.TempDir()))
			t.Cleanup(worker.Close)

			id := fmt.Sprintf("run_worker_plan_%d", testNum)
			if err := backing.Save(ctx, &run.Run{
				ID: id, Tool: run.ToolTerraform, Command: "infra/prod", Status: run.StatusPending,
				Actor: "operator-1", ActorType: "session", CreatedAt: time.Now(), Queue: "plans",
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
			if apply.PlanSHA256 == "" {
				t.Error("the proposed apply binds no plan file, so it would plan again when it runs")
			}
			if test.WantStatus == run.StatusSucceeded {
				if got, _ := applied.Load().(string); got != string(plantest.File) {
					t.Errorf("the apply carried out plan file %q, want the one the plan saved", got)
				}
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
			seen = fmt.Sprintf("apply %s in status %q (%s)", r.ID, r.Status, r.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the apply proposed from %s never reached %q, last seen: %s", planID, want, seen)
	return nil
}
