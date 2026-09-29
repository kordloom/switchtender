package run

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestComparisonRowsOrderStably pins that two hosts sharing a verdict come back in a fixed order.
//
// The rows are gathered from a map, which has no order, so without the name as a tie-break a
// comparison page reorders itself on every refresh and the exported document does not match the
// page it was exported from. The whole point of the view is that two people looking at the same
// comparison see the same thing.
func TestComparisonRowsOrderStably(t *testing.T) {
	t.Parallel()
	a := &Run{ID: "run_a", Status: StatusFailed, CreatedAt: filterBase}
	b := &Run{ID: "run_b", Status: StatusFailed, CreatedAt: filterBase.Add(-time.Hour)}
	hostsA := []HostSummary{
		{Host: "web03", Worst: "failed"}, {Host: "web01", Worst: "failed"},
		{Host: "web02", Worst: "failed"}, {Host: "db01", Worst: "ok"}, {Host: "db02", Worst: "ok"},
	}
	hostsB := []HostSummary{
		{Host: "web03", Worst: "ok"}, {Host: "web01", Worst: "ok"}, {Host: "web02", Worst: "ok"},
		{Host: "db01", Worst: "ok"}, {Host: "db02", Worst: "ok"},
	}
	want := []string{"web01", "web02", "web03", "db01", "db02"}
	for range 5 {
		c := Compare(a, b, hostsA, hostsB, nil, nil)
		var got []string
		for _, h := range c.Hosts {
			got = append(got, h.Host)
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("comparison rows (-want +got):\n%s", diff)
		}
	}
}

// TestComparisonOfTwoEmptyRunsSaysNothingHappened pins the degenerate comparison: two runs with no
// host summaries and no timing produce an answer rather than a panic or an invented delta.
//
// A run compared before its summaries land, or a run of a playbook that touched no host, both reach
// this. Inventing a duration delta from a missing start time would put a number on the page that no
// run produced.
func TestComparisonOfTwoEmptyRunsSaysNothingHappened(t *testing.T) {
	t.Parallel()
	a := &Run{ID: "run_a", CreatedAt: filterBase}
	b := &Run{ID: "run_b", CreatedAt: filterBase.Add(-time.Hour)}
	c := Compare(a, b, nil, nil, nil, nil)
	if len(c.Hosts) != 0 || len(c.Tasks) != 0 {
		t.Errorf("hosts %v and tasks %v, want neither invented", c.Hosts, c.Tasks)
	}
	if c.Totals != (ComparisonTotals{}) {
		t.Errorf("totals = %+v, want nothing counted", c.Totals)
	}
	if c.DurationDeltaSeconds != nil {
		t.Errorf("duration delta = %v, want absent when neither run has a duration",
			*c.DurationDeltaSeconds)
	}
	if c.SameSource {
		t.Error("SameSource = true for two runs with no source, so a host-by-host reading would " +
			"be presented as apples to apples when it is not")
	}
	if c.A.DurationSeconds != nil || c.B.DurationSeconds != nil {
		t.Error("a duration was invented for a run that never started")
	}

	// A run that started but never ended still has no duration, since the end is what bounds it.
	started := filterBase
	a.StartedAt = &started
	if got := Compare(a, b, nil, nil, nil, nil); got.A.DurationSeconds != nil {
		t.Errorf("duration = %v for a run that has not ended", *got.A.DurationSeconds)
	}

	// Two runs from the same template are the case where a host-by-host reading means something,
	// and an empty source id is not a shared source however equal it is.
	a.Source, a.SourceID = "template", ""
	b.Source, b.SourceID = "template", ""
	if got := Compare(a, b, nil, nil, nil, nil); got.SameSource {
		t.Error("two runs with an empty source id read as the same source")
	}
	a.SourceID, b.SourceID = "tpl_1", "tpl_1"
	if got := Compare(a, b, nil, nil, nil, nil); !got.SameSource {
		t.Error("two runs of one template did not read as the same source")
	}
}

