package run

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// seedRun saves one run under key, created at at, so a dedupe lookup can find it.
func seedRun(t *testing.T, store Store, id, key string, at time.Time) {
	t.Helper()
	r := &Run{
		ID: id, Playbook: "site.yml", Inventory: "hosts.ini", Status: StatusPending,
		CreatedAt: at, IdempotencyKey: key,
	}
	if err := store.Save(context.Background(), r); err != nil {
		t.Fatalf("Save(%s) error = %v", id, err)
	}
}

// TestResolveDedupeBoundsTheWindow pins that a repeat collapses onto an existing run only while
// that run is inside DedupeWindow, not merely while it shares a bucket with the request.
//
// The lookup spans the bucket holding now and the one before it so two clicks either side of a
// boundary still agree, but without bounding the match by the run's own creation time that made the
// real window anything from one to two buckets depending on where in a bucket the first click fell.
func TestResolveDedupeBoundsTheWindow(t *testing.T) {
	t.Parallel()
	// A time sitting at the very start of a bucket, so the offsets below are exact.
	base := time.Unix(0, (time.Now().UnixNano()/int64(DedupeWindow))*int64(DedupeWindow)).UTC()
	tests := []struct {
		Name     string
		Age      time.Duration
		WantSame bool
	}{{ // Test 0: An impatient second click a moment later is the same request.
		Name: "immediate repeat", Age: 100 * time.Millisecond, WantSame: true,
	}, { // Test 1: Just inside the window still collapses.
		Name: "just inside", Age: DedupeWindow - time.Second, WantSame: true,
	}, { // Test 2: Past the window is a deliberate rerun, even though the buckets still overlap.
		Name: "past the window", Age: DedupeWindow + 5*time.Second, WantSame: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := NewMemStore()
			// Sit the request near the end of its bucket. That is what stretches the two-bucket
			// lookup furthest: the previous bucket starts a full window earlier, so a run recorded
			// at the top of it is nearly two windows old and still shares a key.
			now := base.Add(2*DedupeWindow - time.Second)
			created := now.Add(-test.Age)
			seedRun(t, store, "run_first", DedupeKey("rerun", "run_a", created), created)

			existing, key, err := ResolveDedupe(context.Background(), store, "rerun", "run_a", now)
			if err != nil {
				t.Fatalf("ResolveDedupe() error = %v", err)
			}
			if gotSame := existing != nil; gotSame != test.WantSame {
				t.Errorf("collapsed onto the existing run = %v, want %v (age %s, window %s)",
					gotSame, test.WantSame, test.Age, DedupeWindow)
			}
			if !test.WantSame && key == "" {
				t.Error("key is empty, so a legitimate rerun submits with no dedupe protection " +
					"even though the current bucket is free")
			}
		})
	}
}

// TestResolveDedupeYieldsTheKeyWhenTheBucketIsStale pins that a request whose own bucket is already
// held by a run outside the window starts a fresh run instead of collapsing onto that run.
//
// Only a clock that went backwards puts a stale run in the current bucket, since bucket numbers
// rise with wall time. When it happens the operator asked for a run, and starting one without
// dedupe protection beats silently swallowing it.
func TestResolveDedupeYieldsTheKeyWhenTheBucketIsStale(t *testing.T) {
	t.Parallel()
	store := NewMemStore()
	now := time.Now()
	// A run holding this request's own key, but recorded far outside the window.
	seedRun(t, store, "run_stale", DedupeKey("rerun", "run_a", now), now.Add(-time.Hour))

	existing, key, err := ResolveDedupe(context.Background(), store, "rerun", "run_a", now)
	if err != nil {
		t.Fatalf("ResolveDedupe() error = %v", err)
	}
	if existing != nil {
		t.Errorf("collapsed onto %s, recorded %s ago, so the requested run never happens",
			existing.ID, now.Sub(existing.CreatedAt))
	}
	if key != "" {
		t.Errorf("key = %q, want empty: the derived key is taken, so submitting under it would be "+
			"rejected by the unique index", key)
	}
}

// TestResolveDedupeStraddlesABucketBoundary pins that two clicks a moment apart agree even when a
// bucket boundary falls between them, which is the reason the lookup checks the previous bucket.
func TestResolveDedupeStraddlesABucketBoundary(t *testing.T) {
	t.Parallel()
	store := NewMemStore()
	// One nanosecond before a boundary, and one nanosecond after it.
	boundary := time.Unix(0, ((time.Now().UnixNano()/int64(DedupeWindow))+1)*int64(DedupeWindow)).UTC()
	first := boundary.Add(-time.Nanosecond)
	second := boundary.Add(time.Nanosecond)
	if DedupeKey("rerun", "run_a", first) == DedupeKey("rerun", "run_a", second) {
		t.Fatal("the two clicks share a bucket, so this test is not exercising the boundary")
	}
	seedRun(t, store, "run_first", DedupeKey("rerun", "run_a", first), first)

	existing, _, err := ResolveDedupe(context.Background(), store, "rerun", "run_a", second)
	if err != nil {
		t.Fatalf("ResolveDedupe() error = %v", err)
	}
	if existing == nil {
		t.Error("a repeat two nanoseconds later started a second run because a bucket boundary " +
			"fell between the clicks")
	}
}

// TestClientKeyRefusesTheReservedNamespace pins that a caller cannot plant a run under a key the
// server derives, which would make a later rerun resolve to it and never execute.
func TestClientKeyRefusesTheReservedNamespace(t *testing.T) {
	t.Parallel()
	if _, err := ClientKey(DedupeKey("rerun", "run_a", time.Now())); !errors.Is(err, ErrReservedKey) {
		t.Errorf("ClientKey(derived key) error = %v, want ErrReservedKey", err)
	}
	if got, err := ClientKey("caller-supplied"); err != nil || got != "caller-supplied" {
		t.Errorf("ClientKey(caller-supplied) = (%q, %v), want (caller-supplied, nil)", got, err)
	}
}
