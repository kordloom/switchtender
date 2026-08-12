package dispatch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// blippingStore refuses reads for a window, standing in for a control node that is restarting or a
// contended writer, and counts what it refused.
type blippingStore struct {
	run.Store
	// until is when reads start succeeding again.
	until time.Time
	// refused counts the reads turned away.
	refused atomic.Int64
}

// Get refuses while the outage window is open, the way a refused connection does: immediately.
func (b *blippingStore) Get(ctx context.Context, id string) (*run.Run, error) {
	if time.Now().Before(b.until) {
		b.refused.Add(1)
		return nil, errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
	}
	return b.Store.Get(ctx, id)
}

// TestCancelFenceOutageDoesNotDestroyAHealthyRun checks that a store blip on the pre-execution cancel
// check leaves the run recoverable rather than terminally failed.
//
// The check has to fail closed: starting a tool because nobody could answer whether the run was
// canceled is the shape every other gate here was written to avoid. Failing the run outright went
// too far. A refused connection comes back in microseconds, so the retry budget is spent in well
// under a second, and any control node restart outlasts it. That terminally failed every run that
// happened to start during a rolling upgrade, and each had to be found and replayed by hand.
//
// Nothing executes either way. What must not happen is a terminal failure that needs a person.
func TestCancelFenceOutageDoesNotDestroyAHealthyRun(t *testing.T) {
	t.Parallel()
	base := run.NewMemStore()
	store := &blippingStore{Store: base, until: time.Now().Add(2 * time.Second)}
	runner := &countingRunnerLister{hosts: []string{"web01"}}
	d := New(store, runner, nil, WithNoJanitor())
	defer d.Close()
	ctx := context.Background()

	r := &run.Run{
		ID: "run_blip", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now(),
	}
	if err := base.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got := d.streamSpec(ctx, r.Clone(), false, nil,
		func(roundhouse.Result, error, *masker, *run.SummaryFold) run.Status { return run.StatusSucceeded })

	if store.refused.Load() == 0 {
		t.Fatal("the outage was never exercised")
	}
	if n := runner.executions.Load(); n != 0 {
		t.Errorf("%d executions while the cancel check could not be answered", n)
	}
	stored, err := base.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.Status.Terminal() {
		t.Errorf("a healthy run is terminally %q after a store blip, so an operator has to find "+
			"and replay it by hand; the lease sweep should recover it instead", stored.Status)
	}
	if got.Terminal() {
		t.Errorf("streamSpec returned terminal %q, which finalizes the run", got)
	}
}
