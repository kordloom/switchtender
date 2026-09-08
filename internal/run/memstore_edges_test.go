package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
)

// TestStampApprovedSpecTouchesNothingElse pins that recording the digest an approver decided on is
// a narrow write.
//
// The executor recomputes the spec digest before running and refuses a mismatch, so this value is
// what makes an approval release exactly the change that was decided on. Writing it through a full
// Save from a stale snapshot would clobber a claim or a cancel that landed in between, and the run
// would execute under a lease the store had already moved or ignore a cancel it had already
// recorded.
func TestStampApprovedSpecTouchesNothingElse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	claimed := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusPendingApproval, CreatedAt: claimed,
		ClaimedBy: "coordinator-1", ClaimedAt: &claimed, CancelRequested: true,
		HeldByPolicy: "prod gate",
	})
	if err := store.StampApprovedSpec(ctx, "run_1", "sha256:abc"); err != nil {
		t.Fatalf("StampApprovedSpec() error = %v", err)
	}
	got := getRun(t, store, "run_1")
	if got.ApprovedSpecDigest != "sha256:abc" {
		t.Errorf("ApprovedSpecDigest = %q, want the decided digest", got.ApprovedSpecDigest)
	}
	if got.Status != StatusPendingApproval || got.ClaimedBy != "coordinator-1" ||
		!got.CancelRequested || got.HeldByPolicy != "prod gate" {
		t.Errorf("the narrow write disturbed the run: %s held by %q, cancel %v, rule %q",
			got.Status, got.ClaimedBy, got.CancelRequested, got.HeldByPolicy)
	}

	// A run that is gone is reported rather than silently created, so an approval of a purged run
	// cannot look like it landed.
	if err := store.StampApprovedSpec(ctx, "run_gone", "sha256:abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("StampApprovedSpec(missing) error = %v, want ErrNotFound", err)
	}
}

