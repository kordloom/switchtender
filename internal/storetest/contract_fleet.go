package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/kordloom/switchtender/internal/run"
)

// testRunSummaries verifies one run's host and task summaries read back whole and ordered, since
// the comparison view is built from exactly this read.
func testRunSummaries(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	hosts := []run.HostSummary{
		{Host: "web02", OK: 3, Changed: 1, Worst: "changed", DurationSeconds: 2.5, RanAt: base},
		{Host: "web01", OK: 4, Failures: 1, Worst: "failed", DurationSeconds: 1.5, RanAt: base},
	}
	if err := store.SaveHostSummary(ctx, "rs1", hosts); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	tasks := []run.TaskSummary{
		{Task: "restart nginx", Seconds: 4.25, RanAt: base},
		{Task: "copy config", Seconds: 1.5, RanAt: base},
	}
	if err := store.SaveTaskSummary(ctx, "rs1", tasks); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}

	gotHosts, err := store.RunHostSummaries(ctx, "rs1")
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if len(gotHosts) != 2 || gotHosts[0].Host != "web01" || gotHosts[1].Host != "web02" {
		t.Fatalf("hosts = %+v, want both back ordered by host", gotHosts)
	}
	if gotHosts[0].Worst != "failed" || gotHosts[0].Failures != 1 || gotHosts[0].RunID != "rs1" {
		t.Errorf("web01 = %+v, want its counts and run id carried", gotHosts[0])
	}
	gotTasks, err := store.RunTaskSummaries(ctx, "rs1")
	if err != nil {
		t.Fatalf("RunTaskSummaries() error = %v", err)
	}
	if len(gotTasks) != 2 || gotTasks[0].Task != "copy config" || gotTasks[1].Seconds != 4.25 {
		t.Fatalf("tasks = %+v, want both back ordered by task", gotTasks)
	}

	// A run with no summaries answers empty, not an error.
	if none, err := store.RunHostSummaries(ctx, "rs-none"); err != nil || len(none) != 0 {
		t.Errorf("RunHostSummaries(unknown) = %v, %v, want empty", none, err)
	}
}

// testAppendSummaries verifies the SummaryAppender capability: a report split across batches lands its
// first batch as a replace and appends the rest, upserting by key without disturbing the run's other
// rows. This is what lets a run wider than one report batch accumulate its full summary instead of the
// relay server reading and rewriting the whole growing set on every continuation.
func testAppendSummaries(t *testing.T, store run.Store) {
	ctx := context.Background()
	appender, ok := store.(run.SummaryAppender)
	if !ok {
		t.Fatalf("%T does not implement run.SummaryAppender", store)
	}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// Batch one replaces, clearing any partial from an earlier attempt.
	if err := store.SaveHostSummary(ctx, "rw", []run.HostSummary{
		{Host: "h1", OK: 1, Worst: "ok", RanAt: base},
		{Host: "h2", OK: 1, Worst: "ok", RanAt: base},
	}); err != nil {
		t.Fatalf("SaveHostSummary(batch 1) error = %v", err)
	}
	// A continuation appends only its own hosts, leaving batch one in place.
	if err := appender.AppendHostSummary(ctx, "rw", []run.HostSummary{
		{Host: "h3", Changed: 1, Worst: "changed", RanAt: base},
		{Host: "h4", OK: 1, Worst: "ok", RanAt: base},
	}); err != nil {
		t.Fatalf("AppendHostSummary(batch 2) error = %v", err)
	}
	// A host repeated in a later batch upserts to its newest outcome, not a second row.
	if err := appender.AppendHostSummary(ctx, "rw", []run.HostSummary{
		{Host: "h1", Failures: 1, Worst: "failed", RanAt: base},
	}); err != nil {
		t.Fatalf("AppendHostSummary(update) error = %v", err)
	}

	got, err := store.RunHostSummaries(ctx, "rw")
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if want := []string{"h1", "h2", "h3", "h4"}; len(got) != len(want) {
		t.Fatalf("host summaries = %d rows, want %d (no truncation, no duplicate)", len(got), len(want))
	} else {
		for i, h := range want {
			if got[i].Host != h {
				t.Fatalf("hosts = %+v, want %v ordered by host", got, want)
			}
		}
	}
	if got[0].Worst != "failed" || got[0].Failures != 1 {
		t.Errorf("h1 = %+v, want the appended update (failed), not the original ok", got[0])
	}

	// Tasks accumulate and upsert the same way.
	if err := store.SaveTaskSummary(ctx, "rw", []run.TaskSummary{{Task: "t1", Seconds: 1, RanAt: base}}); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}
	if err := appender.AppendTaskSummary(ctx, "rw", []run.TaskSummary{{Task: "t2", Seconds: 2, RanAt: base}}); err != nil {
		t.Fatalf("AppendTaskSummary() error = %v", err)
	}
	if err := appender.AppendTaskSummary(ctx, "rw", []run.TaskSummary{{Task: "t1", Seconds: 9, RanAt: base}}); err != nil {
		t.Fatalf("AppendTaskSummary(update) error = %v", err)
	}
	tasks, err := store.RunTaskSummaries(ctx, "rw")
	if err != nil {
		t.Fatalf("RunTaskSummaries() error = %v", err)
	}
	if len(tasks) != 2 || tasks[0].Task != "t1" || tasks[0].Seconds != 9 {
		t.Fatalf("tasks = %+v, want t1 upserted to 9 and t2 present", tasks)
	}

	// An empty batch is a no-op, matching Save.
	if err := appender.AppendHostSummary(ctx, "rw", nil); err != nil {
		t.Fatalf("AppendHostSummary(empty) error = %v", err)
	}
	if again, _ := store.RunHostSummaries(ctx, "rw"); len(again) != 4 {
		t.Fatalf("empty append changed the set to %d rows, want 4", len(again))
	}
}

