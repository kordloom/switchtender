package sqlitestore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// summaryAppender returns the batch-append capability the store advertises, or fails the test. It
// is a separate interface from run.Store so the transport stores are not made to implement it, and
// a store that quietly stopped providing it would leave relayed reports rewriting whole sets.
func summaryAppender(t *testing.T, store run.Store) run.SummaryAppender {
	t.Helper()
	a, ok := store.(run.SummaryAppender)
	if !ok {
		t.Fatal("the SQLite store does not append summary batches, so a relayed report rewrites " +
			"the whole accumulated set on every continuation")
	}
	return a
}

// TestTerminalRunRefusesEveryAuxiliaryWrite pins the fence a reclaimed-but-alive worker runs into.
// A worker whose lease expired keeps executing and keeps reporting, and the sweep has already
// written the run's outcome. Every one of these writes would otherwise land on a finished run:
// logs and events appended after the end, and summaries overwriting the final ones with a partial
// view. The fence is silent rather than an error, because the worker is doing nothing wrong, it
// just no longer owns the run.
func TestTerminalRunRefusesEveryAuxiliaryWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()
	appender := summaryAppender(t, store)

	statuses := []run.Status{
		run.StatusSucceeded, run.StatusFailed, run.StatusCanceled, run.StatusInterrupted,
		run.StatusRejected,
	}
	for testNum, status := range statuses {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id := fmt.Sprintf("run_%s", status)

			// Everything is written while the run is alive, then the run is settled and the same
			// writes are made again.
			saveRuns(t, store, &run.Run{ID: id, Status: run.StatusRunning, CreatedAt: baseTime})
			if err := store.AppendLog(ctx, id, []byte("alive\n")); err != nil {
				t.Fatalf("AppendLog(alive) error = %v", err)
			}
			if err := store.AppendEvents(ctx, id,
				[]event.Event{{Type: event.TypePlayStart, Time: baseTime, Play: "alive"}}); err != nil {
				t.Fatalf("AppendEvents(alive) error = %v", err)
			}
			if err := store.SaveHostSummary(ctx, id, []run.HostSummary{
				{Host: "web01", OK: 1, Worst: "ok", RanAt: baseTime},
			}); err != nil {
				t.Fatalf("SaveHostSummary(alive) error = %v", err)
			}
			if err := store.SaveTaskSummary(ctx, id, []run.TaskSummary{
				{Task: "install", Seconds: 1, RanAt: baseTime},
			}); err != nil {
				t.Fatalf("SaveTaskSummary(alive) error = %v", err)
			}

			saveRuns(t, store, &run.Run{ID: id, Status: status, CreatedAt: baseTime})

			// Every late write is accepted without error and changes nothing.
			if err := store.AppendLog(ctx, id, []byte("late\n")); err != nil {
				t.Errorf("AppendLog(after %s) = %v, want a silent no-op", status, err)
			}
			if err := store.AppendEvents(ctx, id,
				[]event.Event{{Type: event.TypePlayStart, Time: baseTime, Play: "late"}}); err != nil {
				t.Errorf("AppendEvents(after %s) = %v, want a silent no-op", status, err)
			}
			if err := store.SaveHostSummary(ctx, id, []run.HostSummary{
				{Host: "web01", OK: 999, Worst: "failed", RanAt: baseTime},
			}); err != nil {
				t.Errorf("SaveHostSummary(after %s) = %v, want a silent no-op", status, err)
			}
			if err := appender.AppendHostSummary(ctx, id, []run.HostSummary{
				{Host: "db01", OK: 999, Worst: "failed", RanAt: baseTime},
			}); err != nil {
				t.Errorf("AppendHostSummary(after %s) = %v, want a silent no-op", status, err)
			}
			if err := store.SaveTaskSummary(ctx, id, []run.TaskSummary{
				{Task: "wrong", Seconds: 99, RanAt: baseTime},
			}); err != nil {
				t.Errorf("SaveTaskSummary(after %s) = %v, want a silent no-op", status, err)
			}
			if err := appender.AppendTaskSummary(ctx, id, []run.TaskSummary{
				{Task: "also wrong", Seconds: 99, RanAt: baseTime},
			}); err != nil {
				t.Errorf("AppendTaskSummary(after %s) = %v, want a silent no-op", status, err)
			}

			// The record still says exactly what it said when the run ended.
			body, err := store.Log(ctx, id)
			if err != nil {
				t.Fatalf("Log() error = %v", err)
			}
			if string(body) != "alive\n" {
				t.Errorf("the log of a %s run reads %q, so a late chunk was appended", status, body)
			}
			events, err := store.Events(ctx, id)
			if err != nil {
				t.Fatalf("Events() error = %v", err)
			}
			if len(events) != 1 || events[0].Play != "alive" {
				t.Errorf("the events of a %s run are %+v, so a late event was appended", status,
					events)
			}
			hosts, err := store.RunHostSummaries(ctx, id)
			if err != nil {
				t.Fatalf("RunHostSummaries() error = %v", err)
			}
			want := []run.HostSummary{{RunID: id, Host: "web01", OK: 1, Worst: "ok", RanAt: baseTime}}
			if diff := cmp.Diff(want, hosts, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("host summaries of a %s run (-final +got):\n%s", status, diff)
			}
			tasks, err := store.RunTaskSummaries(ctx, id)
			if err != nil {
				t.Fatalf("RunTaskSummaries() error = %v", err)
			}
			if len(tasks) != 1 || tasks[0].Task != "install" {
				t.Errorf("task summaries of a %s run are %+v, so a late write landed", status, tasks)
			}
		})
	}
}

