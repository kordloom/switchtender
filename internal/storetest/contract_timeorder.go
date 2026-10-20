package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// timeOrderContract pins that runs order and compare by time below the second. Stored times trim
// the fraction, so a run on a whole second stores "10:00:00Z", which sorts as text after
// "10:00:00.5Z" although it came first. Each case below fails on a store that orders or compares
// the raw text.
func timeOrderContract(t *testing.T, newStore func() run.Store) {
	t.Helper()
	t.Run("list is in time order below the second", func(t *testing.T) {
		testListTimeOrder(t, newStore())
	})
	t.Run("list bounds are exact below the second", func(t *testing.T) {
		testListTimeBounds(t, newStore())
	})
	t.Run("purge at a whole-second cutoff keeps what came after it", func(t *testing.T) {
		testPurgeCutoff(t, newStore(), time.Second, []string{"t01", "t012"})
	})
	t.Run("purge between seconds deletes the whole second before it", func(t *testing.T) {
		testPurgeCutoff(t, newStore(), 500*time.Millisecond, []string{"t005", "t007", "t01", "t012"})
	})
}

// subSecondRuns are finished top-level runs at instants inside and around one second, in a
// shuffled insertion order, keyed by an id that says when each was made.
var subSecondRuns = []struct {
	// ID names the run by its offset from the base instant.
	ID string
	// Offset is the run's creation time after the base instant.
	Offset time.Duration
}{
	{ID: "t005", Offset: 500 * time.Millisecond},
	{ID: "t01", Offset: time.Second},
	{ID: "t00", Offset: 0},
	{ID: "t012", Offset: 1200 * time.Millisecond},
	{ID: "t0000001", Offset: time.Microsecond},
	{ID: "t007", Offset: 700 * time.Millisecond},
	{ID: "t001", Offset: 100 * time.Millisecond},
}

// subSecondBase is the instant subSecondRuns are made after, on a whole second.
var subSecondBase = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

// saveSubSecondRuns saves subSecondRuns as finished top-level runs.
func saveSubSecondRuns(t *testing.T, store run.Store) {
	t.Helper()
	for _, sr := range subSecondRuns {
		r := &run.Run{ID: sr.ID, Playbook: "p.yml", Status: run.StatusSucceeded,
			CreatedAt: subSecondBase.Add(sr.Offset)}
		if err := store.Save(context.Background(), r); err != nil {
			t.Fatalf("Save(%s) error = %v", sr.ID, err)
		}
	}
}

// testListTimeOrder pins that the run list and its pages, newest and oldest first, put runs made
// inside one second in the order they were made.
func testListTimeOrder(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	saveSubSecondRuns(t, store)
	oldest := []string{"t00", "t0000001", "t001", "t005", "t007", "t01", "t012"}
	newest := make([]string, len(oldest))
	for i, id := range oldest {
		newest[len(oldest)-1-i] = id
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff(newest, runIDs(all)); diff != "" {
		t.Errorf("List() mismatch (-want +got):\n%s", diff)
	}
	tests := []struct {
		WantIDs []string
		Filter  run.ListFilter
	}{{ // Test 0: Newest first.
		WantIDs: newest,
	}, { // Test 1: Oldest first.
		WantIDs: oldest, Filter: run.ListFilter{OldestFirst: true},
	}}
	for testNum, test := range tests {
		got, err := store.ListPage(ctx, test.Filter, 100, 0)
		if err != nil {
			t.Fatalf("test %d: ListPage() error = %v", testNum, err)
		}
		if diff := cmp.Diff(test.WantIDs, runIDs(got)); diff != "" {
			t.Errorf("test %d: ListPage() mismatch (-want +got):\n%s", testNum, diff)
		}
	}
}

// testListTimeBounds pins that the run list's After bound includes a run made at it and excludes
// one made before it, and its Before bound excludes a run made at or after it, below the second.
func testListTimeBounds(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	saveSubSecondRuns(t, store)
	tests := []struct {
		WantIDs []string
		After   time.Duration
		Before  time.Duration
	}{{ // Test 0: Between two fractions, which excludes the whole second before and after.
		WantIDs: []string{"t007", "t005"}, After: 500 * time.Millisecond, Before: time.Second,
	}, { // Test 1: From a whole second to a fraction after it.
		WantIDs: []string{"t01"}, After: time.Second, Before: 1200 * time.Millisecond,
	}, { // Test 2: From a fraction to the next whole second's fraction.
		WantIDs: []string{"t01", "t007"}, After: 600 * time.Millisecond, Before: 1100 * time.Millisecond,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			filter := run.ListFilter{After: subSecondBase.Add(test.After),
				Before: subSecondBase.Add(test.Before)}
			got, err := store.ListPage(ctx, filter, 100, 0)
			if err != nil {
				t.Fatalf("ListPage() error = %v", err)
			}
			if diff := cmp.Diff(test.WantIDs, runIDs(got)); diff != "" {
				t.Errorf("ListPage() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// testPurgeCutoff pins that a purge at cut after the base instant deletes exactly the runs made
// before it and keeps wantKept, newest first.
func testPurgeCutoff(t *testing.T, store run.Store, cut time.Duration, wantKept []string) {
	t.Helper()
	ctx := context.Background()
	saveSubSecondRuns(t, store)
	if _, err := store.PurgeRunsBefore(ctx, subSecondBase.Add(cut)); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := make([]string, len(wantKept))
	for i, id := range wantKept {
		want[len(wantKept)-1-i] = id
	}
	if diff := cmp.Diff(want, runIDs(got)); diff != "" {
		t.Errorf("runs kept mismatch (-want +got):\n%s", diff)
	}
}