// TestEventAndLogCursorsStartAtZeroAndAdvance pins the two cursors a live stream starts from.
//
// A stream opens by asking where the record currently ends and then sends only what lands after
// that. A cursor that reported something other than the true end would either replay output the
// viewer already has or skip output nobody ever sees, and the same values are the paging cursors
// the API hands to callers.
func TestEventAndLogCursorsStartAtZeroAndAdvance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
	})

	seq, err := store.LastEventSeq(ctx, "run_1")
	if err != nil || seq != 0 {
		t.Fatalf("LastEventSeq() on a run with no events = (%d, %v), want (0, nil)", seq, err)
	}
	logSeq, err := store.LastLogSeq(ctx, "run_1")
	if err != nil || logSeq != 0 {
		t.Fatalf("LastLogSeq() on a run with no log = (%d, %v), want (0, nil)", logSeq, err)
	}

	if err := store.AppendEvents(ctx, "run_1", []event.Event{
		{Type: event.TypeTaskStart, Task: "a"}, {Type: event.TypeRunnerOK, Host: "web01"},
	}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	if err := store.AppendLog(ctx, "run_1", []byte("hello")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if seq, err = store.LastEventSeq(ctx, "run_1"); err != nil || seq != 2 {
		t.Errorf("LastEventSeq() = (%d, %v), want (2, nil)", seq, err)
	}
	if logSeq, err = store.LastLogSeq(ctx, "run_1"); err != nil || logSeq != 5 {
		t.Errorf("LastLogSeq() = (%d, %v), want (5, nil)", logSeq, err)
	}

	// Both report a missing run rather than an empty stream, so a viewer of a purged run is told.
	for _, name := range []string{"events", "log"} {
		if name == "events" {
			_, err = store.LastEventSeq(ctx, "run_gone")
		} else {
			_, err = store.LastLogSeq(ctx, "run_gone")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("last %s seq on a missing run = %v, want ErrNotFound", name, err)
		}
	}
}

// TestLogAfterHandlesEveryCursorPosition pins what a log stream sees for each cursor a caller can
// send, including the ones no honest client produces.
//
// The cursor comes back from the client on every poll, so a negative or out-of-range value is one
// header edit away. Reading from a negative offset would slice a byte buffer out of range, and a
// cursor past the end must return nothing rather than replaying the whole log.
func TestLogAfterHandlesEveryCursorPosition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
	})
	if err := store.AppendLog(ctx, "run_1", []byte("hello world")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	tests := []struct {
		Name     string
		AfterSeq int64
		WantData string
	}{
		{Name: "from the start", AfterSeq: 0, WantData: "hello world"},      // Test 0: Everything.
		{Name: "negative", AfterSeq: -1, WantData: "hello world"},           // Test 1: Clamped to zero.
		{Name: "far negative", AfterSeq: -1 << 40, WantData: "hello world"}, // Test 2: Same.
		{Name: "mid stream", AfterSeq: 6, WantData: "world"},                // Test 3: The tail only.
		{Name: "one before the end", AfterSeq: 10, WantData: "d"},           // Test 4: The last byte.
		{Name: "exactly the end", AfterSeq: 11, WantData: ""},               // Test 5: Nothing new.
		{Name: "past the end", AfterSeq: 999, WantData: ""},                 // Test 6: Nothing new.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			chunks, err := store.LogAfter(ctx, "run_1", test.AfterSeq, 0)
			if err != nil {
				t.Fatalf("LogAfter() error = %v", err)
			}
			var got bytes.Buffer
			for _, c := range chunks {
				got.Write(c.Data)
			}
			if got.String() != test.WantData {
				t.Errorf("LogAfter(after=%d) = %q, want %q", test.AfterSeq, got.String(),
					test.WantData)
			}
			for _, c := range chunks {
				if c.Seq <= test.AfterSeq {
					t.Errorf("a chunk came back with seq %d, at or before the cursor %d, so a "+
						"stream polling with it would loop forever", c.Seq, test.AfterSeq)
				}
			}
		})
	}

	// A chunk returned to the caller is a copy, so editing it cannot rewrite the stored log.
	chunks, err := store.LogAfter(ctx, "run_1", 0, 0)
	if err != nil {
		t.Fatalf("LogAfter() error = %v", err)
	}
	chunks[0].Data[0] = 'H'
	back, err := store.Log(ctx, "run_1")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if string(back) != "hello world" {
		t.Errorf("stored log = %q, want the reader's edit not to reach it", back)
	}
	if _, err := store.LogAfter(ctx, "run_gone", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("LogAfter(missing) error = %v, want ErrNotFound", err)
	}
}

// TestEventsAfterRespectsItsLimit pins that the page cap bounds a read, since the limit is what
// stops one request from serializing a long run's whole event list.
func TestEventsAfterRespectsItsLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
	})
	events := make([]event.Event, 10)
	for i := range events {
		events[i] = event.Event{Type: event.TypeRunnerOK, Host: fmt.Sprintf("web%02d", i)}
	}
	if err := store.AppendEvents(ctx, "run_1", events); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	tests := []struct {
		AfterSeq  int64
		Limit     int
		WantCount int
		WantFirst int64
	}{
		{AfterSeq: 0, Limit: 0, WantCount: 10, WantFirst: 1},   // Test 0: No limit reads them all.
		{AfterSeq: 0, Limit: -1, WantCount: 10, WantFirst: 1},  // Test 1: A negative limit likewise.
		{AfterSeq: 0, Limit: 1, WantCount: 1, WantFirst: 1},    // Test 2: One at a time.
		{AfterSeq: 0, Limit: 100, WantCount: 10, WantFirst: 1}, // Test 3: More than exist.
		{AfterSeq: 5, Limit: 3, WantCount: 3, WantFirst: 6},    // Test 4: Paging from a cursor.
		{AfterSeq: 10, Limit: 3, WantCount: 0},                 // Test 5: Caught up.
		{AfterSeq: 99, Limit: 3, WantCount: 0},                 // Test 6: A cursor past the end.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := store.EventsAfter(ctx, "run_1", test.AfterSeq, test.Limit)
			if err != nil {
				t.Fatalf("EventsAfter() error = %v", err)
			}
			if len(got) != test.WantCount {
				t.Fatalf("EventsAfter(after=%d, limit=%d) returned %d events, want %d",
					test.AfterSeq, test.Limit, len(got), test.WantCount)
			}
			if test.WantCount > 0 && got[0].Seq != test.WantFirst {
				t.Errorf("first seq = %d, want %d", got[0].Seq, test.WantFirst)
			}
		})
	}
}