// TestAuxiliaryWritesOnAMissingRunAreReportedNotSwallowed pins the other half of the fence. A
// terminal run is a no-op because the worker is merely late, but a run that does not exist at all
// is a caller mistake or a lost write, and it has to be named. Silently accepting logs for an
// unknown run is how output disappears with nothing to point at.
func TestAuxiliaryWritesOnAMissingRunAreReportedNotSwallowed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	tests := []struct {
		Name string
		Call func() error
		Want error
	}{{ // Test 0: Appending output to a run that was never stored.
		Name: "append log", Want: run.ErrNotFound,
		Call: func() error { return store.AppendLog(ctx, "run_ghost", []byte("x")) },
	}, { // Test 1: Appending events to one.
		Name: "append events", Want: run.ErrNotFound,
		Call: func() error {
			return store.AppendEvents(ctx, "run_ghost",
				[]event.Event{{Type: event.TypePlayStart, Time: baseTime}})
		},
	}, { // Test 2: Reading its log.
		Name: "log", Want: run.ErrNotFound,
		Call: func() error { _, err := store.Log(ctx, "run_ghost"); return err },
	}, { // Test 3: Reading its log after a cursor.
		Name: "log after", Want: run.ErrNotFound,
		Call: func() error { _, err := store.LogAfter(ctx, "run_ghost", 0, 0); return err },
	}, { // Test 4: Asking for its last log sequence.
		Name: "last log seq", Want: run.ErrNotFound,
		Call: func() error { _, err := store.LastLogSeq(ctx, "run_ghost"); return err },
	}, { // Test 5: Reading its events.
		Name: "events", Want: run.ErrNotFound,
		Call: func() error { _, err := store.Events(ctx, "run_ghost"); return err },
	}, { // Test 6: Reading its events after a cursor.
		Name: "events after", Want: run.ErrNotFound,
		Call: func() error { _, err := store.EventsAfter(ctx, "run_ghost", 0, 0); return err },
	}, { // Test 7: Asking for its last event sequence.
		Name: "last event seq", Want: run.ErrNotFound,
		Call: func() error { _, err := store.LastEventSeq(ctx, "run_ghost"); return err },
	}, { // Test 8: Reading the facts of a host nobody gathered.
		Name: "host facts", Want: run.ErrNotFound,
		Call: func() error { _, err := store.HostFactsFor(ctx, "host_ghost"); return err },
	}, { // Test 9: Reading a run that does not exist.
		Name: "get", Want: run.ErrNotFound,
		Call: func() error { _, err := store.Get(ctx, "run_ghost"); return err },
	}, { // Test 10: Heartbeating a lease nobody holds.
		Name: "heartbeat", Want: run.ErrNotFound,
		Call: func() error { return store.Heartbeat(ctx, "run_ghost", "worker-1") },
	}, { // Test 11: Requesting a cancel on a run that is not there.
		Name: "request cancel", Want: run.ErrNotFound,
		Call: func() error { return store.RequestCancel(ctx, "run_ghost") },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := test.Call(); !errors.Is(err, test.Want) {
				t.Errorf("%s on a missing run = %v, want %v", test.Name, err, test.Want)
			}
		})
	}
}

