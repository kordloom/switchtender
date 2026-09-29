package run

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// saveRun stores r and fails the test if the store refuses it.
func saveRun(t *testing.T, store Store, r *Run) {
	t.Helper()
	if err := store.Save(context.Background(), r); err != nil {
		t.Fatalf("Save(%s) error = %v", r.ID, err)
	}
}

// getRun reads a run back and fails the test if it is gone.
func getRun(t *testing.T, store Store, id string) *Run {
	t.Helper()
	got, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return got
}

// TestStatusTerminalCoversEveryDeclaredStatus pins the terminal test against the full status set,
// not a sample of it.
//
// Terminal decides what the janitor sweeps, what a fenced write refuses, what retention purges, and
// whether a run is still claimable. A status left out of the switch reads as non-terminal, so a
// rejected or interrupted run would keep being swept and could be written to again by a worker that
// outlived its lease. The existing coverage stops before interrupted, pending_approval, and
// rejected, which are the three added last and therefore the three most likely to be missed.
func TestStatusTerminalCoversEveryDeclaredStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In           Status
		WantTerminal bool
	}{
		{In: StatusPending, WantTerminal: false},         // Test 0: Waiting for a worker.
		{In: StatusRunning, WantTerminal: false},         // Test 1: Under way.
		{In: StatusPendingApproval, WantTerminal: false}, // Test 2: Resting until somebody decides.
		{In: StatusSucceeded, WantTerminal: true},        // Test 3: Finished green.
		{In: StatusFailed, WantTerminal: true},           // Test 4: Finished red.
		{In: StatusCanceled, WantTerminal: true},         // Test 5: Stopped on request.
		{In: StatusInterrupted, WantTerminal: true},      // Test 6: Abandoned, cannot resume.
		{In: StatusRejected, WantTerminal: true},         // Test 7: Denied, never executed.
		{In: Status(""), WantTerminal: false},            // Test 8: An empty status is not final.
		{In: Status("PENDING"), WantTerminal: false},     // Test 9: The match is exact, not folded.
		{In: Status("succeeded "), WantTerminal: false},  // Test 10: A stray space is not succeeded.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.In.Terminal(); got != test.WantTerminal {
				t.Errorf("Status(%q).Terminal() = %v, want %v", test.In, got, test.WantTerminal)
			}
		})
	}
}

// TestFinalizeRunningRefusesEveryNonRunningStatus pins that only a running run can be terminalized,
// so an executor cannot overwrite an outcome another actor already recorded.
//
// The dangerous case is the last one. A worker whose heartbeats died to a partition comes back
// after the janitor has already interrupted its run and committed that outcome to the audit chain.
// If its terminal write landed, the chain would carry two contradictory outcomes for one run and
// the later one would be the one every view reads.
func TestFinalizeRunningRefusesEveryNonRunningStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Status      Status
		WantChanged bool
	}{
		{Name: "pending", Status: StatusPending, WantChanged: false},         // Test 0: Never started.
		{Name: "held", Status: StatusPendingApproval, WantChanged: false},    // Test 1: Not released.
		{Name: "running", Status: StatusRunning, WantChanged: true},          // Test 2: The one case.
		{Name: "succeeded", Status: StatusSucceeded, WantChanged: false},     // Test 3: Already ended.
		{Name: "failed", Status: StatusFailed, WantChanged: false},           // Test 4: Already ended.
		{Name: "canceled", Status: StatusCanceled, WantChanged: false},       // Test 5: Already ended.
		{Name: "interrupted", Status: StatusInterrupted, WantChanged: false}, // Test 6: Swept away.
		{Name: "rejected", Status: StatusRejected, WantChanged: false},       // Test 7: Never ran.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			saveRun(t, store, &Run{
				ID: "run_1", Playbook: "site.yml", Status: test.Status, CreatedAt: time.Now(),
				ClaimedBy: "worker-a", Error: "the outcome already on the chain",
			})
			code := 0
			changed, err := store.FinalizeRunning(ctx, "run_1", Finalization{
				Status: StatusSucceeded, ExitCode: &code, EndedAt: time.Now(),
			})
			if err != nil {
				t.Fatalf("FinalizeRunning() error = %v", err)
			}
			if changed != test.WantChanged {
				t.Errorf("FinalizeRunning() on a %s run changed = %v, want %v",
					test.Status, changed, test.WantChanged)
			}
			got := getRun(t, store, "run_1")
			if !test.WantChanged && got.Status != test.Status {
				t.Errorf("the run moved from %s to %s despite the fence refusing the write",
					test.Status, got.Status)
			}
		})
	}

	// A run that is not there at all is refused rather than created.
	changed, err := NewMemStore().FinalizeRunning(context.Background(), "run_missing",
		Finalization{Status: StatusSucceeded, EndedAt: time.Now()})
	if err != nil || changed {
		t.Errorf("FinalizeRunning(missing) = (%v, %v), want (false, nil)", changed, err)
	}
}

