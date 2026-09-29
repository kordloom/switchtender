package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// TestControlNodeEndsARunPastItsTimeout covers the gap a heartbeating executor sat in.
//
// The lease sweep reclaims on a stale heartbeat, so it never touches a worker that keeps its lease
// fresh, and a run's timeout was enforced only inside the executing process. That bound a cooperative
// executor and nothing else: a relay that claimed work and heartbeated held it forever, so one
// compromised relay could take the queue and stall the estate, and an honest worker that wedged held
// its run until somebody noticed by hand.
func TestControlNodeEndsARunPastItsTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	started := now.Add(-time.Hour)
	fresh := now.Add(-time.Second) // heartbeating, so the lease sweep leaves it alone

	overrun := &run.Run{
		ID: "run_overrun", Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: started, StartedAt: &started, ClaimedBy: "worker-a", ClaimedAt: &fresh,
		Timeout: 60,
	}
	within := &run.Run{
		ID: "run_within", Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: started, StartedAt: &started, ClaimedBy: "worker-a", ClaimedAt: &fresh,
		Timeout: 86400,
	}
	untimed := &run.Run{
		ID: "run_untimed", Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: started, StartedAt: &started, ClaimedBy: "worker-a", ClaimedAt: &fresh,
	}
	for _, r := range []*run.Run{overrun, within, untimed} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s): %v", r.ID, err)
		}
	}

	d := &Dispatcher{store: store, log: zap.NewNop(), ctx: ctx, now: func() time.Time { return now }}
	d.settleOverrunning()

	got, err := store.Get(ctx, "run_overrun")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != run.StatusFailed {
		t.Errorf("a run an hour past its sixty-second timeout is still %q, so a heartbeating "+
			"executor holds it forever", got.Status)
	}
	if !strings.Contains(got.Error, "timed out") {
		t.Errorf("the run does not say why it ended: %q", got.Error)
	}

	// A run inside its timeout, and one with no timeout at all, are left alone.
	for _, id := range []string{"run_within", "run_untimed"} {
		r, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if r.Status != run.StatusRunning {
			t.Errorf("%s was ended at %q, but it has not overrun anything", id, r.Status)
		}
	}
}

// TestOverrunGraceLetsTheExecutorFinishFirst checks the control node waits before stepping in, so a
// run that ends itself keeps its real exit code and output rather than being overwritten.
func TestOverrunGraceLetsTheExecutorFinishFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	// Just past the timeout, well inside the grace.
	started := now.Add(-70 * time.Second)
	if err := store.Save(ctx, &run.Run{
		ID: "run_recent", Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: started, StartedAt: &started, ClaimedBy: "worker-a", ClaimedAt: &now,
		Timeout: 60,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	d := &Dispatcher{store: store, log: zap.NewNop(), ctx: ctx, now: func() time.Time { return now }}
	d.settleOverrunning()

	got, err := store.Get(ctx, "run_recent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != run.StatusRunning {
		t.Errorf("status = %q, want it left running inside the grace so its executor can finish it",
			got.Status)
	}
}