// TestLastSequencesStartAtZeroForARunWithNothingStored pins the cursor a live tail starts from. A
// run with no output yet has a last sequence of zero rather than an error, so the streaming reader
// can open on a run that has not printed anything and wait, instead of treating an empty run as a
// missing one and giving up.
func TestLastSequencesStartAtZeroForARunWithNothingStored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store, &run.Run{ID: "run_quiet", Status: run.StatusRunning, CreatedAt: baseTime})
	if seq, err := store.LastLogSeq(ctx, "run_quiet"); err != nil || seq != 0 {
		t.Errorf("LastLogSeq(no output) = (%d, %v), want (0, nil)", seq, err)
	}
	if seq, err := store.LastEventSeq(ctx, "run_quiet"); err != nil || seq != 0 {
		t.Errorf("LastEventSeq(no events) = (%d, %v), want (0, nil)", seq, err)
	}

	// After a write the sequences advance and match what the cursor reads back.
	if err := store.AppendLog(ctx, "run_quiet", []byte("hello")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := store.AppendEvents(ctx, "run_quiet",
		[]event.Event{{Type: event.TypePlayStart, Time: baseTime, Play: "p"}}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	logSeq, err := store.LastLogSeq(ctx, "run_quiet")
	if err != nil || logSeq <= 0 {
		t.Fatalf("LastLogSeq() = (%d, %v), want a positive sequence", logSeq, err)
	}
	if chunks, err := store.LogAfter(ctx, "run_quiet", logSeq, 0); err != nil || len(chunks) != 0 {
		t.Errorf("LogAfter(head) = (%d chunks, %v), want nothing past the head", len(chunks), err)
	}
	eventSeq, err := store.LastEventSeq(ctx, "run_quiet")
	if err != nil || eventSeq <= 0 {
		t.Fatalf("LastEventSeq() = (%d, %v), want a positive sequence", eventSeq, err)
	}
	if events, err := store.EventsAfter(ctx, "run_quiet", eventSeq, 0); err != nil || len(events) != 0 {
		t.Errorf("EventsAfter(head) = (%d events, %v), want nothing past the head", len(events), err)
	}
}

// TestEmptyAndBinaryLogChunksSurviveTheRoundTrip pins that the log column is bytes, not text. A
// tool's output carries ANSI escapes, NUL bytes, and partial multi-byte sequences at a chunk
// boundary, and any of those being cleaned or rejected would corrupt the transcript an operator
// reads to work out what happened.
func TestEmptyAndBinaryLogChunksSurviveTheRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store, &run.Run{ID: "run_1", Status: run.StatusRunning, CreatedAt: baseTime})
	chunks := [][]byte{
		[]byte("plain\n"),
		{},
		{0x1b, '[', '3', '1', 'm', 'r', 'e', 'd', 0x1b, '[', '0', 'm'},
		{0x00, 0xff, 0xfe},
		[]byte("日本語\n"),
	}
	var want []byte
	for _, c := range chunks {
		if err := store.AppendLog(ctx, "run_1", c); err != nil {
			t.Fatalf("AppendLog(%v) error = %v", c, err)
		}
		want = append(want, c...)
	}
	got, err := store.Log(ctx, "run_1")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the reassembled log (-want +got):\n%s", diff)
	}

	// The chunks come back individually in order too, which is what a tail reads.
	after, err := store.LogAfter(ctx, "run_1", 0, 0)
	if err != nil {
		t.Fatalf("LogAfter() error = %v", err)
	}
	if len(after) != len(chunks) {
		t.Fatalf("LogAfter() returned %d chunks, want %d: an empty chunk was dropped",
			len(after), len(chunks))
	}
	var prev int64
	for i, c := range after {
		if c.Seq <= prev {
			t.Errorf("chunk %d has sequence %d, want it above %d", i, c.Seq, prev)
		}
		prev = c.Seq
		if diff := cmp.Diff(chunks[i], c.Data, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("chunk %d (-want +got):\n%s", i, diff)
		}
	}
}

// TestSummariesForARunWithNoRecordCountAsApplies pins the reading a missing run gets. Cross-run
// summary views are keyed by run id rather than by a stored run, because retention deletes runs and
// keeps summaries. A summary whose run is gone therefore has to be stored, and it counts as an
// apply rather than a check, because nothing proves it was a check and drift must not be invented
// from a missing record.
func TestSummariesForARunWithNoRecordCountAsApplies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()
	appender := summaryAppender(t, store)

	if err := appender.AppendHostSummary(ctx, "run_purged", []run.HostSummary{
		{Host: "web01", Changed: 5, Worst: "changed", RanAt: baseTime},
	}); err != nil {
		t.Fatalf("AppendHostSummary(purged run) error = %v", err)
	}
	stored, err := store.RunHostSummaries(ctx, "run_purged")
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("a summary for a run with no record was dropped: %+v", stored)
	}
	if stored[0].DryRun {
		t.Error("a summary for a run with no record was recorded as a check, so drift is being " +
			"invented from a run nobody can prove was a check")
	}
	drift, err := store.DriftStatus(ctx)
	if err != nil {
		t.Fatalf("DriftStatus() error = %v", err)
	}
	if len(drift) != 0 {
		t.Errorf("DriftStatus() reported %+v from a summary whose run is gone", drift)
	}
	// It is still visible to fleet health, which is what keeps the two views of one fleet
	// reconciling after retention runs.
	health, err := store.FleetHealth(ctx, 10)
	if err != nil {
		t.Fatalf("FleetHealth() error = %v", err)
	}
	if len(health) != 1 || health[0].Host != "web01" {
		t.Errorf("FleetHealth() = %+v, want the host the purged run touched", health)
	}
}

