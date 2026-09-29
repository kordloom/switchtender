package relay_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAWorkerCannotChooseWhichHistoryItsFactsReplace covers a report that deletes evidence instead
// of adding it.
//
// Estate history is kept per host in buckets, and the bucket is derived from the time the facts say
// they were gathered. A worker supplies that time, so a worker supplied it: a reading stamped far
// enough in the past lands in an old bucket and replaces whatever was there, and enough of them push
// a host's real readings past the retained depth and out of the table entirely. The report path was
// hardened so a worker can only write facts for hosts its own run touched, which makes a fabrication
// attributable, but attribution does not help once the older readings are gone.
//
// A reported start and end are already held to the window the control node observed. The gather time
// is held the same way and for a stronger reason, because it is not only a label on the row: it
// decides which row.
func TestAWorkerCannotChooseWhichHistoryItsFactsReplace(t *testing.T) {
	t.Parallel()
	run.SetFactsInterval(0)
	run.SetFactsDepth(0)
	t.Cleanup(func() {
		run.SetFactsInterval(run.DefaultFactsInterval)
		run.SetFactsDepth(run.DefaultFactsDepth)
	})

	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)
	worker := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	created := time.Now().Add(-time.Hour)
	if err := backing.Save(ctx, &run.Run{
		ID: "run_1", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: created,
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}
	leased, err := worker.Claim(ctx, "worker-a", []string{""})
	if err != nil || leased == nil {
		t.Fatalf("Claim: %v (leased %v)", err, leased)
	}
	if err := worker.SaveHostSummary(ctx, leased.ID, []run.HostSummary{{
		Host: "web01", OK: 1, Worst: "ok", RanAt: created,
	}}); err != nil {
		t.Fatalf("report per host result: %v", err)
	}

	// A year before the run existed, which is the reach that lets one report evict real history.
	forged := created.AddDate(-1, 0, 0)
	if err := worker.SaveHostFacts(ctx, leased.ID, []run.HostFacts{{
		Host: "web01", Facts: map[string]string{"kernel": "forged"}, GatheredAt: forged,
	}}); err != nil {
		t.Fatalf("report facts: %v", err)
	}

	stored, err := backing.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("read facts back: %v", err)
	}
	if stored.GatheredAt.Before(created) {
		t.Errorf("the control node stored the gather time as %s, which is before the run it came "+
			"from was created (%s). A worker choosing that time chooses which stored reading its "+
			"report replaces, so one call can evict a host's real history",
			stored.GatheredAt.UTC(), created.UTC())
	}
}

// TestAWorkerCannotRenewACoordinatorsLease covers the boundary between what a worker executes and
// what the control node drives.
//
// A split or pipeline parent is coordinated here and executed by nobody, and the claim loop skips
// any run carrying a kind, so no worker is ever handed one. A worker still learns its parent's id,
// because its own claim response carries it, and a shard inherits its parent's queue, so the parent
// is a run this pool serves. Reporting on a parent and appending output to one were both refused.
// Renewing its lease was not, which lets a worker keep a parent alive from outside or cover for a
// coordinator that has stopped, so the sweep that reclaims an abandoned parent never fires.
func TestAWorkerCannotRenewACoordinatorsLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)
	worker := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	two := 2
	if err := backing.Save(ctx, &run.Run{
		ID: "run_parent", Playbook: "site.yml", Kind: run.KindSplit, ShardCount: &two,
		Status: run.StatusRunning, CreatedAt: time.Now(), ClaimedBy: "coordinator",
	}); err != nil {
		t.Fatalf("save parent: %v", err)
	}

	// The coordinator's own owner name, which is what an attacker uses: the parent is claimed by the
	// control node without a per-claim capability, so the lease check falls back to matching that
	// name and a worker that knows it passes. Sending a different name is refused for the wrong
	// reason and proves nothing.
	if err := worker.Heartbeat(ctx, "run_parent", "coordinator"); err == nil {
		t.Error("a worker renewed the lease on a run the control node coordinates, so it can hold " +
			"a parent open or stand in for a coordinator that has stopped")
	}
}