// TestFinalizeRunningIsFencedOnTheLease pins that an executor naming a lease can only terminalize a
// run it still holds.
//
// Fencing on status alone was not enough. A dispatcher that lost its heartbeats to a partition can
// come back after the janitor requeued its run and a second worker claimed and started it. The
// status matched, because the second worker had made it running again, so the first one's terminal
// write landed on the run the second one was still executing. Naming the lease closes that.
func TestFinalizeRunningIsFencedOnTheLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Holder      string
		Owner       string
		WantChanged bool
	}{{ // Test 0: The executor that still holds the lease finishes its own run.
		Name: "same owner", Holder: "worker-a", Owner: "worker-a", WantChanged: true,
	}, { // Test 1: A worker whose lease was requeued to somebody else is refused.
		Name: "stale owner", Holder: "worker-b", Owner: "worker-a", WantChanged: false,
	}, { // Test 2: An unleased run is not this executor's to finish.
		Name: "no holder", Holder: "", Owner: "worker-a", WantChanged: false,
	}, { // Test 3: The two non-executor callers, the timeout sweep and the relay handler, name no
		// lease and are gated elsewhere, so an empty owner is not a fence at all.
		Name: "unfenced caller", Holder: "worker-b", Owner: "", WantChanged: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			saveRun(t, store, &Run{
				ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
				ClaimedBy: test.Holder,
			})
			changed, err := store.FinalizeRunning(ctx, "run_1", Finalization{
				Status: StatusSucceeded, Owner: test.Owner, EndedAt: time.Now(),
			})
			if err != nil {
				t.Fatalf("FinalizeRunning() error = %v", err)
			}
			if changed != test.WantChanged {
				t.Errorf("FinalizeRunning(owner=%q) on a run held by %q changed = %v, want %v",
					test.Owner, test.Holder, changed, test.WantChanged)
			}
		})
	}
}

// TestFinalizeRunningWritesEveryFactWithTheStatus pins that the terminal write lands as one value.
//
// Moving the status first and writing the exit code, failure text, image, commit, and end time
// after left a run terminal with none of them whenever the second write failed, and nothing sweeps
// a terminal run: the janitor only reclaims pending and running ones. The image and the commit in
// particular are resolved while the run is under way, after the last whole-run save, so this write
// is the only chance to store them and the outcome digest commits to both.
func TestFinalizeRunningWritesEveryFactWithTheStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
		ClaimedBy: "worker-a",
	})
	code := 2
	ended := time.Date(2026, 8, 4, 12, 30, 0, 0, time.UTC)
	changed, err := store.FinalizeRunning(ctx, "run_1", Finalization{
		Status: StatusFailed, ExitCode: &code, Error: "task failed on web01",
		Image: "ghcr.io/org/img:1", CommitSHA: "deadbeef", PullCredentialID: "cred_pull",
		Owner: "worker-a", Outputs: map[string]any{"version": "1.2.3"},
		Warning: "no per-host result recorded", EndedAt: ended,
	})
	if err != nil || !changed {
		t.Fatalf("FinalizeRunning() = (%v, %v), want (true, nil)", changed, err)
	}
	got := getRun(t, store, "run_1")
	if got.Status != StatusFailed || got.ExitCode == nil || *got.ExitCode != 2 {
		t.Errorf("status/exit = %s/%v, want failed with exit 2", got.Status, got.ExitCode)
	}
	want := map[string]string{
		"error": "task failed on web01", "image": "ghcr.io/org/img:1",
		"commit": "deadbeef", "pull credential": "cred_pull",
		"warning": "no per-host result recorded",
	}
	gotFields := map[string]string{
		"error": got.Error, "image": got.Image, "commit": got.CommitSHA,
		"pull credential": got.PullCredentialID, "warning": got.Warning,
	}
	if diff := cmp.Diff(want, gotFields, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the terminal write lost a fact (-want +got):\n%s", diff)
	}
	if got.EndedAt == nil || !got.EndedAt.Equal(ended) {
		t.Errorf("EndedAt = %v, want %v", got.EndedAt, ended)
	}
	if diff := cmp.Diff(map[string]any{"version": "1.2.3"}, got.Outputs,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("outputs mismatch (-want +got):\n%s", diff)
	}
}

// TestApplyRunningProgressCannotResurrectASettledRun pins the fence that stops a mid-run report
// from putting a settled run back on its feet.
//
// The relay's progress handler used to re-read the row, check it was not terminal, and save the
// whole row back. Between that read and that write the janitor could settle the run, and the save
// then restored the status, the lease, and the cleared claim secret from the snapshot. The run came
// back to life, the worker kept executing under a lease the control node had already declared dead,
// and its later terminal report put a second, contradictory outcome on the audit chain beside the
// interrupted one the sweep had already committed.
func TestApplyRunningProgressCannotResurrectASettledRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Status      Status
		Owner       string
		WantChanged bool
	}{{ // Test 0: The worker that holds a live run reports normally.
		Name: "running and held", Status: StatusRunning, Owner: "worker-a", WantChanged: true,
	}, { // Test 1: The janitor already interrupted it, so the report is too late.
		Name: "already interrupted", Status: StatusInterrupted, Owner: "worker-a", WantChanged: false,
	}, { // Test 2: Somebody canceled it and it settled.
		Name: "already canceled", Status: StatusCanceled, Owner: "worker-a", WantChanged: false,
	}, { // Test 3: It finished, and a late report must not reopen it.
		Name: "already succeeded", Status: StatusSucceeded, Owner: "worker-a", WantChanged: false,
	}, { // Test 4: It was requeued and is waiting for a new worker.
		Name: "requeued to pending", Status: StatusPending, Owner: "worker-a", WantChanged: false,
	}, { // Test 5: Another worker holds it now, so this report is from a lost lease.
		Name: "lease moved on", Status: StatusRunning, Owner: "worker-stale", WantChanged: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			saveRun(t, store, &Run{
				ID: "run_1", Playbook: "site.yml", Status: test.Status, CreatedAt: time.Now(),
				ClaimedBy: "worker-a",
			})
			started := time.Now()
			changed, err := store.ApplyRunningProgress(ctx, "run_1", test.Owner, Progress{
				StartedAt: &started, Warning: "late note",
				Outputs: map[string]any{"leaked": true},
			})
			if err != nil {
				t.Fatalf("ApplyRunningProgress() error = %v", err)
			}
			if changed != test.WantChanged {
				t.Errorf("ApplyRunningProgress() changed = %v, want %v", changed, test.WantChanged)
			}
			got := getRun(t, store, "run_1")
			if got.Status != test.Status {
				t.Errorf("status moved from %s to %s, so a progress report changed the lifecycle",
					test.Status, got.Status)
			}
			if !test.WantChanged && (got.Warning != "" || len(got.Outputs) != 0) {
				t.Errorf("a refused report still wrote warning %q and outputs %v",
					got.Warning, got.Outputs)
			}
		})
	}
}