// TestWindowedFleetReadsClampANonPositiveWindow pins the floor every windowed read applies. A
// window of zero would produce a ROW_NUMBER filter that matches nothing, so a caller passing an
// unset window would get an empty fleet page rather than the most recent run per host, and read it
// as a fleet with no history.
func TestWindowedFleetReadsClampANonPositiveWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store, &run.Run{ID: "run_1", Status: run.StatusRunning, CreatedAt: baseTime})
	if err := store.SaveHostSummary(ctx, "run_1", []run.HostSummary{
		{Host: "web01", OK: 2, Worst: "ok", DurationSeconds: 4, RanAt: baseTime},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	if err := store.SaveTaskSummary(ctx, "run_1", []run.TaskSummary{
		{Task: "install", Seconds: 3, RanAt: baseTime},
	}); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}

	for testNum, window := range []int{0, -1, -1000} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			health, err := store.FleetHealth(ctx, window)
			if err != nil || len(health) != 1 {
				t.Errorf("FleetHealth(%d) = (%d hosts, %v), want the one host clamped in",
					window, len(health), err)
			}
			trends, err := store.TaskTrends(ctx, window)
			if err != nil || len(trends) != 1 {
				t.Errorf("TaskTrends(%d) = (%d tasks, %v), want the one task clamped in",
					window, len(trends), err)
			}
			costs, err := store.HostCosts(ctx, window)
			if err != nil || len(costs) != 1 {
				t.Errorf("HostCosts(%d) = (%v, %v), want the one host clamped in", window, costs, err)
			}
			history, err := store.HostHistory(ctx, "web01", window)
			if err != nil || len(history) != 1 {
				t.Errorf("HostHistory(%d) = (%d rows, %v), want one row clamped in", window,
					len(history), err)
			}
		})
	}
}

// TestSubSecondSummaryOrderIsChronological pins the ordering expression every fleet read uses.
// Stored times keep RFC 3339's trimming, so a whole second has no fractional part and its trailing
// Z outranks the dot of a later instant in the same second. Sorting the raw column therefore puts a
// later run ahead of an earlier one, which reorders host history and moves the wrong rows inside
// every window function built on it.
func TestSubSecondSummaryOrderIsChronological(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	// Four instants inside one second, written in a shuffled order, including the two shapes that
	// invert under a raw text sort: a whole second, and one fraction that is a prefix of another.
	second := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	instants := []struct {
		ID string
		At time.Time
	}{
		{"run_c", second.Add(500 * time.Millisecond)},
		{"run_a", second},
		{"run_d", second.Add(500*time.Millisecond + 1*time.Microsecond)},
		{"run_b", second.Add(100 * time.Microsecond)},
	}
	for _, in := range instants {
		saveRuns(t, store, &run.Run{ID: in.ID, Status: run.StatusRunning, CreatedAt: in.At})
		if err := store.SaveHostSummary(ctx, in.ID, []run.HostSummary{
			{Host: "web01", OK: 1, Worst: "ok", RanAt: in.At},
		}); err != nil {
			t.Fatalf("SaveHostSummary(%s) error = %v", in.ID, err)
		}
	}

	history, err := store.HostHistory(ctx, "web01", 10)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	got := make([]string, len(history))
	for i, h := range history {
		got[i] = h.RunID
	}
	want := []string{"run_d", "run_c", "run_b", "run_a"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("host history inside one second (-newest first +got):\n%s", diff)
	}

	// The same ordering decides which row the drift and health windows keep, so the newest run has
	// to be the one those views report.
	health, err := store.FleetHealth(ctx, 1)
	if err != nil {
		t.Fatalf("FleetHealth() error = %v", err)
	}
	if len(health) != 1 || len(health[0].RecentRuns) != 1 || health[0].RecentRuns[0] != "run_d" {
		t.Errorf("FleetHealth(window 1) kept %+v, want the newest run in the second", health)
	}
}

