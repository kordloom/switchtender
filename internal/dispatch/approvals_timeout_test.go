package dispatch

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestApprovalStepTimeoutFromAStaleSweepIsNotRecorded pins what a replica's timeout sweep may
// write. Every replica lists the waiting steps, then times out each one past its deadline. A
// replica whose listing predates another's decision, a timeout or a person's approval, used to
// commit its own timed_out entry before the compare-and-swap that refused it, so the chain held two
// endings for one step: timed out twice, or approved by a person and also given up on, while the
// workflow followed only one of them.
func TestApprovalStepTimeoutFromAStaleSweepIsNotRecorded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// First decides the step before the stale sweep runs.
		First func(t *testing.T, d *Dispatcher, node *run.Run)
		// WantPaths are the step's chain entries afterward, in order.
		WantPaths []string
	}{{ // Test 0: Another replica's sweep timed the step out first.
		First: func(t *testing.T, d *Dispatcher, node *run.Run) {
			t.Helper()
			if !d.timeOutStep(context.Background(), node) {
				t.Fatalf("the first timeout did not settle the step")
			}
		},
		WantPaths: []string{outcome.StepRequested, outcome.StepTimedOut},
	}, { // Test 1: A person approved the step first.
		First: func(t *testing.T, d *Dispatcher, node *run.Run) {
			t.Helper()
			if _, err := d.DecideStep(context.Background(), node.ID, StepDecision{Approve: true,
				By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
				t.Fatalf("DecideStep() error = %v", err)
			}
		},
		WantPaths: []string{outcome.StepRequested, outcome.StepApproved},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			audits := audit.NewMemStore()
			a := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithAudits(audits),
				WithOwner("replica-a"))
			defer a.Close()
			b := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithAudits(audits),
				WithOwner("replica-b"))
			defer b.Close()
			parent, err := a.SubmitPipeline(context.Background(), "release", "",
				gatedWorkflow(3600, true))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			// Replica b's listing, read while the step still waited.
			stale := waitParked(t, store, parent.ID)
			test.First(t, a, stale)
			if b.timeOutStep(context.Background(), stale) {
				t.Errorf("a sweep holding a stale listing settled a step already decided")
			}
			waitTerminal(t, store, parent.ID)
			if diff := cmp.Diff(test.WantPaths, stepPaths(t, audits)); diff != "" {
				t.Errorf("the step's chain entries (-want +got):\n%s", diff)
			}
		})
	}
}
