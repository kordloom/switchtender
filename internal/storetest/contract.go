// Package storetest provides a shared behavior contract for run.Store implementations so that
// different backends, such as the in memory store and the SQLite store, cannot drift apart.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/run"
)

// Contract runs the full run.Store contract against a fresh store from newStore. Each subtest gets
// its own store so they do not share state.
func Contract(t *testing.T, newStore func() run.Store) {
	t.Helper()
	t.Run("save and get", func(t *testing.T) { testSaveGet(t, newStore()) })
	t.Run("get missing", func(t *testing.T) { testGetNotFound(t, newStore()) })
	t.Run("save updates existing", func(t *testing.T) { testSaveUpdate(t, newStore()) })
	t.Run("list newest first", func(t *testing.T) { testList(t, newStore()) })
	t.Run("log append and read", func(t *testing.T) { testLog(t, newStore()) })
	t.Run("events append and read", func(t *testing.T) { testEvents(t, newStore()) })
	t.Run("shards excluded from list", func(t *testing.T) { testShards(t, newStore()) })
	t.Run("pipeline steps ordered", func(t *testing.T) { testSteps(t, newStore()) })
	t.Run("non-terminal runs", func(t *testing.T) { testNonTerminal(t, newStore()) })
	t.Run("fleet health ranking", func(t *testing.T) { testFleetHealth(t, newStore()) })
	t.Run("host costs", func(t *testing.T) { testHostCosts(t, newStore()) })
	t.Run("flaky detection", func(t *testing.T) { testFlaky(t, newStore()) })
	t.Run("host history", func(t *testing.T) { testHostHistory(t, newStore()) })
	t.Run("task trends", func(t *testing.T) { testTaskTrends(t, newStore()) })
	t.Run("claim leases oldest", func(t *testing.T) { testClaim(t, newStore()) })
	t.Run("heartbeat and reclaim", func(t *testing.T) { testLeaseLifecycle(t, newStore()) })
	t.Run("cancel request", func(t *testing.T) { testRequestCancel(t, newStore()) })
}

// sampleRun returns a fully populated terminal run with deterministic times.
func sampleRun(id string) *run.Run {
	created := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	started := created.Add(time.Second)
	ended := created.Add(2 * time.Second)
	code := 0
	retryOf := "run_prior"
	return &run.Run{
		ID: id, Playbook: "play.yml", Inventory: "inventory.ini",
		Status: run.StatusSucceeded, ExitCode: &code,
		CreatedAt: created, StartedAt: &started, EndedAt: &ended,
		RetryOf:   &retryOf,
		ExtraVars: map[string]any{"version": "1.2.3"},
		Outputs:   map[string]any{"built": true, "count": float64(2)},
	}
}

// testSaveGet verifies a run round trips and that returned values are independent copies.
func testSaveGet(t *testing.T, store run.Store) {
	ctx := context.Background()
	want := sampleRun("run_1")
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Get() mismatch (-want +got):\n%s", diff)
	}

	got.Playbook = "mutated"
	again, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Playbook != "play.yml" {
		t.Error("mutating the returned run changed stored state")
	}
}

// testGetNotFound verifies a missing run reports run.ErrNotFound across all read methods.
func testGetNotFound(t *testing.T, store run.Store) {
	ctx := context.Background()
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Get() = %v, want ErrNotFound", err)
	}
	if _, err := store.Log(ctx, "missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Log() = %v, want ErrNotFound", err)
	}
	if _, err := store.Events(ctx, "missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Events() = %v, want ErrNotFound", err)
	}
	if err := store.AppendLog(ctx, "missing", []byte("x")); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("AppendLog() = %v, want ErrNotFound", err)
	}
	batch := []event.Event{{Type: event.TypePlayStart}}
	if err := store.AppendEvents(ctx, "missing", batch); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("AppendEvents() = %v, want ErrNotFound", err)
	}
}

// testSaveUpdate verifies that saving an existing id replaces the stored run.
func testSaveUpdate(t *testing.T, store run.Store) {
	ctx := context.Background()
	r := &run.Run{ID: "run_1", Playbook: "p", Status: run.StatusPending, CreatedAt: time.Now()}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	code := 2
	r.Status = run.StatusFailed
	r.ExitCode = &code
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusFailed || got.ExitCode == nil || *got.ExitCode != 2 {
		t.Errorf("after update got status=%q exit=%v, want failed exit=2", got.Status, got.ExitCode)
	}
}