// TestSaveHostFactsSkipsWhatItCannotRecord pins the two shapes host facts arrive in that must not
// become rows. A host with no name has nothing to key on, and a host with no facts is a gather that
// produced nothing, which must not overwrite the facts a real gather stored earlier.
func TestSaveHostFactsSkipsWhatItCannotRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store, &run.Run{ID: "run_1", Status: run.StatusRunning, CreatedAt: baseTime})
	if err := store.SaveHostFacts(ctx, "run_1", []run.HostFacts{
		{Host: "web01", Facts: map[string]string{"os": "linux"}, GatheredAt: baseTime},
	}); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}

	// An empty batch is not an error and writes nothing.
	if err := store.SaveHostFacts(ctx, "run_1", nil); err != nil {
		t.Errorf("SaveHostFacts(nil) = %v, want a no-op", err)
	}
	// A nameless host and a factless host are both skipped rather than stored or erasing anything.
	if err := store.SaveHostFacts(ctx, "run_2", []run.HostFacts{
		{Host: "", Facts: map[string]string{"os": "linux"}},
		{Host: "web01", Facts: nil},
		{Host: "web01", Facts: map[string]string{}},
	}); err != nil {
		t.Errorf("SaveHostFacts(unusable rows) = %v, want them skipped quietly", err)
	}
	got, err := store.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if got.RunID != "run_1" || got.Facts["os"] != "linux" {
		t.Errorf("stored facts = %+v, want the real gather untouched by the empty ones", got)
	}
	if _, err := store.HostFactsFor(ctx, ""); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("HostFactsFor(nameless host) = %v, want ErrNotFound", err)
	}

	// A newer gather replaces the stored one, since the newest gather is the truth about a host.
	later := baseTime.Add(time.Hour)
	saveRuns(t, store, &run.Run{ID: "run_2", Status: run.StatusRunning, CreatedAt: later})
	if err := store.SaveHostFacts(ctx, "run_2", []run.HostFacts{
		{Host: "web01", Facts: map[string]string{"os": "linux", "kernel": "6.9"}, GatheredAt: later},
	}); err != nil {
		t.Fatalf("SaveHostFacts(newer) error = %v", err)
	}
	got, err = store.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if got.RunID != "run_2" || got.Facts["kernel"] != "6.9" || !got.GatheredAt.Equal(later) {
		t.Errorf("stored facts = %+v, want the newer gather to win", got)
	}
}

// TestSaveHostFactsStampsAGatherThatCarriesNoTime pins the fallback for a report with no timestamp.
// The gathered time is what the facts view sorts and ages by, so leaving it at the zero value would
// date every such gather to the year one and make it look permanently stale.
func TestSaveHostFactsStampsAGatherThatCarriesNoTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	before := time.Now()
	saveRuns(t, store, &run.Run{ID: "run_1", Status: run.StatusRunning, CreatedAt: baseTime})
	if err := store.SaveHostFacts(ctx, "run_1", []run.HostFacts{
		{Host: "web01", Facts: map[string]string{"os": "linux"}},
	}); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}
	got, err := store.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if got.GatheredAt.Before(before.Add(-time.Minute)) || got.GatheredAt.IsZero() {
		t.Errorf("GatheredAt = %v, want the gather stamped with the clock, not left at zero",
			got.GatheredAt)
	}
}

// TestWorkersOnlyListLeasesInsideTheWindow pins the bound on the worker listing. A terminal run
// keeps the last lease that held it, so without the window every executor that ever touched the
// install would stay in the list forever and the page would grow with history rather than showing
// who is working now.
func TestWorkersOnlyListLeasesInsideTheWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	recent := time.Now().Add(-time.Hour)
	ancient := time.Now().Add(-2 * run.WorkerWindow)
	saveRuns(t, store,
		&run.Run{ID: "run_recent", Status: run.StatusRunning, CreatedAt: baseTime,
			ClaimedBy: "worker-live", ClaimedAt: &recent},
		&run.Run{ID: "run_done", Status: run.StatusSucceeded, CreatedAt: baseTime,
			ClaimedBy: "worker-live", ClaimedAt: &recent},
		&run.Run{ID: "run_failed", Status: run.StatusFailed, CreatedAt: baseTime,
			ClaimedBy: "worker-live", ClaimedAt: &recent},
		&run.Run{ID: "run_ancient", Status: run.StatusSucceeded, CreatedAt: baseTime,
			ClaimedBy: "worker-retired", ClaimedAt: &ancient},
		&run.Run{ID: "run_unclaimed", Status: run.StatusPending, CreatedAt: baseTime},
	)

	got, err := store.Workers(ctx)
	if err != nil {
		t.Fatalf("Workers() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Workers() = %+v, want only the executor seen inside the window", got)
	}
	w := got[0]
	if w.Owner != "worker-live" {
		t.Errorf("Workers() named %q, want worker-live", w.Owner)
	}
	if w.Active != 1 || w.Completed != 1 || w.Failed != 1 {
		t.Errorf("Workers() counts = active %d, completed %d, failed %d, want 1/1/1",
			w.Active, w.Completed, w.Failed)
	}
}

