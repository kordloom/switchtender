package relay

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// startWorker is a worker of the node's pool that claims under owner.
func startWorker(n *bndNode) *Client {
	return NewClient(NewHTTPTransport(n.URL, "tok", nil))
}

// startCount returns how many entries of method the node's chain holds under run id's paths.
func startCount(t *testing.T, n *bndNode, id, method string) int {
	t.Helper()
	chain, err := n.Audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	count := 0
	for _, e := range chain {
		if e.Method == method && strings.HasPrefix(e.Path, "/runs/"+id+"/") {
			count++
		}
	}
	return count
}

// runOnce has worker claim, start, and finish run id as succeeded, failing the test on any step
// that does not go through.
func runOnce(t *testing.T, worker *Client, owner, id string) {
	t.Helper()
	ctx := context.Background()
	leased, err := worker.Claim(ctx, owner, []string{""})
	if err != nil || leased.ID != id {
		t.Fatalf("Claim() = %v, %v, want %s", leased, err, id)
	}
	if moved, err := worker.StartClaimed(ctx, id, owner, "", time.Now()); err != nil || !moved {
		t.Fatalf("StartClaimed() = %t, %v, want the run started", moved, err)
	}
	code := 0
	leased.Status, leased.ExitCode = run.StatusSucceeded, &code
	if err := worker.Save(ctx, leased); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
}

// TestARelayClaimOfARunThatAlreadyRanIsRefused pins that an attempt starts once on a relay worker.
// A database writer resets a run a worker already ran to pending with its claim cleared, so the
// next claim leases it again under a valid seal. The control node fixes each attempt's start on
// the chain before a worker hears it may run, so the second worker's start is answered as not
// moved and the run is ended failed with the refusal. A worker that reports without starting, as
// one claimed before the start endpoint existed does, is refused the same way. Either way the chain
// holds one start and one outcome, and the run is announced once more, failed.
func TestARelayClaimOfARunThatAlreadyRanIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		// ReportOnly has the second worker report the run finished without starting it.
		ReportOnly bool
	}{{ // Test 0: The second worker's start is refused.
		Name: "start",
	}, { // Test 1: The second worker's report without a start is refused.
		Name: "report without a start", ReportOnly: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			n := newBndNode(t, bndConfig{})
			id := n.submitted(t, "run_once").ID
			runOnce(t, startWorker(n), bndWorker, id)
			if got := startCount(t, n, id, audit.MethodStart); got != 1 {
				t.Fatalf("the honest run left %d starts on the chain, want 1", got)
			}
			n.rewrite(t, id, func(r *run.Run) {
				r.Status, r.ClaimedBy, r.ClaimSecret, r.Error = run.StatusPending, "", "", ""
				r.ClaimedAt, r.StartedAt, r.EndedAt, r.ExitCode = nil, nil, nil, nil
			})
			second := startWorker(n)
			leased, err := second.Claim(ctx, "worker-2", []string{""})
			if err != nil || leased.ID != id {
				t.Fatalf("Claim() = %v, %v, want the reset run leased", leased, err)
			}
			if test.ReportOnly {
				code := 0
				leased.Status, leased.ExitCode = run.StatusSucceeded, &code
				err := second.Save(ctx, leased)
				if err == nil || !strings.Contains(err.Error(), "start could not be recorded") {
					t.Errorf("Save() error = %v, want the report refused for its start", err)
				}
			} else if moved, err := second.StartClaimed(ctx, id, "worker-2", "", time.Now()); err != nil ||
				moved {
				t.Errorf("StartClaimed() = %t, %v, want the replay answered as not moved", moved, err)
			}
			got, err := n.Store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != run.StatusFailed || !strings.HasPrefix(got.Error, "refused") ||
				!strings.Contains(got.Error, "already started once") {
				t.Errorf("the replayed run is %s (%s), want failed and refused as started once",
					got.Status, got.Error)
			}
			if got := startCount(t, n, id, audit.MethodStart); got != 1 {
				t.Errorf("chain holds %d starts, want 1", got)
			}
			if got := startCount(t, n, id, audit.MethodRun); got != 1 {
				t.Errorf("chain holds %d outcomes, want 1", got)
			}
			want := []string{id + " running", id + " succeeded", id + " failed"}
			if got := n.announcements(); !slices.Equal(got, want) {
				t.Errorf("announcements = %v, want %v", got, want)
			}
		})
	}
}

// TestAReLeasedRunAfterALostWorkerRunsOnce pins the honest path the start entry's placement keeps
// open. A worker claims a run and is lost before it starts it, and the sweep requeues the run at
// the same attempt. The start is recorded when a worker starts the run, not when it claims it, so
// the next worker that claims the run starts and finishes it, and the chain holds one start. The
// lost worker waking after the sweep is refused its start under the claim that ended and records
// nothing.
func TestAReLeasedRunAfterALostWorkerRunsOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		// LateStart has the lost worker ask to start after the sweep.
		LateStart bool
	}{{ // Test 0: The next worker runs the requeued run.
		Name: "next worker",
	}, { // Test 1: The lost worker's late start is refused and records no start.
		Name: "late start", LateStart: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			n := newBndNode(t, bndConfig{})
			id := n.submitted(t, "run_lost").ID
			lost := startWorker(n)
			if leased, err := lost.Claim(ctx, bndWorker, []string{""}); err != nil || leased.ID != id {
				t.Fatalf("Claim() = %v, %v, want %s", leased, err, id)
			}
			// A negative lifetime is the sweep without waiting a lease's worth of wall clock.
			if swept, err := n.Store.ReclaimStale(ctx, -time.Minute); err != nil || swept != 1 {
				t.Fatalf("ReclaimStale() = %d, %v, want the lost claim requeued", swept, err)
			}
			if test.LateStart {
				if moved, err := lost.StartClaimed(ctx, id, bndWorker, "", time.Now()); err == nil && moved {
					t.Errorf("the lost worker started a run its claim no longer holds")
				}
				if got := startCount(t, n, id, audit.MethodStart); got != 0 {
					t.Errorf("the refused late start left %d starts on the chain, want 0", got)
				}
			}
			runOnce(t, startWorker(n), "worker-2", id)
			got, err := n.Store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != run.StatusSucceeded {
				t.Errorf("the re-leased run is %s (%s), want succeeded", got.Status, got.Error)
			}
			if got := startCount(t, n, id, audit.MethodStart); got != 1 {
				t.Errorf("chain holds %d starts, want 1", got)
			}
		})
	}
}
