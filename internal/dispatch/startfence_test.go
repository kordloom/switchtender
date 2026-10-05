package dispatch

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAStalledClaimantCannotStartARunItNoLongerHolds pins the start fence on the claim rather than
// the status. A claimant that stalled past its lease wakes up holding the run as it claimed it. The
// janitor has requeued the run, which clears the claim and its capability and leaves it pending,
// and another slot of the same process may have claimed it again under the same owner name.
//
// A fence on the status alone matched both, because the run still read pending: the stale claimant
// moved it to running and executed its tool under a claim that had ended. The stealback test covers
// a run already running elsewhere, which the status alone did catch.
func TestAStalledClaimantCannotStartARunItNoLongerHolds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		// ClaimAgain claims the requeued run again under the stale claimant's own owner name.
		ClaimAgain bool
		WantOwner  string
	}{{ // Test 0: Requeued, and nobody has claimed it since.
		Name: "requeued", WantOwner: "",
	}, { // Test 1: Requeued and claimed again by another slot of the same process.
		Name: "claimed again", ClaimAgain: true, WantOwner: "worker-A",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			var executions atomic.Int32
			runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec, io.Writer) (
				roundhouse.Result, error) {
				executions.Add(1)
				return roundhouse.Result{}, nil
			})
			d := New(store, runner, nil, WithOwner("worker-A"), WithNoJanitor(),
				WithClaimGate(func() error { return errNoClaimingInThisTest }))
			defer d.Close()
			queue := []string{"q-stall"}
			if err := store.Save(ctx, &run.Run{ID: "run_stall", Playbook: "site.yml",
				Status: run.StatusPending, Queue: queue[0], Tool: run.ToolBash, Command: "true",
				CreatedAt: time.Now()}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			stale, err := store.Claim(ctx, "worker-A", queue)
			if err != nil {
				t.Fatalf("Claim() error = %v", err)
			}
			if _, err := store.ReclaimStale(ctx, -time.Minute); err != nil {
				t.Fatalf("ReclaimStale() error = %v", err)
			}
			if test.ClaimAgain {
				if _, err := store.Claim(ctx, "worker-A", queue); err != nil {
					t.Fatalf("second Claim() error = %v", err)
				}
			}

			var finished atomic.Int32
			d.streamSpec(ctx, stale, false, nil,
				func(roundhouse.Result, error, *masker, *run.SummaryFold) run.Status {
					finished.Add(1)
					return run.StatusSucceeded
				})

			if n := executions.Load(); n != 0 {
				t.Errorf("the stalled claimant executed its tool %d time(s) under a claim that ended", n)
			}
			if n := finished.Load(); n != 0 {
				t.Errorf("the stalled claimant finalized %d time(s) a run it no longer held", n)
			}
			got, err := store.Get(ctx, "run_stall")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != run.StatusPending || got.ClaimedBy != test.WantOwner {
				t.Errorf("run is %s held by %q, want pending held by %q", got.Status, got.ClaimedBy,
					test.WantOwner)
			}
		})
	}
}
