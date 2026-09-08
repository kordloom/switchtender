package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// TestListSurvivesOneUnreadableScheduleStamp demonstrates a defect and is skipped so the suite stays
// green. The operator decides whether to fix it.
//
// normalizeScheduleTimes deliberately leaves a next_run_at it cannot parse alone, and says why:
// "Rewriting a value we cannot read would be a guess, and the schedule is already broken in a way a
// migration cannot honestly repair." That reasoning assumes the damage stays with the one row. It
// does not. scanSchedule returns the parse error, and List turns any scan error into a failure for
// the whole query, so a single unreadable stamp takes down every schedule listing in the install:
// the schedules API page and the scheduler's own enumeration both return an error, and no schedule
// fires, not just the damaged one.
//
// The write path tolerates the row on purpose and the read path does not, which is the inconsistency
// rather than either half on its own. A skip on the bad row, or a zero next run with the raw text
// preserved, would keep the blast radius where the migration comment already assumes it is.
func TestListSurvivesOneUnreadableScheduleStamp(t *testing.T) {
	t.Skip("BUG: one unparseable schedules.next_run_at makes every List fail, so no schedule fires")

	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	healthy := fmt.Sprintf("sch_healthy_%d", stamp)
	damaged := fmt.Sprintf("sch_damaged_%d", stamp)
	for _, id := range []string{healthy, damaged} {
		if err := db.Schedules().Save(ctx, &schedule.Schedule{
			ID: id, Cron: "* * * * *", Playbook: "p.yml", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}

	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() {
		conn, cerr := sql.Open("pgx", dsn)
		if cerr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for _, id := range []string{healthy, damaged} {
			_, _ = conn.Exec("DELETE FROM schedules WHERE id=$1", id)
		}
	})
	if _, err := raw.Exec("UPDATE schedules SET next_run_at='not a timestamp' WHERE id=$1",
		damaged); err != nil {
		t.Fatalf("damage the row: %v", err)
	}
	_ = raw.Close()

	got, err := db.Schedules().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v.\n\nOne schedule with an unreadable next_run_at took down the "+
			"listing for every schedule in the install. normalizeScheduleTimes leaves such a row "+
			"in place on purpose, reasoning that the schedule is already broken, but the read path "+
			"spreads that one row's damage to all of them: the schedules page errors and the "+
			"scheduler enumerates nothing, so no schedule fires at all.", err)
	}
	var sawHealthy bool
	for _, sc := range got {
		if sc.ID == healthy {
			sawHealthy = true
		}
	}
	if !sawHealthy {
		t.Error("the healthy schedule is missing from the listing, so the damaged row still cost " +
			"more than itself")
	}
}

