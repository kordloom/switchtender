package outcome_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestStepDecisionPathRoundTrips pins the chain path an approval step entry is written at. Every
// reader that collects a run's entries by the run's id, and the receipt that rebuilds a step's
// body, read it back through ParseStepDecisionPath, so a path it misreads is an entry the evidence
// loses.
func TestStepDecisionPathRoundTrips(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Path is the chain path read.
		Path string
		// WantRun is the workflow run it names.
		WantRun string
		// WantStep is the approval step record it names.
		WantStep string
		// WantVerdict is the verdict it carries.
		WantVerdict string
		// WantOK reports whether it is a step entry.
		WantOK bool
	}{{ // Test 0: A written path reads back whole.
		Path:    outcome.StepDecisionPath("run_wf", "run_gate", outcome.StepApproved),
		WantRun: "run_wf", WantStep: "run_gate", WantVerdict: outcome.StepApproved, WantOK: true,
	}, { // Test 1: A timeout reads back.
		Path:    outcome.StepDecisionPath("run_wf", "run_gate", outcome.StepTimedOut),
		WantRun: "run_wf", WantStep: "run_gate", WantVerdict: outcome.StepTimedOut, WantOK: true,
	}, { // Test 2: A run's own decision is not a step's.
		Path: "/runs/run_wf/decision/approved",
	}, { // Test 3: An unknown verdict is not read as a step entry.
		Path: "/runs/run_wf/steps/run_gate/decision/expired",
	}, { // Test 4: A trailing segment is not a verdict.
		Path: "/runs/run_wf/steps/run_gate/decision/approved/x",
	}, { // Test 5: An empty step id is refused.
		Path: "/runs/run_wf/steps//decision/approved",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runID, stepID, verdict, ok := outcome.ParseStepDecisionPath(test.Path)
			if ok != test.WantOK || runID != test.WantRun || stepID != test.WantStep ||
				verdict != test.WantVerdict {
				t.Errorf("ParseStepDecisionPath(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					test.Path, runID, stepID, verdict, ok, test.WantRun, test.WantStep,
					test.WantVerdict, test.WantOK)
			}
		})
	}
}

// TestStepStateBindsWhatUpstreamDryRunsForced pins that an approval step's state carries why each
// upstream dry run was not change free, so the digest an approver decides on changes when that
// does, and that a state with nothing found serializes without the field, digesting as it did
// before the field existed.
func TestStepStateBindsWhatUpstreamDryRunsForced(t *testing.T) {
	t.Parallel()
	forced := []string{`site.yml: task "Restart" sets check_mode to false`}
	tests := []struct {
		// Forced is what the upstream step's dry run forced.
		Forced []string
		// WantField is whether the serialized state names the field at all.
		WantField bool
	}{{ // Test 0: Nothing forced leaves the state as it always was.
		Forced: nil, WantField: false,
	}, { // Test 1: A forcing upstream dry run is part of the state.
		Forced: forced, WantField: true,
	}}
	digests := make([]string, len(tests))
	for testNum, test := range tests {
		ctx := context.Background()
		store := run.NewMemStore()
		parent := &run.Run{ID: "run_wf", Kind: run.KindPipeline, Status: run.StatusRunning,
			CreatedAt: time.Now(), Steps: []run.PipelineStep{
				{Name: "build", Playbook: "site.yml", DryRun: true},
				{Name: "gate", Type: run.StepApproval, DependsOn: []string{"build"}},
			}}
		zero, one := 0, 1
		build := &run.Run{ID: "run_build", ParentID: &parent.ID, StepIndex: &zero, StepName: "build",
			Playbook: "site.yml", DryRun: true, Status: run.StatusSucceeded, CreatedAt: time.Now(),
			DryRunScans: []run.DryRunScan{{Tool: run.ToolAnsible, Findings: test.Forced}}}
		gate := &run.Run{ID: "run_gate", ParentID: &parent.ID, StepIndex: &one, StepName: "gate",
			Status: run.StatusPendingApproval, CreatedAt: time.Now()}
		for _, r := range []*run.Run{parent, build, gate} {
			if err := store.Save(ctx, r); err != nil {
				t.Fatalf("Save(%s) error = %v", r.ID, err)
			}
		}
		state, err := outcome.StepStateOf(ctx, store, parent, gate)
		if err != nil {
			t.Fatalf("test %d: StepStateOf() error = %v", testNum, err)
		}
		if len(state.Upstream) != 1 {
			t.Fatalf("test %d: upstream = %+v, want the build step", testNum, state.Upstream)
		}
		if diff := cmp.Diff(test.Forced, state.Upstream[0].DryRunFindings,
			cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("test %d: forced mismatch (-want +got):\n%s", testNum, diff)
		}
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("test %d: Marshal() error = %v", testNum, err)
		}
		if got := strings.Contains(string(raw), "dry_run_findings"); got != test.WantField {
			t.Errorf("test %d: state names dry_run_findings = %v, want %v", testNum, got,
				test.WantField)
		}
		if digests[testNum], err = state.Digest(); err != nil {
			t.Fatalf("test %d: Digest() error = %v", testNum, err)
		}
	}
	if digests[0] == digests[1] {
		t.Error("the state digest does not change when an upstream dry run forced real work")
	}
}