// TestPurgeKeepsRunsNobodyHasDecidedOn pins the retention predicate as a set of terminal statuses
// rather than as "not pending or running". Stated the loose way, pending_approval counted as
// finished and retention deleted the runs that were waiting for an approver, which is the one class
// of run whose deletion nobody can undo and nobody would notice until the approval arrived.
func TestPurgeKeepsRunsNobodyHasDecidedOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	old := baseTime.Add(-30 * 24 * time.Hour)
	statuses := []run.Status{
		run.StatusPending, run.StatusRunning, run.StatusPendingApproval,
		run.StatusSucceeded, run.StatusFailed, run.StatusCanceled, run.StatusInterrupted,
		run.StatusRejected,
	}
	var wantKept []string
	for _, s := range statuses {
		id := fmt.Sprintf("run_%s", s)
		// Events are written while the run is alive, since a terminal run refuses them, then the
		// run is moved to the status retention will judge it by.
		saveRuns(t, store, &run.Run{ID: id, Status: run.StatusRunning, CreatedAt: old})
		if err := store.AppendEvents(ctx, id,
			[]event.Event{{Type: event.TypePlayStart, Time: old, Play: "p"}}); err != nil {
			t.Fatalf("AppendEvents(%s) error = %v", id, err)
		}
		saveRuns(t, store, &run.Run{ID: id, Status: s, CreatedAt: old})
		if !s.Terminal() {
			wantKept = append(wantKept, id)
		}
	}

	// Trimming events first: only the terminal runs lose theirs.
	trimmed, err := store.PurgeEventsBefore(ctx, baseTime)
	if err != nil {
		t.Fatalf("PurgeEventsBefore() error = %v", err)
	}
	if trimmed != len(statuses)-len(wantKept) {
		t.Errorf("PurgeEventsBefore() trimmed %d runs, want the %d terminal ones", trimmed,
			len(statuses)-len(wantKept))
	}
	for _, id := range wantKept {
		events, err := store.Events(ctx, id)
		if err != nil {
			t.Fatalf("Events(%s) error = %v", id, err)
		}
		if len(events) != 1 {
			t.Errorf("%s lost its events to retention while nobody had decided on it", id)
		}
	}

	deleted, err := store.PurgeRunsBefore(ctx, baseTime)
	if err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	if deleted != len(statuses)-len(wantKept) {
		t.Errorf("PurgeRunsBefore() deleted %d runs, want the %d terminal ones", deleted,
			len(statuses)-len(wantKept))
	}
	for _, id := range wantKept {
		if _, err := store.Get(ctx, id); err != nil {
			t.Errorf("%s was deleted by retention: %v", id, err)
		}
	}
	for _, s := range statuses {
		if !s.Terminal() {
			continue
		}
		id := fmt.Sprintf("run_%s", s)
		if _, err := store.Get(ctx, id); !errors.Is(err, run.ErrNotFound) {
			t.Errorf("the terminal run %s survived retention: %v", id, err)
		}
	}
}

// TestPurgeKeepsRunsInsideTheCutoff pins the other boundary. Retention deletes what is older than
// the cutoff, so a run created exactly at it is inside the retained window and has to stay: a
// comparison one character off deletes a day of history on every sweep.
func TestPurgeKeepsRunsInsideTheCutoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store,
		&run.Run{ID: "run_before", Status: run.StatusSucceeded,
			CreatedAt: baseTime.Add(-time.Second)},
		&run.Run{ID: "run_at", Status: run.StatusSucceeded, CreatedAt: baseTime},
		&run.Run{ID: "run_after", Status: run.StatusSucceeded, CreatedAt: baseTime.Add(time.Second)},
	)
	deleted, err := store.PurgeRunsBefore(ctx, baseTime)
	if err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	if deleted != 1 {
		t.Errorf("PurgeRunsBefore() deleted %d runs, want only the one strictly older", deleted)
	}
	for _, id := range []string{"run_at", "run_after"} {
		if _, err := store.Get(ctx, id); err != nil {
			t.Errorf("%s was deleted although it is inside the retained window: %v", id, err)
		}
	}
}