// TestByIdempotencyKeyIsNotFoundWhenTheIndexIsStale pins that a key pointing at a run that is no
// longer stored answers not found rather than panicking or returning a nil run with no error.
//
// The index outlives its run when the run is saved again without its key and then purged, since the
// purge cleans the index by reading the key off the stored run. A caller resolving a dedupe key has
// to be told there is nothing there, so it submits the run instead of returning a nil to a handler.
func TestByIdempotencyKeyIsNotFoundWhenTheIndexIsStale(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	old := time.Now().Add(-48 * time.Hour)
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusSucceeded, CreatedAt: old,
		IdempotencyKey: "nightly",
	})
	// Saving the run again without its key leaves the index entry behind.
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusSucceeded, CreatedAt: old,
	})
	if _, err := store.PurgeRunsBefore(ctx, time.Now()); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	got, err := store.ByIdempotencyKey(ctx, "nightly")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ByIdempotencyKey() = (%v, %v), want ErrNotFound for a stale index entry", got, err)
	}

	// An empty key is never found, so a keyless submission is never deduped onto anything.
	if _, err := store.ByIdempotencyKey(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("ByIdempotencyKey(\"\") error = %v, want ErrNotFound", err)
	}
}

// TestSaveRefusesAKeyAnotherRunHolds pins the race backstop that lets one of two concurrent
// submissions win a key, and that the winner keeps it.
//
// Without it a retried submission would create a second run doing the same work, which for a
// destructive playbook means doing it twice. The rejection has to name a distinguishable error, not
// a generic one, because the caller's response to it is to fetch and return the winner rather than
// to fail the request.
func TestSaveRefusesAKeyAnotherRunHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_first", Playbook: "site.yml", Status: StatusPending, CreatedAt: time.Now(),
		IdempotencyKey: "nightly",
	})
	err := store.Save(ctx, &Run{
		ID: "run_second", Playbook: "site.yml", Status: StatusPending, CreatedAt: time.Now(),
		IdempotencyKey: "nightly",
	})
	if !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("Save() with a taken key error = %v, want ErrDuplicateKey", err)
	}
	if _, err := store.Get(ctx, "run_second"); !errors.Is(err, ErrNotFound) {
		t.Error("the losing submission was stored anyway, so the work runs twice")
	}
	// The winner may keep saving under its own key as it progresses.
	saveRun(t, store, &Run{
		ID: "run_first", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
		IdempotencyKey: "nightly",
	})
	if got := getRun(t, store, "run_first"); got.Status != StatusRunning {
		t.Errorf("the key holder could not update itself, status = %s", got.Status)
	}
}

// TestStepsOrderTheAttemptsWithinAStep pins that a retried pipeline step reads in the order it
// happened, so the newest attempt is not shown ahead of the try it replaced.
func TestStepsOrderTheAttemptsWithinAStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	parent := "run_pipeline"
	saveRun(t, store, &Run{
		ID: parent, Playbook: "site.yml", Kind: KindPipeline, Status: StatusRunning,
		CreatedAt: time.Now(),
	})
	idx := func(n int) *int { return &n }
	for _, r := range []*Run{
		{ID: "run_b0", StepName: "b", StepIndex: idx(1), Attempt: 0},
		{ID: "run_a1", StepName: "a", StepIndex: idx(0), Attempt: 1},
		{ID: "run_a0", StepName: "a", StepIndex: idx(0), Attempt: 0},
		{ID: "run_x", StepName: "x"},
	} {
		r.Playbook = "site.yml"
		r.Status = StatusSucceeded
		r.CreatedAt = time.Now()
		r.ParentID = &parent
		saveRun(t, store, r)
	}
	steps, err := store.Steps(ctx, parent)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	var got []string
	for _, s := range steps {
		got = append(got, s.ID)
	}
	want := []string{"run_a0", "run_a1", "run_b0", "run_x"}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("step order (-want +got):\n%s", diff)
	}
}

