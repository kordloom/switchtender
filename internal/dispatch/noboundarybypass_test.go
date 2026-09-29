package dispatch

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestNoExecutionPathGoesAroundTheBoundary is the test for the product's central promise.
//
// The homepage says nothing runs until it clears one path: request, policy, approval. Every claim
// the product makes rests on that being true of every way a run can be born, not just the obvious
// one. An origin that skips the gate is not a missing feature, it is the promise being false, and
// it would be false silently: the run executes, the trail records it, and nothing anywhere says a
// rule was passed by.
//
// The concern is not hypothetical. Four policy evaluations in this package were handed an ungraded
// run and found only by reading the source, and the apply proposal path consulted no policy at all
// until it was noticed. Those were found by looking rather than by failing, which is the argument
// for this test existing.
//
// Each entry point is driven against a deny-everything rule and must refuse. Some consult the gate
// themselves and some reach it by delegating to one that does; this does not care which, only that
// nothing gets through.
func TestNoExecutionPathGoesAroundTheBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	denyAll := policy.NewMemStore()
	if err := denyAll.Save(ctx, &policy.Policy{
		ID: "pol_deny", Name: "refuse everything", Effect: policy.EffectDeny,
		MaxDestroy: policy.DisabledMaxDestroy,
	}); err != nil {
		t.Fatalf("Save policy: %v", err)
	}

	// A store already holding a finished split and a finished run, so the paths that derive a new
	// run from an old one have something to derive from. They are seeded directly rather than
	// submitted, because submitting them would itself be refused.
	store := run.NewMemStore()
	seedDerivable(t, ctx, store)

	// A runner that can enumerate hosts, because a split lists them before it reaches the gate.
	// Nothing here should ever execute: every case below is meant to be refused first.
	runner := &listingRunner{
		hosts: []string{"web01", "web02", "web03"},
		run: func(context.Context) (roundhouse.Result, error) {
			t.Error("a run executed despite a deny-everything rule")
			return roundhouse.Result{ExitCode: 0}, nil
		},
	}
	d := New(store, runner, zap.NewNop(), WithPolicies(denyAll))
	t.Cleanup(d.Close)

	origins := []struct {
		Name string
		Fire func() error
	}{{
		Name: "a plain submission",
		Fire: func() error {
			_, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"))
			return err
		},
	}, {
		Name: "a split",
		Fire: func() error {
			_, err := d.SubmitSplit(ctx, "site.yml", "hosts.ini", 3)
			return err
		},
	}, {
		Name: "a pipeline",
		Fire: func() error {
			_, err := d.SubmitPipeline(ctx, "release", "hosts.ini", []run.PipelineStep{
				{Name: "deploy", Playbook: "site.yml"},
			})
			return err
		},
	}, {
		// Inherits the parent's entire execution spec, which is exactly why it has to face the
		// rules again: otherwise retrying is a way to run a spec an approver would have refused.
		Name: "a retry of a failed split",
		Fire: func() error {
			_, err := d.RetryFailedShards(ctx, "run_split")
			return err
		},
	}, {
		// Reaches the gate by delegating to Submit rather than consulting it directly. That is
		// fine and is the reason this test drives the entry point instead of reading the code.
		Name: "a relaunch of failed hosts",
		Fire: func() error {
			_, err := d.RelaunchFailedHosts(ctx, "run_failed", run.WithActor("ops"), run.WithActorType("human"))
			return err
		},
	}}

	for _, o := range origins {
		t.Run(o.Name, func(t *testing.T) {
			err := o.Fire()
			if err == nil {
				t.Fatalf("%s was allowed past a deny-everything rule. There is an execution path "+
					"around the boundary, which makes the product's central claim false", o.Name)
			}
			// Refused by the gate, not by something incidental. An unrelated error would make this
			// test pass while the run still reached an executor by another route.
			if !errors.Is(err, ErrPolicyDenied) {
				t.Errorf("%s failed with %v, want ErrPolicyDenied: it has to be the rule that "+
					"stopped it, not an accident of setup", o.Name, err)
			}
		})
	}

	// The apply proposal is the sixth origin and is not a Dispatcher method, so it is driven
	// directly. It wrote straight to the store and consulted no policy at all until that was
	// noticed, which is precisely the shape of bug this test exists to catch.
	t.Run("a terraform apply proposed from a plan", func(t *testing.T) {
		plan := &run.Run{ID: "run_plan", Tool: run.ToolTerraform, Command: "/srv/tf",
			Status: run.StatusSucceeded, DryRun: true}
		if err := store.Save(ctx, plan); err != nil {
			t.Fatalf("save plan: %v", err)
		}
		rules, err := denyAll.List(ctx)
		if err != nil {
			t.Fatalf("list policies: %v", err)
		}
		if _, err := ProposeApplyFor(ctx, store, rules, plan, 3, false); !errors.Is(err, ErrPolicyDenied) {
			t.Errorf("an apply proposed from a plan failed with %v, want ErrPolicyDenied", err)
		}
	})
}

// seedDerivable writes a finished split with a failed shard and a finished run with a failed host,
// so the derive-from-an-existing-run paths have real parents. Written to the store rather than
// submitted, since a deny-everything rule would refuse the submission.
func seedDerivable(t *testing.T, ctx context.Context, store run.Store) {
	t.Helper()
	count := 1
	parent := &run.Run{ID: "run_split", Playbook: "site.yml", Inventory: "hosts.ini",
		Kind: run.KindSplit, Status: run.StatusFailed, ShardCount: &count}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("save split parent: %v", err)
	}
	idx := 0
	shard := &run.Run{ID: "run_shard", Playbook: "site.yml", Inventory: "hosts.ini",
		Status: run.StatusFailed, ParentID: &parent.ID, ShardIndex: &idx, Limit: "web01"}
	if err := store.Save(ctx, shard); err != nil {
		t.Fatalf("save shard: %v", err)
	}

	failed := &run.Run{ID: "run_failed", Playbook: "site.yml", Inventory: "hosts.ini",
		Status: run.StatusRunning}
	if err := store.Save(ctx, failed); err != nil {
		t.Fatalf("save failed run: %v", err)
	}
	if err := store.SaveHostSummary(ctx, failed.ID, []run.HostSummary{
		{Host: "web01", Failures: 1, Worst: "failed"},
	}); err != nil {
		t.Fatalf("save host summary: %v", err)
	}
	// Terminal only after its results are in. SaveHostSummary fences a terminal run, so writing the
	// summary second would be silently dropped and the relaunch would find no failed host.
	failed.Status = run.StatusFailed
	if err := store.Save(ctx, failed); err != nil {
		t.Fatalf("finalize failed run: %v", err)
	}
}
