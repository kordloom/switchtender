package schedule

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// schCounter is a Submitter that counts every fire, so a test can say how many times one schedule
// ran.
type schCounter struct {
	// fired counts every submission, whatever its shape.
	fired atomic.Int64
}

// Submit records a single-run fire.
func (c *schCounter) Submit(context.Context, string, string, ...run.SubmitOption) (*run.Run, error) {
	return &run.Run{ID: fmt.Sprintf("run_%d", c.fired.Add(1))}, nil
}

// SubmitSplit records a split fire.
func (c *schCounter) SubmitSplit(context.Context, string, string, int,
	...run.SubmitOption) (*run.Run, error) {
	return &run.Run{ID: fmt.Sprintf("run_%d", c.fired.Add(1))}, nil
}

// SubmitPipeline records a pipeline fire.
func (c *schCounter) SubmitPipeline(context.Context, string, string, []run.PipelineStep,
	...run.SubmitOption) (*run.Run, error) {
	return &run.Run{ID: fmt.Sprintf("run_%d", c.fired.Add(1))}, nil
}

// schZone loads an IANA zone, which the embedded zone database always provides.
func schZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("the zone database is embedded, so %s failing to load is a real failure: %v",
			name, err)
	}
	return loc
}

// TestScheduleLateFireOnFallBackNightRunsTheRepeatedSlotAgain fires a nightly cron schedule set
// inside the hour that happens twice on the night the clocks go back, through the real tick loop,
// with its first tick landing late.
//
// The schedule documents that a time which happens twice fires once, at the first of the two, and
// nextCronFire guards it by skipping a next fire that shares a wall-clock minute with the time it
// is asked about. The scheduler asks about the tick's own time rather than the occurrence it fired,
// so the guard only holds while the tick lands inside the scheduled minute. A tick a minute or more
// late, which is what a restart, a database failover, a suspended host, or a --schedule-interval
// longer than a minute produces, computes the second 01:30 as the next fire and runs the same
// non-idempotent nightly job twice on one night.
func TestScheduleLateFireOnFallBackNightRunsTheRepeatedSlotAgain(t *testing.T) {
	t.Parallel()
	chicago := schZone(t, "America/Chicago")
	// 01:30 CDT on 2026-11-01. Chicago goes from 02:00 CDT back to 01:00 CST that night, so 01:30
	// happens again an hour later, at 07:30 UTC.
	first := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)
	if _, off := first.In(chicago).Zone(); off != -5*3600 {
		t.Fatalf("the fixture is meant to start in daylight time, got offset %d", off)
	}
	tests := []struct {
		// Name says why the tick was late.
		Name string
		// Late is how long after the first 01:30 the first tick lands.
		Late time.Duration
		// Interval is the scheduler's tick interval.
		Interval time.Duration
	}{{ // Test 0: The server came back up a minute after the slot.
		Name: "one minute late", Late: 61 * time.Second, Interval: DefaultInterval,
	}, { // Test 1: A database failover held the scheduler for half an hour.
		Name: "half an hour late", Late: 30 * time.Minute, Interval: DefaultInterval,
	}, { // Test 2: A --schedule-interval of five minutes put the first tick at 01:33.
		Name: "five minute interval", Late: 3 * time.Minute, Interval: 5 * time.Minute,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			due := first
			if err := store.Save(ctx, &Schedule{
				ID: "sch_nightly", Name: "nightly patch", Cron: "30 1 * * *",
				Timezone: "America/Chicago", Playbook: "patch.yml", Enabled: true,
				CreatedAt: first.Add(-72 * time.Hour), NextRunAt: &due,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			sub := &schCounter{}
			s := NewScheduler(store, sub, nil)
			t.Cleanup(s.Close)
			// Tick on the interval from the late first tick until well past the second 01:30 and
			// before the next night's slot.
			for at := first.Add(test.Late); at.Before(first.Add(4 * time.Hour)); at = at.Add(
				test.Interval) {
				s.tick(at)
			}
			if got := sub.fired.Load(); got != 1 {
				sc, _ := store.Get(ctx, "sch_nightly")
				next := "none"
				if sc != nil && sc.NextRunAt != nil {
					next = sc.NextRunAt.In(chicago).Format(time.RFC3339)
				}
				t.Errorf("the nightly 01:30 schedule fired %d times on the night the clocks went "+
					"back, want once at the first 01:30; next fire now %s", got, next)
			}
		})
	}
}
