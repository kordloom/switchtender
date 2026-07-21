package dispatch

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

func TestParsePlanChanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want int
	}{
		{"Plan: 1 to add, 2 to change, 3 to destroy", 6},
		{"noise line\nPlan: 0 to add, 0 to change, 0 to destroy\nmore", 0},
		{"Apply complete! Resources: 2 added, 0 changed, 0 destroyed.", 0},
		{"No changes. Your infrastructure matches the configuration.", 0},
		{"", 0},
	}
	for i, test := range tests {
		if got := parsePlanChanges(test.In); got != test.Want {
			t.Errorf("test %d: parsePlanChanges(%q) = %d, want %d", i, test.In, got, test.Want)
		}
	}
}

// TestDispatcherRecordsTerraformDrift verifies a Terraform dry run that finds pending changes lands
// on the Drift page keyed on its working directory, with the plan's resource count, and finishes as
// a success rather than a failure.
func TestDispatcherRecordsTerraformDrift(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			_, _ = io.WriteString(out, "Plan: 1 to add, 2 to change, 0 to destroy\n")
			return roundhouse.Result{ExitCode: 0, Drift: true}, nil
		})
	d := New(store, runner, nil)
	defer d.Close()

	created, err := d.Submit(context.Background(), "", "inv",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra/network"), run.WithDryRun(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := waitTerminal(t, store, created.ID)
	if got.Status != run.StatusSucceeded {
		t.Errorf("status = %q, want succeeded since drift is not a failure", got.Status)
	}

	// The drift summary is saved just after the run finalizes, so poll the Drift page for it.
	var found *run.HostDrift
	deadline := time.Now().Add(3 * time.Second)
	for found == nil && time.Now().Before(deadline) {
		drift, err := store.DriftStatus(context.Background())
		if err != nil {
			t.Fatalf("DriftStatus() error = %v", err)
		}
		for i := range drift {
			if drift[i].Host == "infra/network" {
				found = &drift[i]
			}
		}
		if found == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if found == nil {
		t.Fatal("the Drift page has no row for infra/network")
	}
	if found.DriftedTasks != 3 || found.RunID != created.ID {
		t.Errorf("drift = %+v, want 3 changed resources from run %s", found, created.ID)
	}
}
