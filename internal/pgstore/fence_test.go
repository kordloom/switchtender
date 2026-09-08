package pgstore_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/run"
)

// sharedOnce guards the one open database these tests share.
var (
	sharedOnce sync.Once
	sharedDB   *pgstore.DB
	sharedErr  error
)

// openShared returns the one *pgstore.DB the parallel tests in this package share.
//
// Opening per test was tried and reverted. Open runs the whole migration inside one transaction that
// takes AccessExclusiveLock on every table, so a dozen parallel tests each opening their own handle
// had migrations and ordinary writes deadlocking against each other. A real deployment opens once per
// process, so one handle per test binary is both the faithful shape and the working one.
func openShared(t *testing.T) *pgstore.DB {
	t.Helper()
	dsn := testDSN(t)
	sharedOnce.Do(func() { sharedDB, sharedErr = pgstore.Open(dsn) })
	if sharedErr != nil {
		t.Fatalf("Open() error = %v", sharedErr)
	}
	return sharedDB
}

// fenceStore returns the shared database's run store. Each test builds runs under ids of its own, so
// the tests share a database without sharing rows and can run in parallel.
func fenceStore(t *testing.T) (context.Context, run.Store) {
	t.Helper()
	return context.Background(), openShared(t).Runs()
}

// saveRun stores r and fails the test if the write is refused, so a fence test never mistakes a
// broken setup for the refusal it is trying to prove.
func saveRun(t *testing.T, ctx context.Context, s run.Store, r *run.Run) {
	t.Helper()
	if err := s.Save(ctx, r); err != nil {
		t.Fatalf("Save(%s) error = %v", r.ID, err)
	}
}

// TestClaimRefusesEveryUnclaimableRun pins the claim predicate one condition at a time. Claim is the
// gate between a stored row and code executing on real hosts, so every clause in its WHERE is a
// safety fence and each one has to be proven separately: a predicate that accidentally drops a
// clause still passes any test that only exercises the happy path. The parent-status clause in
// particular has its own incident on record, where a split canceled before its coordinator started
// had its shards claimed and executed anyway.
//
//nolint:funlen // Test function.
func TestClaimRefusesEveryUnclaimableRun(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	queue := fmt.Sprintf("q_claimfence_%d", time.Now().UnixNano())

	// Each case builds the rows it needs under a queue of its own, so one case cannot claim
	// another's work and the refusals stay independent.
	tests := []struct {
		Name  string
		Build func(t *testing.T, q string)
	}{{ // Test 0: A cancel requested while the run waited must not be started.
		Name: "cancel requested",
		Build: func(t *testing.T, q string) {
			r := newPending(q, "cancelreq")
			r.CancelRequested = true
			saveRun(t, ctx, s, r)
		},
	}, { // Test 1: A run another worker already holds is not up for grabs.
		Name: "already claimed",
		Build: func(t *testing.T, q string) {
			r := newPending(q, "claimed")
			r.ClaimedBy = "someone-else"
			claimed := time.Now().UTC()
			r.ClaimedAt = &claimed
			saveRun(t, ctx, s, r)
		},
	}, { // Test 2: A coordinator run carries a kind and is driven, never claimed.
		Name: "coordinator kind",
		Build: func(t *testing.T, q string) {
			r := newPending(q, "kinded")
			r.Kind = "split"
			saveRun(t, ctx, s, r)
		},
	}, { // Test 3: A shard under a merely pending parent is not claimable. This is the incident.
		Name: "parent still pending",
		Build: func(t *testing.T, q string) {
			parent := newPending(q, "pendingparent")
			parent.Kind = "split"
			saveRun(t, ctx, s, parent)
			child := newPending(q, "childofpending")
			pid := parent.ID
			child.ParentID = &pid
			saveRun(t, ctx, s, child)
		},
	}, { // Test 4: Nor one under a running parent whose cancel was requested.
		Name: "parent canceling",
		Build: func(t *testing.T, q string) {
			parent := newPending(q, "cancelingparent")
			parent.Kind = "split"
			parent.Status = run.StatusRunning
			parent.CancelRequested = true
			saveRun(t, ctx, s, parent)
			child := newPending(q, "childofcanceling")
			pid := parent.ID
			child.ParentID = &pid
			saveRun(t, ctx, s, child)
		},
	}, { // Test 5: A run awaiting an approver has not been approved, so it must not start.
		Name: "pending approval",
		Build: func(t *testing.T, q string) {
			r := newPending(q, "awaiting")
			r.Status = run.StatusPendingApproval
			saveRun(t, ctx, s, r)
		},
	}, { // Test 6: A run on a queue this worker does not serve is not its work.
		Name: "other queue",
		Build: func(t *testing.T, q string) {
			r := newPending(q+"_elsewhere", "otherqueue")
			saveRun(t, ctx, s, r)
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			q := fmt.Sprintf("%s_%d", queue, testNum)
			test.Build(t, q)
			got, err := s.Claim(ctx, "worker-a", []string{q})
			if !errors.Is(err, run.ErrNonePending) {
				t.Fatalf("%s: Claim() returned run %v, err %v; want run.ErrNonePending. A claim "+
					"that wins here starts code on real hosts that nothing authorized.",
					test.Name, idOf(got), err)
			}
		})
	}
}