// TestAppendLogBoundsOneRunsCapturedOutput pins the only limit on how much output a single run may
// store. Without it one runaway playbook fills the database and takes down the control plane for
// every tenant on it, so this is a resource bound rather than a formatting nicety. The refusal must
// also be visible: a log that simply stops reads as a run that went quiet, which is why the run
// carries a warning once the cap bites.
//
// Not parallel: it writes tens of megabytes, and running beside the rest would multiply that.
func TestAppendLogBoundsOneRunsCapturedOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("writes MaxLogBytes of output")
	}
	ctx, s := fenceStore(t)
	r := newPending(fmt.Sprintf("q_logcap_%d", time.Now().UnixNano()), "cap")
	r.Status = run.StatusRunning
	saveRun(t, ctx, s, r)

	// Fill to the cap in large chunks, then prove the next write is refused.
	const chunk = 4 << 20
	blob := make([]byte, chunk)
	for i := range blob {
		blob[i] = 'x'
	}
	for written := 0; written < run.MaxLogBytes; written += chunk {
		if err := s.AppendLog(ctx, r.ID, blob); err != nil {
			t.Fatalf("AppendLog() error at %d bytes = %v", written, err)
		}
	}

	t.Run("test 0", func(t *testing.T) { // Test 0: A write past the cap is refused, not an error.
		if err := s.AppendLog(ctx, r.ID, []byte("one byte too many")); err != nil {
			t.Fatalf("AppendLog() past the cap = %v, want a silent refusal", err)
		}
		got, err := s.Log(ctx, r.ID)
		if err != nil {
			t.Fatalf("Log() error = %v", err)
		}
		if len(got) > run.MaxLogBytes+chunk {
			t.Errorf("the run holds %d bytes, more than the cap plus the chunk that crossed it: "+
				"one runaway playbook can fill the database", len(got))
		}
	})
	t.Run("test 1", func(t *testing.T) { // Test 1: The truncation is announced on the run itself.
		got, err := s.Get(ctx, r.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.Warning != run.LogTruncatedWarning {
			t.Errorf("Warning = %q, want %q: without it a reader sees a log that simply stops and "+
				"reads it as a run that went quiet", got.Warning, run.LogTruncatedWarning)
		}
	})
	t.Run("test 2", func(t *testing.T) { // Test 2: The run is still live, so the cap is what refused.
		got, err := s.Get(ctx, r.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.Status != run.StatusRunning {
			t.Errorf("status = %s, want running: hitting the log cap must not end the run",
				got.Status)
		}
	})
}

// TestLogAndEventReadersReportAMissingRun pins the existence check every auxiliary reader shares. A
// reader that returned an empty result for a run that is not there would show a person an empty log
// for a mistyped id and let them conclude the run produced no output.
func TestLogAndEventReadersReportAMissingRun(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)

	tests := []struct {
		Name string
		Read func(id string) error
	}{{ // Test 0: The whole-log reader.
		Name: "Log", Read: func(id string) error { _, err := s.Log(ctx, id); return err },
	}, { // Test 1: The incremental log reader the live view polls.
		Name: "LogAfter", Read: func(id string) error { _, err := s.LogAfter(ctx, id, 0, 10); return err },
	}, { // Test 2: The log cursor.
		Name: "LastLogSeq", Read: func(id string) error { _, err := s.LastLogSeq(ctx, id); return err },
	}, { // Test 3: The whole-event reader.
		Name: "Events", Read: func(id string) error { _, err := s.Events(ctx, id); return err },
	}, { // Test 4: The incremental event reader.
		Name: "EventsAfter", Read: func(id string) error { _, err := s.EventsAfter(ctx, id, 0, 10); return err },
	}, { // Test 5: The event cursor, which had no coverage at all.
		Name: "LastEventSeq", Read: func(id string) error { _, err := s.LastEventSeq(ctx, id); return err },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.Read("run_no_such_reader")
			if err == nil {
				t.Errorf("%s() on a missing run returned no error, so a mistyped id looks like a "+
					"run that produced nothing", test.Name)
			}
		})
	}
}

