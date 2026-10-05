// Package attentiontest provides the shared behavior contract for attention.Store implementations,
// so the in-memory store and both database stores cannot drift apart.
package attentiontest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/attention"
)

// Contract runs the attention.Store contract against a fresh store from newStore for each subtest.
// The subtests run one after another, because a database store hands each of them the same
// database, emptied first.
func Contract(t *testing.T, newStore func() attention.Store) {
	t.Helper()
	t.Run("note and list workers", func(t *testing.T) { testNoteWorkers(t, newStore()) })
	t.Run("a report keeps the first sighting", func(t *testing.T) { testFirstSeenKept(t, newStore()) })
	t.Run("touch refreshes only a known worker", func(t *testing.T) { testTouch(t, newStore()) })
	t.Run("workers since excludes the silent", func(t *testing.T) { testSince(t, newStore()) })
	t.Run("an alert is claimed once", func(t *testing.T) { testClaimOnce(t, newStore()) })
	t.Run("racing claims yield one winner", func(t *testing.T) { testClaimRace(t, newStore()) })
}

// workerView is the part of a worker report the contract compares, with the times left out.
type workerView struct {
	// Owner is the lease name.
	Owner string
	// Queues are the queues it serves.
	Queues []string
	// Slots is how many runs it takes at once.
	Slots int
}

// views reduces reports to what the contract compares.
func views(ws []attention.Worker) []workerView {
	out := make([]workerView, 0, len(ws))
	for _, w := range ws {
		out = append(out, workerView{Owner: w.Owner, Queues: w.Queues, Slots: w.Slots})
	}
	return out
}

// past is far enough back that every report made during a test is after it.
func past() time.Time { return time.Now().Add(-time.Hour) }

// testNoteWorkers pins that a report round trips its queues and slots, including the default
// queue's empty name and a worker that names no queue, and that the list is ordered by owner.
func testNoteWorkers(t *testing.T, store attention.Store) {
	ctx := context.Background()
	tests := []struct {
		Owner  string
		Queues []string
		Slots  int
	}{{ // Test 0: The default queue's empty name survives.
		Owner: "worker-b", Queues: []string{""}, Slots: 4,
	}, { // Test 1: Several named queues survive in order.
		Owner: "worker-a", Queues: []string{"prod", "dmz"}, Slots: 2,
	}, { // Test 2: A worker that names no queue reads back as naming none.
		Owner: "worker-c", Queues: nil, Slots: 0,
	}}
	for testNum, test := range tests {
		if err := store.NoteWorker(ctx, test.Owner, test.Queues, test.Slots); err != nil {
			t.Fatalf("test %d: NoteWorker() error = %v", testNum, err)
		}
	}
	got, err := store.Workers(ctx, past())
	if err != nil {
		t.Fatalf("Workers() error = %v", err)
	}
	want := []workerView{
		{Owner: "worker-a", Queues: []string{"prod", "dmz"}, Slots: 2},
		{Owner: "worker-b", Queues: []string{""}, Slots: 4},
		{Owner: "worker-c", Queues: []string{}, Slots: 0},
	}
	if diff := cmp.Diff(want, views(got), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Workers() mismatch (-want +got):\n%s", diff)
	}
	for _, w := range got {
		if w.FirstSeen.IsZero() || w.LastSeen.IsZero() {
			t.Errorf("worker %s carries no sighting times: first %v, last %v", w.Owner,
				w.FirstSeen, w.LastSeen)
		}
	}
}

