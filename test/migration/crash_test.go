package migration

import (
	"testing"
	"time"
)

// TestKilledServerDuringApprovalWaitResumesCorrectly is scenario eleven. The process coordinating
// an imported workflow is killed outright while the workflow waits at its approval step, with no
// chance to clean up. The wait has to survive: the step is still pending, a person's approval is
// still accepted, the workflow resumes and ships exactly once, and a restarted process does not run
// anything a second time. It runs on one server with SQLite, restarted after the kill, and on two
// servers sharing PostgreSQL, where the survivor takes over.
func TestKilledServerDuringApprovalWaitResumesCorrectly(t *testing.T) {
	t.Parallel()
	for _, store := range []storeKind{onSQLite, onPostgres} {
		t.Run(string(store), func(t *testing.T) {
			t.Parallel()
			in := newInstall(t, installOptions{Store: store})
			holder := in.startServer("holder")
			var survivor *server
			if store == onPostgres {
				survivor = in.startServer("survivor")
			}

			wf := in.launched(holder, "operator", "release", nil)
			step := in.waitPending(holder, "operator", wf.ID)
			in.requireSteps(holder, wf.ID, map[string]int{"build": 1, "ship": 0, "page": 0})
			holder.kill()

			if survivor == nil {
				survivor = in.startServer("restarted")
			}
			if again := in.waitPending(survivor, "operator", wf.ID); again.ID != step.ID ||
				again.StateDigest != step.StateDigest {
				t.Fatalf("after the kill the workflow waits at %+v, want the same step in the same "+
					"state as %+v", again, step)
			}
			in.must(survivor, "approver", "POST", "/v1/runs/"+step.ID+"/approve",
				map[string]any{"state_digest": step.StateDigest}, 200)
			if done := in.waitDone(survivor, wf.ID); done.Status != "succeeded" {
				t.Fatalf("the workflow after the kill = %s: %s", done.Status, describe(done.Raw))
			}

			// On PostgreSQL the killed replica comes back too, as its supervisor restarts it, and
			// sweeps for parked workflows on its janitor tick. Waiting past that tick, on either
			// store, is what makes "nothing ran twice" a measurement.
			back := survivor
			if store == onPostgres {
				back = in.startServer("holder-back")
			}
			time.Sleep(12 * time.Second)
			in.requireSteps(back, wf.ID, map[string]int{"build": 1, "ship": 1, "page": 0})
			in.noPending(back, wf.ID)

			ev := in.checkEvidence(back, wf.ID)
			rec := ev.Receipts[wf.ID]
			requireRecord(t, rec, recordWant{Launcher: "operator-laptop", OnBehalfOf: "operator"})
			requireChildren(t, rec, map[string]string{
				"build": "succeeded", "approve": "succeeded", "ship": "succeeded",
			})
			requireStepDecision(t, ev, step.ID, "approved", "approver-laptop")
			requireReceiptDecision(t, rec, step.ID, "approved", "approver-laptop", "approver")
		})
	}
}
