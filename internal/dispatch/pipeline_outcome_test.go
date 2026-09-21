package dispatch

import (
	"context"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestPipelineOutcomeDistinguishesAnInterruptFromACancel pins the mapping a step-executor death
// used to get wrong.
//
// A step whose own executor dies ends interrupted while the coordinator stays healthy. The old
// code folded interrupted into canceled and then asked stoppedStatus, which reads the
// coordinator's live context and answers "canceled": a crashed pipeline recorded as a withdrawal,
// and the interrupted state a rerun resumes from erased. An interrupt is the step's word about
// what happened to it, not the coordinator's word about itself.
func TestPipelineOutcomeDistinguishesAnInterruptFromACancel(t *testing.T) {
	t.Parallel()
	// A healthy coordinator: its context is live, so stoppedStatus would say canceled.
	d := &Dispatcher{ctx: context.Background()}

	tests := []struct {
		// Name says which ending the steps reported.
		Name string
		// Res is how the steps ended.
		Res stepsResult
		// Want is the parent status that ending must produce.
		Want run.Status
		// WantReason is whether a stated reason must accompany it.
		WantReason bool
	}{
		{"a step executor died", stepsResult{interrupted: true}, run.StatusInterrupted, true},
		{"the coordinator was canceled", stepsResult{canceled: true}, run.StatusCanceled, false},
		{"a step failed", stepsResult{failed: true}, run.StatusFailed, false},
		{"every step succeeded", stepsResult{}, run.StatusSucceeded, false},
		// Interrupt beats failure: a run whose executor died before its failure could be trusted
		// is interrupted, so it can be resumed, not failed, which reads as a decision the run made.
		{"interrupt outranks a failure", stepsResult{failed: true, interrupted: true},
			run.StatusInterrupted, true},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			status, reason, _ := d.pipelineOutcome(test.Res)
			if status != test.Want {
				t.Errorf("status = %q, want %q", status, test.Want)
			}
			if (reason != "") != test.WantReason {
				t.Errorf("reason = %q, want a stated reason: %v", reason, test.WantReason)
			}
		})
	}
}