// TestRunTimingsExcludesChildrenAndRespectsItsLimit pins the shape of the read the metrics scrape
// makes on a schedule.
//
// It exists so the histograms do not decode a run's extra vars, steps, labels, and notification
// targets for every run on every scrape. A limit that did not bound the read, or a listing that
// included every shard of every split, would put that cost back and grow it with run history.
func TestRunTimingsExcludesChildrenAndRespectsItsLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	parent := "run_top_2"
	for i := range 3 {
		saveRun(t, store, &Run{
			ID: fmt.Sprintf("run_top_%d", i), Playbook: "site.yml", Status: StatusSucceeded,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	saveRun(t, store, &Run{
		ID: "run_shard", Playbook: "site.yml", Status: StatusSucceeded,
		CreatedAt: base.Add(time.Hour), ParentID: &parent,
	})

	all, err := store.RunTimings(ctx, 0)
	if err != nil {
		t.Fatalf("RunTimings() error = %v", err)
	}
	var ids []string
	for _, tmg := range all {
		ids = append(ids, tmg.ID)
	}
	want := []string{"run_top_2", "run_top_1", "run_top_0"}
	if diff := cmp.Diff(want, ids, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("timings newest first, children excluded (-want +got):\n%s", diff)
	}
	capped, err := store.RunTimings(ctx, 2)
	if err != nil {
		t.Fatalf("RunTimings() error = %v", err)
	}
	if len(capped) != 2 || capped[0].ID != "run_top_2" {
		t.Errorf("RunTimings(2) = %d rows starting at %v, want the two newest", len(capped), capped)
	}
}

// TestRunTimingsDoesNotHandOutTheStoredTimestamps pins that the metrics read returns its own copies
// of a run's start and end instants.
//
// Every other read on this store clones what it returns, precisely so a reader cannot reach back
// into stored state. This one is read on a schedule by the metrics endpoint, which folds the values
// into running totals, so a pointer shared with the store is a scrape able to rewrite a run's
// recorded duration.
func TestRunTimingsDoesNotHandOutTheStoredTimestamps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	started := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusSucceeded, CreatedAt: started,
		StartedAt: &started, EndedAt: &ended,
	})
	timings, err := store.RunTimings(ctx, 10)
	if err != nil {
		t.Fatalf("RunTimings() error = %v", err)
	}
	*timings[0].StartedAt = started.Add(-24 * time.Hour)
	*timings[0].EndedAt = started.Add(24 * time.Hour)

	got := getRun(t, store, "run_1")
	if !got.StartedAt.Equal(started) || !got.EndedAt.Equal(ended) {
		t.Errorf("the stored run now runs %v to %v, want %v to %v",
			got.StartedAt, got.EndedAt, started, ended)
	}
}

// TestRunStatusCountsIgnoresChildren pins that the summary cards count the runs a person submitted
// rather than every shard those runs fanned out into.
//
// A split of sixty shards would otherwise report sixty-one failures for one failed change, which
// makes the cards read as an outage.
func TestRunStatusCountsIgnoresChildren(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	parent := "run_parent"
	saveRun(t, store, &Run{
		ID: parent, Playbook: "site.yml", Kind: KindSplit, Status: StatusFailed,
		CreatedAt: time.Now(),
	})
	for i := range 3 {
		saveRun(t, store, &Run{
			ID: fmt.Sprintf("run_shard_%d", i), Playbook: "site.yml", Status: StatusFailed,
			CreatedAt: time.Now(), ParentID: &parent,
		})
	}
	saveRun(t, store, &Run{
		ID: "run_plain", Playbook: "site.yml", Status: StatusSucceeded, CreatedAt: time.Now(),
	})
	counts, err := store.RunStatusCounts(ctx)
	if err != nil {
		t.Fatalf("RunStatusCounts() error = %v", err)
	}
	want := map[Status]int{StatusFailed: 1, StatusSucceeded: 1}
	if diff := cmp.Diff(want, counts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("status counts (-want +got):\n%s", diff)
	}
}

