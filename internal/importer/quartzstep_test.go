package importer

import (
	"fmt"
	"testing"
)

// TestAQuartzWeekdayStepKeepsItsInterval covers a schedule that imports as a different schedule.
//
// Quartz numbers Sunday as one and cron numbers it as zero, so every weekday in the field shifts
// down by one. The number after a slash is not a weekday: it is how many days to skip. Shifting it
// turned "every second day starting Sunday" into "from Sunday, every day", so an alternate-day job
// came across firing daily, with the right-looking expression and nothing in the plan to say the
// meaning had changed. A job that runs a backup every other day then runs it every day.
func TestAQuartzWeekdayStepKeepsItsInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want string
		OK   bool
	}{{ // Test 0: The step is an interval and survives; the start day is renumbered.
		In: "1/2", Want: "0/2", OK: true,
	}, { // Test 1: A range with a step: both ends renumber, the interval does not.
		In: "2-6/2", Want: "1-5/2", OK: true,
	}, { // Test 2: A list mixing a stepped term and a plain day.
		In: "1/2,6", Want: "0/2,5", OK: true,
	}, { // Test 3: No step at all, the ordinary case.
		In: "1,7", Want: "0,6", OK: true,
	}, { // Test 4: A zero interval is not a schedule and is refused rather than imported.
		In: "1/0", Want: "", OK: false,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := &Plan{}
			got, ok := p.convertQuartzDOW(test.In, "nightly")
			if ok != test.OK {
				t.Fatalf("test %d: convertQuartzDOW(%q) ok = %v, want %v", testNum, test.In, ok, test.OK)
			}
			if !test.OK {
				return
			}
			if got != test.Want {
				t.Errorf("test %d: convertQuartzDOW(%q) = %q, want %q. The interval after the "+
					"slash is how many days to skip, so renumbering it changes how often the job "+
					"runs rather than which day it starts on", testNum, test.In, got, test.Want)
			}
		})
	}
}
