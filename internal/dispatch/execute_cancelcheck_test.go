package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// errStoreDown is a persistent store read failure used to drive the cancel-check error branch.
var errStoreDown = errors.New("store unavailable")

// getFailStore delegates every store operation to the embedded store but fails Get, so the start-time
// cancel check cannot read the run. Everything else, including the run's own running save, still works.
type getFailStore struct {
	run.Store
	// err is returned from every Get.
	err error
}

// Get always fails, standing in for a control node that is restarting when a relay worker asks it.
func (s *getFailStore) Get(context.Context, string) (*run.Run, error) {
	return nil, s.err
}

// TestExecuteLeavesRunOpenWhenCancelCheckErrors proves that when the start-time cancel check cannot
// read the store the run is left non-terminal for the sweep to reclaim and retry, and its output
// stream is not closed. Closing it would tell a co-located live viewer the run ended and stop it from
// watching a run that is about to run again. Before the fix this branch called CloseRun.
func TestExecuteLeavesRunOpenWhenCancelCheckErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	store := &getFailStore{Store: backing, err: errStoreDown}
	pub := newCapturingPublisher()
	d := New(store, okRunner(), nil, WithPublisher(pub), WithNoJanitor())
	defer d.Close()

	r := &run.Run{ID: "run_leavealone", Playbook: "p.yml", Status: run.StatusPending, CreatedAt: time.Now()}

	status := d.execute(ctx, r)
	if status != run.StatusRunning {
		t.Errorf("execute() = %q, want %q so the sweep reclaims and retries it", status, run.StatusRunning)
	}
	for _, id := range pub.closedIDs() {
		if id == r.ID {
			t.Fatalf("CloseRun fired for %s, want the stream left open so a live viewer resumes", r.ID)
		}
	}
	got, err := backing.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status.Terminal() {
		t.Errorf("run status = %q, want it left non-terminal for the sweep", got.Status)
	}
}