// TestProgressNeverMovesAStartTimeBackward pins that a repeated report cannot rewrite when a run
// began, and that an empty warning or nil output map leaves what is stored alone.
//
// A worker retrying a dropped report sends the same body again, sometimes long after the first one
// landed. A start time that moved with each retry would stretch or shrink the run's duration in the
// metrics histograms and in every comparison against a baseline run.
func TestProgressNeverMovesAStartTimeBackward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
		ClaimedBy: "worker-a",
	})
	first := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)
	earlier := first.Add(-time.Hour)

	for _, at := range []time.Time{first, later, earlier} {
		stamp := at
		if _, err := store.ApplyRunningProgress(ctx, "run_1", "worker-a",
			Progress{StartedAt: &stamp, Warning: "capture unavailable",
				Outputs: map[string]any{"v": 1}}); err != nil {
			t.Fatalf("ApplyRunningProgress() error = %v", err)
		}
	}
	got := getRun(t, store, "run_1")
	if got.StartedAt == nil || !got.StartedAt.Equal(first) {
		t.Errorf("StartedAt = %v, want the first report's %v", got.StartedAt, first)
	}

	// An empty warning and a nil output map leave what is already stored.
	if _, err := store.ApplyRunningProgress(ctx, "run_1", "worker-a", Progress{}); err != nil {
		t.Fatalf("ApplyRunningProgress() error = %v", err)
	}
	got = getRun(t, store, "run_1")
	if got.Warning != "capture unavailable" {
		t.Errorf("Warning = %q, want the stored note kept by an empty report", got.Warning)
	}
	if diff := cmp.Diff(map[string]any{"v": 1}, got.Outputs, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("an empty report cleared the stored outputs (-want +got):\n%s", diff)
	}
}

// TestTransitionStatusRefusesAWrongStartingStatus pins the compare-and-swap two approvers race
// through, so only one of them can win.
//
// Approve and reject are both this call. Without the from check, two people clicking at once would
// both succeed and the run would carry whichever decision landed second, with both recorded in the
// chain as having taken effect.
func TestTransitionStatusRefusesAWrongStartingStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Stored      Status
		From        Status
		To          Status
		WantChanged bool
	}{{ // Test 0: A held run released to pending.
		Name: "approve a held run", Stored: StatusPendingApproval, From: StatusPendingApproval,
		To: StatusPending, WantChanged: true,
	}, { // Test 1: The second approver finds it already released and loses.
		Name: "second approver", Stored: StatusPending, From: StatusPendingApproval,
		To: StatusPending, WantChanged: false,
	}, { // Test 2: A rejection after somebody already rejected it loses.
		Name: "second rejecter", Stored: StatusRejected, From: StatusPendingApproval,
		To: StatusRejected, WantChanged: false,
	}, { // Test 3: A run already executing cannot be released again.
		Name: "already running", Stored: StatusRunning, From: StatusPendingApproval,
		To: StatusPending, WantChanged: false,
	}, { // Test 4: A finished run cannot be walked back to pending.
		Name: "finished run", Stored: StatusSucceeded, From: StatusRunning,
		To: StatusPending, WantChanged: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			saveRun(t, store, &Run{
				ID: "run_1", Playbook: "site.yml", Status: test.Stored, CreatedAt: time.Now(),
			})
			changed, err := store.TransitionStatus(ctx, "run_1", test.From, test.To)
			if err != nil {
				t.Fatalf("TransitionStatus() error = %v", err)
			}
			if changed != test.WantChanged {
				t.Errorf("TransitionStatus(%s -> %s) on a %s run changed = %v, want %v",
					test.From, test.To, test.Stored, changed, test.WantChanged)
			}
			if got := getRun(t, store, "run_1"); !test.WantChanged && got.Status != test.Stored {
				t.Errorf("the run moved to %s on a refused transition", got.Status)
			}
		})
	}

	// A missing run changes nothing and is not an error, so a racing delete is not a failure.
	changed, err := NewMemStore().TransitionStatus(context.Background(), "run_gone",
		StatusPending, StatusRunning)
	if err != nil || changed {
		t.Errorf("TransitionStatus(missing) = (%v, %v), want (false, nil)", changed, err)
	}
}