// TestPurgeSurvivesMoreChildRowsThanOneBatchHolds pins the batching loop. Retention deletes child
// rows in bounded batches so the first sweep on a mature database never holds the single writer for
// minutes on one statement, which would time out every worker heartbeat in flight. The loop exits
// when a batch comes back short, so a backlog larger than one batch is the case that proves it
// neither stops early nor spins.
func TestPurgeSurvivesMoreChildRowsThanOneBatchHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	old := baseTime.Add(-30 * 24 * time.Hour)
	saveRuns(t, store, &run.Run{ID: "run_1", Status: run.StatusRunning, CreatedAt: old})
	// More chunks than the 2000-row batch, so the delete loop has to run more than once.
	const chunks = 2500
	for i := 0; i < chunks; i++ {
		if err := store.AppendLog(ctx, "run_1", []byte("line\n")); err != nil {
			t.Fatalf("AppendLog(%d) error = %v", i, err)
		}
	}
	saveRuns(t, store, &run.Run{ID: "run_1", Status: run.StatusSucceeded, CreatedAt: old})

	trimmed, err := store.PurgeEventsBefore(ctx, baseTime)
	if err != nil {
		t.Fatalf("PurgeEventsBefore() error = %v", err)
	}
	if trimmed != 1 {
		t.Errorf("PurgeEventsBefore() trimmed %d runs, want the one that held output", trimmed)
	}
	body, err := store.Log(ctx, "run_1")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if len(body) != 0 {
		t.Errorf("%d bytes of output survived the batched delete, so the loop stopped early",
			len(body))
	}
	// The run itself is kept: trimming output is not deleting history.
	if _, err := store.Get(ctx, "run_1"); err != nil {
		t.Errorf("trimming output deleted the run: %v", err)
	}
}

// TestTrimSummariesKeepsTheNewestPerKey pins what summary retention bounds and what it must never
// drop. The trim is per host and per task, so a busy host cannot push a quiet host's history out,
// and a keep of zero would erase the fleet view entirely rather than keeping one row.
func TestTrimSummariesKeepsTheNewestPerKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("run_%02d", i)
		at := baseTime.Add(time.Duration(i) * time.Minute)
		saveRuns(t, store, &run.Run{ID: id, Status: run.StatusRunning, CreatedAt: at})
		hosts := []run.HostSummary{{Host: "busy", OK: 1, Worst: "ok", RanAt: at}}
		if i == 0 {
			hosts = append(hosts, run.HostSummary{Host: "quiet", OK: 1, Worst: "ok", RanAt: at})
		}
		if err := store.SaveHostSummary(ctx, id, hosts); err != nil {
			t.Fatalf("SaveHostSummary(%s) error = %v", id, err)
		}
		if err := store.SaveTaskSummary(ctx, id, []run.TaskSummary{
			{Task: "install", Seconds: float64(i), RanAt: at},
		}); err != nil {
			t.Fatalf("SaveTaskSummary(%s) error = %v", id, err)
		}
	}

	deleted, err := store.TrimSummaries(ctx, 2)
	if err != nil {
		t.Fatalf("TrimSummaries() error = %v", err)
	}
	// Four excess host rows for the busy host and four excess task rows.
	if deleted != 8 {
		t.Errorf("TrimSummaries(2) deleted %d rows, want 8", deleted)
	}
	busy, err := store.HostHistory(ctx, "busy", 10)
	if err != nil {
		t.Fatalf("HostHistory(busy) error = %v", err)
	}
	if len(busy) != 2 || busy[0].RunID != "run_05" || busy[1].RunID != "run_04" {
		t.Errorf("busy host history = %+v, want the two newest rows kept", busy)
	}
	quiet, err := store.HostHistory(ctx, "quiet", 10)
	if err != nil {
		t.Fatalf("HostHistory(quiet) error = %v", err)
	}
	if len(quiet) != 1 {
		t.Errorf("quiet host history = %+v, want its one row untouched by the busy host's trim",
			quiet)
	}

	// A keep of zero or less is clamped to one rather than clearing the fleet view.
	for testNum, keep := range []int{0, -5} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			if _, err := store.TrimSummaries(ctx, keep); err != nil {
				t.Fatalf("TrimSummaries(%d) error = %v", keep, err)
			}
			left, err := store.HostHistory(ctx, "busy", 10)
			if err != nil {
				t.Fatalf("HostHistory() error = %v", err)
			}
			if len(left) != 1 {
				t.Errorf("TrimSummaries(%d) left %d rows for a host, want exactly one", keep,
					len(left))
			}
		})
	}
}

