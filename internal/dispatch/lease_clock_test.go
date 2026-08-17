package dispatch

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// skewedClockStore stands in for a Postgres control node whose clock is ahead of the worker's. It stamps
// every lease from its own clock, the way pgstore does, and ages leases against that same clock, so a
// lease written from the worker's time reads as far older than it is.
type skewedClockStore struct {
	run.Store
	// skew is how far the store's clock runs ahead of this process's.
	skew time.Duration
	// interrupted counts the runs a sweep drove terminal for a lease it judged stale.
	interrupted atomic.Int64
}

// now is the store's clock.
func (s *skewedClockStore) now() time.Time { return time.Now().Add(s.skew) }

// Claim stamps the lease from the store's clock, as the database does.
func (s *skewedClockStore) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	r, err := s.Store.Claim(ctx, owner, queues)
	if err != nil {
		return nil, err
	}
	at := s.now()
	r.ClaimedAt = &at
	if serr := s.Save(ctx, r); serr != nil {
		return nil, serr
	}
	return r, nil
}

// Heartbeat renews the lease from the store's clock.
func (s *skewedClockStore) Heartbeat(ctx context.Context, id, owner string) error {
	got, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if got.ClaimedBy != owner {
		return run.ErrNotFound
	}
	at := s.now()
	got.ClaimedAt = &at
	return s.Save(ctx, got)
}

// ReclaimStale ages leases against the store's clock and interrupts a running run whose lease it judges
// expired, which is what the janitor does.
func (s *skewedClockStore) ReclaimStale(ctx context.Context, ttl time.Duration) (int, error) {
	cut := s.now().Add(-ttl)
	list, err := s.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range list {
		if r.Status != run.StatusRunning || r.ClaimedAt == nil || !r.ClaimedAt.Before(cut) {
			continue
		}
		ended := s.now()
		r.Status = run.StatusInterrupted
		r.EndedAt = &ended
		r.Error = "interrupted: executor lease expired"
		if serr := s.Save(ctx, r); serr != nil {
			return n, serr
		}
		s.interrupted.Add(1)
		n++
	}
	return n, nil
}

// TestAFreshLeaseIsNotAgedByClockSkew covers a healthy run the janitor killed for being new.
//
// Postgres stamps a lease from the database clock and ages leases against that same clock, deliberately,
// because a fleet of workers has a fleet of clocks and only one of them can be the authority. The
// executor then wrote its own time over that stamp in the same save that set the run running. A worker
// whose clock trails the database by more than the lease lifetime therefore recorded a lease that was
// already expired the moment it was written, and any sweep in the window that followed interrupted a run
// that had just started: its later writes were fenced, its own terminal record was refused, and a split's
// shards were orphaned in the same pass.
//
// The lease belongs to whoever stamped it. The executor names itself the holder and leaves the time to
// the store, and the watcher renews it at once so the store's clock owns it from the first moment.
func TestAFreshLeaseIsNotAgedByClockSkew(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	// The store's clock runs well past the lease lifetime ahead of this process's.
	store := &skewedClockStore{Store: base, skew: 3 * leaseTTL}

	running := make(chan struct{})
	runner := roundhouse.RunnerFunc(
		func(ctx context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			close(running)
			// Long enough for several janitor sweeps to consider this run.
			select {
			case <-time.After(6 * time.Second):
			case <-ctx.Done():
				return roundhouse.Result{ExitCode: -1}, ctx.Err()
			}
			_, _ = io.WriteString(out, "done\n")
			return roundhouse.Result{ExitCode: 0}, nil
		})

	d := New(store, runner, zap.NewNop())
	defer d.Close()

	created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
		run.WithActor("casey"), run.WithActorType("session"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	select {
	case <-running:
	case <-time.After(20 * time.Second):
		t.Fatal("the run never started")
	}

	// Sweep the way the janitor does, repeatedly, while the run is executing.
	for i := 0; i < 6; i++ {
		if _, serr := store.ReclaimStale(ctx, leaseTTL); serr != nil {
			t.Fatalf("ReclaimStale() error = %v", serr)
		}
		time.Sleep(200 * time.Millisecond)
	}

	deadline := time.Now().Add(30 * time.Second)
	var got *run.Run
	for time.Now().Before(deadline) {
		got, err = base.Get(ctx, created.ID)
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
	if store.interrupted.Load() > 0 {
		t.Errorf("a sweep interrupted %d run(s) whose lease had just been stamped, because the "+
			"executor wrote its own clock over the store's", store.interrupted.Load())
	}
	if got.Status != run.StatusSucceeded {
		t.Errorf("status = %q, want succeeded: the run was healthy throughout (%s)",
			got.Status, got.Error)
	}
}