// TestSummaryWindowsClampToAtLeastOne pins that a window or limit of zero or below is treated as
// one rather than dividing by zero or returning nothing.
//
// The window is caller supplied, straight off a query string, so zero and negative are one URL
// edit away. The averages divide by the number of rows the window admitted, so a window of zero
// that admitted nothing would divide by zero and produce a NaN in a JSON body.
func TestSummaryWindowsClampToAtLeastOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		id := fmt.Sprintf("run_%d", i)
		saveRun(t, store, &Run{
			ID: id, Playbook: "site.yml", Status: StatusRunning,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
		if err := store.SaveHostSummary(ctx, id, []HostSummary{{
			Host: "web01", OK: 1, Worst: "ok", DurationSeconds: float64(i + 1),
			RanAt: base.Add(time.Duration(i) * time.Minute),
		}}); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
		if err := store.SaveTaskSummary(ctx, id, []TaskSummary{{
			Task: "install", Seconds: float64(i + 1),
			RanAt: base.Add(time.Duration(i) * time.Minute),
		}}); err != nil {
			t.Fatalf("SaveTaskSummary() error = %v", err)
		}
	}

	for testNum, window := range []int{0, -1, -1 << 40} {
		t.Run(fmt.Sprintf("test %d window %d", testNum, window), func(t *testing.T) {
			t.Parallel()
			health, err := store.FleetHealth(ctx, window)
			if err != nil {
				t.Fatalf("FleetHealth() error = %v", err)
			}
			if len(health) != 1 || health[0].Total != 1 {
				t.Errorf("FleetHealth(%d) = %+v, want one host over one run", window, health)
			}
			trends, err := store.TaskTrends(ctx, window)
			if err != nil {
				t.Fatalf("TaskTrends() error = %v", err)
			}
			if len(trends) != 1 || trends[0].Runs != 1 || trends[0].AvgSeconds != 3 {
				t.Errorf("TaskTrends(%d) = %+v, want the newest run only", window, trends)
			}
			costs, err := store.HostCosts(ctx, window)
			if err != nil {
				t.Fatalf("HostCosts() error = %v", err)
			}
			if costs["web01"] != 3 {
				t.Errorf("HostCosts(%d)[web01] = %v, want the newest run's 3", window, costs["web01"])
			}
			history, err := store.HostHistory(ctx, "web01", window)
			if err != nil {
				t.Fatalf("HostHistory() error = %v", err)
			}
			if len(history) != 1 {
				t.Errorf("HostHistory(%d) returned %d rows, want one", window, len(history))
			}
		})
	}
}

// TestTrimSummariesNeverEmptiesTheTables pins that a trim count below one keeps one row per host
// and per task rather than deleting everything.
//
// Nothing else removes a summary. They outlive the runs they came from on purpose, so a host's
// outcome history survives run retention, which makes this the only bound on those two tables and
// the only way to lose that history. A misconfigured zero must not be the thing that erases it.
func TestTrimSummariesNeverEmptiesTheTables(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for testNum, keep := range []int{0, -1, -1 << 40} {
		t.Run(fmt.Sprintf("test %d keep %d", testNum, keep), func(t *testing.T) {
			t.Parallel()
			store := NewMemStore()
			for i := range 4 {
				id := fmt.Sprintf("run_%d", i)
				at := base.Add(time.Duration(i) * time.Minute)
				saveRun(t, store, &Run{
					ID: id, Playbook: "site.yml", Status: StatusRunning, CreatedAt: at,
				})
				if err := store.SaveHostSummary(ctx, id, []HostSummary{
					{Host: "web01", Worst: "ok", RanAt: at},
				}); err != nil {
					t.Fatalf("SaveHostSummary() error = %v", err)
				}
				if err := store.SaveTaskSummary(ctx, id, []TaskSummary{
					{Task: "install", Seconds: 1, RanAt: at},
				}); err != nil {
					t.Fatalf("SaveTaskSummary() error = %v", err)
				}
			}
			if _, err := store.TrimSummaries(ctx, keep); err != nil {
				t.Fatalf("TrimSummaries() error = %v", err)
			}
			history, err := store.HostHistory(ctx, "web01", MaxHostHistory)
			if err != nil {
				t.Fatalf("HostHistory() error = %v", err)
			}
			if len(history) != 1 {
				t.Errorf("TrimSummaries(%d) left %d host rows, want exactly one kept", keep,
					len(history))
			}
			if len(history) == 1 && !history[0].RanAt.Equal(base.Add(3*time.Minute)) {
				t.Errorf("the kept row ran at %v, want the newest", history[0].RanAt)
			}
			trends, err := store.TaskTrends(ctx, MaxSummaryWindow)
			if err != nil {
				t.Fatalf("TaskTrends() error = %v", err)
			}
			if len(trends) != 1 || trends[0].Runs != 1 {
				t.Errorf("TrimSummaries(%d) left %+v, want one task row kept", keep, trends)
			}
		})
	}
}