// TestEmptySummaryBatchesAreNoOps pins that a report carrying no rows writes nothing and reports
// nothing. A relayed report is split across batches and the last one is routinely empty, so
// treating an empty batch as an error would make every completed relay report look like a failure.
func TestEmptySummaryBatchesAreNoOps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()
	appender := summaryAppender(t, store)

	saveRuns(t, store, &run.Run{ID: "run_1", Status: run.StatusRunning, CreatedAt: baseTime})
	if err := store.SaveHostSummary(ctx, "run_1", []run.HostSummary{
		{Host: "web01", OK: 1, Worst: "ok", RanAt: baseTime},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	if err := store.SaveTaskSummary(ctx, "run_1", []run.TaskSummary{
		{Task: "install", Seconds: 1, RanAt: baseTime},
	}); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}

	for testNum, batch := range []struct {
		Name string
		Call func() error
	}{{ // Test 0: An empty host batch.
		Name: "host nil",
		Call: func() error { return appender.AppendHostSummary(ctx, "run_1", nil) },
	}, { // Test 1: A host batch that is present but has no rows.
		Name: "host empty",
		Call: func() error {
			return appender.AppendHostSummary(ctx, "run_1", []run.HostSummary{})
		},
	}, { // Test 2: An empty task batch.
		Name: "task nil",
		Call: func() error { return appender.AppendTaskSummary(ctx, "run_1", nil) },
	}, { // Test 3: A task batch that is present but has no rows.
		Name: "task empty",
		Call: func() error {
			return appender.AppendTaskSummary(ctx, "run_1", []run.TaskSummary{})
		},
	}} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			if err := batch.Call(); err != nil {
				t.Errorf("%s = %v, want a no-op", batch.Name, err)
			}
		})
	}

	hosts, err := store.RunHostSummaries(ctx, "run_1")
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if len(hosts) != 1 || hosts[0].Host != "web01" {
		t.Errorf("host summaries = %+v, want the stored row untouched by the empty batches", hosts)
	}
	tasks, err := store.RunTaskSummaries(ctx, "run_1")
	if err != nil {
		t.Fatalf("RunTaskSummaries() error = %v", err)
	}
	if len(tasks) != 1 || tasks[0].Task != "install" {
		t.Errorf("task summaries = %+v, want the stored row untouched", tasks)
	}
}

// TestStreamingCursorsHonorTheirBatchLimit pins the bound on a live tail. Both readers cap the
// batch so a client following a chatty run pulls bounded pages rather than the whole log on every
// poll, and paging by the last sequence has to walk the whole thing without skipping or repeating.
func TestStreamingCursorsHonorTheirBatchLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	const total = 7
	saveRuns(t, store, &run.Run{ID: "run_1", Status: run.StatusRunning, CreatedAt: baseTime})
	for i := 0; i < total; i++ {
		if err := store.AppendLog(ctx, "run_1", []byte(fmt.Sprintf("line %d\n", i))); err != nil {
			t.Fatalf("AppendLog(%d) error = %v", i, err)
		}
		if err := store.AppendEvents(ctx, "run_1", []event.Event{
			{Type: event.TypeTaskStart, Time: baseTime, Task: fmt.Sprintf("t%d", i)},
		}); err != nil {
			t.Fatalf("AppendEvents(%d) error = %v", i, err)
		}
	}

	tests := []struct {
		Name      string
		Limit     int
		WantCount int
	}{{ // Test 0: A limit of one is a valid page.
		Name: "one", Limit: 1, WantCount: 1,
	}, { // Test 1: A limit smaller than the backlog caps the page.
		Name: "partial", Limit: 3, WantCount: 3,
	}, { // Test 2: A limit larger than the backlog returns what there is.
		Name: "over", Limit: 100, WantCount: total,
	}, { // Test 3: Zero means no cap, so the whole backlog comes back.
		Name: "zero", Limit: 0, WantCount: total,
	}, { // Test 4: A negative limit is treated as no cap, not as an empty page.
		Name: "negative", Limit: -1, WantCount: total,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			chunks, err := store.LogAfter(ctx, "run_1", 0, test.Limit)
			if err != nil {
				t.Fatalf("LogAfter(%s) error = %v", test.Name, err)
			}
			if len(chunks) != test.WantCount {
				t.Errorf("LogAfter(limit %d) returned %d chunks, want %d", test.Limit,
					len(chunks), test.WantCount)
			}
			events, err := store.EventsAfter(ctx, "run_1", 0, test.Limit)
			if err != nil {
				t.Fatalf("EventsAfter(%s) error = %v", test.Name, err)
			}
			if len(events) != test.WantCount {
				t.Errorf("EventsAfter(limit %d) returned %d events, want %d", test.Limit,
					len(events), test.WantCount)
			}
		})
	}

	// Paging with a limit walks the whole log exactly once.
	var (
		cursor int64
		seen   []string
	)
	for {
		batch, err := store.LogAfter(ctx, "run_1", cursor, 2)
		if err != nil {
			t.Fatalf("LogAfter(page) error = %v", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, c := range batch {
			if c.Seq <= cursor {
				t.Fatalf("a page returned sequence %d at or before the cursor %d", c.Seq, cursor)
			}
			cursor = c.Seq
			seen = append(seen, string(c.Data))
		}
	}
	if len(seen) != total {
		t.Errorf("paging by cursor saw %d chunks, want %d", len(seen), total)
	}
	for i, line := range seen {
		if line != fmt.Sprintf("line %d\n", i) {
			t.Errorf("chunk %d read %q, want it in order", i, line)
		}
	}
}
