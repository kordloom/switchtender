package schedule

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestABareTimezoneDescriptorIsRefusedNotFatal pins that a cron expression which is nothing but a
// zone descriptor is turned away rather than crashing whoever read it.
//
// The parser splits a "CRON_TZ=" or "TZ=" descriptor at the first space, and with no space anywhere
// in the string it slices to a negative index and panics. A schedule's expression is caller-supplied
// text, so this is reachable from the schedule preview, which feeds the query parameter straight in,
// and from schedule creation, which validates by asking for the next fire. Worse, a row carrying such
// an expression, from a restore or a direct write, is read on every tick, and a panic there takes the
// scheduler goroutine and the process with it, on every restart.
func TestABareTimezoneDescriptorIsRefusedNotFatal(t *testing.T) {
	t.Parallel()
	specs := []string{"CRON_TZ=America/Chicago", "TZ=UTC", "CRON_TZ=", "TZ=", "CRON_TZ=Mars/Olympus"}
	for testNum, spec := range specs {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("NextFire(%q) panicked with %v, so caller-supplied text can crash "+
							"the process rather than being refused", spec, r)
					}
				}()
				if _, err := NextFire(spec, time.Now()); !errors.Is(err, ErrBadCron) {
					t.Errorf("NextFire(%q) error = %v, want ErrBadCron", spec, err)
				}
			}()
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("Validate() panicked with %v for cron %q", r, spec)
					}
				}()
				sc := &Schedule{Cron: spec, Playbook: "site.yml"}
				if err := sc.Validate(); err == nil {
					t.Errorf("Validate() accepted %q", spec)
				}
			}()
		})
	}
}

// TestAnIntervalScheduleMustNameAPositiveInterval pins the mirror of the guard that refuses an
// expression which can never come due.
//
// That guard exists because a schedule read as due on every tick produces a run every fifteen
// seconds forever from one authenticated call. An interval of zero or a negative duration is the
// same harm arriving from the other direction: it parses, it is silently clamped to one second, and
// it then comes due on every tick for as long as the schedule exists. Neither value can be what
// anybody meant, so refusing them costs nothing and closes the same hole.
func TestAnIntervalScheduleMustNameAPositiveInterval(t *testing.T) {
	t.Parallel()
	for testNum, spec := range []string{"@every 0s", "@every 0", "@every -1h", "@every -1s"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			next, err := NextFire(spec, time.Now())
			if err == nil {
				t.Errorf("NextFire(%q) = %v with no error, so the schedule comes due on every tick "+
					"for as long as it exists", spec, next)
			}
			sc := &Schedule{Cron: spec, Playbook: "site.yml"}
			if err := sc.Validate(); err == nil {
				t.Errorf("Validate() accepted %q", spec)
			}
		})
	}
}

// TestASubMinuteIntervalKeepsItsCadence pins that the daylight saving repeat guard does not eat the
// fires of a schedule that legitimately runs more than once a minute.
//
// The guard skips a fire that shares a wall-clock minute with the moment it was computed from,
// reasoning that a cron slot is minute-granular so only a zone rewind can produce one. An "@every"
// interval is not minute-granular, and this parser accepts intervals well under a minute, so a
// schedule written as every thirty seconds fires once a minute and one written as every twenty
// seconds loses one fire in three, silently and everywhere, not only on the two days a year the
// guard was written for.
func TestASubMinuteIntervalKeepsItsCadence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Spec is the interval expression.
		Spec string
		// WantGap is the interval between consecutive fires.
		WantGap time.Duration
	}{
		{Spec: "@every 30s", WantGap: 30 * time.Second}, // Test 0: Twice a minute.
		{Spec: "@every 20s", WantGap: 20 * time.Second}, // Test 1: Three times a minute.
		{Spec: "@every 1m", WantGap: time.Minute},       // Test 2: The control, which is unaffected.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
			for range 4 {
				next, err := NextFire(test.Spec, at)
				if err != nil {
					t.Fatalf("NextFire(%q) error = %v", test.Spec, err)
				}
				if got := next.Sub(at); got != test.WantGap {
					t.Errorf("NextFire(%q) advanced %v, want %v: a fire was dropped as if the clocks "+
						"had gone back", test.Spec, got, test.WantGap)
				}
				at = next
			}
		})
	}
}

// TestCloneCopiesAStepsDependencies pins that the deep copy reaches inside the pipeline steps.
//
// Clone is documented as a deep copy so callers cannot mutate stored state through shared pointers,
// and every read from the store returns one. The step slice is copied, but each step's dependency
// list is a slice of its own and is carried across by reference, so editing a dependency on a copy
// rewrites the graph of the stored schedule. A scheduled pipeline whose step order changed without
// anybody saving anything is exactly the kind of unattended change this product exists to make
// impossible.
func TestCloneCopiesAStepsDependencies(t *testing.T) {
	t.Parallel()
	original := &Schedule{
		ID: "sch_1", Cron: "0 2 * * *",
		Steps: []run.PipelineStep{
			{Name: "build", Playbook: "build.yml"},
			{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"build"}},
		},
	}
	clone := original.Clone()
	clone.Steps[1].DependsOn[0] = "something-else"
	if original.Steps[1].DependsOn[0] != "build" {
		t.Errorf("editing a copy's dependency changed the original to %q, so a handler that edits "+
			"what the store handed it rewrites the stored pipeline graph",
			original.Steps[1].DependsOn[0])
	}
}