// testFirstSeenKept pins that a second report replaces the queues and slots and advances the last
// sighting while the first one stays, which is what dates a queue's wait for a free slot.
func testFirstSeenKept(t *testing.T, store attention.Store) {
	ctx := context.Background()
	if err := store.NoteWorker(ctx, "worker-a", []string{"prod"}, 2); err != nil {
		t.Fatalf("NoteWorker() error = %v", err)
	}
	first, err := store.Workers(ctx, past())
	if err != nil || len(first) != 1 {
		t.Fatalf("Workers() = %v, %v, want one worker", first, err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := store.NoteWorker(ctx, "worker-a", []string{"prod", "dmz"}, 8); err != nil {
		t.Fatalf("NoteWorker() again error = %v", err)
	}
	second, err := store.Workers(ctx, past())
	if err != nil || len(second) != 1 {
		t.Fatalf("Workers() = %v, %v, want one worker", second, err)
	}
	if diff := cmp.Diff([]workerView{{Owner: "worker-a", Queues: []string{"prod", "dmz"},
		Slots: 8}}, views(second)); diff != "" {
		t.Errorf("a second report did not replace queues and slots (-want +got):\n%s", diff)
	}
	if !second[0].FirstSeen.Equal(first[0].FirstSeen) {
		t.Errorf("first sighting moved from %v to %v, and it dates the worker's arrival",
			first[0].FirstSeen, second[0].FirstSeen)
	}
	if !second[0].LastSeen.After(first[0].LastSeen) {
		t.Errorf("last sighting did not advance: %v then %v", first[0].LastSeen, second[0].LastSeen)
	}
}

// testTouch pins that a heartbeat keeps a noted worker fresh and does not invent one it never heard
// from, since a heartbeat says nothing about the queues a worker serves.
func testTouch(t *testing.T, store attention.Store) {
	ctx := context.Background()
	if err := store.NoteWorker(ctx, "worker-a", []string{"prod"}, 2); err != nil {
		t.Fatalf("NoteWorker() error = %v", err)
	}
	before, err := store.Workers(ctx, past())
	if err != nil || len(before) != 1 {
		t.Fatalf("Workers() = %v, %v, want one worker", before, err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := store.TouchWorker(ctx, "worker-a"); err != nil {
		t.Fatalf("TouchWorker() error = %v", err)
	}
	if err := store.TouchWorker(ctx, "never-noted"); err != nil {
		t.Fatalf("TouchWorker() on an unknown worker error = %v", err)
	}
	after, err := store.Workers(ctx, past())
	if err != nil {
		t.Fatalf("Workers() error = %v", err)
	}
	if diff := cmp.Diff([]workerView{{Owner: "worker-a", Queues: []string{"prod"}, Slots: 2}},
		views(after)); diff != "" {
		t.Errorf("touch changed what is recorded (-want +got):\n%s", diff)
	}
	if len(after) == 1 && !after[0].LastSeen.After(before[0].LastSeen) {
		t.Errorf("touch did not refresh the last sighting: %v then %v", before[0].LastSeen,
			after[0].LastSeen)
	}
}

// testSince pins that a worker whose last sighting is before the cutoff is left out.
func testSince(t *testing.T, store attention.Store) {
	ctx := context.Background()
	if err := store.NoteWorker(ctx, "worker-a", []string{""}, 1); err != nil {
		t.Fatalf("NoteWorker() error = %v", err)
	}
	now, err := store.Now(ctx)
	if err != nil {
		t.Fatalf("Now() error = %v", err)
	}
	got, err := store.Workers(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Workers() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Workers() after the last sighting = %v, want none", views(got))
	}
}

// testClaimOnce pins that an alert key is recorded by the first claim only, and that another key is
// its own alert.
func testClaimOnce(t *testing.T, store attention.Store) {
	ctx := context.Background()
	tests := []struct {
		Key       string
		WantClaim bool
	}{{ // Test 0: The first claim records the key.
		Key: "no_worker|run_a|run_a|1", WantClaim: true,
	}, { // Test 1: A second claim of the same key loses.
		Key: "no_worker|run_a|run_a|1", WantClaim: false,
	}, { // Test 2: A new episode of the same condition is a new alert.
		Key: "no_worker|run_a|run_a|2", WantClaim: true,
	}}
	for testNum, test := range tests {
		got, err := store.ClaimAlert(ctx, test.Key)
		if err != nil {
			t.Fatalf("test %d: ClaimAlert() error = %v", testNum, err)
		}
		if diff := cmp.Diff(test.WantClaim, got); diff != "" {
			t.Errorf("test %d: ClaimAlert() mismatch (-want +got):\n%s", testNum, diff)
		}
	}
}

// testClaimRace pins that of several processes raising the same alert at once exactly one wins,
// which is what keeps two replicas from alerting twice.
func testClaimRace(t *testing.T, store attention.Store) {
	ctx := context.Background()
	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	var errs []error
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := store.ClaimAlert(ctx, "worker_lost|run_b|run_b|7")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("racer %d: %w", i, err))
				return
			}
			if ok {
				won++
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		t.Errorf("ClaimAlert() error = %v", err)
	}
	if won != 1 {
		t.Errorf("%d racers claimed one alert, want exactly 1", won)
	}
}