// TestClaimAcceptsTheRunsItShould is the other half of the fence. A refusal test alone is satisfied
// by a Claim that never returns anything, so the same predicate has to be shown letting real work
// through: a plain pending run, and a child whose parent actually reached running.
func TestClaimAcceptsTheRunsItShould(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	base := fmt.Sprintf("q_claimok_%d", time.Now().UnixNano())

	t.Run("test 0", func(t *testing.T) { // Test 0: An ordinary pending run is claimable.
		t.Parallel()
		q := base + "_0"
		r := newPending(q, "plain")
		saveRun(t, ctx, s, r)
		got, err := s.Claim(ctx, "worker-a", []string{q})
		if err != nil {
			t.Fatalf("Claim() error = %v, want the pending run", err)
		}
		if got.ID != r.ID {
			t.Errorf("claimed %s, want %s", got.ID, r.ID)
		}
		if got.ClaimedBy != "worker-a" {
			t.Errorf("ClaimedBy = %q, want worker-a", got.ClaimedBy)
		}
		if got.ClaimSecret == "" {
			t.Error("the claim returned no capability secret, so the executor cannot prove its " +
				"lease on the relay path")
		}
		if got.ClaimedAt == nil || got.ClaimedAt.IsZero() {
			t.Error("the claim stamped no lease time, so the stale sweep can never age it")
		}
	})
	t.Run("test 1", func(t *testing.T) { // Test 1: A child under a running parent is claimable.
		t.Parallel()
		q := base + "_1"
		parent := newPending(q, "runningparent")
		parent.Kind = "split"
		parent.Status = run.StatusRunning
		saveRun(t, ctx, s, parent)
		child := newPending(q, "childofrunning")
		pid := parent.ID
		child.ParentID = &pid
		saveRun(t, ctx, s, child)
		got, err := s.Claim(ctx, "worker-b", []string{q})
		if err != nil {
			t.Fatalf("Claim() error = %v, want the shard under the running parent", err)
		}
		if got.ID != child.ID {
			t.Errorf("claimed %s, want the child %s", got.ID, child.ID)
		}
	})
}

// TestClaimNeverHandsOneRunToTwoWorkers pins the exclusivity the SKIP LOCKED row lock provides.
// Every worker in a cluster polls this method against the same database, so a row handed out twice
// is the same playbook applied twice to the same hosts. Run with -race.
func TestClaimNeverHandsOneRunToTwoWorkers(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	q := fmt.Sprintf("q_claimrace_%d", time.Now().UnixNano())

	const runs = 8
	const workers = 12
	for i := range runs {
		saveRun(t, ctx, s, newPending(q, fmt.Sprintf("race%d", i)))
	}

	var mu sync.Mutex
	seen := map[string]string{}
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			for {
				got, err := s.Claim(ctx, owner, []string{q})
				if err != nil {
					return
				}
				mu.Lock()
				if prev, dup := seen[got.ID]; dup {
					t.Errorf("run %s was claimed by both %s and %s: the same work executes twice "+
						"on the same hosts", got.ID, prev, owner)
				}
				seen[got.ID] = owner
				mu.Unlock()
			}
		}(fmt.Sprintf("worker-%d", w))
	}
	wg.Wait()
	if len(seen) != runs {
		t.Errorf("claimed %d distinct runs, want %d: work was left stranded", len(seen), runs)
	}
}

