package dispatch

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestWithRetriesDoesNotRepeatAPartlyDeliveredWrite covers the one error a retry must not act on.
//
// Retrying is right for a write that did nothing. A write that landed in part already recorded what
// arrived, and the store that reports it has retried the part that actually failed, so repeating the call
// records the delivered part a second time. That is how a relay worker's event flush wrote the same
// tasks into a run's record twice.
func TestWithRetriesDoesNotRepeatAPartlyDeliveredWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says what the write reported.
		Name string
		// Err is what it returned every time.
		Err error
		// WantCalls is how many times it should have been called.
		WantCalls int
	}{{ // Test 0: An ordinary failure is retried, which is what the wrapper is for.
		Name: "an ordinary failure", Err: errors.New("database is locked"), WantCalls: 4,
	}, { // Test 1: A partly delivered write is attempted once and reported.
		Name: "a partly delivered write", Err: run.ErrPartlyDelivered, WantCalls: 1,
	}, { // Test 2: Wrapped, which is how a transport reports it alongside the underlying cause.
		Name:      "wrapped in its cause",
		Err:       fmt.Errorf("%w: append events: unexpected status 502", run.ErrPartlyDelivered),
		WantCalls: 1,
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			err := withRetries(func() error {
				calls++
				return test.Err
			})
			if !errors.Is(err, test.Err) {
				t.Errorf("test %d: withRetries returned %v, want the write's own error", testNum, err)
			}
			if calls != test.WantCalls {
				t.Errorf("test %d: the write was attempted %d times, want %d",
					testNum, calls, test.WantCalls)
			}
		})
	}

	// A write that comes good on a later attempt still succeeds, so the marker did not turn the wrapper
	// into a single attempt for everything.
	t.Run("a write that recovers", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := withRetries(func() error {
			calls++
			if calls < 3 {
				return errors.New("database is locked")
			}
			return nil
		})
		if err != nil {
			t.Errorf("withRetries returned %v for a write that recovered", err)
		}
		if calls != 3 {
			t.Errorf("the write was attempted %d times, want 3", calls)
		}
	})
}
