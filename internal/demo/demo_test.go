package demo

import (
	"context"
	"fmt"
	"github.com/kordloom/switchtender/internal/audit"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// fakeSubmitter applies the submitted options to a run and stores it, standing in for the dispatcher
// so the governance seed is exercised without executing anything.
type fakeSubmitter struct {
	// store holds every run the seeder submitted.
	store run.Store
}

// Submit records a single run built from the submitted options.
func (f *fakeSubmitter) Submit(ctx context.Context, playbook, inventory string,
	opts ...run.SubmitOption) (*run.Run, error) {
	r := &run.Run{ID: run.NewID(), Playbook: playbook, Inventory: inventory, Status: run.StatusPending}
	for _, o := range opts {
		o(r)
	}
	return r, f.store.Save(ctx, r)
}

// SubmitSplit is unused by the governance seed and records nothing.
func (f *fakeSubmitter) SubmitSplit(ctx context.Context, playbook, inventory string, shards int,
	opts ...run.SubmitOption) (*run.Run, error) {
	return f.Submit(ctx, playbook, inventory, opts...)
}

// SubmitPipeline is unused by the governance seed and records nothing.
func (f *fakeSubmitter) SubmitPipeline(ctx context.Context, name, inventory string,
	steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error) {
	return f.Submit(ctx, name, inventory, opts...)
}

// fakeApprover enforces the separation-of-duties rule the real dispatcher enforces, so a seed that
// tried to release a run with the account that asked for it fails here rather than passing quietly.
type fakeApprover struct {
	// store holds the run being decided on.
	store run.Store
	// approvedBy is who released the run.
	approvedBy string
}

// Approve releases a held run, refusing a decision by the account that requested it.
func (f *fakeApprover) Approve(ctx context.Context, id, by, byType string) (*run.Run, error) {
	r, err := f.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Status != run.StatusPendingApproval {
		return nil, fmt.Errorf("run %s is %s, not held for approval", id, r.Status)
	}
	if r.RequireDistinctApprover && by == r.Actor {
		return nil, fmt.Errorf("%q asked for this run and cannot release it", by)
	}
	f.approvedBy = by
	r.Status = run.StatusSucceeded
	return r, f.store.Save(ctx, r)
}

// TestSeedGovernanceShowsTheGateHoldingAndReleasing covers the two runs that are the only evidence on
// the demo that the policy boundary does anything. Every other seeded run goes straight from submit to
// execution, so without these the rules are listed as configuration and no run has ever been stopped
// by one, which leaves the product's central claim as the one thing a visitor cannot see.
func TestSeedGovernanceShowsTheGateHoldingAndReleasing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	approver := &fakeApprover{store: store}
	deps := Deps{Submitter: &fakeSubmitter{store: store}, Runs: store, Approver: approver}

	seedGovernance(ctx, deps, "site.yml", "inv.ini", "infra/network", zap.NewNop())

	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	var held, released *run.Run
	for _, r := range all {
		switch {
		case r.Status == run.StatusPendingApproval:
			held = r
		case r.HeldByPolicy != "":
			released = r
		}
	}

	// Test 0: a run the gate is still holding, so the demo always shows a change being refused.
	if held == nil {
		t.Error("no run left held for approval, so the demo shows no change the gate is stopping")
		return
	}
	if held.HeldByPolicy == "" {
		t.Error("the held run names no rule, so a visitor cannot tell what stopped it")
	}
	if !held.RequireDistinctApprover {
		t.Error("the held run accepts its own requester as approver, which is not the rule it shows")
	}

	// Test 1: a run carried through the whole gate, released by somebody other than the requester.
	if released == nil {
		t.Error("no run was held and then released, so the demo never shows an approval")
		return
	}
	if !released.Status.Terminal() {
		t.Errorf("the approved run is %s, so the demo shows an approval that never executed",
			released.Status)
	}
	if approver.approvedBy == "" {
		t.Fatal("nothing was approved, so no decision reaches the chain")
	}
	if approver.approvedBy == released.Actor {
		t.Errorf("run requested by %q and released by the same account, which the rule forbids",
			released.Actor)
	}
}

// TestSeedAnchorsDegradesToUnanchoredOffline covers the seeder's anchoring step when the authority
// cannot be reached, and when none is configured at all.
//
// The demo seeds on laptops with no network and in tests that must stay hermetic, so an anchoring
// failure has to leave a seeded, unanchored demo behind rather than a failed seed. The anchored
// demo is proven the other way, by the live droplet reseeding nightly against a real authority; what
// this pins is that the graceful path stays graceful, because an error return added here would take
// every offline seed down with it.
func TestSeedAnchorsDegradesToUnanchoredOffline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := audit.NewMemStore()
	anchorStore, ok := store.(audit.AnchorStore)
	if !ok {
		t.Fatal("the memory audit store no longer keeps anchors")
	}
	if err := store.Append(ctx, &audit.Entry{
		Actor: "seed", Method: "POST", Path: "/api/demo", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Test 0: an unreachable authority is a warning, not a failure, and saves nothing.
	seedAnchors(ctx, Deps{
		Audit: store, AnchorTSA: "http://127.0.0.1:1", InstallID: "install-demo",
	}, zap.NewNop())
	anchors, err := anchorStore.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("anchors: %v", err)
	}
	if len(anchors) != 0 {
		t.Fatalf("anchors = %d, want none from an unreachable authority", len(anchors))
	}

	// Test 1: no authority configured skips silently, which is what hermetic seeds rely on.
	seedAnchors(ctx, Deps{Audit: store, InstallID: "install-demo"}, zap.NewNop())
	// Test 2: no install identity skips too, rather than minting an anchor bound to nothing.
	seedAnchors(ctx, Deps{Audit: store, AnchorTSA: "http://127.0.0.1:1"}, zap.NewNop())
	anchors, err = anchorStore.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("anchors: %v", err)
	}
	if len(anchors) != 0 {
		t.Fatalf("anchors = %d, want none", len(anchors))
	}
}
