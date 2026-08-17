package server

import (
	"fmt"
	"sync"
	"testing"
)

// TestLiveStreamsAreBoundedPerCallerAndInTotal covers what one account could make the server do.
//
// A live stream stays open for as long as its run does, and each one holds a goroutine, a subscription,
// and a poll of the store every second. Nothing counted them, so a viewer, the lowest role that can read
// a run at all, could open thousands against still-executing runs and hold the store at thousands of
// reads a second indefinitely, from one account and with no error anywhere to show for it.
//
// Both limits sit far above what a person browsing produces, which is one stream per run page open in
// front of them, so the bound is invisible until somebody is doing something else.
func TestLiveStreamsAreBoundedPerCallerAndInTotal(t *testing.T) {
	t.Parallel()

	t.Run("one caller is bounded", func(t *testing.T) {
		t.Parallel()
		var live streamCount
		releases := make([]func(), 0, maxStreamsPerActor)
		for i := range maxStreamsPerActor {
			release, ok := live.admit("user:alice")
			if !ok {
				t.Fatalf("stream %d of %d was refused, below the limit", i+1, maxStreamsPerActor)
			}
			releases = append(releases, release)
		}
		if _, ok := live.admit("user:alice"); ok {
			t.Errorf("a caller opened more than %d live streams, so one account can hold the store at "+
				"a read per second per stream for as long as it likes", maxStreamsPerActor)
		}

		// Another caller is unaffected, so one busy account cannot lock everybody else out.
		other, ok := live.admit("user:bob")
		if !ok {
			t.Error("a second caller was refused because the first one was at its limit")
		}
		other()

		// Closing a stream gives the slot back, or the limit would be a lifetime quota.
		releases[0]()
		if _, ok := live.admit("user:alice"); !ok {
			t.Error("a caller who closed a stream could not open another")
		}
	})

	t.Run("the server is bounded in total", func(t *testing.T) {
		t.Parallel()
		var live streamCount
		opened := 0
		for i := 0; opened < maxStreamsTotal+10; i++ {
			// A fresh caller each time, so only the total limit can stop this.
			if _, ok := live.admit(fmt.Sprintf("user:%d", i)); !ok {
				break
			}
			opened++
		}
		if opened > maxStreamsTotal {
			t.Errorf("the server held %d live streams open, above the %d total", opened, maxStreamsTotal)
		}
		if opened != maxStreamsTotal {
			t.Errorf("the server admitted %d streams, want the full %d before refusing",
				opened, maxStreamsTotal)
		}
	})

	// Releasing twice must not free a slot that was never taken, or the count drifts below zero and the
	// limit stops meaning anything.
	t.Run("a release is counted once", func(t *testing.T) {
		t.Parallel()
		var live streamCount
		release, ok := live.admit("user:alice")
		if !ok {
			t.Fatal("the first stream was refused")
		}
		release()
		release()
		if live.total != 0 {
			t.Errorf("total = %d after one stream opened and released twice, want 0", live.total)
		}
	})

	// Concurrent opens and closes keep the count honest, since this runs per request.
	t.Run("concurrent callers", func(t *testing.T) {
		t.Parallel()
		var live streamCount
		var wg sync.WaitGroup
		for i := range 64 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if release, ok := live.admit(fmt.Sprintf("user:%d", i%4)); ok {
					release()
				}
			}(i)
		}
		wg.Wait()
		if live.total != 0 {
			t.Errorf("total = %d after every stream closed, want 0", live.total)
		}
		if len(live.byActor) != 0 {
			t.Errorf("byActor still holds %d callers after every stream closed", len(live.byActor))
		}
	})
}
