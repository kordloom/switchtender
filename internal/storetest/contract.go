// Package storetest provides a shared behavior contract for run.Store implementations so that
// different backends, such as the in memory store and the SQLite store, cannot drift apart.
package storetest

import (
	"context"
	"errors"
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
