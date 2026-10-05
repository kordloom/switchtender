package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// keyedSubmitter creates one run per idempotency key and hands back the run a key already holds,
// the way the dispatcher dedupes a fire, recording the key of every submission.
type keyedSubmitter struct {
	// mu guards runs and keys.
	mu sync.Mutex
	// runs maps an idempotency key to the run it holds.
	runs map[string]string
	// keys records the key each submission carried, in order.
	keys []string
}

// newKeyedSubmitter returns a keyedSubmitter holding the given runs by key.
func newKeyedSubmitter(runs map[string]string) *keyedSubmitter {
	if runs == nil {
		runs = map[string]string{}
	}
	return &keyedSubmitter{runs: runs}
}

// Submit records a single submission.
func (k *keyedSubmitter) Submit(_ context.Context, _, _ string, opts ...run.SubmitOption) (*run.Run,
	error) {
	return k.submit(opts), nil
}

// SubmitSplit records a split submission.
func (k *keyedSubmitter) SubmitSplit(_ context.Context, _, _ string, _ int,
	opts ...run.SubmitOption) (*run.Run, error) {
	return k.submit(opts), nil
}

// SubmitPipeline records a pipeline submission.
func (k *keyedSubmitter) SubmitPipeline(_ context.Context, _, _ string, _ []run.PipelineStep,
	opts ...run.SubmitOption) (*run.Run, error) {
	return k.submit(opts), nil
}

// submit returns the run the submission's key holds, creating it when the key holds none.
func (k *keyedSubmitter) submit(opts []run.SubmitOption) *run.Run {
	r := &run.Run{}
	run.ApplyOptions(r, opts)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, r.IdempotencyKey)
	id, ok := k.runs[r.IdempotencyKey]
	if !ok {
		id = fmt.Sprintf("run_%d", len(k.runs)+1)
		k.runs[r.IdempotencyKey] = id
	}
	return &run.Run{ID: id}
}

// byKey finds the run a key holds, standing in for the run store's lookup.
func (k *keyedSubmitter) byKey(_ context.Context, key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.runs[key], nil
}

// submitted returns the keys submitted so far.
func (k *keyedSubmitter) submitted() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.keys...)
}

