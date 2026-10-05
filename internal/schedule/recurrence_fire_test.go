package schedule

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// racingSubmitter counts fires across goroutines, so racing schedulers can share one.
type racingSubmitter struct {
	// fired counts every submission.
	fired atomic.Int64
}

// Submit records a fire.
func (c *racingSubmitter) Submit(context.Context, string, string, ...run.SubmitOption) (*run.Run, error) {
	return &run.Run{ID: fmt.Sprintf("run_%d", c.fired.Add(1))}, nil
}

// SubmitSplit records a fire.
func (c *racingSubmitter) SubmitSplit(context.Context, string, string, int,
	...run.SubmitOption) (*run.Run, error) {
	return &run.Run{ID: fmt.Sprintf("run_%d", c.fired.Add(1))}, nil
}

// SubmitPipeline records a fire.
func (c *racingSubmitter) SubmitPipeline(context.Context, string, string, []run.PipelineStep,
	...run.SubmitOption) (*run.Run, error) {
	return &run.Run{ID: fmt.Sprintf("run_%d", c.fired.Add(1))}, nil
}

// TestSchedulerRecurrenceNeverDoubleFires races several schedulers over one store holding a due
// recurrence schedule, the shape of a highly available pair or more, and proves it fires once. It
// covers the ordinary fire, which advances the next fire time, and the last fire of a bounded rule,
// which clears it: both must be a compare-and-set, and the last one must still fire.
func TestSchedulerRecurrenceNeverDoubleFires(t *testing.T) {
	t.Parallel()
	start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	stamp := start.Format("20060102T150405Z")
	tests := []struct {
		Rule      string
		Due       time.Time
		WantFinal bool
	}{{ // Test 0: An unbounded hourly rule advances past now.
		Rule: "DTSTART:" + stamp + " RRULE:FREQ=HOURLY",
		Due:  start.Add(time.Hour),
	}, { // Test 1: The last of two occurrences fires once and leaves no next fire.
		Rule: "DTSTART:" + stamp + " RRULE:FREQ=HOURLY;COUNT=2",
		Due:  start.Add(time.Hour), WantFinal: true,
	}, { // Test 2: The same through UNTIL rather than COUNT.
		Rule: "DTSTART:" + stamp + " RRULE:FREQ=HOURLY;UNTIL=" +
			start.Add(time.Hour).Format("20060102T150405Z"),
		Due: start.Add(time.Hour), WantFinal: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			due := test.Due
			if err := store.Save(ctx, &Schedule{
				ID: "sch_rr", RRule: test.Rule, Playbook: "p.yml", Enabled: true,
				CreatedAt: start, NextRunAt: &due,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			sub := &racingSubmitter{}
			const replicas = 6
			for round := range 3 {
				var wg sync.WaitGroup
				gate := make(chan struct{})
				for range replicas {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-gate
						NewScheduler(store, sub, nil).tick(time.Now())
					}()
				}
				close(gate)
				wg.Wait()
				if got := sub.fired.Load(); got != 1 {
					t.Fatalf("round %d: %d fires across %d schedulers, want exactly 1", round, got,
						replicas)
				}
			}
			got, err := store.Get(ctx, "sch_rr")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			switch {
			case test.WantFinal && got.NextRunAt != nil:
				t.Errorf("NextRunAt = %v after the last occurrence, want none", got.NextRunAt)
			case !test.WantFinal && (got.NextRunAt == nil || !got.NextRunAt.After(time.Now())):
				t.Errorf("NextRunAt = %v, want a fire in the future", got.NextRunAt)
			}
			if got.LastRunID != "run_1" || got.LastError != "" {
				t.Errorf("LastRunID = %q LastError = %q, want run_1 and no error", got.LastRunID,
					got.LastError)
			}
		})
	}
}