// TestTransitionStatusAndClaimRefusesACanceledRun pins the fence the method's own comment says was
// bought with an incident: cancel is a flag, not a status, so a compare-and-swap that checks only
// the status cannot see it, and a pipeline canceled after approval still won the swap and executed.
// The flag has to be in the predicate, not checked before it.
func TestTransitionStatusAndClaimRefusesACanceledRun(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()

	tests := []struct {
		Name       string
		Cancel     bool
		From       run.Status
		Stored     run.Status
		WantMoved  bool
		WantReason string
	}{{ // Test 0: The flag alone blocks the swap even though the status matches.
		Name: "canceled", Cancel: true, From: run.StatusPendingApproval,
		Stored: run.StatusPendingApproval, WantMoved: false,
		WantReason: "a canceled run won the status swap and would have executed on real hosts",
	}, { // Test 1: With no cancel the same swap succeeds, so the fence is not simply always closed.
		Name: "clean", Cancel: false, From: run.StatusPendingApproval,
		Stored: run.StatusPendingApproval, WantMoved: true,
		WantReason: "an approved run could not be started at all",
	}, { // Test 2: A status that does not match the expected from is refused.
		Name: "wrong from", Cancel: false, From: run.StatusPendingApproval,
		Stored: run.StatusPending, WantMoved: false,
		WantReason: "the swap ignored the status it was told to move from",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := newPending(fmt.Sprintf("q_tsc_%d_%d", stamp, testNum), test.Name)
			r.Status = test.Stored
			r.CancelRequested = test.Cancel
			saveRun(t, ctx, s, r)
			moved, err := s.TransitionStatusAndClaim(ctx, r.ID, test.From, run.StatusRunning, "w1")
			if err != nil {
				t.Fatalf("TransitionStatusAndClaim() error = %v", err)
			}
			if moved != test.WantMoved {
				t.Fatalf("moved = %v, want %v: %s", moved, test.WantMoved, test.WantReason)
			}
			got, err := s.Get(ctx, r.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if test.WantMoved {
				if got.Status != run.StatusRunning {
					t.Errorf("status = %s, want running", got.Status)
				}
				if got.ClaimedBy != "w1" {
					t.Errorf("ClaimedBy = %q, want w1: the run must never be visible in the new "+
						"status without an owner", got.ClaimedBy)
				}
				if got.StartedAt == nil {
					t.Error("StartedAt was not stamped by the same statement that started the run")
				}
				return
			}
			if got.Status != test.Stored {
				t.Errorf("status = %s, want it left at %s", got.Status, test.Stored)
			}
			if got.ClaimedBy != "" {
				t.Errorf("ClaimedBy = %q, want empty: a refused transition must not leave a lease",
					got.ClaimedBy)
			}
		})
	}
}

// TestFinalizeRunningHonorsTheOwnerFence pins the lease check on the terminal write. The field's own
// doc records why it exists: a second dispatcher on a shared database, whose own start save failed
// and whose heartbeats lapsed, could finalize a run it never held. The empty owner is the documented
// bypass for callers that are not executors, so it is pinned as deliberate rather than left to look
// like a hole.
func TestFinalizeRunningHonorsTheOwnerFence(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()

	tests := []struct {
		Name      string
		Holder    string
		Owner     string
		Status    run.Status
		WantEnded bool
	}{{ // Test 0: The holder of the lease may finalize its own run.
		Name: "holder", Holder: "w1", Owner: "w1", Status: run.StatusRunning, WantEnded: true,
	}, { // Test 1: Another executor may not, however sure it is that the run is dead.
		Name: "stranger", Holder: "w1", Owner: "w2", Status: run.StatusRunning, WantEnded: false,
	}, { // Test 2: An empty owner is the documented bypass for a non-executor caller.
		Name: "no owner", Holder: "w1", Owner: "", Status: run.StatusRunning, WantEnded: true,
	}, { // Test 3: A run that is not running cannot be finalized at all, holder or not.
		Name: "not running", Holder: "w1", Owner: "w1", Status: run.StatusPending, WantEnded: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := newPending(fmt.Sprintf("q_fin_%d_%d", stamp, testNum), test.Name)
			r.Status = test.Status
			r.ClaimedBy = test.Holder
			saveRun(t, ctx, s, r)
			code := 0
			ok, err := s.FinalizeRunning(ctx, r.ID, run.Finalization{
				Status: run.StatusSucceeded, ExitCode: &code, EndedAt: time.Now().UTC(),
				Owner: test.Owner,
			})
			if err != nil {
				t.Fatalf("FinalizeRunning() error = %v", err)
			}
			if ok != test.WantEnded {
				t.Fatalf("finalized = %v, want %v: a write that lands without the lease lets a "+
					"process that lost its claim declare somebody else's run finished", ok,
					test.WantEnded)
			}
			got, err := s.Get(ctx, r.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if test.WantEnded && got.Status != run.StatusSucceeded {
				t.Errorf("status = %s, want succeeded", got.Status)
			}
			if !test.WantEnded && got.Status == run.StatusSucceeded {
				t.Errorf("status = succeeded after a refused finalize")
			}
		})
	}
}