// TestTransitionAndClaimRefusesACanceledRun pins that a cancel requested between an approval and
// the coordinator picking the run up stops it, in the same statement that makes the transition.
//
// Cancel is a flag rather than a status, so a fence comparing only the status cannot see one: a
// pipeline canceled after it was approved and before its coordinator arrived still read as running,
// won the compare-and-swap, and executed on real hosts. Checking the flag first and swapping second
// leaves the same gap one scheduling delay wide, which is why it belongs in the predicate.
func TestTransitionAndClaimRefusesACanceledRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Stored      Status
		Cancel      bool
		From        Status
		WantChanged bool
	}{{ // Test 0: An ordinary release takes the lease in the same step.
		Name: "clean release", Stored: StatusPendingApproval, From: StatusPendingApproval,
		WantChanged: true,
	}, { // Test 1: A cancel requested in the gap refuses the claim.
		Name: "canceled in the gap", Stored: StatusPendingApproval, Cancel: true,
		From: StatusPendingApproval, WantChanged: false,
	}, { // Test 2: The wrong starting status refuses it too.
		Name: "wrong from", Stored: StatusRunning, From: StatusPendingApproval, WantChanged: false,
	}, { // Test 3: A canceled run in the right status is still refused, which is the whole point.
		Name: "canceled pending", Stored: StatusPending, Cancel: true, From: StatusPending,
		WantChanged: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			saveRun(t, store, &Run{
				ID: "run_1", Playbook: "site.yml", Kind: KindPipeline, Status: test.Stored,
				CreatedAt: time.Now(), CancelRequested: test.Cancel,
			})
			changed, err := store.TransitionStatusAndClaim(ctx, "run_1", test.From,
				StatusRunning, "coordinator-1")
			if err != nil {
				t.Fatalf("TransitionStatusAndClaim() error = %v", err)
			}
			if changed != test.WantChanged {
				t.Errorf("TransitionStatusAndClaim() changed = %v, want %v", changed, test.WantChanged)
			}
			got := getRun(t, store, "run_1")
			if !test.WantChanged {
				if got.Status != test.Stored {
					t.Errorf("a refused transition still moved the run to %s", got.Status)
				}
				if got.ClaimedBy != "" {
					t.Errorf("a refused transition still stamped the lease %q", got.ClaimedBy)
				}
				return
			}
			if got.Status != StatusRunning || got.ClaimedBy != "coordinator-1" {
				t.Errorf("run is %s held by %q, want running held by the coordinator: a run in the "+
					"to status with no lease is what the abandoned-parent sweep settles",
					got.Status, got.ClaimedBy)
			}
			if got.StartedAt == nil {
				t.Error("the transition recorded no start time")
			}
		})
	}
}

// TestTransitionAndClaimKeepsTheFirstStartTime pins that a second transition does not move the
// start, so a parent restarted by a retry keeps the instant it actually began.
func TestTransitionAndClaimKeepsTheFirstStartTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	first := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Kind: KindSplit, Status: StatusPending,
		CreatedAt: first, StartedAt: &first,
	})
	if _, err := store.TransitionStatusAndClaim(ctx, "run_1", StatusPending,
		StatusRunning, "coordinator-1"); err != nil {
		t.Fatalf("TransitionStatusAndClaim() error = %v", err)
	}
	if got := getRun(t, store, "run_1"); got.StartedAt == nil || !got.StartedAt.Equal(first) {
		t.Errorf("StartedAt = %v, want the original %v", got.StartedAt, first)
	}
}

// TestCancelPendingOnlyTakesAnUnclaimedWaitingRun pins which runs the atomic cancel may settle by
// itself and which are left to cooperative cancellation.
//
// A run already in some executor's hands cannot be ended by a database write: the process is still
// running the tool, so declaring it canceled here would put a terminal outcome on the chain while
// the change kept happening on the hosts. Those go through RequestCancel and the flag their holder
// watches. Getting this boundary wrong in either direction is worse than a missed cancel.
func TestCancelPendingOnlyTakesAnUnclaimedWaitingRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Status      Status
		ClaimedBy   string
		WantChanged bool
	}{{ // Test 0: Waiting in the queue with nobody holding it.
		Name: "unclaimed pending", Status: StatusPending, WantChanged: true,
	}, { // Test 1: Held for approval, which never reached a worker.
		Name: "unclaimed held", Status: StatusPendingApproval, WantChanged: true,
	}, { // Test 2: Claimed but not yet started, which its holder must stop.
		Name: "claimed pending", Status: StatusPending, ClaimedBy: "worker-a", WantChanged: false,
	}, { // Test 3: Executing right now.
		Name: "running", Status: StatusRunning, ClaimedBy: "worker-a", WantChanged: false,
	}, { // Test 4: Running with no lease is still not this call's to settle.
		Name: "running unleased", Status: StatusRunning, WantChanged: false,
	}, { // Test 5: Already finished.
		Name: "succeeded", Status: StatusSucceeded, WantChanged: false,
	}, { // Test 6: Already canceled, so a second click is not a second cancel.
		Name: "already canceled", Status: StatusCanceled, WantChanged: false,
	}, { // Test 7: Rejected runs never executed and are already final.
		Name: "rejected", Status: StatusRejected, WantChanged: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			saveRun(t, store, &Run{
				ID: "run_1", Playbook: "site.yml", Status: test.Status, CreatedAt: time.Now(),
				ClaimedBy: test.ClaimedBy,
			})
			changed, err := store.CancelPending(ctx, "run_1")
			if err != nil {
				t.Fatalf("CancelPending() error = %v", err)
			}
			if changed != test.WantChanged {
				t.Errorf("CancelPending() on a %s run held by %q changed = %v, want %v",
					test.Status, test.ClaimedBy, changed, test.WantChanged)
			}
			got := getRun(t, store, "run_1")
			if test.WantChanged {
				if got.Status != StatusCanceled {
					t.Errorf("status = %s, want canceled", got.Status)
				}
				if got.EndedAt == nil {
					t.Error("the canceled run records no end, so it has no duration in history")
				}
				// A second call finds it terminal and reports no change, so a double click does
				// not record a second cancellation.
				if again, _ := store.CancelPending(ctx, "run_1"); again {
					t.Error("a second cancel of the same run reported a change")
				}
				return
			}
			if got.Status != test.Status {
				t.Errorf("a refused cancel still moved the run to %s", got.Status)
			}
		})
	}

	// A run that is not there reports no change and no error, matching a racing delete.
	changed, err := NewMemStore().CancelPending(context.Background(), "run_gone")
	if err != nil || changed {
		t.Errorf("CancelPending(missing) = (%v, %v), want (false, nil)", changed, err)
	}
}

