package dispatch

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestGatheredAtHoldsTheFileTimeInsideTheRun stamps collected facts from the time Ansible wrote
// their file, held inside the run's window, so a play that set the file's time or a clock that
// stepped can neither make facts look fresher than the run that gathered them nor older than the
// run itself.
func TestGatheredAtHoldsTheFileTimeInsideTheRun(t *testing.T) {
	t.Parallel()
	prepared := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	now := prepared.Add(5 * time.Hour)
	tests := []struct {
		Wrote    time.Time
		WantTime time.Time
	}{{ // Test 0: A file written during the run keeps its time.
		Wrote: prepared.Add(time.Minute), WantTime: prepared.Add(time.Minute),
	}, { // Test 1: A file time from before the run is held to the run's start.
		Wrote: prepared.Add(-time.Hour), WantTime: prepared,
	}, { // Test 2: A file time ahead of now is held to now.
		Wrote: now.Add(24 * time.Hour), WantTime: now,
	}, { // Test 3: No file time at all is the end of the run, as before.
		Wrote: time.Time{}, WantTime: now,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantTime, gatheredAt(test.Wrote, prepared, now)); diff != "" {
				t.Errorf("gatheredAt() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
