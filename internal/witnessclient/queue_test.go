package witnessclient

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestQueueKeepsOrderAndDropsOldest pins the queue: pushes leave in the order beats were appended,
// and a trim of a full queue drops its oldest pushes, counted, never the newest.
func TestQueueKeepsOrderAndDropsOldest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Capacity is how many pushes the queue holds.
		Capacity int
		// Add are the beats added in order.
		Add []int64
		// Pop is how many pushes leave the front after the adds.
		Pop int
		// WantBeats are the beats left, oldest first.
		WantBeats []int64
		// WantDropped are the beats dropped to make room, in order.
		WantDropped []int64
	}{{ // Test 0: Under capacity nothing is dropped.
		Capacity: 3, Add: []int64{1, 2}, WantBeats: []int64{1, 2},
	}, { // Test 1: Past capacity the oldest go first.
		Capacity: 3, Add: []int64{1, 2, 3, 4, 5}, WantBeats: []int64{3, 4, 5},
		WantDropped: []int64{1, 2},
	}, { // Test 2: Pushes leave from the front.
		Capacity: 3, Add: []int64{1, 2, 3}, Pop: 2, WantBeats: []int64{3},
	}, { // Test 3: A capacity under one holds one.
		Capacity: 0, Add: []int64{1, 2}, WantBeats: []int64{2}, WantDropped: []int64{1},
	}, { // Test 4: Popping an empty queue is harmless.
		Capacity: 2, Pop: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			q := newQueue(test.Capacity)
			var dropped []int64
			for _, b := range test.Add {
				q.add(pending{Beat: b, Seq: b * 10})
				if len(q.items) > max(1, test.Capacity)+1 {
					t.Fatalf("the queue holds %d before a trim, want at most one past capacity",
						len(q.items))
				}
				for _, old := range q.trim() {
					dropped = append(dropped, old.Beat)
				}
			}
			for range test.Pop {
				q.pop()
			}
			var left []int64
			for _, p := range q.items {
				left = append(left, p.Beat)
			}
			if diff := cmp.Diff(test.WantBeats, left, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("queued beats mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantDropped, dropped, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("dropped beats mismatch (-want +got):\n%s", diff)
			}
			if q.dropped != int64(len(test.WantDropped)) || q.size() != len(test.WantBeats) {
				t.Errorf("queue counts %d dropped and %d waiting, want %d and %d", q.dropped,
					q.size(), len(test.WantDropped), len(test.WantBeats))
			}
			if front, ok := q.front(); ok != (len(test.WantBeats) > 0) ||
				(ok && front.Beat != test.WantBeats[0]) {
				t.Errorf("front() = %d, %v; want the oldest waiting beat", front.Beat, ok)
			}
		})
	}
}

// TestQueueCapIsOneDayOfBeats pins the queue's size: one day at the cadence, and at least one.
func TestQueueCapIsOneDayOfBeats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Cadence is the beat cadence.
		Cadence time.Duration
		// WantCap is the queue's capacity.
		WantCap int
	}{{ // Test 0: A beat a minute.
		Cadence: time.Minute, WantCap: 1440,
	}, { // Test 1: A beat every six hours.
		Cadence: 6 * time.Hour, WantCap: 4,
	}, { // Test 2: A cadence longer than a day still queues one.
		Cadence: 48 * time.Hour, WantCap: 1,
	}, { // Test 3: No cadence.
		Cadence: 0, WantCap: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := QueueCap(test.Cadence); got != test.WantCap {
				t.Errorf("QueueCap(%s) = %d, want %d", test.Cadence, got, test.WantCap)
			}
		})
	}
}