// testFleetHealth verifies host summaries persist and rank hosts by recent failures with windowing.
func testFleetHealth(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	save := func(runID string, at time.Time, hosts map[string]string) {
		var sums []run.HostSummary
		for host, worst := range hosts {
			sums = append(sums, run.HostSummary{Host: host, Worst: worst, RanAt: at})
		}
		if err := store.SaveHostSummary(ctx, runID, sums); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
	}
	save("r1", base, map[string]string{"db01": "failed", "web01": "ok"})
	save("r2", base.Add(time.Hour), map[string]string{"db01": "failed", "web01": "ok"})
	save("r3", base.Add(2*time.Hour), map[string]string{"db01": "ok", "web01": "changed"})

	health, err := store.FleetHealth(ctx, 10)
	if err != nil {
		t.Fatalf("FleetHealth() error = %v", err)
	}
	byHost := make(map[string]run.HostHealth, len(health))
	for _, h := range health {
		byHost[h.Host] = h
	}
	if db := byHost["db01"]; db.Failures != 2 || db.Total != 3 || db.LastOutcome != "ok" {
		t.Errorf("db01 = %+v, want failures 2 total 3 last ok", db)
	}
	if web := byHost["web01"]; web.Failures != 0 {
		t.Errorf("web01 failures = %d, want 0", web.Failures)
	}
	if len(health) < 2 || health[0].Host != "db01" {
		t.Errorf("ranking = %+v, want db01 first", health)
	}

	windowed, err := store.FleetHealth(ctx, 1)
	if err != nil {
		t.Fatalf("FleetHealth() error = %v", err)
	}
	for _, h := range windowed {
		if h.Total != 1 {
			t.Errorf("window 1 total for %s = %d, want 1", h.Host, h.Total)
		}
		if h.Host == "db01" && h.Failures != 0 {
			t.Errorf("db01 window 1 failures = %d, want 0 since most recent run was ok", h.Failures)
		}
	}
}

