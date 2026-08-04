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
