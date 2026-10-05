package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// driftCheck stores a running terraform dry run of command in project, the shape of a drift check
// still executing, and returns it.
func driftCheck(t *testing.T, store run.Store, id, project, command string) *run.Run {
	t.Helper()
	r := &run.Run{ID: id, Tool: run.ToolTerraform, Command: command, ProjectID: project,
		DryRun: true, Status: run.StatusRunning, ClaimedBy: "worker-a", CreatedAt: time.Now()}
	if err := store.Save(context.Background(), r); err != nil {
		t.Fatalf("Save(%s) error = %v", id, err)
	}
	return r
}

// keptPlan returns the plan the check id kept, failing the test on an error.
func keptPlan(t *testing.T, store run.Store, id string) string {
	t.Helper()
	sealed, err := store.DriftPlan(context.Background(), id)
	if err != nil {
		t.Fatalf("DriftPlan(%s) error = %v", id, err)
	}
	return sealed
}

// testDriftPlanKept pins how every backend keeps the plan a drift check saved: it is written only
// while the check is a running dry run, it outlives the check's end and every wipe of sealed
// material, it never appears in the run's JSON, and a later check of the same working directory in
// the same project replaces it, whether or not that check found drift.
//
// Each rule is load bearing. A reconcile carries out the plan its check kept, so a plan wiped when
// the check ended could never be reconciled from, and a plan an older check kept could be carried
// out after a newer check saw something else. A plan attached to a run that is not a running drift
// check would be a plan nobody's check made.
func testDriftPlanKept(t *testing.T, store run.Store) {
	ctx := context.Background()
	first := driftCheck(t, store, "run_drift_first", "proj_a", "infra/network")
	if err := store.KeepDriftPlan(ctx, first.ID, "sealed-plan-first"); err != nil {
		t.Fatalf("KeepDriftPlan(first) error = %v", err)
	}
	if got := keptPlan(t, store, first.ID); got != "sealed-plan-first" {
		t.Errorf("kept plan = %q, want the sealed plan the check saved", got)
	}

	// The check ends, and neither its end nor a wipe of sealed material removes the kept plan.
	moved, err := store.FinalizeRunning(ctx, first.ID, run.Finalization{Status: run.StatusSucceeded,
		EndedAt: time.Now()})
	if err != nil || !moved {
		t.Fatalf("FinalizeRunning() = %v, %v, want moved", moved, err)
	}
	if err := store.WipeSealed(ctx, first.ID); err != nil {
		t.Fatalf("WipeSealed() error = %v", err)
	}
	if _, err := store.SweepSealed(ctx); err != nil {
		t.Fatalf("SweepSealed() error = %v", err)
	}
	if got := keptPlan(t, store, first.ID); got != "sealed-plan-first" {
		t.Errorf("kept plan after the check ended = %q, want it kept for a reconcile", got)
	}
	ended, err := store.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	data, err := json.Marshal(ended)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(data), "sealed-plan-first") {
		t.Errorf("the check's JSON carries its kept plan: %s", data)
	}

	// Only a running dry run keeps a plan.
	err = store.KeepDriftPlan(ctx, first.ID, "sealed-plan-late")
	if !errors.Is(err, run.ErrNoDriftCheck) {
		t.Errorf("KeepDriftPlan(ended check) error = %v, want ErrNoDriftCheck", err)
	}
	apply := driftCheck(t, store, "run_drift_apply", "proj_a", "infra/network")
	apply.DryRun = false
	if err := store.Save(ctx, apply); err != nil {
		t.Fatalf("Save(apply) error = %v", err)
	}
	err = store.KeepDriftPlan(ctx, apply.ID, "sealed-plan-apply")
	if !errors.Is(err, run.ErrNoDriftCheck) {
		t.Errorf("KeepDriftPlan(apply) error = %v, want ErrNoDriftCheck", err)
	}
	if got := keptPlan(t, store, first.ID); got != "sealed-plan-first" {
		t.Errorf("a refused keep dropped the earlier plan: %q", got)
	}
	if err := store.KeepDriftPlan(ctx, "run_drift_missing", "x"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("KeepDriftPlan(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := store.DriftPlan(ctx, "run_drift_missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("DriftPlan(missing) error = %v, want ErrNotFound", err)
	}

	// A check of another working directory, or of the same one in another project, leaves it alone.
	other := driftCheck(t, store, "run_drift_other", "proj_a", "infra/storage")
	if err := store.KeepDriftPlan(ctx, other.ID, "sealed-plan-other"); err != nil {
		t.Fatalf("KeepDriftPlan(other) error = %v", err)
	}
	elsewhere := driftCheck(t, store, "run_drift_elsewhere", "proj_b", "infra/network")
	if err := store.KeepDriftPlan(ctx, elsewhere.ID, "sealed-plan-elsewhere"); err != nil {
		t.Fatalf("KeepDriftPlan(elsewhere) error = %v", err)
	}
	if got := keptPlan(t, store, first.ID); got != "sealed-plan-first" {
		t.Errorf("a check of another target dropped the plan: %q", got)
	}

	// A newer check of the same target replaces it.
	second := driftCheck(t, store, "run_drift_second", "proj_a", "infra/network")
	if err := store.KeepDriftPlan(ctx, second.ID, "sealed-plan-second"); err != nil {
		t.Fatalf("KeepDriftPlan(second) error = %v", err)
	}
	if got := keptPlan(t, store, first.ID); got != "" {
		t.Errorf("the older check still keeps %q after a newer check of its target", got)
	}
	if got := keptPlan(t, store, second.ID); got != "sealed-plan-second" {
		t.Errorf("kept plan = %q, want the newer check's", got)
	}

	// And a newer check that found no drift keeps nothing and drops what the last one kept.
	clean := driftCheck(t, store, "run_drift_clean", "proj_a", "infra/network")
	if err := store.KeepDriftPlan(ctx, clean.ID, ""); err != nil {
		t.Fatalf("KeepDriftPlan(clean) error = %v", err)
	}
	for _, id := range []string{second.ID, clean.ID} {
		if got := keptPlan(t, store, id); got != "" {
			t.Errorf("check %s keeps %q after a check that found no drift", id, got)
		}
	}
	if got := keptPlan(t, store, other.ID); got != "sealed-plan-other" {
		t.Errorf("another target's plan = %q, want it untouched", got)
	}
	if got := keptPlan(t, store, elsewhere.ID); got != "sealed-plan-elsewhere" {
		t.Errorf("another project's plan = %q, want it untouched", got)
	}
}