// TestWorkersOrderStablyWhenLastSeenTies pins that two executors whose leases carry the same
// instant come back in a fixed order.
//
// The listing is gathered from a map, so without the owner name as a tie-break the workers page
// reorders itself between refreshes, which reads as workers appearing and disappearing.
func TestWorkersOrderStablyWhenLastSeenTies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	at := time.Now().Add(-time.Minute)
	for _, owner := range []string{"worker-c", "worker-a", "worker-b"} {
		stamp := at
		saveRun(t, store, &Run{
			ID: "run_" + owner, Playbook: "site.yml", Status: StatusRunning, CreatedAt: at,
			ClaimedBy: owner, ClaimedAt: &stamp,
		})
	}
	want := []string{"worker-a", "worker-b", "worker-c"}
	for range 5 {
		workers, err := store.Workers(ctx)
		if err != nil {
			t.Fatalf("Workers() error = %v", err)
		}
		var got []string
		for _, w := range workers {
			got = append(got, w.Owner)
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("worker order (-want +got):\n%s", diff)
		}
	}
}

// TestDriftOrdersStablyWhenTheDriftCountTies pins that two hosts with the same number of drifted
// tasks come back in a fixed order.
//
// Drift is what an operator scans to decide where to reconcile first. A list that shuffled between
// refreshes would make "the worst host" a different host each time it was looked at, which is worse
// than showing no order at all.
func TestDriftOrdersStablyWhenTheDriftCountTies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_check", Playbook: "site.yml", Status: StatusRunning, CreatedAt: filterBase,
		DryRun: true,
	})
	if err := store.SaveHostSummary(ctx, "run_check", []HostSummary{
		{Host: "web03", Changed: 2, Worst: "changed", RanAt: filterBase},
		{Host: "web01", Changed: 2, Worst: "changed", RanAt: filterBase},
		{Host: "web02", Changed: 2, Worst: "changed", RanAt: filterBase},
		{Host: "db01", Changed: 9, Worst: "changed", RanAt: filterBase},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	want := []string{"db01", "web01", "web02", "web03"}
	for range 5 {
		drift, err := store.DriftStatus(ctx)
		if err != nil {
			t.Fatalf("DriftStatus() error = %v", err)
		}
		var got []string
		for _, d := range drift {
			got = append(got, d.Host)
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("drift order (-want +got):\n%s", diff)
		}
	}
}

// TestOnlyDryRunsCarryADriftSignal pins that a host whose history holds no check run is left out of
// the drift listing rather than reported as being in sync.
//
// Drift is measured in check mode, where a changed result means a task would change. A real apply's
// changed count means a task did change, which is the opposite reading, so counting an apply as a
// drift signal would report every host that was just configured as having drifted.
func TestOnlyDryRunsCarryADriftSignal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_apply", Playbook: "site.yml", Status: StatusRunning, CreatedAt: filterBase,
	})
	if err := store.SaveHostSummary(ctx, "run_apply", []HostSummary{
		{Host: "web01", Changed: 5, Worst: "changed", RanAt: filterBase},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	drift, err := store.DriftStatus(ctx)
	if err != nil {
		t.Fatalf("DriftStatus() error = %v", err)
	}
	if len(drift) != 0 {
		t.Errorf("DriftStatus() = %+v, want nothing: an apply's changes are not drift", drift)
	}

	// The same host after a check run does carry one, and the check is what the entry names.
	saveRun(t, store, &Run{
		ID: "run_check", Playbook: "site.yml", Status: StatusRunning,
		CreatedAt: filterBase.Add(time.Minute), DryRun: true,
	})
	if err := store.SaveHostSummary(ctx, "run_check", []HostSummary{
		{Host: "web01", Changed: 2, Worst: "changed", RanAt: filterBase.Add(time.Minute)},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	drift, err = store.DriftStatus(ctx)
	if err != nil {
		t.Fatalf("DriftStatus() error = %v", err)
	}
	want := []HostDrift{{
		Host: "web01", DriftedTasks: 2, RunID: "run_check",
		CheckedAt: filterBase.Add(time.Minute),
	}}
	if diff := cmp.Diff(want, drift, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("drift (-want +got):\n%s", diff)
	}
}
