package dispatch

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestBaseStepVarsSeedsFromParent proves a pipeline's steps start from the parent's own vars, and
// that a step mutating its inputs cannot reach the parent.
func TestBaseStepVarsSeedsFromParent(t *testing.T) {
	t.Parallel()
	parent := &run.Run{ExtraVars: map[string]any{"environment": "prod", "region": "us-east"}}
	got := baseStepVars(parent)
	if got["environment"] != "prod" || got["region"] != "us-east" {
		t.Errorf("baseStepVars = %v, want the parent's vars", got)
	}
	got["environment"] = "mutated"
	if parent.ExtraVars["environment"] != "prod" {
		t.Error("mutating a step's vars reached the parent's map")
	}
	// A parent with no vars yields an empty, non-nil, mutable map for the linear accumulator.
	if empty := baseStepVars(&run.Run{}); empty == nil {
		t.Error("baseStepVars returned nil, which the linear accumulator cannot copy into")
	}
}

// TestStepRunInheritsParentLimit proves a launch-time host limit reaches each step.
func TestStepRunInheritsParentLimit(t *testing.T) {
	t.Parallel()
	parent := &run.Run{ID: "run_p", Limit: "web01,web02", Inventory: "prod"}
	child := stepRun(parent, run.PipelineStep{Name: "one", Playbook: "p.yml"}, 0, 0, nil)
	if child.Limit != "web01,web02" {
		t.Errorf("step limit = %q, want the parent's launch limit", child.Limit)
	}
}

// recordingRunner captures the ExtraVars and Limit of every spec it executes, so a test can assert
// what actually reached each step.
type recordingRunner struct {
	mu    sync.Mutex
	specs []roundhouse.Spec
}

func (r *recordingRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	return roundhouse.Result{ExitCode: 0}, nil
}

func (r *recordingRunner) Hosts(context.Context, string) ([]string, error) { return nil, nil }

// TestPipelineStepsReceiveParentExtraVars is the end-to-end proof of the blocking gap the design
// review found: a saved workflow applies its survey answers and extra vars to the pipeline parent,
// and without seeding they reached no step. Every step must execute with the parent's vars.
func TestPipelineStepsReceiveParentExtraVars(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	rec := &recordingRunner{}
	d := New(store, rec, nil, WithNoJanitor())
	defer d.Close()

	// Both engines must seed from the parent, so this runs once as a graph (a step with DependsOn,
	// which takes the DAG path) and once as a plain sequence (no dependencies, the linear path).
	cases := []struct {
		Name  string
		Steps []run.PipelineStep
	}{
		{"dag", []run.PipelineStep{
			{Name: "build", Tool: "bash", Command: "echo build"},
			{Name: "deploy", Tool: "bash", Command: "echo deploy", DependsOn: []string{"build"}},
		}},
		{"linear", []run.PipelineStep{
			{Name: "one", Tool: "bash", Command: "echo one"},
			{Name: "two", Tool: "bash", Command: "echo two"},
		}},
	}
	for _, tc := range cases {
		rec.mu.Lock()
		rec.specs = nil
		rec.mu.Unlock()
		parent, err := d.SubmitPipeline(ctx, "ship-"+tc.Name, "prod", tc.Steps,
			run.WithExtraVars(map[string]any{"environment": "staging"}),
			run.WithLimit("web01"))
		if err != nil {
			t.Fatalf("SubmitPipeline(%s) error = %v", tc.Name, err)
		}
		waitTerminal(t, store, parent.ID)

		rec.mu.Lock()
		specs := append([]roundhouse.Spec(nil), rec.specs...)
		rec.mu.Unlock()
		if len(specs) != 2 {
			t.Fatalf("%s: ran %d steps, want 2", tc.Name, len(specs))
		}
		for _, spec := range specs {
			if spec.ExtraVars["environment"] != "staging" {
				t.Errorf("%s: a step ran without the workflow's extra vars: %v", tc.Name, spec.ExtraVars)
			}
			if spec.Limit != "web01" {
				t.Errorf("%s: a step ran with limit %q, want the launch limit web01", tc.Name, spec.Limit)
			}
		}
	}
}
