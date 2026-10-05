package migration

import (
	"os"
	"path/filepath"
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
			// The step is listed as soon as the walk reaches it, and the workflow parks a moment
			// later, once nothing else is left to run. The park is the wait the store keeps, so the
			// kill lands after it. Killed before it, the coordinator still held the workflow's
			// lease, and the lease sweep ended the workflow after its approval was accepted.
			if got := in.waitStatus(holder, wf.ID, "pending_approval", "succeeded", "failed",
				"canceled"); got.Status != "pending_approval" {
				t.Fatalf("the workflow reached %q, want it parked at its approval step: %s",
					got.Status, describe(got.Raw))
			}
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

// startHeldServer starts a server whose coordinators stop in the moment between listing a
// workflow's approval step and parking the workflow, and stay there until the server is killed.
// The file at marker names the workflow once one is held.
func (in *install) startHeldServer(name, marker string) *server {
	in.t.Helper()
	env := in.env
	in.env = append(append([]string(nil), env...), holdParkEnv+"="+marker)
	defer func() { in.env = env }()
	return in.startServer(name)
}

// waitHeld waits until a held server's coordinator reached the park of workflowID.
func (in *install) waitHeld(marker, workflowID string) {
	in.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		if got, err := os.ReadFile(marker); err == nil && string(got) == workflowID {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	in.t.Fatalf("the coordinator of workflow %s never reached its park", workflowID)
}

// TestKilledServerBeforeItsParkKeepsTheApproval kills the process coordinating an imported
// workflow in the moment between listing the workflow's approval step and parking the workflow,
// held there so the kill lands inside that moment every time. The workflow is left running under a
// lease nobody renews while its step is still listed, and a person approves the step. The lease
// sweep used to end the workflow as interrupted once that lease expired, so the accepted approval
// was lost. It has to park the workflow instead and resume it on the approval, which ships exactly
// once with the approval on the chain as the approver's. It runs on one server with SQLite,
// restarted after the kill, and on two servers sharing PostgreSQL, where the survivor takes over.
func TestKilledServerBeforeItsParkKeepsTheApproval(t *testing.T) {
	t.Parallel()
	for _, store := range []storeKind{onSQLite, onPostgres} {
		t.Run(string(store), func(t *testing.T) {
			t.Parallel()
			in := newInstall(t, installOptions{Store: store})
			marker := filepath.Join(in.root, "held-park")
			holder := in.startHeldServer("holder", marker)
			var survivor *server
			if store == onPostgres {
				survivor = in.startServer("survivor")
			}

			wf := in.launched(holder, "operator", "release", nil)
			step := in.waitPending(holder, "operator", wf.ID)
			in.waitHeld(marker, wf.ID)
			if got := in.getRun(holder, wf.ID); got.Status != "running" {
				t.Fatalf("the held workflow is %q, want running under its coordinator's lease: %s",
					got.Status, describe(got.Raw))
			}
			holder.kill()

			if survivor == nil {
				survivor = in.startServer("restarted")
			}
			if again := in.waitPending(survivor, "operator", wf.ID); again.ID != step.ID {
				t.Fatalf("after the kill the workflow waits at %+v, want step %s", again, step.ID)
			}
			// Accepted while the dead coordinator's lease still holds the workflow, so nothing can
			// resume it yet: the lease sweep is what hands it on.
			in.must(survivor, "approver", "POST", "/v1/runs/"+step.ID+"/approve",
				map[string]any{"state_digest": step.StateDigest}, 200)
			if done := in.waitDone(survivor, wf.ID); done.Status != "succeeded" {
				t.Fatalf("the workflow after the kill = %s, want the approval honored: %s",
					done.Status, describe(done.Raw))
			}
			in.requireSteps(survivor, wf.ID, map[string]int{"build": 1, "ship": 1, "page": 0})
			in.noPending(survivor, wf.ID)

			ev := in.checkEvidence(survivor, wf.ID)
			rec := ev.Receipts[wf.ID]
			requireChildren(t, rec, map[string]string{
				"build": "succeeded", "approve": "succeeded", "ship": "succeeded",
			})
			requireStepDecision(t, ev, step.ID, "approved", "approver-laptop")
			requireReceiptDecision(t, rec, step.ID, "approved", "approver-laptop", "approver")
		})
	}
}