// TestRequestCancelOnAMissingRunIsNotFound pins that asking to cancel a run nobody has is an error
// the caller can tell apart from a cancel that landed, since the API answers 404 from it.
func TestRequestCancelOnAMissingRunIsNotFound(t *testing.T) {
	t.Parallel()
	err := NewMemStore().RequestCancel(context.Background(), "run_gone")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("RequestCancel(missing) error = %v, want ErrNotFound", err)
	}
}

// TestClaimRefusesEveryRunThatIsNotWaitingWork pins which runs the claim loop may lease.
//
// Each refusal here stands for a way the product has executed something it should not have. A held
// run taken by a worker is an approval gate that did nothing. A parent taken as if it were work is
// a coordination record run as a playbook. A cancel-flagged run taken is a cancellation ignored.
// And a child taken while its parent is still merely pending is a split that a cancel correctly
// refused to start, whose shards ran on real hosts anyway because they were already claimable.
func TestClaimRefusesEveryRunThatIsNotWaitingWork(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	parentID := "run_parent"
	tests := []struct {
		Name        string
		Run         *Run
		Parent      *Run
		WantClaimed bool
	}{{ // Test 0: An ordinary pending run on the default pool is claimed.
		Name: "plain pending", Run: &Run{ID: "run_1", Status: StatusPending, CreatedAt: created},
		WantClaimed: true,
	}, { // Test 1: A run held for approval is never selected, which is what the gate is.
		Name: "held for approval",
		Run:  &Run{ID: "run_1", Status: StatusPendingApproval, CreatedAt: created},
	}, { // Test 2: A rejected run never executes.
		Name: "rejected", Run: &Run{ID: "run_1", Status: StatusRejected, CreatedAt: created},
	}, { // Test 3: A run somebody else already leased.
		Name: "already leased",
		Run:  &Run{ID: "run_1", Status: StatusPending, CreatedAt: created, ClaimedBy: "worker-b"},
	}, { // Test 4: A split parent is a coordination record, not work.
		Name: "split parent",
		Run:  &Run{ID: "run_1", Status: StatusPending, CreatedAt: created, Kind: KindSplit},
	}, { // Test 5: A pipeline parent, likewise.
		Name: "pipeline parent",
		Run:  &Run{ID: "run_1", Status: StatusPending, CreatedAt: created, Kind: KindPipeline},
	}, { // Test 6: A run somebody asked to cancel is not started.
		Name: "cancel requested",
		Run: &Run{ID: "run_1", Status: StatusPending, CreatedAt: created,
			CancelRequested: true},
	}, { // Test 7: A shard whose parent is still pending is not yet claimable.
		Name: "child of a pending parent",
		Run: &Run{ID: "run_1", Status: StatusPending, CreatedAt: created,
			ParentID: &parentID},
		Parent: &Run{ID: parentID, Status: StatusPending, CreatedAt: created, Kind: KindSplit},
	}, { // Test 8: A shard whose parent is running is claimable, which is the working case.
		Name: "child of a running parent",
		Run: &Run{ID: "run_1", Status: StatusPending, CreatedAt: created,
			ParentID: &parentID},
		Parent: &Run{ID: parentID, Status: StatusRunning, CreatedAt: created, Kind: KindSplit,
			ClaimedBy: "coordinator-1"},
		WantClaimed: true,
	}, { // Test 9: A shard whose parent somebody canceled is not started.
		Name: "child of a canceling parent",
		Run: &Run{ID: "run_1", Status: StatusPending, CreatedAt: created,
			ParentID: &parentID},
		Parent: &Run{ID: parentID, Status: StatusRunning, CreatedAt: created, Kind: KindSplit,
			CancelRequested: true},
	}, { // Test 10: A shard whose parent was interrupted is not started.
		Name: "child of an interrupted parent",
		Run: &Run{ID: "run_1", Status: StatusPending, CreatedAt: created,
			ParentID: &parentID},
		Parent: &Run{ID: parentID, Status: StatusInterrupted, CreatedAt: created, Kind: KindSplit},
	}, { // Test 11: A shard whose parent is gone entirely is not started.
		Name: "child of a missing parent",
		Run: &Run{ID: "run_1", Status: StatusPending, CreatedAt: created,
			ParentID: &parentID},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			if test.Parent != nil {
				test.Parent.Playbook = "site.yml"
				saveRun(t, store, test.Parent)
			}
			test.Run.Playbook = "site.yml"
			saveRun(t, store, test.Run)

			got, err := store.Claim(ctx, "worker-a", []string{""})
			if !test.WantClaimed {
				if !errors.Is(err, ErrNonePending) {
					t.Fatalf("Claim() = (%v, %v), want ErrNonePending: nothing here is waiting work",
						got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Claim() error = %v, want the run leased", err)
			}
			if got.ID != "run_1" {
				t.Errorf("Claim() leased %q, want run_1", got.ID)
			}
			if got.ClaimedBy != "worker-a" || got.ClaimedAt == nil {
				t.Errorf("the leased run is held by %q at %v, want a stamped lease",
					got.ClaimedBy, got.ClaimedAt)
			}
			if got.ClaimSecret == "" {
				t.Error("the claim minted no capability, so a report from this worker cannot be " +
					"told apart from any other worker holding the shared relay token")
			}
		})
	}
}

// TestReclaimSettlesARunningRunAsInterrupted pins the sweep's outcome classification: a run whose
// executor stopped renewing is interrupted rather than failed, its lease and its per-claim
// capability are cleared, and it carries a reason.
//
// The distinction matters downstream. Failed means the tool ran and returned non-zero, which a
// retry and an alert both treat as a real result. Interrupted means nobody knows what happened,
// which is why the status exists and why the run says so in its error text.
func TestReclaimSettlesARunningRunAsInterrupted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	stale := time.Now().Add(-10 * time.Minute)
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: stale,
		ClaimedBy: "worker-dead", ClaimedAt: &stale, ClaimSecret: "secret",
	})
	saveRun(t, store, &Run{
		ID: "run_2", Playbook: "site.yml", Status: StatusRunning, CreatedAt: stale,
		ClaimedBy: "worker-dead", ClaimedAt: &stale, Error: "the tool already said why",
	})
	fresh := time.Now()
	saveRun(t, store, &Run{
		ID: "run_3", Playbook: "site.yml", Status: StatusRunning, CreatedAt: stale,
		ClaimedBy: "worker-alive", ClaimedAt: &fresh,
	})

	if _, err := store.ReclaimStale(ctx, 30*time.Second); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}

	swept := getRun(t, store, "run_1")
	if swept.Status != StatusInterrupted {
		t.Errorf("status = %s, want interrupted: nobody knows how this run ended", swept.Status)
	}
	if swept.Error == "" {
		t.Error("the swept run carries no reason, so a reader cannot tell why it stopped")
	}
	if swept.ClaimedBy != "" || swept.ClaimedAt != nil {
		t.Errorf("the swept run still holds the lease %q, so a dead worker still owns it",
			swept.ClaimedBy)
	}
	if swept.ClaimSecret != "" {
		t.Error("the swept run kept its per-claim capability, so a report minted against the lost " +
			"claim would still verify")
	}
	if swept.EndedAt == nil {
		t.Error("the swept run records no end")
	}

	kept := getRun(t, store, "run_2")
	if kept.Error != "the tool already said why" {
		t.Errorf("error = %q, want the executor's own reason kept rather than overwritten", kept.Error)
	}

	alive := getRun(t, store, "run_3")
	if alive.Status != StatusRunning {
		t.Errorf("a run whose lease is fresh was swept to %s", alive.Status)
	}
}