// testList verifies runs come back newest first.
func testList(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range []*run.Run{
		{ID: "a", Status: run.StatusSucceeded, CreatedAt: base},
		{ID: "b", Status: run.StatusSucceeded, CreatedAt: base.Add(time.Second)},
		{ID: "c", Status: run.StatusSucceeded, CreatedAt: base.Add(2 * time.Second)},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	runs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	gotIDs := make([]string, len(runs))
	for i, r := range runs {
		gotIDs[i] = r.ID
	}
	if diff := cmp.Diff([]string{"c", "b", "a"}, gotIDs); diff != "" {
		t.Errorf("List() order mismatch (-want +got):\n%s", diff)
	}
}

// testLog verifies log append, read, ordering, and copy independence.
func testLog(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{ID: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.AppendLog(ctx, "x", []byte("hello ")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := store.AppendLog(ctx, "x", []byte("world")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}

	body, err := store.Log(ctx, "x")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if string(body) != "hello world" {
		t.Errorf("Log() = %q, want %q", body, "hello world")
	}

	body[0] = 'X'
	again, err := store.Log(ctx, "x")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if len(again) == 0 || again[0] != 'h' {
		t.Error("mutating the returned log changed stored state")
	}
}

// testEvents verifies event append, read, ordering, and copy independence.
func testEvents(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{ID: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.AppendEvents(ctx, "x",
		[]event.Event{{Type: event.TypePlayStart, Time: at, Play: "demo"}}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	if err := store.AppendEvents(ctx, "x",
		[]event.Event{{Type: event.TypeRunnerOK, Time: at, Play: "demo", Task: "t", Host: "h"}}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}

	got, err := store.Events(ctx, "x")
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	want := []event.Event{
		{Type: event.TypePlayStart, Time: at, Play: "demo"},
		{Type: event.TypeRunnerOK, Time: at, Play: "demo", Task: "t", Host: "h"},
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Events() mismatch (-want +got):\n%s", diff)
	}

	got[0].Play = "mutated"
	again, err := store.Events(ctx, "x")
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	if again[0].Play != "demo" {
		t.Error("mutating the returned events changed stored state")
	}
}

// testShards verifies that shard runs are excluded from List and returned by Shards in order.
func testShards(t *testing.T, store run.Store) {
	ctx := context.Background()
	parentID := "run_parent"
	if err := store.Save(ctx,
		&run.Run{ID: parentID, Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for i := range 2 {
		idx, count := i, 2
		child := &run.Run{
			ID: fmt.Sprintf("run_child_%d", i), Status: run.StatusSucceeded, CreatedAt: time.Now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &count, Limit: "host" + fmt.Sprint(i),
		}
		if err := store.Save(ctx, child); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	top, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	parentSeen := false
	for _, r := range top {
		if r.ParentID != nil {
			t.Errorf("List returned shard run %s", r.ID)
		}
		if r.ID == parentID {
			parentSeen = true
		}
	}
	if !parentSeen {
		t.Error("List did not return the parent run")
	}

	shards, err := store.Shards(ctx, parentID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("Shards len = %d, want 2", len(shards))
	}
	if shards[0].ShardIndex == nil || *shards[0].ShardIndex != 0 {
		t.Errorf("first shard index = %v, want 0", shards[0].ShardIndex)
	}
	if shards[0].Limit != "host0" {
		t.Errorf("shard limit = %q, want host0", shards[0].Limit)
	}
}

// testSteps verifies pipeline step runs are ordered by step index and excluded from List.
func testSteps(t *testing.T, store run.Store) {
	ctx := context.Background()
	parentID := "run_pipeline"
	if err := store.Save(ctx, &run.Run{
		ID: parentID, Kind: run.KindPipeline, Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for i := range 2 {
		idx := i
		if err := store.Save(ctx, &run.Run{
			ID: fmt.Sprintf("run_step_%d", i), Playbook: fmt.Sprintf("step%d.yml", i),
			Status: run.StatusSucceeded, CreatedAt: time.Now(),
			ParentID: &parentID, StepIndex: &idx, StepName: fmt.Sprintf("step-%d", i),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	steps, err := store.Steps(ctx, parentID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(steps))
	}
	if steps[0].StepName != "step-0" || steps[0].StepIndex == nil || *steps[0].StepIndex != 0 {
		t.Errorf("first step = %+v, want name step-0 index 0", steps[0])
	}

	parent, err := store.Get(ctx, parentID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if parent.Kind != run.KindPipeline {
		t.Errorf("parent kind = %q, want %q", parent.Kind, run.KindPipeline)
	}

	top, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, r := range top {
		if r.ParentID != nil {
			t.Errorf("List returned step run %s", r.ID)
		}
	}
}

// testNonTerminal verifies only pending and running runs are returned, including shards.
func testNonTerminal(t *testing.T, store run.Store) {
	ctx := context.Background()
	parentID := "p"
	idx, count := 0, 1
	for _, r := range []*run.Run{
		{ID: "pending", Status: run.StatusPending, CreatedAt: time.Now()},
		{ID: "running", Status: run.StatusRunning, CreatedAt: time.Now()},
		{ID: "done", Status: run.StatusSucceeded, CreatedAt: time.Now()},
		{ID: "gone", Status: run.StatusInterrupted, CreatedAt: time.Now()},
		{
			ID: "shard", Status: run.StatusRunning, CreatedAt: time.Now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
		},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	got, err := store.NonTerminal(ctx)
	if err != nil {
		t.Fatalf("NonTerminal() error = %v", err)
	}
	seen := make(map[string]bool, len(got))
	for _, r := range got {
		seen[r.ID] = true
	}
	if !seen["pending"] || !seen["running"] || !seen["shard"] {
		t.Errorf("NonTerminal missing active runs, got %v", seen)
	}
	if seen["done"] || seen["gone"] {
		t.Error("NonTerminal returned a terminal run")
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
	if h := byHost["fixed"]; h.Flips != 1 || h.Flaky {
		t.Errorf("fixed = flips %d flaky %v, want 1 false", h.Flips, h.Flaky)
	}
	if h := byHost["solid"]; h.Flips != 0 || h.Flaky {
		t.Errorf("solid = flips %d flaky %v, want 0 false", h.Flips, h.Flaky)
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

// testClaim verifies claiming takes the oldest unclaimed plain run exactly once and skips
// children, parents, and non-pending runs.
func testClaim(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	parentID := "run_split"
	idx, count := 0, 1
	for _, r := range []*run.Run{
		{ID: "run_new", Playbook: "p", Status: run.StatusPending, CreatedAt: base.Add(time.Minute)},
		{ID: "run_old", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "run_done", Playbook: "p", Status: run.StatusSucceeded, CreatedAt: base},
		{ID: parentID, Playbook: "p", Kind: run.KindSplit, Status: run.StatusPending, CreatedAt: base},
		{
			ID: "run_shard", Playbook: "p", Status: run.StatusPending, CreatedAt: base,
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
		},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	first, err := store.Claim(ctx, "worker-a")
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if first.ID != "run_old" || first.ClaimedBy != "worker-a" || first.ClaimedAt == nil {
		t.Errorf("first claim = %+v, want run_old leased by worker-a", first)
	}

	second, err := store.Claim(ctx, "worker-b")
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if second.ID != "run_new" {
		t.Errorf("second claim = %s, want run_new", second.ID)
	}

	if _, err := store.Claim(ctx, "worker-c"); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("third claim error = %v, want ErrNonePending", err)
	}
}

// testLeaseLifecycle verifies heartbeats renew a lease and stale leases requeue pending runs and
// interrupt running ones.
func testLeaseLifecycle(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{
		ID: "run_q", Playbook: "p", Status: run.StatusPending, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	claimed, err := store.Claim(ctx, "worker-a")
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}

	if err := store.Heartbeat(ctx, claimed.ID, "worker-a"); err != nil {
		t.Errorf("Heartbeat() error = %v", err)
	}
	if err := store.Heartbeat(ctx, claimed.ID, "impostor"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Heartbeat() wrong owner error = %v, want ErrNotFound", err)
	}

	// A fresh lease survives a sweep with an old cutoff.
	n, err := store.ReclaimStale(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	if n != 0 {
		t.Errorf("ReclaimStale() = %d, want 0 while the lease is fresh", n)
	}

	// A future cutoff makes the lease stale: the pending run goes back in the queue.
	n, err = store.ReclaimStale(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	if n != 1 {
		t.Errorf("ReclaimStale() = %d, want 1 requeued", n)
	}
	back, err := store.Get(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if back.ClaimedBy != "" || back.Status != run.StatusPending {
		t.Errorf("requeued run = %+v, want unclaimed pending", back)
	}

	// A stale lease on a running run interrupts it instead.
	reclaimed, err := store.Claim(ctx, "worker-b")
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	reclaimed.Status = run.StatusRunning
	if err := store.Save(ctx, reclaimed); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if n, err = store.ReclaimStale(ctx, time.Now().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("ReclaimStale() = %d, %v, want 1 interrupted", n, err)
	}
	gone, err := store.Get(ctx, reclaimed.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if gone.Status != run.StatusInterrupted || gone.EndedAt == nil {
		t.Errorf("stale running run = %+v, want interrupted with an end time", gone)
	}
}

// testRequestCancel verifies the cancel flag round trips and unknown runs report ErrNotFound.
func testRequestCancel(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{
		ID: "run_c", Playbook: "p", Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.RequestCancel(ctx, "run_c"); err != nil {
		t.Fatalf("RequestCancel() error = %v", err)
	}
	got, err := store.Get(ctx, "run_c")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.CancelRequested {
		t.Error("CancelRequested not set after RequestCancel")
	}
	if err := store.RequestCancel(ctx, "ghost"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("RequestCancel(ghost) error = %v, want ErrNotFound", err)
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