// testDriftStatus verifies drift comes only from dry-run checks and reports the latest check per host,
// so a real run's changes and a stale check do not distort the current drift.
func testDriftStatus(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	saveRun := func(id string, at time.Time, dry bool, changed map[string]int) {
		// The summary is written while the run is still running, as a real run does, then the run
		// finalizes, since the store fences summary writes to a terminal run.
		if err := store.Save(ctx, &run.Run{
			ID: id, Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: at, DryRun: dry,
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		var sums []run.HostSummary
		for host, c := range changed {
			sums = append(sums, run.HostSummary{Host: host, Changed: c, Worst: "changed", RanAt: at})
		}
		if err := store.SaveHostSummary(ctx, id, sums); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
		if err := store.Save(ctx, &run.Run{
			ID: id, Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: at, DryRun: dry,
		}); err != nil {
			t.Fatalf("Save() finalize error = %v", err)
		}
	}
	// An older check finds drift on web01. A real run then changes it. The newest check finds it near
	// clean. db01 has one check that found it in sync.
	saveRun("chk1", base, true, map[string]int{"web01": 3, "db01": 0})
	saveRun("apply1", base.Add(time.Hour), false, map[string]int{"web01": 5})
	saveRun("chk2", base.Add(2*time.Hour), true, map[string]int{"web01": 1})

	drift, err := store.DriftStatus(ctx)
	if err != nil {
		t.Fatalf("DriftStatus() error = %v", err)
	}
	byHost := make(map[string]run.HostDrift, len(drift))
	for _, d := range drift {
		byHost[d.Host] = d
	}
	// web01's current drift is its latest check, chk2 with one drifted task, not the real run's five
	// or the older check's three.
	if w := byHost["web01"]; w.DriftedTasks != 1 || w.RunID != "chk2" {
		t.Errorf("web01 drift = %+v, want 1 drifted task from chk2", w)
	}
	// db01's only check found it in sync.
	if d := byHost["db01"]; d.DriftedTasks != 0 || d.RunID != "chk1" {
		t.Errorf("db01 drift = %+v, want 0 drifted tasks from chk1", d)
	}
	// The most drifted host ranks first.
	if len(drift) < 1 || drift[0].Host != "web01" {
		t.Errorf("drift order = %+v, want web01 first", drift)
	}
}

// testDriftSurvivesPurge verifies the drift view and the fleet health view stay reconciled across a
// retention purge. Purging deletes the run records but deliberately keeps the host summaries, so a
// host that has a drift check must keep its drift entry, and a host that only ever had real runs
// must stay out of the drift view. Both views read the same surviving rows and must agree on them.
func testDriftSurvivesPurge(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// web01 was checked by a dry run. app01 only ever had a real run, so it has no drift signal.
	// app01's summary arrives claiming to be a check, as a compromised remote worker could, and the
	// store must overwrite that with the run's own flag rather than trust it.
	saveFinishedRun(t, store, "chk1", base, true,
		[]run.HostSummary{{Host: "web01", Changed: 2, Worst: "changed", RanAt: base}})
	saveFinishedRun(t, store, "apply1", base.Add(time.Hour), false, []run.HostSummary{
		{Host: "app01", Changed: 7, Worst: "changed", RanAt: base.Add(time.Hour), DryRun: true},
	})

	wantDrift := []run.HostDrift{{Host: "web01", DriftedTasks: 2, RunID: "chk1", CheckedAt: base}}
	before, err := store.DriftStatus(ctx)
	if err != nil {
		t.Fatalf("DriftStatus() before purge error = %v", err)
	}
	if diff := cmp.Diff(wantDrift, before, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("DriftStatus() before purge mismatch (-want +got):\n%s", diff)
	}
	beforeHealth, err := hostsInHealth(ctx, store)
	if err != nil {
		t.Fatalf("FleetHealth() before purge error = %v", err)
	}
	if diff := cmp.Diff([]string{"app01", "web01"}, beforeHealth, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("FleetHealth() before purge mismatch (-want +got):\n%s", diff)
	}

	deleted, err := store.PurgeRunsBefore(ctx, base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	if deleted != 2 {
		t.Fatalf("PurgeRunsBefore() deleted = %d, want 2", deleted)
	}

	// Host history is kept on purpose, so fleet health still knows both hosts.
	afterHealth, err := hostsInHealth(ctx, store)
	if err != nil {
		t.Fatalf("FleetHealth() after purge error = %v", err)
	}
	if diff := cmp.Diff([]string{"app01", "web01"}, afterHealth, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("FleetHealth() after purge mismatch (-want +got):\n%s", diff)
	}
	// The drift view reads the same surviving rows, so it must be unchanged: web01 keeps its check
	// and app01 stays out, since a purged run must not turn a real run into a drift check.
	after, err := store.DriftStatus(ctx)
	if err != nil {
		t.Fatalf("DriftStatus() after purge error = %v", err)
	}
	if diff := cmp.Diff(wantDrift, after, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("DriftStatus() after purge mismatch (-want +got):\n%s", diff)
	}

	// The surviving rows carry the flag drift is read from, so it is readable on its own and the
	// answer is traceable to the row rather than to a run record that no longer exists.
	wantStamps := map[string]bool{"chk1": true, "apply1": false}
	for runID, wantDry := range wantStamps {
		sums, err := store.RunHostSummaries(ctx, runID)
		if err != nil {
			t.Fatalf("RunHostSummaries(%s) error = %v", runID, err)
		}
		if len(sums) != 1 {
			t.Fatalf("RunHostSummaries(%s) = %d rows, want 1 kept past the purge", runID, len(sums))
		}
		if sums[0].DryRun != wantDry {
			t.Errorf("RunHostSummaries(%s) dry run = %v, want %v", runID, sums[0].DryRun, wantDry)
		}
	}
	// The same flag reads back through host history, the view that outlives the run.
	hist, err := store.HostHistory(ctx, "app01", 10)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	if len(hist) != 1 || hist[0].DryRun {
		t.Errorf("HostHistory(app01) = %+v, want one apply row, since a worker cannot claim a check", hist)
	}
}

// testFlaky verifies flip counting marks intermittent hosts flaky and steady hosts not.
func testFlaky(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	save := func(runID string, at time.Time, hosts map[string]string) {
		var sums []run.HostSummary
		for host, worst := range hosts {
			sums = append(sums, run.HostSummary{Host: host, Worst: worst, RanAt: at})
		}
		if err := store.SaveHostSummary(ctx, runID, sums); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
	}
	// flappy alternates, fixed fails then recovers once, solid never fails.
	save("r1", base, map[string]string{"flappy": "failed", "fixed": "failed", "solid": "ok"})
	save("r2", base.Add(time.Hour), map[string]string{"flappy": "ok", "fixed": "failed", "solid": "ok"})
	save("r3", base.Add(2*time.Hour), map[string]string{"flappy": "failed", "fixed": "ok", "solid": "changed"})
	save("r4", base.Add(3*time.Hour), map[string]string{"flappy": "ok", "fixed": "ok", "solid": "ok"})

	health, err := store.FleetHealth(ctx, 10)
	if err != nil {
		t.Fatalf("FleetHealth() error = %v", err)
	}
	byHost := make(map[string]run.HostHealth, len(health))
	for _, h := range health {
		byHost[h.Host] = h
	}
	if h := byHost["flappy"]; h.Flips != 3 || !h.Flaky {
		t.Errorf("flappy = flips %d flaky %v, want 3 true", h.Flips, h.Flaky)
	}
	wantRecent := []string{"ok", "failed", "ok", "failed"}
	if diff := cmp.Diff(wantRecent, byHost["flappy"].Recent); diff != "" {
		t.Errorf("flappy recent mismatch (-want +got):\n%s", diff)
	}
	wantRecentRuns := []string{"r4", "r3", "r2", "r1"}
	if diff := cmp.Diff(wantRecentRuns, byHost["flappy"].RecentRuns); diff != "" {
		t.Errorf("flappy recent runs mismatch (-want +got):\n%s", diff)
	}
	if h := byHost["fixed"]; h.Flips != 1 || h.Flaky {
		t.Errorf("fixed = flips %d flaky %v, want 1 false", h.Flips, h.Flaky)
	}
	if h := byHost["solid"]; h.Flips != 0 || h.Flaky {
		t.Errorf("solid = flips %d flaky %v, want 0 false", h.Flips, h.Flaky)
	}
}

// testSubSecondHostOrder pins the order of host summaries that share a second, which is where the
// stored timestamp stops being a reliable sort key.
//
// Two things go wrong there. Times are stored as RFC 3339 with the fractional second trimmed, so a
// run on a whole second is stored with no fraction at all and sorts, as text, after a later run in
// the same second. And two runs can carry the very same instant, which leaves the order undecided
// unless something else decides it, so the in-memory store answered differently from one call to
// the next. Both move the wrong row into the head of the window, so the fleet view reports the wrong
// current outcome for a host.
//
// The fixture covers both: r1 lands on a whole second, r4 a quarter second later, and r2 and r3
// share the same instant. The expected answer is checked exactly, not merely for self-consistency,
// because a wrong order can be perfectly repeatable. The whole fixture is rebuilt from a fresh store
// on every pass so a store that leans on map iteration order gets many chances to disagree.
func testSubSecondHostOrder(t *testing.T, newStore func() run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	half := base.Add(500 * time.Millisecond)
	writes := []struct {
		RunID string
		Worst string
		RanAt time.Time
	}{
		{RunID: "r1", Worst: "failed", RanAt: base},
		{RunID: "r2", Worst: "ok", RanAt: half},
		{RunID: "r3", Worst: "changed", RanAt: half},
		{RunID: "r4", Worst: "failed", RanAt: base.Add(250 * time.Millisecond)},
	}
	// Newest first: r3 and r2 tie on the half second and the run id breaks it, then r4, then r1 on
	// the whole second, which plain text order would have hoisted to the front.
	wantHealth := []run.HostHealth{{
		Host: "db01", Failures: 2, Total: 4, LastOutcome: "changed", LastRun: half, Flips: 1,
		Recent:     []string{"changed", "ok", "failed", "failed"},
		RecentRuns: []string{"r3", "r2", "r4", "r1"},
	}}
	wantWindowed := []run.HostHealth{{
		Host: "db01", Failures: 0, Total: 1, LastOutcome: "changed", LastRun: half,
		Recent: []string{"changed"}, RecentRuns: []string{"r3"},
	}}
	wantHistory := []string{"r3", "r2", "r4", "r1"}

	const passes = 20
	for pass := range passes {
		store := newStore()
		for _, w := range writes {
			sums := []run.HostSummary{{Host: "db01", Worst: w.Worst, RanAt: w.RanAt}}
			if err := store.SaveHostSummary(ctx, w.RunID, sums); err != nil {
				t.Fatalf("pass %d: SaveHostSummary() error = %v", pass, err)
			}
		}
		health, err := store.FleetHealth(ctx, 10)
		if err != nil {
			t.Fatalf("pass %d: FleetHealth() error = %v", pass, err)
		}
		if diff := cmp.Diff(wantHealth, health, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("pass %d: FleetHealth mismatch (-want +got):\n%s", pass, diff)
		}
		windowed, err := store.FleetHealth(ctx, 1)
		if err != nil {
			t.Fatalf("pass %d: FleetHealth(1) error = %v", pass, err)
		}
		if diff := cmp.Diff(wantWindowed, windowed, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("pass %d: FleetHealth(1) mismatch (-want +got):\n%s", pass, diff)
		}
		history, err := store.HostHistory(ctx, "db01", 10)
		if err != nil {
			t.Fatalf("pass %d: HostHistory() error = %v", pass, err)
		}
		gotHistory := make([]string, len(history))
		for i, hs := range history {
			gotHistory[i] = hs.RunID
		}
		if diff := cmp.Diff(wantHistory, gotHistory, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("pass %d: HostHistory order mismatch (-want +got):\n%s", pass, diff)
		}
	}
}

// testHostHistory verifies per host history comes back newest first with run ids and windowing.
func testHostHistory(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, worst := range []string{"ok", "failed", "changed"} {
		sums := []run.HostSummary{
			{Host: "db01", Worst: worst, DurationSeconds: float64(i + 1), RanAt: base.Add(time.Duration(i) * time.Hour)},
			{Host: "web01", Worst: "ok", RanAt: base.Add(time.Duration(i) * time.Hour)},
		}
		if err := store.SaveHostSummary(ctx, fmt.Sprintf("r%d", i), sums); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
	}

	history, err := store.HostHistory(ctx, "db01", 10)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history len = %d, want 3", len(history))
	}
	if history[0].RunID != "r2" || history[0].Worst != "changed" {
		t.Errorf("newest = %+v, want run r2 worst changed", history[0])
	}
	for _, hs := range history {
		if hs.Host != "db01" {
			t.Errorf("history returned host %q", hs.Host)
		}
		if hs.RunID == "" {
			t.Error("history entry missing run id")
		}
	}

	limited, err := store.HostHistory(ctx, "db01", 1)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	if len(limited) != 1 || limited[0].RunID != "r2" {
		t.Errorf("limited = %+v, want only r2", limited)
	}

	empty, err := store.HostHistory(ctx, "ghost", 5)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("unknown host history = %+v, want empty", empty)
	}
}

// testTaskTrends verifies task durations persist and aggregate over the recent window.
func testTaskTrends(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	save := func(runID string, at time.Time, tasks map[string]float64) {
		var sums []run.TaskSummary
		for task, seconds := range tasks {
			sums = append(sums, run.TaskSummary{Task: task, Seconds: seconds, RanAt: at})
		}
		if err := store.SaveTaskSummary(ctx, runID, sums); err != nil {
			t.Fatalf("SaveTaskSummary() error = %v", err)
		}
	}
	save("r1", base, map[string]float64{"install": 10, "restart": 1})
	save("r2", base.Add(time.Hour), map[string]float64{"install": 20, "restart": 1})
	save("r3", base.Add(2*time.Hour), map[string]float64{"install": 30})

	trends, err := store.TaskTrends(ctx, 10)
	if err != nil {
		t.Fatalf("TaskTrends() error = %v", err)
	}
	byTask := make(map[string]run.TaskTrend, len(trends))
	for _, tr := range trends {
		byTask[tr.Task] = tr
	}
	install := byTask["install"]
	if install.Runs != 3 || install.AvgSeconds != 20 || install.LastSeconds != 30 {
		t.Errorf("install = %+v, want runs 3 avg 20 last 30", install)
	}
	restart := byTask["restart"]
	if restart.Runs != 2 || restart.AvgSeconds != 1 {
		t.Errorf("restart = %+v, want runs 2 avg 1", restart)
	}

	windowed, err := store.TaskTrends(ctx, 1)
	if err != nil {
		t.Fatalf("TaskTrends() error = %v", err)
	}
	byTask = make(map[string]run.TaskTrend, len(windowed))
	for _, tr := range windowed {
		byTask[tr.Task] = tr
	}
	if w := byTask["install"]; w.Runs != 1 || w.AvgSeconds != 30 {
		t.Errorf("windowed install = %+v, want runs 1 avg 30", w)
	}
}

// testWorkers verifies executors are listed from their leases with active counts and freshness,
// and that a lease older than run.WorkerWindow is excluded.
func testWorkers(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	older, newer := base, base.Add(time.Minute)
	stale := base.Add(-run.WorkerWindow)
	for _, r := range []*run.Run{
		{ID: "r1", Playbook: "p", Status: run.StatusRunning, CreatedAt: base, ClaimedBy: "goat-1", ClaimedAt: &newer},
		{ID: "r2", Playbook: "p", Status: run.StatusSucceeded, CreatedAt: base, ClaimedBy: "goat-1", ClaimedAt: &older},
		{ID: "r3", Playbook: "p", Status: run.StatusRunning, CreatedAt: base, ClaimedBy: "serve-1", ClaimedAt: &older},
		{ID: "r4", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "r5", Playbook: "p", Status: run.StatusSucceeded, CreatedAt: stale, ClaimedBy: "ghost-1", ClaimedAt: &stale},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	workers, err := store.Workers(ctx)
	if err != nil {
		t.Fatalf("Workers() error = %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(workers))
	}
	if workers[0].Owner != "goat-1" || workers[0].Active != 1 || workers[0].Completed != 1 || !workers[0].LastSeen.Equal(newer) {
		t.Errorf("first worker = %+v, want goat-1 active 1 completed 1 seen %v", workers[0], newer)
	}
	if workers[1].Owner != "serve-1" || workers[1].Active != 1 {
		t.Errorf("second worker = %+v, want serve-1 active 1", workers[1])
	}
}

// testHostCosts verifies per host durations persist and average over the recent window.
func testHostCosts(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	save := func(runID string, at time.Time, durations map[string]float64) {
		var sums []run.HostSummary
		for host, d := range durations {
			sums = append(sums, run.HostSummary{Host: host, Worst: "ok", DurationSeconds: d, RanAt: at})
		}
		if err := store.SaveHostSummary(ctx, runID, sums); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
	}
	save("r1", base, map[string]float64{"db01": 30, "web01": 4})
	save("r2", base.Add(time.Hour), map[string]float64{"db01": 20, "web01": 2})
	save("r3", base.Add(2*time.Hour), map[string]float64{"db01": 10})

	costs, err := store.HostCosts(ctx, 10)
	if err != nil {
		t.Fatalf("HostCosts() error = %v", err)
	}
	want := map[string]float64{"db01": 20, "web01": 3}
	if diff := cmp.Diff(want, costs, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("HostCosts() mismatch (-want +got):\n%s", diff)
	}

	windowed, err := store.HostCosts(ctx, 1)
	if err != nil {
		t.Fatalf("HostCosts() error = %v", err)
	}
	wantWindowed := map[string]float64{"db01": 10, "web01": 2}
	if diff := cmp.Diff(wantWindowed, windowed, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("HostCosts(window 1) mismatch (-want +got):\n%s", diff)
	}

	empty, err := store.HostCosts(ctx, 0)
	if err != nil {
		t.Fatalf("HostCosts() error = %v", err)
	}
	if diff := cmp.Diff(wantWindowed, empty, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("HostCosts(window 0) should clamp to 1 (-want +got):\n%s", diff)
	}
}

// testHostFacts verifies gathered facts round trip per host, that a later gather replaces an
// earlier one, and that a host nobody has gathered reports not found rather than an empty record.
func testHostFacts(t *testing.T, store run.Store) {
	ctx := context.Background()
	at := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)

	if _, err := store.HostFactsFor(ctx, "never-gathered"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("HostFactsFor(unknown) error = %v, want ErrNotFound", err)
	}

	first := []run.HostFacts{
		{Host: "web01", Facts: map[string]string{"distribution": "Debian", "kernel": "6.1.0"}, GatheredAt: at},
		{Host: "db01", Facts: map[string]string{"distribution": "Ubuntu"}, GatheredAt: at},
	}
	if err := store.SaveHostFacts(ctx, "run_1", first); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}
	got, err := store.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if got.Facts["distribution"] != "Debian" || got.Facts["kernel"] != "6.1.0" || got.RunID != "run_1" {
		t.Errorf("facts = %+v, want the Debian gather from run_1", got)
	}

	// A later gather replaces the earlier one, since the newest is the truth about a host.
	later := at.Add(time.Hour)
	if err := store.SaveHostFacts(ctx, "run_2", []run.HostFacts{
		{Host: "web01", Facts: map[string]string{"distribution": "Debian", "kernel": "6.6.0"}, GatheredAt: later},
	}); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}
	got, err = store.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if got.Facts["kernel"] != "6.6.0" || got.RunID != "run_2" {
		t.Errorf("facts after regather = %+v, want the run_2 gather", got)
	}

	// The other host is untouched by that replacement.
	other, err := store.HostFactsFor(ctx, "db01")
	if err != nil || other.Facts["distribution"] != "Ubuntu" {
		t.Errorf("db01 facts = %+v, err %v, want the original Ubuntu gather", other, err)
	}

	// An empty set is a no-op rather than an error, so a run that gathered nothing is fine.
	if err := store.SaveHostFacts(ctx, "run_3", nil); err != nil {
		t.Errorf("SaveHostFacts(nil) error = %v, want nil", err)
	}
}