// TestOrphanedChildrenAreSettledByHowFarTheyGot pins what happens to a split or pipeline's children
// when the coordinator holding their rollup dies.
//
// A child nothing started is canceled outright: nothing will ever collect it, and leaving it
// pending means it stays claimable long after its split is over, so a worker would pick it up and
// run a change belonging to a split that already ended. A child already executing is asked to stop
// through the flag its executor watches, because a database write cannot stop a running process. A
// child that already finished keeps its own outcome.
func TestOrphanedChildrenAreSettledByHowFarTheyGot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	created := time.Now().Add(-time.Hour)
	parent := "run_parent"
	saveRun(t, store, &Run{
		ID: parent, Playbook: "site.yml", Kind: KindSplit, Status: StatusInterrupted,
		CreatedAt: created,
	})
	children := []*Run{
		{ID: "run_pending", Status: StatusPending},
		{ID: "run_held", Status: StatusPendingApproval},
		{ID: "run_running", Status: StatusRunning, ClaimedBy: "worker-a"},
		{ID: "run_done", Status: StatusSucceeded, Error: "", Warning: "finished before the parent died"},
	}
	for _, c := range children {
		c.Playbook = "site.yml"
		c.CreatedAt = created
		c.ParentID = &parent
		saveRun(t, store, c)
	}

	if _, err := store.ReclaimStale(ctx, time.Hour); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}

	for _, id := range []string{"run_pending", "run_held"} {
		got := getRun(t, store, id)
		if got.Status != StatusCanceled {
			t.Errorf("%s is %s, want canceled: nothing will collect it and it stays claimable",
				id, got.Status)
		}
		if got.Error != OrphanError() {
			t.Errorf("%s error = %q, want %q", id, got.Error, OrphanError())
		}
		if got.EndedAt == nil {
			t.Errorf("%s records no end", id)
		}
		if got.ClaimedBy != "" || got.ClaimSecret != "" {
			t.Errorf("%s kept a lease or a capability after being settled", id)
		}
	}

	running := getRun(t, store, "run_running")
	if running.Status != StatusRunning {
		t.Errorf("run_running is %s, want still running: a write cannot stop a live process",
			running.Status)
	}
	if !running.CancelRequested {
		t.Error("run_running was not asked to stop, so its executor keeps changing hosts for a " +
			"split that has already ended")
	}

	done := getRun(t, store, "run_done")
	if done.Status != StatusSucceeded {
		t.Errorf("run_done is %s, want its own outcome kept", done.Status)
	}
	if done.Error != "" {
		t.Errorf("run_done was stamped %q, overwriting the outcome it earned", done.Error)
	}
}

