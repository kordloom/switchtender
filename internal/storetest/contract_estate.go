package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// testEstateHistory holds every backend to the same behavior for the point-in-time estate.
//
// The three implementations are genuinely different: two build SQL against a stored bucket column
// and the third recomputes buckets in memory. They were written by hand, one after another, and
// only the SQLite one had a test. A contract is what stops them agreeing in the author's head and
// disagreeing in the product, which is how a Postgres install would have answered a different
// estate from a SQLite one for the same history.
func testEstateHistory(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()

	// Keep every gather, so the test does not depend on what day it runs.
	run.SetFactsInterval(0)
	run.SetFactsDepth(0)
	t.Cleanup(func() {
		run.SetFactsInterval(run.DefaultFactsInterval)
		run.SetFactsDepth(run.DefaultFactsDepth)
	})

	// Nothing gathered: no horizon, and an estate rather than an error.
	if horizon, err := store.EstateHorizon(ctx); err != nil {
		t.Fatalf("EstateHorizon() on an empty store error = %v", err)
	} else if !horizon.IsZero() {
		t.Errorf("EstateHorizon() = %v on an empty store, want the zero time", horizon)
	}
	if hosts, err := store.EstateAt(ctx, time.Now(), 0); err != nil {
		t.Fatalf("EstateAt() on an empty store error = %v", err)
	} else if len(hosts) != 0 {
		t.Errorf("EstateAt() returned %d hosts on an empty store", len(hosts))
	}

	march := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	april := march.AddDate(0, 1, 0)
	save := func(runID, host, kernel string, at time.Time) {
		t.Helper()
		if err := store.SaveHostFacts(ctx, runID, []run.HostFacts{{
			Host: host, Facts: map[string]string{"kernel": kernel}, GatheredAt: at,
		}}); err != nil {
			t.Fatalf("SaveHostFacts(%s) error = %v", host, err)
		}
	}
	save("run_a", "web01", "5.15.0", march)
	save("run_b", "web01", "6.8.0", april)
	save("run_b", "db01", "6.1.0", april)

	// The horizon is the oldest reading held, which is what separates an empty estate from records
	// that do not reach back far enough.
	horizon, err := store.EstateHorizon(ctx)
	if err != nil {
		t.Fatalf("EstateHorizon() error = %v", err)
	}
	if !horizon.Equal(march) {
		t.Errorf("EstateHorizon() = %v, want the oldest reading %v", horizon, march)
	}

	// In effect at an instant is the newest gather at or before it, and a host first seen later is
	// absent rather than invented.
	at, err := store.EstateAt(ctx, march.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("EstateAt(march) error = %v", err)
	}
	if len(at) != 1 || at[0].Host != "web01" || at[0].Facts["kernel"] != "5.15.0" {
		t.Fatalf("estate in March = %+v, want web01 alone on 5.15.0", at)
	}
	if at[0].RunID != "run_a" {
		t.Errorf("estate names run %q, want the run that gathered it, run_a", at[0].RunID)
	}
	if !at[0].GatheredAt.Equal(march) {
		t.Errorf("gathered at %v, want %v", at[0].GatheredAt, march)
	}

	// Later: both hosts, web01 on its newer kernel, ordered by host.
	now, err := store.EstateAt(ctx, april.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("EstateAt(april) error = %v", err)
	}
	if len(now) != 2 {
		t.Fatalf("estate in April holds %d hosts, want 2: %+v", len(now), now)
	}
	if now[0].Host != "db01" || now[1].Host != "web01" {
		t.Errorf("estate order = %s, %s, want db01 then web01", now[0].Host, now[1].Host)
	}
	if now[1].Facts["kernel"] != "6.8.0" {
		t.Errorf("web01 kernel = %q, want the newer 6.8.0", now[1].Facts["kernel"])
	}

	// Before anything was gathered: empty, not back-filled with readings taken since.
	before, err := store.EstateAt(ctx, march.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("EstateAt(before) error = %v", err)
	}
	if len(before) != 0 {
		t.Errorf("estate before the first gather holds %d hosts, want none: %+v", len(before), before)
	}

	// The limit bounds the query, and a capped answer is the first rows in order so two identical
	// requests agree about what they show.
	capped, err := store.EstateAt(ctx, april.Add(time.Hour), 1)
	if err != nil {
		t.Fatalf("EstateAt(limit 1) error = %v", err)
	}
	if len(capped) != 1 {
		t.Fatalf("EstateAt with a limit of 1 returned %d rows", len(capped))
	}
	if capped[0].Host != "db01" {
		t.Errorf("a capped estate showed %s, want the first host in order, db01", capped[0].Host)
	}
}

