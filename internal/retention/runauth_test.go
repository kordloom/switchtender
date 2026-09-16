package retention_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/retention"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTheSweepCollectsAuthorizationNothingNeeds covers the wiring rather than the store method.
//
// The collection itself is held by the store contract in all three backends. What that cannot show
// is whether the sweeper ever calls it, and an uncalled collector is indistinguishable from a
// missing one: the decisions simply accumulate, forever, on every install.
//
// It also covers the ordering. Collection runs after the trims, so anything those just removed is
// collectable on the same pass rather than sitting until the next one.
func TestTheSweepCollectsAuthorizationNothingNeeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	now := time.Now()

	// Old enough that the sweep deletes it, and it leaves nothing behind that needs its decision.
	old := &run.Run{ID: "run_old", Status: run.StatusSucceeded, ProjectID: "proj_1",
		CreatedAt: now.Add(-100 * 24 * time.Hour)}
	if err := store.Save(ctx, old); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	sweeper := retention.NewSweeper(store, nil,
		retention.WithRetainRuns(30*24*time.Hour),
		retention.WithInterval(time.Hour))
	t.Cleanup(sweeper.Close)
	sweeper.Start()

	// The sweeper sweeps once immediately on start.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := store.Get(ctx, old.ID); errors.Is(err, run.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run was never purged, so this test is not exercising what it claims")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The decision was retained by the purge and then collected by the same sweep, because nothing
	// the run produced survived it. Left uncollected it would sit there for the life of the install,
	// one row per run ever deleted.
	for {
		_, err := store.RunAuthFor(ctx, old.ID)
		if errors.Is(err, run.ErrNotFound) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retained authorization for a purged run governing nothing was still held "+
				"after the sweep: err = %v. The sweeper is not collecting, so the decisions only "+
				"ever accumulate", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