// TestAbandonedParentNamesExactlyTheUnrecoverableParents pins the rule the three SQL stores each
// express as a WHERE clause, so all four implementations can be read against one definition.
//
// Two directions are both damaging. Sweeping too widely cancels every gated split and workflow that
// outlives one sweep interval, since a parent awaiting approval is resting legitimately for as long
// as a person takes to decide. Sweeping too narrowly leaves a parent sitting pending forever while
// its children stay claimable with nothing to roll them up.
//
//nolint:funlen // Test function.
func TestAbandonedParentNamesExactlyTheUnrecoverableParents(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)
	recent := cutoff.Add(time.Hour)
	tests := []struct {
		Name          string
		Run           Run
		WantAbandoned bool
	}{{ // Test 0: A pending split parent with no coordinator, old enough, is unrecoverable.
		Name: "pending split, no lease, old",
		Run:  Run{Kind: KindSplit, Status: StatusPending, CreatedAt: old}, WantAbandoned: true,
	}, { // Test 1: A pipeline parent in the same shape, likewise.
		Name: "pending pipeline, no lease, old",
		Run:  Run{Kind: KindPipeline, Status: StatusPending, CreatedAt: old}, WantAbandoned: true,
	}, { // Test 2: A running parent with no lease is the approval-released case nothing else
		// covered: an approved parent goes straight to running, so running-and-unleased means the
		// coordinator never arrived.
		Name: "running split, no lease, old",
		Run:  Run{Kind: KindSplit, Status: StatusRunning, CreatedAt: old}, WantAbandoned: true,
	}, { // Test 3: A parent a live coordinator holds is being coordinated.
		Name: "running split with a lease",
		Run: Run{Kind: KindSplit, Status: StatusRunning, CreatedAt: old,
			ClaimedBy: "coordinator-1"},
	}, { // Test 4: A pending parent with a lease is likewise not abandoned.
		Name: "pending split with a lease",
		Run: Run{Kind: KindSplit, Status: StatusPending, CreatedAt: old,
			ClaimedBy: "coordinator-1"},
	}, { // Test 5: A parent still awaiting approval is resting, not abandoned, however long it
		// waits. Sweeping this would cancel every gated split that outlived a sweep interval.
		Name: "held for approval",
		Run:  Run{Kind: KindSplit, Status: StatusPendingApproval, CreatedAt: old},
	}, { // Test 6: A parent created after the cutoff is too young to judge.
		Name: "pending split, too young",
		Run:  Run{Kind: KindSplit, Status: StatusPending, CreatedAt: recent},
	}, { // Test 7: A parent created exactly at the cutoff is not yet past it.
		Name: "pending split, exactly at the cutoff",
		Run:  Run{Kind: KindSplit, Status: StatusPending, CreatedAt: cutoff},
	}, { // Test 8: An ordinary run is not a parent and is swept by the lease rules instead.
		Name: "plain run", Run: Run{Status: StatusPending, CreatedAt: old},
	}, { // Test 9: A parent that already reached a terminal state needs no sweeping.
		Name: "interrupted parent",
		Run:  Run{Kind: KindSplit, Status: StatusInterrupted, CreatedAt: old},
	}, { // Test 10: A succeeded parent, likewise.
		Name: "succeeded parent",
		Run:  Run{Kind: KindSplit, Status: StatusSucceeded, CreatedAt: old},
	}, { // Test 11: An unknown kind is not a coordination record.
		Name: "unknown kind", Run: Run{Kind: "something-else", Status: StatusPending, CreatedAt: old},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := test.Run
			if got := AbandonedParent(&r, cutoff); got != test.WantAbandoned {
				t.Errorf("AbandonedParent(%s %s, lease %q) = %v, want %v",
					r.Kind, r.Status, r.ClaimedBy, got, test.WantAbandoned)
			}
		})
	}
}

// TestASweptParentSettlesItsChildrenInTheSameSweep pins that interrupting an abandoned parent and
// resolving the children it orphans happen together.
//
// Interrupting the parent is what makes orphan resolution fire, since that keys off an interrupted
// parent. If the two ran in separate sweeps the children would stay claimable for a whole sweep
// interval after their parent was declared dead, which is exactly long enough for a worker to pick
// one up and change hosts for a split that has already ended.
func TestASweptParentSettlesItsChildrenInTheSameSweep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	created := time.Now().Add(-time.Hour)
	parent := "run_parent"
	saveRun(t, store, &Run{
		ID: parent, Playbook: "site.yml", Kind: KindPipeline, Status: StatusPending,
		CreatedAt: created,
	})
	child := &Run{
		ID: "run_step", Playbook: "site.yml", Status: StatusPending, CreatedAt: created,
		ParentID: &parent,
	}
	saveRun(t, store, child)

	if _, err := store.ReclaimStale(ctx, time.Minute); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}

	gotParent := getRun(t, store, parent)
	if gotParent.Status != StatusInterrupted {
		t.Errorf("parent is %s, want interrupted", gotParent.Status)
	}
	if gotParent.Error != AbandonedParentError() {
		t.Errorf("parent error = %q, want %q", gotParent.Error, AbandonedParentError())
	}
	gotChild := getRun(t, store, "run_step")
	if gotChild.Status != StatusCanceled {
		t.Errorf("child is %s after one sweep, want canceled in the same sweep that settled its "+
			"parent, or it stays claimable for a whole interval", gotChild.Status)
	}

	// And a claim loop will not take it afterwards.
	if _, err := store.Claim(ctx, "worker-a", []string{""}); !errors.Is(err, ErrNonePending) {
		t.Errorf("Claim() after the sweep = %v, want ErrNonePending", err)
	}
}