// TestSummariesTieBreakOnRunIDWhenTheInstantIsShared pins the ordering of two summaries recorded at
// the same instant.
//
// They are gathered from a map, which has no order, so without a tie-break the answer changes from
// one call to the next and disagrees with the SQL stores, which break the tie by run id descending.
// A fleet view that reordered itself between refreshes for no reason is the visible symptom; a
// drift reading that picks a different run each time is the one that matters.
func TestSummariesTieBreakOnRunIDWhenTheInstantIsShared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	at := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"run_a", "run_b", "run_c"} {
		saveRun(t, store, &Run{
			ID: id, Playbook: "site.yml", Status: StatusRunning, CreatedAt: at, DryRun: true,
		})
		if err := store.SaveHostSummary(ctx, id, []HostSummary{
			{Host: "web01", Worst: "changed", Changed: 1, RanAt: at},
		}); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
		if err := store.SaveTaskSummary(ctx, id, []TaskSummary{
			{Task: "install", Seconds: 1, RanAt: at},
		}); err != nil {
			t.Fatalf("SaveTaskSummary() error = %v", err)
		}
	}
	for range 5 {
		history, err := store.HostHistory(ctx, "web01", 3)
		if err != nil {
			t.Fatalf("HostHistory() error = %v", err)
		}
		var ids []string
		for _, h := range history {
			ids = append(ids, h.RunID)
		}
		want := []string{"run_c", "run_b", "run_a"}
		if diff := cmp.Diff(want, ids, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("host history order (-want +got):\n%s", diff)
		}
		drift, err := store.DriftStatus(ctx)
		if err != nil {
			t.Fatalf("DriftStatus() error = %v", err)
		}
		if len(drift) != 1 || drift[0].RunID != "run_c" {
			t.Fatalf("drift reported %+v, want the tie broken toward run_c every time", drift)
		}
		trends, err := store.TaskTrends(ctx, 1)
		if err != nil {
			t.Fatalf("TaskTrends() error = %v", err)
		}
		if len(trends) != 1 || !trends[0].LastRun.Equal(at) {
			t.Fatalf("task trends reported %+v, want a stable newest row", trends)
		}
	}
}

// TestEmptySummaryBatchClearsAndAppendKeeps pins the difference between the replacing writes and
// the appending ones when a batch is empty.
//
// Save replaces a run's whole set, so an empty batch means the run has no summaries and the stored
// ones must go. Append writes only the rows it is given, so an empty batch means this continuation
// carried nothing and must leave the accumulated set alone. Getting them the same way round would
// either strand rows nothing can clear or let one empty continuation of a long relay report erase
// every batch that already landed.
func TestEmptySummaryBatchClearsAndAppendKeeps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
	})
	appender, ok := store.(SummaryAppender)
	if !ok {
		t.Fatal("the memory store no longer appends summaries")
	}
	if err := store.SaveHostSummary(ctx, "run_1", []HostSummary{
		{Host: "web01", Worst: "ok"},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	if err := store.SaveTaskSummary(ctx, "run_1", []TaskSummary{
		{Task: "install", Seconds: 1},
	}); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}

	// An empty append leaves what already landed.
	if err := appender.AppendHostSummary(ctx, "run_1", nil); err != nil {
		t.Fatalf("AppendHostSummary() error = %v", err)
	}
	if err := appender.AppendTaskSummary(ctx, "run_1", nil); err != nil {
		t.Fatalf("AppendTaskSummary() error = %v", err)
	}
	hosts, err := store.RunHostSummaries(ctx, "run_1")
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if len(hosts) != 1 {
		t.Errorf("an empty append left %d host rows, want the accumulated one kept", len(hosts))
	}
	tasks, err := store.RunTaskSummaries(ctx, "run_1")
	if err != nil {
		t.Fatalf("RunTaskSummaries() error = %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("an empty append left %d task rows, want the accumulated one kept", len(tasks))
	}

	// An empty save clears the run's set.
	if err := store.SaveHostSummary(ctx, "run_1", nil); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	if err := store.SaveTaskSummary(ctx, "run_1", nil); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}
	if hosts, err = store.RunHostSummaries(ctx, "run_1"); err != nil || len(hosts) != 0 {
		t.Errorf("RunHostSummaries() after an empty save = (%v, %v), want none", hosts, err)
	}
	if tasks, err = store.RunTaskSummaries(ctx, "run_1"); err != nil || len(tasks) != 0 {
		t.Errorf("RunTaskSummaries() after an empty save = (%v, %v), want none", tasks, err)
	}
}