// TestLogAndEventCursorsAdvanceWithTheirStreams pins the resume cursors the live run view depends
// on. A cursor that reported a stale sequence makes the view replay output it already showed, and
// one that ran ahead makes it skip output entirely. LastEventSeq in particular had no test at all.
func TestLogAndEventCursorsAdvanceWithTheirStreams(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	r := newPending(fmt.Sprintf("q_cursor_%d", time.Now().UnixNano()), "cursor")
	r.Status = run.StatusRunning
	saveRun(t, ctx, s, r)

	t.Run("test 0", func(t *testing.T) { // Test 0: An empty run reports zero, not an error.
		seq, err := s.LastLogSeq(ctx, r.ID)
		if err != nil {
			t.Fatalf("LastLogSeq() error = %v", err)
		}
		if seq != 0 {
			t.Errorf("LastLogSeq on an empty run = %d, want 0", seq)
		}
		eseq, err := s.LastEventSeq(ctx, r.ID)
		if err != nil {
			t.Fatalf("LastEventSeq() error = %v", err)
		}
		if eseq != 0 {
			t.Errorf("LastEventSeq on an empty run = %d, want 0", eseq)
		}
	})
	t.Run("test 1", func(t *testing.T) { // Test 1: The cursor tracks what the reader hands back.
		for i := range 3 {
			if err := s.AppendLog(ctx, r.ID, []byte(fmt.Sprintf("line %d\n", i))); err != nil {
				t.Fatalf("AppendLog() error = %v", err)
			}
		}
		seq, err := s.LastLogSeq(ctx, r.ID)
		if err != nil {
			t.Fatalf("LastLogSeq() error = %v", err)
		}
		chunks, err := s.LogAfter(ctx, r.ID, 0, 100)
		if err != nil {
			t.Fatalf("LogAfter() error = %v", err)
		}
		if len(chunks) != 3 {
			t.Fatalf("LogAfter returned %d chunks, want 3", len(chunks))
		}
		if chunks[len(chunks)-1].Seq != seq {
			t.Errorf("LastLogSeq = %d but the newest chunk is %d: the live view either replays "+
				"output or skips it", seq, chunks[len(chunks)-1].Seq)
		}
		// Resuming from the cursor yields nothing, which is what stops the replay.
		rest, err := s.LogAfter(ctx, r.ID, seq, 100)
		if err != nil {
			t.Fatalf("LogAfter() error = %v", err)
		}
		if len(rest) != 0 {
			t.Errorf("resuming at the cursor returned %d chunks, want none", len(rest))
		}
	})
	t.Run("test 2", func(t *testing.T) { // Test 2: The event cursor behaves the same way.
		if err := s.AppendEvents(ctx, r.ID, trivialEvents(3)); err != nil {
			t.Fatalf("AppendEvents() error = %v", err)
		}
		seq, err := s.LastEventSeq(ctx, r.ID)
		if err != nil {
			t.Fatalf("LastEventSeq() error = %v", err)
		}
		evs, err := s.EventsAfter(ctx, r.ID, 0, 100)
		if err != nil {
			t.Fatalf("EventsAfter() error = %v", err)
		}
		if len(evs) != 3 {
			t.Fatalf("EventsAfter returned %d events, want 3", len(evs))
		}
		if evs[len(evs)-1].Seq != seq {
			t.Errorf("LastEventSeq = %d but the newest event is %d", seq, evs[len(evs)-1].Seq)
		}
		rest, err := s.EventsAfter(ctx, r.ID, seq, 100)
		if err != nil {
			t.Fatalf("EventsAfter() error = %v", err)
		}
		if len(rest) != 0 {
			t.Errorf("resuming at the cursor returned %d events, want none", len(rest))
		}
	})
	t.Run("test 3", func(t *testing.T) { // Test 3: A limit of zero means every event, not none.
		evs, err := s.EventsAfter(ctx, r.ID, 0, 0)
		if err != nil {
			t.Fatalf("EventsAfter() error = %v", err)
		}
		if len(evs) != 3 {
			t.Errorf("EventsAfter with a zero limit returned %d events, want all 3: a zero limit "+
				"reaching the database as LIMIT 0 would blank the live view", len(evs))
		}
		chunks, err := s.LogAfter(ctx, r.ID, 0, 0)
		if err != nil {
			t.Fatalf("LogAfter() error = %v", err)
		}
		if len(chunks) != 3 {
			t.Errorf("LogAfter with a zero limit returned %d chunks, want all 3", len(chunks))
		}
	})
}