// testEstateOneRowPerHost holds every backend to returning a host once when two readings share an
// instant.
//
// The SQL backends pick a row per host with a correlated subquery, and the memory store scans a
// slice. An earlier SQL version grouped on the maximum time and joined back on equality, which
// returns a row per match rather than per host and put the same host in the estate twice. An estate
// that reports more hosts than an estate has is not an answer an audit can use.
func testEstateOneRowPerHost(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	run.SetFactsInterval(0)
	run.SetFactsDepth(0)
	t.Cleanup(func() {
		run.SetFactsInterval(run.DefaultFactsInterval)
		run.SetFactsDepth(run.DefaultFactsDepth)
	})

	same := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"run_a", "run_b"} {
		if err := store.SaveHostFacts(ctx, id, []run.HostFacts{{
			Host: "web01", Facts: map[string]string{"from": id}, GatheredAt: same,
		}}); err != nil {
			t.Fatalf("SaveHostFacts(%s) error = %v", id, err)
		}
	}

	first, err := store.EstateAt(ctx, same.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("EstateAt() error = %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("estate holds %d rows for one host, want 1: %+v", len(first), first)
	}
	second, err := store.EstateAt(ctx, same.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("EstateAt() again error = %v", err)
	}
	if len(second) != 1 || second[0].RunID != first[0].RunID {
		t.Errorf("two identical requests picked %q then %q, want the same reading both times",
			first[0].RunID, second[0].RunID)
	}
}

// testEstateDepthCap holds every backend to bounding the history it keeps per host.
//
// A fact set runs hundreds of kilobytes, so an unbounded history is a disk problem the operator did
// not choose. The cap is applied where the row is written rather than by the retention sweeper,
// because the sweeper only trims when it is configured to and it defaults to off.
func testEstateDepthCap(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	run.SetFactsInterval(0)
	run.SetFactsDepth(3)
	t.Cleanup(func() {
		run.SetFactsInterval(run.DefaultFactsInterval)
		run.SetFactsDepth(run.DefaultFactsDepth)
	})

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		if err := store.SaveHostFacts(ctx, fmt.Sprintf("run_%d", i), []run.HostFacts{{
			Host:  "web01",
			Facts: map[string]string{"kernel": fmt.Sprintf("6.%d.0", i)},
			// Ordered, so the newest three are the last three written.
			GatheredAt: base.Add(time.Duration(i) * time.Hour),
		}}); err != nil {
			t.Fatalf("SaveHostFacts(%d) error = %v", i, err)
		}
	}

	// The oldest readings are gone, so the horizon has moved forward off the first gather.
	horizon, err := store.EstateHorizon(ctx)
	if err != nil {
		t.Fatalf("EstateHorizon() error = %v", err)
	}
	if !horizon.After(base) {
		t.Errorf("EstateHorizon() = %v, want it past the first gather at %v: the depth cap kept "+
			"every reading, so the history is unbounded", horizon, base)
	}

	// The newest reading still wins, which is the property the cap must not break.
	now, err := store.EstateAt(ctx, base.Add(24*time.Hour), 0)
	if err != nil {
		t.Fatalf("EstateAt() error = %v", err)
	}
	if len(now) != 1 || now[0].Facts["kernel"] != "6.5.0" {
		t.Fatalf("estate = %+v, want web01 on the newest kernel 6.5.0", now)
	}
}
