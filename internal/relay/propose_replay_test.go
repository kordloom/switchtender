package relay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestProposeApplyRefusesAnythingButALivePlan covers the one endpoint that lets a worker cause a run to
// exist.
//
// A worker has no way to create a run, deliberately, except this: when a plan-content policy gates a
// terraform apply, the worker plans first and asks the control node to create the apply its plan gated.
// The endpoint checked only that the run was in the worker's queue and that the presented lease matched.
// It never checked that the run was a live plan. A worker's lease secret is never cleared when a run
// finishes, so the capability lasted forever, and every field of the apply is copied from the named run
// with the dry-run flag forced off.
//
// So a worker that had once legitimately claimed anything could, at any later time, post to this
// endpoint with the lease it kept and have the control node create and queue a real execution of it: a
// check-mode preview becomes a live change against the same hosts with the same credentials, and the
// call can be repeated to mint as many as it likes. The only trace is one relay audit line.
func TestProposeApplyRefusesAnythingButALivePlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		// Name says what the worker is trying to replay.
		Name string
		// Run is the state the control node holds for it.
		Run *run.Run
	}{{ // Test 0: A finished check-mode Ansible run. Nothing about it is a plan.
		Name: "a finished dry run",
		Run: &run.Run{
			Tool: run.ToolAnsible, Playbook: "site.yml", Inventory: "prod",
			Status: run.StatusSucceeded, DryRun: true,
		},
	}, { // Test 1: A finished terraform plan. It was a plan, but it is over.
		Name: "a finished plan",
		Run: &run.Run{
			Tool: run.ToolTerraform, Playbook: "infra/", Status: run.StatusSucceeded, DryRun: true,
		},
	}, { // Test 2: A finished real apply, which would be replayed as another real apply.
		Name: "a finished apply",
		Run: &run.Run{
			Tool: run.ToolTerraform, Playbook: "infra/", Status: run.StatusSucceeded,
		},
	}, { // Test 3: A proposal already awaiting a person. Proposing from it would route around them.
		Name: "a run held for approval",
		Run: &run.Run{
			Tool: run.ToolTerraform, Playbook: "infra/", Status: run.StatusPendingApproval,
		},
	}}

	for testNum, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			backing := run.NewMemStore()
			ts := httptest.NewServer(
				relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
			t.Cleanup(ts.Close)

			claimed := time.Now().Add(-2 * time.Hour)
			held := test.Run
			held.ID = "run_held"
			held.CreatedAt = claimed
			held.ClaimedBy = "worker-a"
			held.ClaimedAt = &claimed
			held.ClaimSecret = "secret-a"
			if held.Status.Terminal() {
				ended := claimed.Add(time.Minute)
				held.EndedAt = &ended
			}
			if err := backing.Save(ctx, held); err != nil {
				t.Fatalf("seed Save() error = %v", err)
			}

			code := postWithLease(t, ts.URL, "/relay/v1/runs/run_held/propose-apply", "secret-a",
				[]byte(`{"destroys":0,"read":true}`))
			if code < 400 {
				t.Errorf("test %d: proposing an apply from %s answered %d, want a refusal",
					testNum, test.Name, code)
			}

			// The effect is what matters: no run may have come into existence.
			list, err := backing.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			for _, got := range list {
				if got.ID == held.ID {
					continue
				}
				t.Errorf("test %d: a worker minted run %q (%s %q, dry_run=%v) from %s",
					testNum, got.ID, got.Tool, got.Playbook, got.DryRun, test.Name)
			}
		})
	}
}

// TestProposeApplyDemandsTheClaimCapability covers the fallback that let a caller past the lease check
// entirely. The report endpoints accept a run whose stored secret is empty, because a run claimed before
// the capability existed has none and its worker still has to be able to finish it. This endpoint is the
// one that makes a run exist, so the same leniency meant any worker presenting the shared token could
// name such a run and have a real apply built from it, holding no capability at all.
func TestProposeApplyDemandsTheClaimCapability(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)

	claimed := time.Now()
	// A live plan with no per-claim secret, the shape a run claimed before the capability existed has.
	plan := &run.Run{
		ID: "run_old", Tool: run.ToolTerraform, Playbook: "infra/", Status: run.StatusRunning,
		DryRun: true, CreatedAt: claimed, ClaimedBy: "worker-a", ClaimedAt: &claimed,
	}
	if err := backing.Save(ctx, plan); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	code := postWithLease(t, ts.URL, "/relay/v1/runs/run_old/propose-apply", "",
		[]byte(`{"destroys":0,"read":true}`))
	if code != http.StatusForbidden {
		t.Errorf("proposing an apply while holding no capability = %d, want %d", code, http.StatusForbidden)
	}
	list, err := backing.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 {
		t.Errorf("a caller holding no capability minted an apply: %d runs stored", len(list))
	}
}

// TestProposeApplyStillWorksForTheLivePlanItIsFor is the feature the guard must not break. A worker
// executing a real, gated terraform plan has to be able to ask for its apply, or a plan-content policy
// can never complete on a relay deployment.
func TestProposeApplyStillWorksForTheLivePlanItIsFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)

	claimed := time.Now()
	plan := &run.Run{
		ID: "run_plan", Tool: run.ToolTerraform, Playbook: "infra/", Status: run.StatusRunning,
		DryRun: true, CreatedAt: claimed, ClaimedBy: "worker-a", ClaimedAt: &claimed,
		ClaimSecret: "secret-a", Actor: "casey", ActorType: "session",
	}
	if err := backing.Save(ctx, plan); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	code := postWithLease(t, ts.URL, "/relay/v1/runs/run_plan/propose-apply", "secret-a",
		[]byte(`{"destroys":3,"read":true}`))
	if code != http.StatusCreated {
		t.Fatalf("proposing the apply for a live plan = %d, want %d", code, http.StatusCreated)
	}

	list, err := backing.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var proposal *run.Run
	for _, got := range list {
		if got.ID != plan.ID {
			proposal = got
		}
	}
	if proposal == nil {
		t.Fatal("the live plan's apply was not created, so a plan-content policy can never complete")
	}
	if proposal.DryRun {
		t.Error("the proposed apply is still a dry run, so it would change nothing")
	}

	// Asking twice yields the one apply, not two. A worker whose response was lost retries, which is
	// legitimate and must not mint a second real change, so the call is idempotent rather than refused.
	second := postWithLease(t, ts.URL, "/relay/v1/runs/run_plan/propose-apply", "secret-a",
		[]byte(`{"destroys":3,"read":true}`))
	if second != http.StatusCreated {
		t.Errorf("a worker retrying its proposal got %d, so a lost response would leave the plan "+
			"gated with no apply", second)
	}
	after, err := backing.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	applies := 0
	for _, got := range after {
		if got.ID != plan.ID {
			applies++
		}
	}
	if applies != 1 {
		t.Errorf("the plan has %d applies, want exactly 1: a worker could mint as many real applies "+
			"as it liked by repeating the call", applies)
	}
}