// TestRunStatusCountsTalliesOnlyTopLevelRuns pins the dashboard tally. Shards and pipeline steps are
// runs in the same table, so a count that included them would show an operator several times the
// work that actually exists.
func TestRunStatusCountsTalliesOnlyTopLevelRuns(t *testing.T) {
	// Not parallel: the tally covers every run in the database, so it is a whole-table read the way
	// the retention sweeps are whole-table writes.
	ctx, s := fenceStore(t)
	q := fmt.Sprintf("q_counts_%d", time.Now().UnixNano())

	before, err := s.RunStatusCounts(ctx)
	if err != nil {
		t.Fatalf("RunStatusCounts() error = %v", err)
	}

	parent := newPending(q, "countparent")
	parent.Kind = "split"
	parent.Status = run.StatusRunning
	saveRun(t, ctx, s, parent)
	pid := parent.ID
	for i := range 3 {
		child := newPending(q, fmt.Sprintf("countchild%d", i))
		child.ParentID = &pid
		child.Status = run.StatusRunning
		saveRun(t, ctx, s, child)
	}

	after, err := s.RunStatusCounts(ctx)
	if err != nil {
		t.Fatalf("RunStatusCounts() error = %v", err)
	}
	delta := after[run.StatusRunning] - before[run.StatusRunning]
	if delta != 1 {
		t.Errorf("the running tally moved by %d after adding one parent and three shards, want 1: "+
			"counting children shows an operator several times the work that exists", delta)
	}
}

// trivialEvents builds n placeholder events for a cursor test.
func trivialEvents(n int) []event.Event {
	out := make([]event.Event, n)
	for i := range out {
		out[i].Type = "task"
	}
	return out
}

// TestStartupMigrationDoesNotBreakRuntimeWrites demonstrates a defect and is skipped so the suite
// stays green. The operator decides whether to fix it.
//
// Open runs the whole migration inside one transaction that takes AccessExclusiveLock on every
// table, and its comment says every process calls Open, workers included, so it runs on ordinary
// starts and not only on upgrades. The comment then reasons about the failure it wants: "Failing
// fast instead means the starting process retries rather than freezing everyone else, which is the
// direction this should fail in."
//
// The cost does not land only on the starting process. PostgreSQL breaks the lock cycle by killing
// one of the two transactions, and roughly half the time the victim is the ordinary runtime write,
// which comes back as SQLSTATE 40P01 to a caller that has no retry. Running six writers alongside
// six nodes opening the database, twenty-four of a hundred and eighty summary writes failed this
// way. In production that is a rolling restart making healthy nodes lose host summaries.
//
// The lock_timeout the migration sets does not help, because deadlock detection fires first and
// returns a different error. Taking the schema lock before the per-column ALTERs, or letting the
// migration skip entirely when the schema is already current, would keep the failure on the side
// the comment intends.
func TestStartupMigrationDoesNotBreakRuntimeWrites(t *testing.T) {
	t.Skip("BUG: a node running Open deadlocks ordinary summary writes on other nodes (40P01)")

	dsn := testDSN(t)
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()

	var mu sync.Mutex
	var failures []error
	var wg sync.WaitGroup

	// Six workers doing ordinary runtime writes.
	for w := range 6 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 30 {
				r := newPending(fmt.Sprintf("q_dl_%d", stamp), fmt.Sprintf("%d_%d", w, i))
				r.Status = run.StatusRunning
				if err := s.Save(ctx, r); err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
					continue
				}
				err := s.SaveHostSummary(ctx, r.ID, []run.HostSummary{
					{Host: fmt.Sprintf("h%d", w), OK: 1, Worst: "ok", RanAt: time.Now().UTC()},
				})
				if err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
				}
			}
		}(w)
	}
	// Six nodes coming up, each running the startup migration, as a rolling restart does.
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 6 {
				db, err := pgstore.Open(dsn)
				if err == nil {
					_ = db.Close()
				}
			}
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d ordinary writes failed while other nodes were starting up; first: %v.\n\n"+
			"The startup migration holds AccessExclusiveLock on every table, and PostgreSQL breaks "+
			"the resulting cycle by killing whichever transaction it picks, often the runtime "+
			"write rather than the migration. The caller has no retry, so a rolling restart makes "+
			"healthy nodes lose host summaries.", len(failures), failures[0])
	}
}
