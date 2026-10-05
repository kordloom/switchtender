package run

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestScheduleKeyNamesOneOccurrenceAndCannotBeForged checks that every fire of one occurrence of
// one schedule derives the same key, that any other occurrence or schedule derives another, and
// that a caller cannot send the key as its own, which would plant a run the schedule's fire
// resolves to.
func TestScheduleKeyNamesOneOccurrenceAndCannotBeForged(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		ID         string
		Occurrence time.Time
		WantSame   bool
	}{{ // Test 0: The same occurrence read back in another zone is the same key.
		ID: "sch_1", Occurrence: at.In(time.FixedZone("x", -5*3600)), WantSame: true,
	}, { // Test 1: The next occurrence is another key.
		ID: "sch_1", Occurrence: at.Add(5 * time.Minute), WantSame: false,
	}, { // Test 2: Another schedule's occurrence at the same instant is another key.
		ID: "sch_2", Occurrence: at, WantSame: false,
	}}
	base := ScheduleKey("sch_1", at)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := ScheduleKey(test.ID, test.Occurrence)
			if diff := cmp.Diff(test.WantSame, got == base); diff != "" {
				t.Errorf("ScheduleKey(%s, %s) = %q beside %q, same mismatch (-want +got):\n%s",
					test.ID, test.Occurrence, got, base, diff)
			}
			if _, err := ClientKey(got, ""); !errors.Is(err, ErrReservedKey) {
				t.Errorf("ClientKey(%q) error = %v, want ErrReservedKey", got, err)
			}
		})
	}
}