// TestApplyRunningProgressHonorsTheOwnerFence pins the same lease check on the progress write. Its
// doc states the danger plainly: a report still in flight when the sweep settled a run must not
// resurrect it. The fence is on both the status and the owner, so both are proven.
func TestApplyRunningProgressHonorsTheOwnerFence(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()

	tests := []struct {
		Name       string
		Holder     string
		Owner      string
		Status     run.Status
		WantWrote  bool
		WantReason string
	}{{ // Test 0: The lease holder's report on a running run lands.
		Name: "holder", Holder: "w1", Owner: "w1", Status: run.StatusRunning, WantWrote: true,
		WantReason: "the legitimate executor could not report its own progress",
	}, { // Test 1: A stranger's report is refused even while the run is running.
		Name: "stranger", Holder: "w1", Owner: "w2", Status: run.StatusRunning, WantWrote: false,
		WantReason: "a process with no lease wrote into a run it does not hold",
	}, { // Test 2: An empty owner is not a bypass here, unlike the terminal write.
		Name: "no owner", Holder: "w1", Owner: "", Status: run.StatusRunning, WantWrote: false,
		WantReason: "an ownerless progress report was accepted",
	}, { // Test 3: A report arriving after the sweep interrupted the run must not revive it.
		Name: "already settled", Holder: "w1", Owner: "w1", Status: run.StatusInterrupted,
		WantWrote: false, WantReason: "a report in flight resurrected a run the sweep had settled",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := newPending(fmt.Sprintf("q_prog_%d_%d", stamp, testNum), test.Name)
			r.Status = test.Status
			r.ClaimedBy = test.Holder
			saveRun(t, ctx, s, r)
			ok, err := s.ApplyRunningProgress(ctx, r.ID, test.Owner,
				run.Progress{Warning: "late report"})
			if err != nil {
				t.Fatalf("ApplyRunningProgress() error = %v", err)
			}
			if ok != test.WantWrote {
				t.Fatalf("wrote = %v, want %v: %s", ok, test.WantWrote, test.WantReason)
			}
			got, err := s.Get(ctx, r.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !test.WantWrote && got.Warning == "late report" {
				t.Errorf("the refused report still landed its warning on the run")
			}
			if test.WantWrote && got.Warning != "late report" {
				t.Errorf("warning = %q, want the accepted report's text", got.Warning)
			}
			if got.Status != test.Status {
				t.Errorf("status = %s, want it left at %s: a progress report must never move a "+
					"run's status", got.Status, test.Status)
			}
		})
	}
}

// TestHeartbeatRefusesAStrangersRenewal pins that only the lease holder can renew. A heartbeat from
// a worker that lost its claim would keep a dead lease alive and stop the sweep from ever
// reclaiming the run.
func TestHeartbeatRefusesAStrangersRenewal(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()

	tests := []struct {
		Name   string
		Holder string
		Owner  string
		Want   error
	}{{ // Test 0: The holder renews.
		Name: "holder", Holder: "w1", Owner: "w1", Want: nil,
	}, { // Test 1: A stranger cannot keep somebody else's lease alive.
		Name: "stranger", Holder: "w1", Owner: "w2", Want: run.ErrNotFound,
	}, { // Test 2: Nor can an empty owner, which is what an unclaimed row holds.
		Name: "empty owner", Holder: "w1", Owner: "", Want: run.ErrNotFound,
	}, { // Test 3: An unclaimed run has no lease to renew.
		Name: "unclaimed", Holder: "", Owner: "w1", Want: run.ErrNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := newPending(fmt.Sprintf("q_hb_%d_%d", stamp, testNum), test.Name)
			r.Status = run.StatusRunning
			r.ClaimedBy = test.Holder
			saveRun(t, ctx, s, r)
			err := s.Heartbeat(ctx, r.ID, test.Owner)
			if !errors.Is(err, test.Want) {
				t.Errorf("Heartbeat() error = %v, want %v: a renewal from a process without the "+
					"lease keeps a dead run from ever being reclaimed", err, test.Want)
			}
		})
	}
	t.Run("test 4", func(t *testing.T) { // Test 4: A run that does not exist cannot be renewed.
		t.Parallel()
		if err := s.Heartbeat(ctx, "run_no_such_heartbeat", "w1"); !errors.Is(err, run.ErrNotFound) {
			t.Errorf("Heartbeat() on a missing run = %v, want run.ErrNotFound", err)
		}
	})
}