// TestReclaimStaleSettledNamesOnlyWhatThisSweepDrove pins the sweep's attribution.
//
// The caller records an outcome for every run the sweep names, so naming a run somebody else
// finished commits a second, contradictory outcome for a run whose real finisher already committed
// one. A before-and-after diff did exactly that, crediting the sweep with anything that happened to
// finish while it ran. Children are left out because a child's outcome rolls up into its parent.
func TestReclaimStaleSettledNamesOnlyWhatThisSweepDrove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	stale := time.Now().Add(-time.Hour)
	parent := "run_parent"
	saveRun(t, store, &Run{
		ID: "run_top", Playbook: "site.yml", Status: StatusRunning, CreatedAt: stale,
		ClaimedBy: "worker-dead", ClaimedAt: &stale,
	})
	saveRun(t, store, &Run{
		ID: parent, Playbook: "site.yml", Kind: KindSplit, Status: StatusPending, CreatedAt: stale,
	})
	saveRun(t, store, &Run{
		ID: "run_child", Playbook: "site.yml", Status: StatusRunning, CreatedAt: stale,
		ParentID: &parent, ClaimedBy: "worker-dead", ClaimedAt: &stale,
	})
	// A run that finished on its own, before the sweep, with nothing stale about it.
	saveRun(t, store, &Run{
		ID: "run_finished", Playbook: "site.yml", Status: StatusSucceeded, CreatedAt: stale,
	})

	settler, ok := store.(interface {
		ReclaimStaleSettled(context.Context, time.Duration) (int, []string, error)
	})
	if !ok {
		t.Fatal("the memory store no longer reports what its sweep settled")
	}
	_, settled, err := settler.ReclaimStaleSettled(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStaleSettled() error = %v", err)
	}
	want := []string{"run_parent", "run_top"}
	if diff := cmp.Diff(want, settled, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the sweep named the wrong runs (-want +got):\n%s", diff)
	}
}

// TestHeartbeatOnlyRenewsTheHoldersOwnLease pins that a worker cannot keep another worker's run
// alive, and that renewing a lease nobody holds is an error rather than a silent success.
//
// The lease is what the janitor reads to decide a worker is gone. A heartbeat accepted from any
// caller would let a healthy process keep a dead one's run out of the sweep forever, and the run
// would never be requeued or settled.
func TestHeartbeatOnlyRenewsTheHoldersOwnLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	stale := time.Now().Add(-time.Hour)
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: stale,
		ClaimedBy: "worker-a", ClaimedAt: &stale,
	})
	if err := store.Heartbeat(ctx, "run_1", "worker-b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Heartbeat from a worker that does not hold the run = %v, want ErrNotFound", err)
	}
	if got := getRun(t, store, "run_1"); !got.ClaimedAt.Equal(stale) {
		t.Error("a stranger's heartbeat renewed the lease, keeping a dead worker's run out of the sweep")
	}
	if err := store.Heartbeat(ctx, "run_1", "worker-a"); err != nil {
		t.Errorf("Heartbeat from the holder error = %v, want nil", err)
	}
	if got := getRun(t, store, "run_1"); got.ClaimedAt.Equal(stale) {
		t.Error("the holder's heartbeat did not renew the lease")
	}
	if err := store.Heartbeat(ctx, "run_gone", "worker-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Heartbeat on a missing run = %v, want ErrNotFound", err)
	}
}

// TestReclaimStaleSettledLeavesOutANestedParent pins that a parent which is itself somebody's child
// is not named as a run this sweep settled.
//
// A pipeline step that fans out is both: it carries a Kind, so the abandonment rule reaches it, and
// it carries a ParentID, so its outcome rolls up into the pipeline above it rather than standing on
// its own. The existing attribution test only has children with no Kind, so dropping the
// top-level test on the abandoned-parent branch changes nothing it can see. The caller records an
// outcome for every name the sweep returns, so naming this run commits an outcome for a step whose
// pipeline is about to commit one too.
func TestReclaimStaleSettledLeavesOutANestedParent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	stale := time.Now().Add(-time.Hour)
	pipeline := "run_pipeline"
	nested := "run_step_split"
	saveRun(t, store, &Run{
		ID: pipeline, Playbook: "site.yml", Kind: KindPipeline, Status: StatusPending,
		CreatedAt: stale,
	})
	// A pipeline step that fans out: a split parent of its own shards, and a child of the pipeline.
	saveRun(t, store, &Run{
		ID: nested, Playbook: "site.yml", Kind: KindSplit, Status: StatusPending,
		CreatedAt: stale, ParentID: &pipeline,
	})
	saveRun(t, store, &Run{
		ID: "run_shard", Playbook: "site.yml", Status: StatusPending, CreatedAt: stale,
		ParentID: &nested,
	})

	settler, ok := store.(interface {
		ReclaimStaleSettled(context.Context, time.Duration) (int, []string, error)
	})
	if !ok {
		t.Fatal("the memory store no longer reports what its sweep settled")
	}
	_, settled, err := settler.ReclaimStaleSettled(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStaleSettled() error = %v", err)
	}
	want := []string{pipeline}
	if diff := cmp.Diff(want, settled, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the sweep named the wrong runs (-want +got):\n%s", diff)
	}
	// The nested parent is still swept terminal, it just is not the sweep's to report.
	if got := getRun(t, store, nested); got.Status != StatusInterrupted {
		t.Errorf("the nested parent's status = %q, want %q: leaving it out of the report must not "+
			"leave it running forever", got.Status, StatusInterrupted)
	}
}
