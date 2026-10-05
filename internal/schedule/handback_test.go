package schedule_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// keyedRuns stands in for a run store's unique index on idempotency keys: a submit carrying a key a
// run already holds returns that run instead of creating another.
type keyedRuns struct {
	// mu guards byKey and created.
	mu sync.Mutex
	// byKey maps an idempotency key to the run created under it.
	byKey map[string]*run.Run
	// created counts the runs created.
	created int
}

// submit creates a run, or returns the one already holding the submission's key.
func (k *keyedRuns) submit(opts []run.SubmitOption) *run.Run {
	k.mu.Lock()
	defer k.mu.Unlock()
	probe := &run.Run{}
	run.ApplyOptions(probe, opts)
	if existing, ok := k.byKey[probe.IdempotencyKey]; ok && probe.IdempotencyKey != "" {
		return existing
	}
	k.created++
	r := &run.Run{ID: fmt.Sprintf("run_%d", k.created), IdempotencyKey: probe.IdempotencyKey}
	if r.IdempotencyKey != "" {
		k.byKey[r.IdempotencyKey] = r
	}
	return r
}

// keyedSubmitter submits into keyedRuns. With hold set, it signals entered and waits for its
// context to end before answering, the way a submit in flight sees a stop, and with land set as
// well it creates the run before it waits, so the run exists while the answer is lost.
type keyedSubmitter struct {
	// runs is the shared run index.
	runs *keyedRuns
	// hold makes every submit wait for its context to end and fail.
	hold bool
	// land creates the run before a held submit waits.
	land bool
	// entered is closed when the first held submit arrives.
	entered chan struct{}
	// once closes entered a single time.
	once sync.Once
}

// do runs one submission.
func (k *keyedSubmitter) do(ctx context.Context, opts []run.SubmitOption) (*run.Run, error) {
	if !k.hold {
		return k.runs.submit(opts), nil
	}
	if k.land {
		k.runs.submit(opts)
	}
	k.once.Do(func() { close(k.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// Submit runs a single-run submission.
func (k *keyedSubmitter) Submit(ctx context.Context, _, _ string, opts ...run.SubmitOption) (*run.Run, error) {
	return k.do(ctx, opts)
}

// SubmitSplit runs a split submission.
func (k *keyedSubmitter) SubmitSplit(ctx context.Context, _, _ string, _ int,
	opts ...run.SubmitOption) (*run.Run, error) {
	return k.do(ctx, opts)
}

// SubmitPipeline runs a pipeline submission.
func (k *keyedSubmitter) SubmitPipeline(ctx context.Context, _, _ string, _ []run.PipelineStep,
	opts ...run.SubmitOption) (*run.Run, error) {
	return k.do(ctx, opts)
}

// TestAnInterruptedFireIsHandedBackAndFiresOnce stops a scheduler while it fires the last
// occurrence of a bounded rule, then starts another over the same SQLite store, the shape of a
// restart or of the other server of a pair taking over.
//
// The claim cleared the next fire before the fire, and a stop canceled the fire and the record of
// it, so the occurrence was lost. It is handed back instead, and the fire that takes it up carries
// the same idempotency key, so the occurrence runs exactly once whether or not the interrupted
// submit had already created its run.
func TestAnInterruptedFireIsHandedBackAndFiresOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the interrupted submit had done.
		Name string
		// Land creates the run before the stop cuts the submit off.
		Land bool
	}{{ // Test 0: The stop landed before the submit created anything.
		Name: "nothing created", Land: false,
	}, { // Test 1: The submit created the run and the stop lost the answer.
		Name: "run created", Land: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			store := db.Schedules()
			start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
			due := start.Add(time.Hour)
			if err := store.Save(ctx, &schedule.Schedule{
				ID: "sch_final", Name: "two runs then stop",
				RRule:    "DTSTART:" + start.Format("20060102T150405Z") + " RRULE:FREQ=HOURLY;COUNT=2",
				Playbook: "migrate.yml", Enabled: true, CreatedAt: start, NextRunAt: &due,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			runs := &keyedRuns{byKey: map[string]*run.Run{}}
			first := &keyedSubmitter{runs: runs, hold: true, land: test.Land,
				entered: make(chan struct{})}
			stopping := schedule.NewScheduler(store, first, nil,
				schedule.WithInterval(10*time.Millisecond))
			stopping.Start()
			select {
			case <-first.entered:
			case <-time.After(30 * time.Second):
				stopping.Close()
				t.Fatal("the last occurrence never reached the submitter")
			}
			stopping.Close()

			handed, err := store.Get(ctx, "sch_final")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if handed.NextRunAt == nil || !handed.NextRunAt.Equal(due) {
				t.Fatalf("after the stop the next fire is %v, want the interrupted occurrence %v "+
					"handed back", handed.NextRunAt, due)
			}
			if handed.LastError == "" {
				t.Errorf("the schedule does not say its fire was interrupted")
			}

			second := &keyedSubmitter{runs: runs}
			taking := schedule.NewScheduler(store, second, nil,
				schedule.WithInterval(10*time.Millisecond))
			taking.Start()
			deadline := time.Now().Add(30 * time.Second)
			var got *schedule.Schedule
			for time.Now().Before(deadline) {
				if got, err = store.Get(ctx, "sch_final"); err == nil && got.LastRunID != "" {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			taking.Close()
			if got == nil || got.LastRunID == "" {
				t.Fatal("the handed back occurrence never fired")
			}
			runs.mu.Lock()
			created := runs.created
			runs.mu.Unlock()
			if diff := cmp.Diff(1, created); diff != "" {
				t.Errorf("runs created for the one occurrence (-want +got):\n%s", diff)
			}
			if got.NextRunAt != nil || got.LastError != "" {
				t.Errorf("after the fire the schedule has next fire %v and error %q, want it "+
					"finished with no error", got.NextRunAt, got.LastError)
			}
		})
	}
}