// TestCancelPendingRefusesAnythingUnderWay pins that the atomic cancel only settles a run that is
// genuinely still waiting. Canceling a claimed or running run out from under its executor would
// mark it terminal while the process is still changing real hosts, which is why that path sets the
// cooperative flag instead.
func TestCancelPendingRefusesAnythingUnderWay(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()

	tests := []struct {
		Name         string
		Status       run.Status
		Holder       string
		WantCanceled bool
	}{{ // Test 0: An unclaimed pending run is cancelable outright.
		Name: "pending", Status: run.StatusPending, Holder: "", WantCanceled: true,
	}, { // Test 1: So is one waiting on an approver, since nothing has started.
		Name: "pending approval", Status: run.StatusPendingApproval, Holder: "", WantCanceled: true,
	}, { // Test 2: A claimed pending run belongs to an executor already.
		Name: "claimed", Status: run.StatusPending, Holder: "w1", WantCanceled: false,
	}, { // Test 3: A running run must be asked to stop, not declared finished.
		Name: "running", Status: run.StatusRunning, Holder: "w1", WantCanceled: false,
	}, { // Test 4: A run that already ended is not cancelable.
		Name: "succeeded", Status: run.StatusSucceeded, Holder: "", WantCanceled: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := newPending(fmt.Sprintf("q_cp_%d_%d", stamp, testNum), test.Name)
			r.Status = test.Status
			r.ClaimedBy = test.Holder
			saveRun(t, ctx, s, r)
			ok, err := s.CancelPending(ctx, r.ID)
			if err != nil {
				t.Fatalf("CancelPending() error = %v", err)
			}
			if ok != test.WantCanceled {
				t.Fatalf("canceled = %v, want %v: canceling a run an executor holds marks it "+
					"terminal while the process is still changing real hosts", ok, test.WantCanceled)
			}
			got, err := s.Get(ctx, r.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if test.WantCanceled {
				if got.Status != run.StatusCanceled {
					t.Errorf("status = %s, want canceled", got.Status)
				}
				if got.EndedAt == nil {
					t.Error("a canceled run carries no end time")
				}
				return
			}
			if got.Status == run.StatusCanceled && test.Status != run.StatusCanceled {
				t.Errorf("status = canceled after a refused cancel")
			}
		})
	}
	t.Run("test 5", func(t *testing.T) { // Test 5: A missing run reports no cancel, not an error.
		t.Parallel()
		ok, err := s.CancelPending(ctx, "run_no_such_cancel")
		if err != nil {
			t.Fatalf("CancelPending() on a missing run error = %v", err)
		}
		if ok {
			t.Error("CancelPending reported it canceled a run that does not exist")
		}
	})
}

// TestSaveNeverErasesACancelAnotherProcessRequested pins the GREATEST merge on cancel_requested. A
// whole-run save from a stale in-memory snapshot is the ordinary way a run is written, and one
// racing a cancel would otherwise silently clear the flag, leaving the run to execute after
// somebody asked for it to stop.
func TestSaveNeverErasesACancelAnotherProcessRequested(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	q := fmt.Sprintf("q_cancelmerge_%d", time.Now().UnixNano())

	r := newPending(q, "mergecancel")
	saveRun(t, ctx, s, r)

	// A snapshot taken before the cancel, the way a dispatcher holds one across a run's life.
	stale, err := s.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stale.CancelRequested {
		t.Fatal("the snapshot already carries a cancel, so this proves nothing")
	}

	// Another process requests the cancel while the snapshot is held.
	if err := s.RequestCancel(ctx, r.ID); err != nil {
		t.Fatalf("RequestCancel() error = %v", err)
	}

	// The stale snapshot is written back, as an ordinary progress save would be.
	stale.Warning = "some later update"
	saveRun(t, ctx, s, stale)

	got, err := s.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.CancelRequested {
		t.Fatal("a save from a stale snapshot erased a cancel another process had already " +
			"requested, so the run goes on to execute after somebody asked it to stop")
	}
	if got.Warning != "some later update" {
		t.Errorf("Warning = %q: the merge must protect the cancel flag without discarding the "+
			"rest of the save", got.Warning)
	}
	// And the fence downstream still holds: a canceled run is not claimable.
	if _, err := s.Claim(ctx, "w1", []string{q}); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("Claim() after the merged cancel = %v, want run.ErrNonePending", err)
	}
}

