package schedule_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// schBlockingSubmitter holds every fire until the context it was handed ends, which is what a
// submit in flight sees when the process is told to stop: serve's deferred Close cancels the
// scheduler's context while the fire is still writing.
type schBlockingSubmitter struct {
	// entered is closed when the first fire reaches the submitter.
	entered chan struct{}
	// once closes entered a single time.
	once sync.Once
}

// hold signals that a fire arrived and waits for its context to end.
func (b *schBlockingSubmitter) hold(ctx context.Context) (*run.Run, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// Submit holds a single-run fire.
func (b *schBlockingSubmitter) Submit(ctx context.Context, _, _ string,
	_ ...run.SubmitOption) (*run.Run, error) {
	return b.hold(ctx)
}

// SubmitSplit holds a split fire.
func (b *schBlockingSubmitter) SubmitSplit(ctx context.Context, _, _ string, _ int,
	_ ...run.SubmitOption) (*run.Run, error) {
	return b.hold(ctx)
}

// SubmitPipeline holds a pipeline fire.
func (b *schBlockingSubmitter) SubmitPipeline(ctx context.Context, _, _ string, _ []run.PipelineStep,
	_ ...run.SubmitOption) (*run.Run, error) {
	return b.hold(ctx)
}

// TestScheduleFinalOccurrenceLostWhenShutdownLandsDuringItsFire stops the scheduler, the way
// serve's deferred Close does on SIGTERM, while it is firing the last occurrence of a bounded
// recurrence, on the real SQLite store.
//
// The tutorial promises that a rule with a COUNT or an UNTIL fires its last occurrence and then
// stops, and that the Doctor page then lists it as finished. The scheduler claims that occurrence
// by clearing the next fire time before it fires, and both the fire and the bookkeeping after it
// run on the scheduler's own context. A stop that lands between the claim and the run's creation
// cancels the submit and then cancels the write that would have recorded why nothing ran. The
// schedule is left finished, with no run, no last error, and no last run time, so nothing will ever
// fire the occurrence again and nothing on the schedule says it never ran. The audit chain holds
// the fire entry, so the evidence says the schedule fired when nothing did. A crash at the same
// point leaves the same state.
func TestScheduleFinalOccurrenceLostWhenShutdownLandsDuringItsFire(t *testing.T) {
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
	sub := &schBlockingSubmitter{entered: make(chan struct{})}
	s := schedule.NewScheduler(store, sub, nil, schedule.WithInterval(10*time.Millisecond),
		schedule.WithAudits(db.Audits()))
	s.Start()
	select {
	case <-sub.entered:
	case <-time.After(30 * time.Second):
		s.Close()
		t.Fatal("the last occurrence never reached the submitter")
	}
	s.Close()

	got, err := store.Get(ctx, "sch_final")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	// A stop that hands the interrupted occurrence back leaves it due, so the next scheduler to tick
	// fires it. Otherwise the claim cleared the next fire and the rule has none left.
	if got.NextRunAt != nil {
		if !got.NextRunAt.Equal(due) {
			t.Fatalf("the interrupted occurrence came back as %v, want %v", got.NextRunAt, due)
		}
	} else if _, err := got.NextFire(time.Now()); err == nil {
		t.Fatalf("the rule should have no fire left, so no scheduler fires it again")
	}
	if got.LastRunID == "" && got.LastError == "" && got.LastRunAt == nil {
		fired := 0
		entries, lerr := db.Audits().Chain(ctx)
		if lerr == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Path, "/sch_final/fired") {
					fired++
				}
			}
		}
		t.Errorf("the last occurrence of a bounded rule ran nothing and the schedule is finished "+
			"with no last run, no last error, and no last run time, while the chain holds %d fire "+
			"entries for it: the occurrence is lost and nothing on the schedule says so", fired)
	}
}
