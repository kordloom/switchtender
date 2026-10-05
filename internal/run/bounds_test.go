package run

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestClientKeyAndQueueAreBounded pins the bounds on the two values a caller sends that are stored
// under an index of the runs table: the Idempotency-Key and the queue name.
//
// PostgreSQL refuses an index entry past about 2.7 kilobytes, so a longer value that did not
// compress failed the submit with a 500. A value at the bound is kept as it was, a value past it is
// refused with an error stating the bound, and an organization's scoping does not move the bound.
func TestClientKeyAndQueueAreBounded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Key       string
		Org       string
		Queue     string
		WantKey   string
		Want      error
		WantQueue error
	}{{ // Test 0: A key at the bound is stored as sent.
		Key: strings.Repeat("k", MaxClientKeyBytes), WantKey: strings.Repeat("k", MaxClientKeyBytes),
	}, { // Test 1: A key one byte past the bound is refused.
		Key: strings.Repeat("k", MaxClientKeyBytes+1), Want: ErrKeyTooLong,
	}, { // Test 2: A key past the bound is refused for an organization too, before it is digested.
		Key: strings.Repeat("k", 4000), Org: "org_a", Want: ErrKeyTooLong,
	}, { // Test 3: A queue name at the bound is accepted.
		Key: "k", WantKey: "k", Queue: strings.Repeat("q", MaxQueueBytes),
	}, { // Test 4: A queue name one byte past the bound is refused.
		Key: "k", WantKey: "k", Queue: strings.Repeat("q", MaxQueueBytes+1), WantQueue: ErrQueueTooLong,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			key, err := ClientKey(test.Key, test.Org)
			if !errors.Is(err, test.Want) {
				t.Errorf("ClientKey() error = %v, want %v", err, test.Want)
			}
			if test.WantKey != "" {
				if diff := cmp.Diff(test.WantKey, key); diff != "" {
					t.Errorf("ClientKey() mismatch (-want +got):\n%s", diff)
				}
			}
			if err != nil && !strings.Contains(err.Error(), "at most 255 bytes") {
				t.Errorf("ClientKey() error %q does not state the bound", err)
			}
			if qerr := CheckQueue(test.Queue); !errors.Is(qerr, test.WantQueue) {
				t.Errorf("CheckQueue() error = %v, want %v", qerr, test.WantQueue)
			}
		})
	}
}