// TestRequestCancelReportsAMissingRun pins that the cancel path does not silently succeed on a run
// that is not there. A caller told the cancel landed when no row was touched would report to a
// person that a run is stopping when nothing was asked to stop.
func TestRequestCancelReportsAMissingRun(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	if err := s.RequestCancel(ctx, "run_no_such_request_cancel"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("RequestCancel() on a missing run = %v, want run.ErrNotFound", err)
	}
}

// TestStampApprovedSpecIsNarrowAndReportsAMissingRun covers the approval digest write, which had no
// test at all. The method's contract is that it records what an approver decided without clobbering
// a concurrent claim or cancel, so both the narrowness and the missing-run refusal are pinned. The
// digest is what ties an approval to the exact spec approved, so a write that quietly did nothing
// would let a run execute as approved against a spec nobody agreed to.
func TestStampApprovedSpecIsNarrowAndReportsAMissingRun(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	q := fmt.Sprintf("q_stamp_%d", time.Now().UnixNano())

	t.Run("test 0", func(t *testing.T) { // Test 0: A missing run is refused, never silently ignored.
		t.Parallel()
		err := s.StampApprovedSpec(ctx, "run_no_such_stamp", "deadbeef")
		if !errors.Is(err, run.ErrNotFound) {
			t.Errorf("StampApprovedSpec() on a missing run = %v, want run.ErrNotFound", err)
		}
	})
	t.Run("test 1", func(t *testing.T) { // Test 1: The stamp lands and touches nothing else.
		t.Parallel()
		r := newPending(q+"_1", "stampme")
		r.Status = run.StatusPendingApproval
		r.ClaimedBy = "w1"
		r.CancelRequested = true
		r.Warning = "keep me"
		saveRun(t, ctx, s, r)
		if err := s.StampApprovedSpec(ctx, r.ID, "sha256:abc"); err != nil {
			t.Fatalf("StampApprovedSpec() error = %v", err)
		}
		got, err := s.Get(ctx, r.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.ApprovedSpecDigest != "sha256:abc" {
			t.Errorf("ApprovedSpecDigest = %q, want sha256:abc", got.ApprovedSpecDigest)
		}
		if !got.CancelRequested {
			t.Error("the stamp cleared a cancel another process had requested")
		}
		if got.ClaimedBy != "w1" {
			t.Errorf("ClaimedBy = %q, want w1: the stamp clobbered a concurrent claim", got.ClaimedBy)
		}
		if got.Status != run.StatusPendingApproval {
			t.Errorf("status = %s, want it untouched", got.Status)
		}
		if got.Warning != "keep me" {
			t.Errorf("Warning = %q, want it untouched", got.Warning)
		}
	})
	t.Run("test 2", func(t *testing.T) { // Test 2: An empty digest is stored as given, not refused.
		t.Parallel()
		r := newPending(q+"_2", "stampempty")
		r.ApprovedSpecDigest = "previous"
		saveRun(t, ctx, s, r)
		if err := s.StampApprovedSpec(ctx, r.ID, ""); err != nil {
			t.Fatalf("StampApprovedSpec() with an empty digest error = %v", err)
		}
		got, err := s.Get(ctx, r.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.ApprovedSpecDigest != "" {
			t.Errorf("ApprovedSpecDigest = %q, want the empty stamp to have cleared it",
				got.ApprovedSpecDigest)
		}
	})
}

// newPending builds a plain claimable pending run on the given queue. The id carries the queue and
// the label so rows from parallel tests sharing one database never collide.
func newPending(queue, label string) *run.Run {
	return &run.Run{
		ID:        fmt.Sprintf("run_%s_%s", queue, label),
		Status:    run.StatusPending,
		CreatedAt: time.Now().UTC(),
		Queue:     queue,
		Tool:      "bash",
		Command:   "echo hi",
	}
}

// idOf renders a run's id for a failure message, tolerating a nil run.
func idOf(r *run.Run) string {
	if r == nil {
		return "<nil>"
	}
	return r.ID
}
