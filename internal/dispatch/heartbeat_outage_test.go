package dispatch

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// heartbeatStore refuses heartbeats, either the way a refused connection does or the way a store says
// the lease is no longer this owner's, and counts what it refused.
type heartbeatStore struct {
	run.Store
	// lost makes every heartbeat report the lease is gone rather than unreachable.
	lost bool
	// until is when heartbeats start succeeding again, for the unreachable case.
	until time.Time
	// refused counts the heartbeats turned away.
	refused atomic.Int64
}

// Heartbeat refuses while the outage window is open, or always when the lease is reported gone.
func (h *heartbeatStore) Heartbeat(ctx context.Context, id, owner string) error {
	if h.lost {
		h.refused.Add(1)
		return run.ErrNotFound
	}
	if time.Now().Before(h.until) {
		h.refused.Add(1)
		return errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
	}
	return h.Store.Heartbeat(ctx, id, owner)
}

// TestAHeartbeatOutageDoesNotKillARunHoldingAValidLease covers what a routine control node restart did
// to every run on every healthy worker.
//
// The executor renews its lease every three seconds and gave up after three consecutive failures, so
// about nine seconds of a thirty second lease. A refused connection comes back in microseconds, so a
// control node restart, or a brief store outage, spent that budget immediately: the executor killed its
// own tool mid-change and the run finalized canceled with no error, indistinguishable from a cancel a
// person asked for. Nothing else could have touched the run, because the lease in the store still had
// twenty seconds to live and no sweep can reclaim a live lease. The kill was entirely self-inflicted.
//
// The executor now keeps working while its lease could still be its own, and stops when the lease has
// actually expired, which is the first moment another process could claim the run.
func TestAHeartbeatOutageDoesNotKillARunHoldingAValidLease(t *testing.T) {
	t.Parallel()
	base := run.NewMemStore()
	// The outage outlasts the old three-tick budget several times over and still ends well inside the
	// lease's own lifetime.
	store := &heartbeatStore{Store: base, until: time.Now().Add(12 * time.Second)}
	ctx := context.Background()

	// A tool that keeps working across the whole outage, the way a terraform apply does.
	started := make(chan struct{})
	runner := roundhouse.RunnerFunc(
		func(ctx context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			close(started)
			select {
			case <-time.After(15 * time.Second):
			case <-ctx.Done():
				// Killed. Report it so the assertion below reads the cancel rather than a timeout.
				return roundhouse.Result{ExitCode: -1}, ctx.Err()
			}
			_, _ = io.WriteString(out, "applied\n")
			return roundhouse.Result{ExitCode: 0}, nil
		})

	d := New(store, runner, nil, WithNoJanitor())
	defer d.Close()

	r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("apply"),
		run.WithActor("casey"), run.WithActorType("session"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the run never started")
	}

	// The run has to reach its own end rather than being killed partway through the outage.
	deadline := time.Now().Add(40 * time.Second)
	var got *run.Run
	for time.Now().Before(deadline) {
		got, err = base.Get(ctx, r.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.Status.Terminal() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got == nil || !got.Status.Terminal() {
		t.Fatalf("the run never finished: %+v", got)
	}
	if store.refused.Load() < 2 {
		t.Fatalf("the outage refused %d heartbeats, so it never exercised the give-up path",
			store.refused.Load())
	}
	if got.Status == run.StatusCanceled {
		t.Errorf("a %s heartbeat outage killed a run whose lease was still valid, and recorded it as "+
			"canceled with error %q", 12*time.Second, got.Error)
	}
	if got.Status != run.StatusSucceeded {
		t.Errorf("status = %q, want succeeded: the tool ran to completion", got.Status)
	}
}

// TestALostLeaseStopsTheRunAtOnce covers the other side. A store that says the lease is not this
// owner's is not an outage: another process holds the run, so continuing would mean two executors
// making changes on the same hosts. That case must stop immediately rather than wait out the lease.
func TestALostLeaseStopsTheRunAtOnce(t *testing.T) {
	t.Parallel()
	base := run.NewMemStore()
	store := &heartbeatStore{Store: base, lost: true}
	ctx := context.Background()

	started := make(chan struct{})
	runner := roundhouse.RunnerFunc(
		func(ctx context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			close(started)
			select {
			case <-time.After(60 * time.Second):
			case <-ctx.Done():
				return roundhouse.Result{ExitCode: -1}, ctx.Err()
			}
			return roundhouse.Result{ExitCode: 0}, nil
		})

	d := New(store, runner, nil, WithNoJanitor())
	defer d.Close()

	r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("apply"),
		run.WithActor("casey"), run.WithActorType("session"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the run never started")
	}

	// Well inside the lease's lifetime, so this proves the stop is driven by the lost lease and not by
	// waiting the lease out.
	deadline := time.Now().Add(leaseTTL - 5*time.Second)
	for time.Now().Before(deadline) {
		got, err := base.Get(ctx, r.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.Status.Terminal() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("a run whose lease another process holds kept executing, so two executors were making " +
		"changes on the same hosts")
}
