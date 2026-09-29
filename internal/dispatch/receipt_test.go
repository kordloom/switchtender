package dispatch

import (
	"context"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestReceiptNamesTheRequestInFlight pins the rule that a run's audit receipt is the request that
// authorized that run, never one replayed from the run it was derived from.
//
// A rerun, a shard retry, and a drift reconcile all replay a source run's execution options. When
// the receipt traveled with those options, the derived run's evidence named whoever launched the
// original, which is worse than naming nobody: an auditor reads a confident attribution that is
// false, and the entry that actually authorized the change appears nowhere.
func TestReceiptNamesTheRequestInFlight(t *testing.T) {
	t.Parallel()
	source := &run.Run{ID: "run_source", Playbook: "site.yml", AuditReceipt: "41:oldlaunch"}

	// The execution spec a derived run replays must not carry the source's receipt.
	derived := &run.Run{ID: "run_derived", Playbook: "site.yml"}
	inheritExecution(derived, source)
	if derived.AuditReceipt != "" {
		t.Errorf("a replayed execution spec carried the source run's receipt %q, so the derived "+
			"run's evidence names the wrong authorizer", derived.AuditReceipt)
	}

	// The request in flight is what the stamp records, and it wins over anything already set.
	ctx := run.WithAuditReceipt(context.Background(), "77:thisrequest")
	stampReceipt(ctx, derived)
	if derived.AuditReceipt != "77:thisrequest" {
		t.Errorf("receipt = %q, want the request that authorized this run", derived.AuditReceipt)
	}

	// A run created outside a recorded request carries none rather than borrowing one.
	unrequested := &run.Run{ID: "run_scheduled"}
	stampReceipt(context.Background(), unrequested)
	if unrequested.AuditReceipt != "" {
		t.Errorf("receipt = %q, want empty for a run no request authorized", unrequested.AuditReceipt)
	}
}

// TestStepRunInheritsThePipelineReceipt pins that a step belongs to the authorization of the
// pipeline it is part of. A step is built after the submitting request may have returned, so the
// parent's receipt is the only truthful source, unlike a rerun which is a new request.
func TestStepRunInheritsThePipelineReceipt(t *testing.T) {
	t.Parallel()
	parent := &run.Run{ID: "run_pipe", Inventory: "hosts.ini", AuditReceipt: "12:pipelaunch"}
	child := stepRun(parent, run.PipelineStep{Name: "migrate", Playbook: "m.yml"}, 0, 1, nil)
	if child.AuditReceipt != "12:pipelaunch" {
		t.Errorf("step receipt = %q, want the pipeline's, or the step's evidence names no "+
			"authorization at all", child.AuditReceipt)
	}
}

// TestChildrenCarryTheirParentAuthorization pins that a shard records the same authorization as
// the split it belongs to. Without this the assignment can be dropped and every shard of an
// operator-launched split renders evidence reading "no recorded request created this run" for a
// run a person explicitly launched.
func TestChildrenCarryTheirParentAuthorization(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &fakeRunnerLister{hosts: []string{"a", "b", "c", "d"}}, nil)
	defer d.Close()

	ctx := run.WithAuditReceipt(context.Background(), "88:splitlaunch")
	parent, err := d.SubmitSplit(ctx, "play.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if parent.AuditReceipt != "88:splitlaunch" {
		t.Fatalf("parent receipt = %q, want the launching request", parent.AuditReceipt)
	}
	shards, err := store.Shards(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) == 0 {
		t.Fatal("no shards were created")
	}
	for _, sh := range shards {
		if sh.AuditReceipt != parent.AuditReceipt {
			t.Errorf("shard %s receipt = %q, want its parent's %q", sh.ID, sh.AuditReceipt,
				parent.AuditReceipt)
		}
	}
}

// TestExplicitReceiptWinsOverAmbientContext pins that a caller stating which request set a run in
// motion is believed over whatever request happens to be in flight. The plan gate depends on it:
// a proposed apply is authorized by the request that submitted the plan, not by the executor.
func TestExplicitReceiptWinsOverAmbientContext(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run_apply"}
	run.ApplyOptions(r, []run.SubmitOption{run.WithAuditReceiptOf("12:planrequest")})
	stampReceipt(run.WithAuditReceipt(context.Background(), "99:ambient"), r)
	if r.AuditReceipt != "12:planrequest" {
		t.Errorf("receipt = %q, want the explicitly stated request", r.AuditReceipt)
	}
}

// TestRetryShardsCarryTheRetryAuthorization pins both halves of the retry rule: the retry run
// records the request that asked for the retry, not the one that launched the original weeks
// earlier, and its shards record the same as the retry they belong to.
func TestRetryShardsCarryTheRetryAuthorization(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := &flakyRunnerLister{hosts: []string{"a", "b", "c", "d"}, failHost: "b"}
	d := New(store, runner, nil)
	defer d.Close()

	launchCtx := run.WithAuditReceipt(context.Background(), "10:originallaunch")
	parent, err := d.SubmitSplit(launchCtx, "play.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if got := waitTerminal(t, store, parent.ID); got.Status != run.StatusFailed {
		t.Fatalf("parent status = %q, want failed", got.Status)
	}

	runner.fixed.Store(true)
	retryCtx := run.WithAuditReceipt(context.Background(), "20:retryrequest")
	retry, err := d.RetryFailedShards(retryCtx, parent.ID)
	if err != nil {
		t.Fatalf("RetryFailedShards() error = %v", err)
	}
	if retry.AuditReceipt != "20:retryrequest" {
		t.Fatalf("retry receipt = %q, want the request that asked for the retry, not %q",
			retry.AuditReceipt, parent.AuditReceipt)
	}
	shards, err := store.Shards(context.Background(), retry.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) == 0 {
		t.Fatal("the retry created no shards")
	}
	for _, sh := range shards {
		if sh.AuditReceipt != "20:retryrequest" {
			t.Errorf("retry shard %s receipt = %q, want the retry's", sh.ID, sh.AuditReceipt)
		}
	}
}