// TestHostFactsIgnoreEmptyGathers pins that a host with no name and a host that reported no facts
// are both skipped rather than stored as an empty record.
//
// The facts come from the target machine, so an unreachable host or a gather that failed produces
// exactly these shapes. Storing them would replace a host's real facts with nothing, since the
// newest gather wins, and the host detail page would go blank after one failed run.
func TestHostFactsIgnoreEmptyGathers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
	})
	if err := store.SaveHostFacts(ctx, "run_1", []HostFacts{
		{Host: "web01", Facts: map[string]string{"distribution": "Debian"}},
	}); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}
	if err := store.SaveHostFacts(ctx, "run_2", []HostFacts{
		{Host: "", Facts: map[string]string{"distribution": "Ubuntu"}},
		{Host: "web01", Facts: nil},
		{Host: "web01", Facts: map[string]string{}},
	}); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}
	got, err := store.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if got.Facts["distribution"] != "Debian" || got.RunID != "run_1" {
		t.Errorf("facts = %+v, want the last real gather kept", got)
	}
	if got.GatheredAt.IsZero() {
		t.Error("a gather with no timestamp was stored without one, so history cannot order it")
	}
	if _, err := store.HostFactsFor(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("HostFactsFor(\"\") error = %v, want ErrNotFound", err)
	}
	if _, err := store.HostFactsFor(ctx, "never-seen"); !errors.Is(err, ErrNotFound) {
		t.Errorf("HostFactsFor(unknown) error = %v, want ErrNotFound", err)
	}

	// The returned facts are the caller's own copy.
	got.Facts["distribution"] = "forged"
	again, err := store.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if again.Facts["distribution"] != "Debian" {
		t.Error("a reader rewrote a host's stored facts through the map it was handed")
	}
}

// TestWorkersCountOutcomesAndBoundTheWindow pins how an executor is described by the leases it
// holds, and that the listing does not reach back through all of run history.
//
// A terminal run keeps its last lease stamp, so without the window the listing would aggregate
// every run ever recorded and report workers that have been gone for months as though they were
// part of the fleet.
func TestWorkersCountOutcomesAndBoundTheWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	now := time.Now()
	recent := now.Add(-time.Minute)
	older := now.Add(-time.Hour)
	ancient := now.Add(-WorkerWindow - time.Hour)
	type seed struct {
		ID        string
		Status    Status
		Owner     string
		ClaimedAt time.Time
	}
	for _, s := range []seed{
		{ID: "run_1", Status: StatusRunning, Owner: "worker-a", ClaimedAt: recent},
		{ID: "run_2", Status: StatusSucceeded, Owner: "worker-a", ClaimedAt: older},
		{ID: "run_3", Status: StatusFailed, Owner: "worker-a", ClaimedAt: older},
		{ID: "run_4", Status: StatusCanceled, Owner: "worker-a", ClaimedAt: older},
		{ID: "run_5", Status: StatusInterrupted, Owner: "worker-a", ClaimedAt: older},
		{ID: "run_6", Status: StatusSucceeded, Owner: "worker-old", ClaimedAt: ancient},
		{ID: "run_7", Status: StatusPending, Owner: "", ClaimedAt: time.Time{}},
	} {
		r := &Run{
			ID: s.ID, Playbook: "site.yml", Status: s.Status, CreatedAt: older,
			ClaimedBy: s.Owner,
		}
		if !s.ClaimedAt.IsZero() {
			at := s.ClaimedAt
			r.ClaimedAt = &at
		}
		saveRun(t, store, r)
	}
	workers, err := store.Workers(ctx)
	if err != nil {
		t.Fatalf("Workers() error = %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("Workers() = %+v, want only the executor seen inside the window", workers)
	}
	got := workers[0]
	if got.Owner != "worker-a" {
		t.Fatalf("Workers()[0].Owner = %q, want worker-a", got.Owner)
	}
	if got.Active != 1 || got.Completed != 1 || got.Failed != 1 {
		t.Errorf("counts = active %d, completed %d, failed %d; want 1/1/1 with canceled and "+
			"interrupted counted as neither", got.Active, got.Completed, got.Failed)
	}
	if !got.LastSeen.Equal(recent) {
		t.Errorf("LastSeen = %v, want the freshest renewal %v", got.LastSeen, recent)
	}
}

