package schedule_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// storedSubmitter saves each submission as a pending run in a run store, the way the dispatcher
// does, and returns the run a key already holds instead of saving another. With hold set it saves
// the run, closes entered, and then waits for its context to end and fails, so the run exists while
// a stop loses the answer.
type storedSubmitter struct {
	// runs is the run store submissions land in.
	runs run.Store
	// hold makes every submit save its run, then wait for its context to end and fail.
	hold bool
	// entered is closed when the first held submit has saved its run.
	entered chan struct{}
	// once closes entered a single time.
	once sync.Once
	// mu guards created.
	mu sync.Mutex
	// created counts the runs saved.
	created int
}

// do runs one submission.
func (s *storedSubmitter) do(ctx context.Context, opts []run.SubmitOption) (*run.Run, error) {
	r := &run.Run{Status: run.StatusPending, CreatedAt: time.Now().UTC()}
	run.ApplyOptions(r, opts)
	existing, err := s.runs.ByIdempotencyKey(ctx, r.IdempotencyKey)
	if err == nil {
		return existing, nil
	}
	s.mu.Lock()
	s.created++
	r.ID = fmt.Sprintf("run_%d", s.created)
	s.mu.Unlock()
	if err := s.runs.Save(ctx, r); err != nil {
		return nil, err
	}
	if !s.hold {
		return r, nil
	}
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// count returns how many runs were saved.
func (s *storedSubmitter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.created
}

// Submit runs a single-run submission.
func (s *storedSubmitter) Submit(ctx context.Context, _, _ string, opts ...run.SubmitOption) (*run.Run, error) {
	return s.do(ctx, opts)
}

// SubmitSplit runs a split submission.
func (s *storedSubmitter) SubmitSplit(ctx context.Context, _, _ string, _ int,
	opts ...run.SubmitOption) (*run.Run, error) {
	return s.do(ctx, opts)
}

// SubmitPipeline runs a pipeline submission.
func (s *storedSubmitter) SubmitPipeline(ctx context.Context, _, _ string, _ []run.PipelineStep,
	opts ...run.SubmitOption) (*run.Run, error) {
	return s.do(ctx, opts)
}

// scheduleStores returns each schedule store a server can run on, by name: the memory store,
// SQLite, and PostgreSQL. PostgreSQL is left out without a server to create a database on, unless
// the full suite is required, which fails the test instead.
func scheduleStores(t *testing.T) map[string]func(t *testing.T) schedule.Store {
	t.Helper()
	stores := map[string]func(t *testing.T) schedule.Store{
		"memory": func(*testing.T) schedule.Store { return schedule.NewMemStore() },
		"sqlite": func(t *testing.T) schedule.Store {
			db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
			if err != nil {
				t.Fatalf("sqlitestore.Open() error = %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db.Schedules()
		},
	}
	if os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN") == "" {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN is not")
		}
		t.Log("SWITCHTENDER_TEST_POSTGRES_DSN is not set, so PostgreSQL is left out")
		return stores
	}
	stores["postgres"] = func(t *testing.T) schedule.Store {
		db, err := pgstore.Open(freshPostgres(t))
		if err != nil {
			t.Fatalf("pgstore.Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Schedules()
	}
	return stores
}

// freshPostgres creates a database of the test's own on the server SWITCHTENDER_TEST_POSTGRES_DSN
// names and returns a DSN for it, dropping it when the test ends.
func freshPostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open the PostgreSQL server: %v", err)
	}
	defer func() { _ = admin.Close() }()
	name := fmt.Sprintf("st_sched_handback_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create the test database: %v", err)
	}
	t.Cleanup(func() {
		drop, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		_, _ = drop.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse the PostgreSQL DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// TestAHandedBackFireThatHadCreatedItsRunIsRecorded stops a scheduler while the submit of a fire
// has already saved its run, then starts another over the same stores, wired the way a server wires
// it: waiting for the schedule's own run still going, and finding a run by its idempotency key.
//
// The stop hands the occurrence back with a note saying so. The scheduler that takes it up found
// the run the interrupted fire had created still pending, read it as an earlier run still going,
// and skipped the occurrence, leaving the note as the schedule's last error until the next fire,
// and for the last occurrence of a bounded rule, for good. The run it finds is the occurrence's
// own, so the fire is recorded with it: the note is cleared, the last run names it, no second run
// is created, and nothing is left in flight.
//
//nolint:funlen // Test function.
func TestAHandedBackFireThatHadCreatedItsRunIsRecorded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Store opens the schedule store.
		Store string
		// Rule is the recurrence after DTSTART.
		Rule string
		// WantFinished is whether the occurrence is the rule's last, so nothing fires after it.
		WantFinished bool
	}{{ // Test 0: The last occurrence of a bounded rule, in memory.
		Store: "memory", Rule: "RRULE:FREQ=HOURLY;COUNT=2", WantFinished: true,
	}, { // Test 1: An occurrence with more to come, in memory.
		Store: "memory", Rule: "RRULE:FREQ=HOURLY",
	}, { // Test 2: The last occurrence of a bounded rule, on SQLite.
		Store: "sqlite", Rule: "RRULE:FREQ=HOURLY;COUNT=2", WantFinished: true,
	}, { // Test 3: An occurrence with more to come, on SQLite.
		Store: "sqlite", Rule: "RRULE:FREQ=HOURLY",
	}, { // Test 4: The last occurrence of a bounded rule, on PostgreSQL.
		Store: "postgres", Rule: "RRULE:FREQ=HOURLY;COUNT=2", WantFinished: true,
	}, { // Test 5: An occurrence with more to come, on PostgreSQL.
		Store: "postgres", Rule: "RRULE:FREQ=HOURLY",
	}}
	stores := scheduleStores(t)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Store), func(t *testing.T) {
			t.Parallel()
			open, ok := stores[test.Store]
			if !ok {
				t.Skipf("no %s server to test against", test.Store)
			}
			ctx := context.Background()
			store := open(t)
			start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
			due := start.Add(time.Hour)
			if err := store.Save(ctx, &schedule.Schedule{
				ID: "sch_handback", Name: "hourly", Playbook: "site.yml", Enabled: true,
				RRule:     "DTSTART:" + start.Format("20060102T150405Z") + " " + test.Rule,
				CreatedAt: start, NextRunAt: &due,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			runs := run.NewMemStore()
			wired := func() []schedule.SchedulerOption {
				return []schedule.SchedulerOption{
					schedule.WithInterval(10 * time.Millisecond),
					schedule.WithRunActive(schedule.ActiveIn(runs)),
					schedule.WithRunByKey(schedule.KeyedIn(runs)),
				}
			}
			first := &storedSubmitter{runs: runs, hold: true, entered: make(chan struct{})}
			stopping := schedule.NewScheduler(store, first, nil, wired()...)
			stopping.Start()
			select {
			case <-first.entered:
			case <-time.After(30 * time.Second):
				stopping.Close()
				t.Fatal("the occurrence never reached the submitter")
			}
			stopping.Close()
			handed, err := store.Get(ctx, "sch_handback")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if handed.NextRunAt == nil || !handed.NextRunAt.Equal(due) ||
				!strings.Contains(handed.LastError, "handed back") {
				t.Fatalf("after the stop the schedule has next fire %v and error %q, want the "+
					"occurrence %v handed back with a note saying so", handed.NextRunAt,
					handed.LastError, due)
			}

			second := &storedSubmitter{runs: runs}
			taking := schedule.NewScheduler(store, second, nil, wired()...)
			taking.Start()
			var got *schedule.Schedule
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				if got, err = store.Get(ctx, "sch_handback"); err == nil && got.LastRunID != "" {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			taking.Close()
			if got, err = store.Get(ctx, "sch_handback"); err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.LastRunID != "run_1" || got.LastError != "" {
				t.Fatalf("after the takeover the schedule's last run is %q and its last error %q, "+
					"want the run the interrupted fire created and the hand-back note gone",
					got.LastRunID, got.LastError)
			}
			if diff := cmp.Diff(1, first.count()+second.count()); diff != "" {
				t.Errorf("runs created for the one occurrence (-want +got):\n%s", diff)
			}
			if finished := got.NextRunAt == nil; finished != test.WantFinished ||
				(!finished && !got.NextRunAt.After(due)) {
				t.Errorf("after the takeover the next fire is %v, want finished = %v and otherwise "+
					"past %v", got.NextRunAt, test.WantFinished, due)
			}
			left, err := store.TakeInFlight(ctx, 0)
			if err != nil {
				t.Fatalf("TakeInFlight() error = %v", err)
			}
			if diff := cmp.Diff([]schedule.InFlight(nil), left, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("occurrences left in flight (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAnEarlierRunStillGoingStillSkipsTheOccurrence keeps the hand-back fix narrow. With the same
// wiring, a run an earlier occurrence fired that is still going is an overlap, so the due
// occurrence is skipped as before: nothing is submitted and no fire is recorded, and the schedule
// moves on to its next occurrence.
func TestAnEarlierRunStillGoingStillSkipsTheOccurrence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Store opens the schedule store.
		Store string
	}{{ // Test 0: In memory.
		Store: "memory",
	}, { // Test 1: On SQLite.
		Store: "sqlite",
	}, { // Test 2: On PostgreSQL.
		Store: "postgres",
	}}
	stores := scheduleStores(t)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Store), func(t *testing.T) {
			t.Parallel()
			open, ok := stores[test.Store]
			if !ok {
				t.Skipf("no %s server to test against", test.Store)
			}
			ctx := context.Background()
			store := open(t)
			start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
			due := start.Add(time.Hour)
			if err := store.Save(ctx, &schedule.Schedule{
				ID: "sch_overlap", Name: "hourly", Playbook: "site.yml", Enabled: true,
				RRule:     "DTSTART:" + start.Format("20060102T150405Z") + " RRULE:FREQ=HOURLY",
				CreatedAt: start, NextRunAt: &due, LastRunID: "run_earlier",
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			runs := run.NewMemStore()
			if err := runs.Save(ctx, &run.Run{
				ID: "run_earlier", Status: run.StatusPending, CreatedAt: start,
				Source: "schedule", SourceID: "sch_overlap",
				IdempotencyKey: run.ScheduleKey("sch_overlap", start),
			}); err != nil {
				t.Fatalf("save the earlier run: %v", err)
			}
			sub := &storedSubmitter{runs: runs}
			s := schedule.NewScheduler(store, sub, nil,
				schedule.WithInterval(10*time.Millisecond),
				schedule.WithRunActive(schedule.ActiveIn(runs)),
				schedule.WithRunByKey(schedule.KeyedIn(runs)))
			s.Start()
			var got *schedule.Schedule
			var err error
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				if got, err = store.Get(ctx, "sch_overlap"); err == nil && got.NextRunAt != nil &&
					got.NextRunAt.After(due) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			s.Close()
			if got, err = store.Get(ctx, "sch_overlap"); err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.NextRunAt == nil || !got.NextRunAt.After(due) {
				t.Fatalf("the next fire is %v, want the skipped occurrence %v passed", got.NextRunAt,
					due)
			}
			if got.LastRunAt != nil || got.LastRunID != "run_earlier" || got.LastError != "" {
				t.Errorf("after the skip the schedule last fired at %v with run %q and error %q, "+
					"want no fire recorded over the earlier run", got.LastRunAt, got.LastRunID,
					got.LastError)
			}
			if diff := cmp.Diff(0, sub.count()); diff != "" {
				t.Errorf("runs submitted while the earlier run was going (-want +got):\n%s", diff)
			}
		})
	}
}