// TestSweepTakesUpAFireAStoppedServerLeftInFlight pins what becomes of an occurrence whose server
// stopped after claiming it and before recording its fire, which used to be lost. The claim left it
// marked in flight, and once the mark is old enough any scheduler's sweep takes it up: a fire that
// created its run is recorded with that run and not fired again, one that did not is fired again
// under the same idempotency key, which finds a run that landed rather than duplicating it, and a
// schedule disabled or deleted since the claim fires nothing. A mark younger than the grace is left
// to the server that may still be firing it.
func TestSweepTakesUpAFireAStoppedServerLeftInFlight(t *testing.T) {
	t.Parallel()
	due := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	next := due.Add(time.Hour)
	key := run.ScheduleKey("sch_1", due)
	tests := []struct {
		Landed        map[string]string
		Change        func(context.Context, Store) error
		Grace         time.Duration
		Name          string
		WantLastRunID string
		WantLastError string
		WantKeys      []string
		NoLookup      bool
		WantMarked    bool
	}{{ // Test 0: The server stopped before the run existed, so the occurrence fires.
		Name: "no run yet", WantKeys: []string{key}, WantLastRunID: "run_1",
	}, { // Test 1: The run landed before the stop, so it is recorded and nothing fires.
		Name: "run landed", Landed: map[string]string{key: "run_landed"},
		WantLastRunID: "run_landed",
	}, { // Test 2: Without a lookup the fire goes again, and its key finds the run that landed.
		Name: "run landed, no lookup", Landed: map[string]string{key: "run_landed"}, NoLookup: true,
		WantKeys: []string{key}, WantLastRunID: "run_landed",
	}, { // Test 3: Disabled since the claim, so it does not fire and says why.
		Name: "disabled", Change: func(ctx context.Context, store Store) error {
			sc, err := store.Get(ctx, "sch_1")
			if err != nil {
				return err
			}
			sc.Enabled = false
			return store.Update(ctx, sc)
		},
		WantLastError: "disabled before the fire was taken up again",
	}, { // Test 4: Deleted since the claim, so there is nothing to fire.
		Name:   "deleted",
		Change: func(ctx context.Context, store Store) error { return store.Delete(ctx, "sch_1") },
	}, { // Test 5: Still within the grace, so the server that claimed it may yet record it.
		Name: "within the grace", Grace: time.Hour, WantMarked: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			at := due
			if err := store.Save(ctx, &Schedule{ID: "sch_1", Name: "nightly", Cron: "0 * * * *",
				Playbook: "site.yml", Enabled: true, CreatedAt: due.Add(-time.Hour),
				NextRunAt: &at}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			// The claim of the server that stopped, which marked the occurrence in flight.
			if won, err := store.ClaimFire(ctx, "sch_1", due, &next); err != nil || !won {
				t.Fatalf("ClaimFire() = %v, %v, want a won claim", won, err)
			}
			if test.Change != nil {
				if err := test.Change(ctx, store); err != nil {
					t.Fatalf("changing the schedule error = %v", err)
				}
			}
			sub := newKeyedSubmitter(test.Landed)
			var opts []SchedulerOption
			if !test.NoLookup {
				opts = append(opts, WithRunByKey(sub.byKey))
			}
			s := NewScheduler(store, sub, zap.NewNop(), opts...)
			s.inFlightGrace = test.Grace
			s.tick(due.Add(time.Minute))

			if diff := cmp.Diff(test.WantKeys, sub.submitted(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("submissions mismatch (-want +got):\n%s", diff)
			}
			marked, err := store.TakeInFlight(ctx, 0)
			if err != nil {
				t.Fatalf("TakeInFlight() error = %v", err)
			}
			if (len(marked) > 0) != test.WantMarked {
				t.Errorf("in flight after the sweep = %v, want marked %v", marked, test.WantMarked)
			}
			sc, err := store.Get(ctx, "sch_1")
			if errors.Is(err, ErrNotFound) {
				return
			}
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if sc.LastRunID != test.WantLastRunID {
				t.Errorf("LastRunID = %q, want %q", sc.LastRunID, test.WantLastRunID)
			}
			if !strings.Contains(sc.LastError, test.WantLastError) ||
				(test.WantLastError == "" && sc.LastError != "") {
				t.Errorf("LastError = %q, want it to say %q", sc.LastError, test.WantLastError)
			}
			if sc.NextRunAt == nil || !sc.NextRunAt.Equal(next) {
				t.Errorf("NextRunAt = %v, want the claim's %v left alone", sc.NextRunAt, next)
			}
		})
	}
}

// TestAFireKeepsItsMarkUntilItIsRecorded pins when a fire's in-flight mark clears: once its run is
// recorded on the schedule, and not before. A fire whose record the store refused keeps the mark,
// and the sweep records the run it made, by its key, without firing the occurrence a second time.
func TestAFireKeepsItsMarkUntilItIsRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := NewMemStore()
	if err := inner.Save(ctx, dueSchedule("sch_1", "0 * * * *")); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	store := &errStore{Store: inner, recordErr: errors.New("db down")}
	sub := newKeyedSubmitter(nil)
	s := NewScheduler(store, sub, zap.NewNop(), WithRunByKey(sub.byKey))
	s.inFlightGrace = 0
	s.tick(time.Now())
	if got := len(sub.submitted()); got != 1 {
		t.Fatalf("fired %d times, want once", got)
	}
	sc, err := inner.Get(ctx, "sch_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if sc.LastRunID != "" {
		t.Fatalf("LastRunID = %q although the record was refused", sc.LastRunID)
	}

	store.recordErr = nil
	s.tick(time.Now())
	if got := sub.submitted(); len(got) != 1 {
		t.Errorf("submissions = %v, want the one fire, taken up without firing again", got)
	}
	sc, err = inner.Get(ctx, "sch_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if sc.LastRunID != "run_1" {
		t.Errorf("LastRunID = %q, want the run the first fire made", sc.LastRunID)
	}
	if marked, err := inner.TakeInFlight(ctx, 0); err != nil || len(marked) != 0 {
		t.Errorf("in flight after the fire was recorded = %v, %v, want none", marked, err)
	}
}

// TestTwoSweepsTakeUpAnOccurrenceOnce pins that the servers of a highly available pair, sweeping at
// the same moment, fire an occurrence left in flight once between them.
//
// The grace is real and the mark is backdated past it, as a crash leaves one. A grace of zero
// removed the very protection under test: taking up a mark stamps it afresh, and only a positive
// grace keeps the other sweep from reading the fresh stamp as stale again, so with zero the second
// sweep fired whenever it ran before the first had settled, which a busy runner made happen.
func TestTwoSweepsTakeUpAnOccurrenceOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	due := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	next := due.Add(time.Hour)
	store := NewMemStore()
	at := due
	if err := store.Save(ctx, &Schedule{ID: "sch_1", Cron: "0 * * * *", Playbook: "site.yml",
		Enabled: true, CreatedAt: due.Add(-time.Hour), NextRunAt: &at}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if won, err := store.ClaimFire(ctx, "sch_1", due, &next); err != nil || !won {
		t.Fatalf("ClaimFire() = %v, %v, want a won claim", won, err)
	}
	mem := store.(*memStore)
	mem.mu.Lock()
	mark := mem.inflight["sch_1"]
	mark.since = time.Now().Add(-time.Hour)
	mem.inflight["sch_1"] = mark
	mem.mu.Unlock()
	sub := &countingKindSubmitter{}
	var wg sync.WaitGroup
	for range 2 {
		s := NewScheduler(store, sub, zap.NewNop())
		s.inFlightGrace = time.Minute
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.sweepInFlight(due.Add(time.Minute))
		}()
	}
	wg.Wait()
	if got := sub.calls.Load(); got != 1 {
		t.Errorf("fired %d times between the two sweeps, want once", got)
	}
}