// testTrimSummaries verifies the only bound on the two summary tables.
//
// Summaries outlive the runs they came from, so nothing in retention deletes them by age and the
// tables grow by one row per host per run forever. TrimSummaries is what stops that, and the
// contract it has to keep on every backend is exact: the newest keep rows per host and per task
// survive, the older ones are gone, a key already under the limit is untouched, the two tables are
// trimmed independently, and no keep can empty a key entirely.
func testTrimSummaries(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	const (
		keep      = 3
		busyRuns  = 10
		taskFrom  = 3
		quietRuns = 2
	)
	runID := func(i int) string { return fmt.Sprintf("run_%02d", i) }

	// The host table gets more rows than the task table, so a store that trimmed one and reported
	// the other's count, or trimmed both to the same depth by accident, is caught by the total.
	for i := range busyRuns {
		at := base.Add(time.Duration(i) * time.Minute)
		hosts := []run.HostSummary{{Host: "busy", Worst: "ok", RanAt: at}}
		if i < quietRuns {
			hosts = append(hosts, run.HostSummary{Host: "quiet", Worst: "ok", RanAt: at})
		}
		if err := store.SaveHostSummary(ctx, runID(i), hosts); err != nil {
			t.Fatalf("SaveHostSummary(%s) error = %v", runID(i), err)
		}
		if i < taskFrom {
			continue
		}
		tasks := []run.TaskSummary{{Task: "busy-task", Seconds: float64(i), RanAt: at}}
		if i < taskFrom+quietRuns {
			tasks = append(tasks, run.TaskSummary{Task: "quiet-task", Seconds: 1, RanAt: at})
		}
		if err := store.SaveTaskSummary(ctx, runID(i), tasks); err != nil {
			t.Fatalf("SaveTaskSummary(%s) error = %v", runID(i), err)
		}
	}

	wantDeleted := (busyRuns - keep) + (busyRuns - taskFrom - keep)
	deleted, err := store.TrimSummaries(ctx, keep)
	if err != nil {
		t.Fatalf("TrimSummaries() error = %v", err)
	}
	if deleted != wantDeleted {
		t.Errorf("TrimSummaries() deleted = %d, want %d", deleted, wantDeleted)
	}

	// The newest survive, in order, and the oldest are gone. Trimming the wrong end would leave the
	// same row count and answer every fleet view with a fossil.
	history, err := store.HostHistory(ctx, "busy", busyRuns)
	if err != nil {
		t.Fatalf("HostHistory(busy) error = %v", err)
	}
	gotRuns := make([]string, len(history))
	for i, hs := range history {
		gotRuns[i] = hs.RunID
	}
	wantRuns := []string{runID(9), runID(8), runID(7)}
	if diff := cmp.Diff(wantRuns, gotRuns, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("busy history after trim mismatch (-want +got):\n%s", diff)
	}

	quiet, err := store.HostHistory(ctx, "quiet", busyRuns)
	if err != nil {
		t.Fatalf("HostHistory(quiet) error = %v", err)
	}
	if len(quiet) != quietRuns {
		t.Errorf("quiet history len = %d, want %d untouched", len(quiet), quietRuns)
	}

	trends, err := store.TaskTrends(ctx, busyRuns)
	if err != nil {
		t.Fatalf("TaskTrends() error = %v", err)
	}
	runsByTask := make(map[string]int, len(trends))
	for _, tr := range trends {
		runsByTask[tr.Task] = tr.Runs
	}
	want := map[string]int{"busy-task": keep, "quiet-task": quietRuns}
	if diff := cmp.Diff(want, runsByTask, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("task rows after trim mismatch (-want +got):\n%s", diff)
	}

	// A second sweep over tables already at the limit deletes nothing.
	again, err := store.TrimSummaries(ctx, keep)
	if err != nil {
		t.Fatalf("TrimSummaries() second error = %v", err)
	}
	if again != 0 {
		t.Errorf("TrimSummaries() second deleted = %d, want 0", again)
	}

	// A keep of zero is not a wipe. Each key is left with its newest row, so a misconfiguration
	// cannot erase the fleet's history.
	if _, err := store.TrimSummaries(ctx, 0); err != nil {
		t.Fatalf("TrimSummaries(0) error = %v", err)
	}
	floored, err := store.HostHistory(ctx, "busy", busyRuns)
	if err != nil {
		t.Fatalf("HostHistory(busy) after zero keep error = %v", err)
	}
	if len(floored) != 1 || floored[0].RunID != runID(9) {
		t.Errorf("busy history after zero keep = %+v, want only %s", floored, runID(9))
	}
}
