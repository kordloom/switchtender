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
	t.Run("non-terminal runs", func(t *testing.T) { testNonTerminal(t, newStore()) })
	t.Run("fleet health ranking", func(t *testing.T) { testFleetHealth(t, newStore()) })
}

// sampleRun returns a fully populated terminal run with deterministic times.
func sampleRun(id string) *run.Run {
	created := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	started := created.Add(time.Second)
	ended := created.Add(2 * time.Second)
	code := 0
	return &run.Run{
		ID: id, Playbook: "play.yml", Inventory: "inventory.ini",
		Status: run.StatusSucceeded, ExitCode: &code,
		CreatedAt: created, StartedAt: &started, EndedAt: &ended,
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
	for i := 0; i < 2; i++ {
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