// TestPurgeKeepsNonTerminalAndRecentRuns pins that retention only removes what it is allowed to.
//
// Purging a run that has not finished would delete the record an executor is still writing to and
// leave a worker running a change nothing tracks. Purging one inside the retention window would
// remove evidence somebody is entitled to.
func TestPurgeKeepsNonTerminalAndRecentRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	cutoff := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)
	recent := cutoff.Add(time.Hour)
	type seed struct {
		ID        string
		Status    Status
		CreatedAt time.Time
		WantKept  bool
	}
	seeds := []seed{
		{ID: "run_old_done", Status: StatusSucceeded, CreatedAt: old},
		{ID: "run_old_running", Status: StatusRunning, CreatedAt: old, WantKept: true},
		{ID: "run_old_pending", Status: StatusPending, CreatedAt: old, WantKept: true},
		{ID: "run_old_held", Status: StatusPendingApproval, CreatedAt: old, WantKept: true},
		{ID: "run_new_done", Status: StatusSucceeded, CreatedAt: recent, WantKept: true},
		{ID: "run_at_cutoff", Status: StatusSucceeded, CreatedAt: cutoff, WantKept: true},
	}
	for _, s := range seeds {
		// The log is appended while the run is still live, because a terminal run fences the
		// append, then the run is saved into the status the case is about.
		saveRun(t, store, &Run{
			ID: s.ID, Playbook: "site.yml", Status: StatusRunning, CreatedAt: s.CreatedAt,
		})
		if err := store.AppendLog(ctx, s.ID, []byte("output")); err != nil {
			t.Fatalf("AppendLog(%s) error = %v", s.ID, err)
		}
		saveRun(t, store, &Run{
			ID: s.ID, Playbook: "site.yml", Status: s.Status, CreatedAt: s.CreatedAt,
		})
	}
	trimmed, err := store.PurgeEventsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeEventsBefore() error = %v", err)
	}
	if trimmed != 1 {
		t.Errorf("PurgeEventsBefore trimmed %d runs, want only the finished old one", trimmed)
	}
	if data, err := store.Log(ctx, "run_old_done"); err != nil || len(data) != 0 {
		t.Errorf("the trimmed run still has %q, want its output gone but its record kept", data)
	}
	if _, err := store.Get(ctx, "run_old_done"); err != nil {
		t.Errorf("PurgeEventsBefore deleted the run record: %v", err)
	}
	// Trimming again counts nothing, since there is nothing left to remove.
	if trimmed, err = store.PurgeEventsBefore(ctx, cutoff); err != nil || trimmed != 0 {
		t.Errorf("a second trim reported %d runs, want none: nothing was left to remove", trimmed)
	}

	deleted, err := store.PurgeRunsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	if deleted != 1 {
		t.Errorf("PurgeRunsBefore deleted %d runs, want only the finished old one", deleted)
	}
	for _, s := range seeds {
		_, err := store.Get(ctx, s.ID)
		kept := err == nil
		if kept != s.WantKept {
			t.Errorf("%s (%s, created %v) kept = %v, want %v", s.ID, s.Status, s.CreatedAt,
				kept, s.WantKept)
		}
	}
}
