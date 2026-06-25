package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/yardmaster/internal/event"
)

func TestMemStoreSaveGet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	r := &Run{ID: "run_1", Playbook: "play.yml", Status: StatusPending, CreatedAt: time.Now()}

	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(r, got, cmpopts.EquateEmpty()); diff != "" {
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

func TestMemStoreGetNotFound(t *testing.T) {
	t.Parallel()
	_, err := NewMemStore().Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestMemStoreList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	base := time.Now()
	for _, r := range []*Run{
		{ID: "a", CreatedAt: base},
		{ID: "b", CreatedAt: base.Add(time.Second)},
		{ID: "c", CreatedAt: base.Add(2 * time.Second)},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	runs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	gotIDs := []string{runs[0].ID, runs[1].ID, runs[2].ID}
	if diff := cmp.Diff([]string{"c", "b", "a"}, gotIDs); diff != "" {
		t.Errorf("List() order mismatch (-want +got):\n%s", diff)
	}
}

func TestMemStoreLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()

	if err := store.AppendLog(ctx, "missing", []byte("hi")); !errors.Is(err, ErrNotFound) {
		t.Errorf("AppendLog() on missing run = %v, want ErrNotFound", err)
	}
	if _, err := store.Log(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Log() on missing run = %v, want ErrNotFound", err)
	}

	if err := store.Save(ctx, &Run{ID: "x", CreatedAt: time.Now()}); err != nil {
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
	if got := string(body); got != "hello world" {
		t.Errorf("Log() = %q, want %q", got, "hello world")
	}

	body[0] = 'X'
	again, err := store.Log(ctx, "x")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if again[0] != 'h' {
		t.Error("mutating returned log changed stored state")
	}
}

func TestMemStoreEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()

	batch := []event.Event{{Type: event.TypePlayStart, Play: "demo"}}
	if err := store.AppendEvents(ctx, "missing", batch); !errors.Is(err, ErrNotFound) {
		t.Errorf("AppendEvents() on missing run = %v, want ErrNotFound", err)
	}
	if _, err := store.Events(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Events() on missing run = %v, want ErrNotFound", err)
	}

	if err := store.Save(ctx, &Run{ID: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.AppendEvents(ctx, "x", batch); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	if err := store.AppendEvents(ctx, "x",
		[]event.Event{{Type: event.TypeTaskStart, Task: "go"}}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}

	got, err := store.Events(ctx, "x")
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	want := []event.Event{
		{Type: event.TypePlayStart, Play: "demo"},
		{Type: event.TypeTaskStart, Task: "go"},
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
		t.Error("mutating returned events changed stored state")
	}
}
