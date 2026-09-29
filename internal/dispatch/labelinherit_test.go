package dispatch

import (
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestEveryDerivedRunKeepsItsLabels covers the Change's core value, which two of three paths drop.
//
// A change is a label, and its whole point is holding a failure together with the fix that followed.
// The three ways to produce that fix are rerun, retry, and relaunch. Rerun adds labels explicitly;
// the other two build from ExecutionOptions, which deliberately carries only how a run executes and
// not what it is. So the natural fix-forward path drops out of the change it is fixing, and the
// change reports failed rather than mixed.
//
// The same loss is already recorded one comment away: keeping a separate list is what cost a rerun
// its timeout and its notifications.
func TestEveryDerivedRunKeepsItsLabels(t *testing.T) {
	t.Parallel()
	src := &run.Run{
		ID: "run_src", Tool: run.ToolBash, Command: "true",
		Labels: map[string]string{"change": "OPS-482", "env": "prod"},
	}

	// ExecutionOptions is how it executes, not what it is, so labels are absent by design. This is
	// the fact every derived path has to account for rather than the bug itself.
	derived := &run.Run{}
	for _, opt := range src.ExecutionOptions() {
		opt(derived)
	}
	if len(derived.Labels) != 0 {
		t.Fatalf("ExecutionOptions now carries labels (%v), so this test is asserting the wrong "+
			"thing and the derived paths need re-checking", derived.Labels)
	}

	// inheritExecution is what retry uses, so whatever it produces is what a retried run carries.
	retried := &run.Run{}
	inheritExecution(retried, src)
	if retried.Labels["change"] != "OPS-482" {
		t.Errorf("a retried run lost its change label, so the fix for a failed split is not part " +
			"of the change it fixes and the change reads as failed rather than mixed")
	}
	if retried.Labels["env"] != "prod" {
		t.Errorf("a retried run lost its other labels too, so every label-based view drops it")
	}
}
