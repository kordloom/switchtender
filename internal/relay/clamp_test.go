package relay

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestClampReportTimes checks a worker cannot attest an execution window this control node never
// observed.
//
// Both values are digested into the run's outcome entry and travel in its receipt, so they were the
// one part of the record a compromised relay could set freely: dated inside an approved maintenance
// window while running outside it, or before the approval that released the run. The receipt
// verified cleanly either way, because the forgery was in the facts rather than in the math.
func TestClampReportTimes(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	now := created.Add(30 * time.Minute)
	at := func(d time.Duration) *time.Time { v := created.Add(d); return &v }

	tests := []struct {
		Name        string
		Started     *time.Time
		Ended       *time.Time
		WantStarted time.Time
		WantEnded   time.Time
		WantWarn    bool
	}{{ // Test 0: An honest report inside the window is left exactly as reported.
		Name: "honest", Started: at(time.Minute), Ended: at(20 * time.Minute),
		WantStarted: created.Add(time.Minute), WantEnded: created.Add(20 * time.Minute),
	}, { // Test 1: Backdated before the run existed, which is how an execution is made to look like
		// it predates the approval that released it.
		Name: "backdated before creation", Started: at(-48 * time.Hour), Ended: at(10 * time.Minute),
		WantStarted: created, WantEnded: created.Add(10 * time.Minute), WantWarn: true,
	}, { // Test 2: Dated into the future, which is how a run is made to look like it happened inside
		// a maintenance window that has not opened yet.
		Name: "future", Started: at(time.Minute), Ended: at(72 * time.Hour),
		WantStarted: created.Add(time.Minute), WantEnded: now, WantWarn: true,
	}, { // Test 3: Ended before it started, which no execution can do.
		Name: "ended before started", Started: at(20 * time.Minute), Ended: at(5 * time.Minute),
		WantStarted: created.Add(20 * time.Minute), WantEnded: created.Add(20 * time.Minute),
		WantWarn: true,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ID: "run_x", CreatedAt: created, StartedAt: test.Started, EndedAt: test.Ended}
			clampReportTimes(r, now)

			if r.StartedAt == nil || !r.StartedAt.Equal(test.WantStarted) {
				t.Errorf("%s: started = %v, want %v", test.Name, r.StartedAt, test.WantStarted)
			}
			if r.EndedAt == nil || !r.EndedAt.Equal(test.WantEnded) {
				t.Errorf("%s: ended = %v, want %v", test.Name, r.EndedAt, test.WantEnded)
			}
			warned := strings.Contains(r.Warning, "outside the window")
			if warned != test.WantWarn {
				t.Errorf("%s: warned = %v, want %v (warning %q)", test.Name, warned, test.WantWarn,
					r.Warning)
			}
		})
	}
}

// TestClampReportTimesKeepsAnExistingWarning checks the note is added to whatever the executor
// already said rather than replacing it, so a clock complaint cannot hide a real one.
func TestClampReportTimesKeepsAnExistingWarning(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	future := created.Add(72 * time.Hour)
	r := &run.Run{ID: "run_x", CreatedAt: created, EndedAt: &future,
		Warning: "one host was unreachable"}

	clampReportTimes(r, created.Add(time.Minute))

	if !strings.Contains(r.Warning, "one host was unreachable") {
		t.Errorf("the executor's own warning was lost: %q", r.Warning)
	}
	if !strings.Contains(r.Warning, "outside the window") {
		t.Errorf("the clamp did not record that it moved a time: %q", r.Warning)
	}
}
