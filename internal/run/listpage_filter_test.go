package run

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
)

// filterBase is the instant the filter fixtures are laid out around.
var filterBase = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// seedFilterRuns stores a small fleet of runs whose fields differ one at a time, so a filter that
// reads the wrong field shows up as the wrong run coming back rather than as no runs at all.
func seedFilterRuns(t *testing.T) Store {
	t.Helper()
	ctx := context.Background()
	store := NewMemStore()
	runs := []*Run{{
		ID: "run_a", Playbook: "deploy.yml", Inventory: "prod.ini", Status: StatusSucceeded,
		Tool: "", CreatedAt: filterBase, Source: "template", SourceID: "tpl_1", Actor: "alice",
		ClaimedBy: "worker-1", HeldByPolicy: "prod gate",
		Labels: map[string]string{"env": "prod"},
	}, {
		ID: "run_b", Playbook: "rollback.yml", Inventory: "staging.ini", Status: StatusFailed,
		Tool: ToolBash, Command: "echo hi", CreatedAt: filterBase.Add(time.Minute),
		Source: "schedule", SourceID: "sch_1", Actor: "bob", ClaimedBy: "worker-2",
		Labels: map[string]string{"env": "staging"},
	}, {
		ID: "run_c", Playbook: "infra.yml", Status: StatusRunning, Tool: ToolTerraform,
		Command: "/infra", CreatedAt: filterBase.Add(2 * time.Minute), Source: "api",
		StepName: "provision", Labels: map[string]string{"team": "core"},
	}}
	for _, r := range runs {
		saveRun(t, store, r)
	}
	if err := store.SaveHostSummary(ctx, "run_c", []HostSummary{
		{Host: "web01", Worst: "ok", RanAt: filterBase},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	return store
}

// TestListPageFiltersReadTheFieldTheyName pins each listing filter against a fleet where only one
// run matches, so a filter reading a neighboring field returns the wrong run rather than nothing.
//
// The runs view is how an operator finds a change after the fact, and these filters are what an
// incident review narrows with. A filter that quietly matched too much would show a reviewer runs
// that are not theirs; one that matched too little would hide the run they came to find.
//
//nolint:funlen // Test function.
func TestListPageFiltersReadTheFieldTheyName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Filter  ListFilter
		WantIDs []string
	}{{ // Test 0: The empty filter lists everything newest first.
		Name: "no filter", Filter: ListFilter{}, WantIDs: []string{"run_c", "run_b", "run_a"},
	}, { // Test 1: The oldest-first flag reverses the whole listing.
		Name: "oldest first", Filter: ListFilter{OldestFirst: true},
		WantIDs: []string{"run_a", "run_b", "run_c"},
	}, { // Test 2: The free-text term matches a playbook name.
		Name: "query on playbook", Filter: ListFilter{Query: "rollback"}, WantIDs: []string{"run_b"},
	}, { // Test 3: And it is case insensitive, since nobody types an id in the right case.
		Name: "query folds case", Filter: ListFilter{Query: "RUN_A"}, WantIDs: []string{"run_a"},
	}, { // Test 4: A query is trimmed, so a pasted term with spaces still matches.
		Name: "query is trimmed", Filter: ListFilter{Query: "  infra  "}, WantIDs: []string{"run_c"},
	}, { // Test 5: It reaches the command, which is where a bash run's real content lives.
		Name: "query on command", Filter: ListFilter{Query: "echo hi"}, WantIDs: []string{"run_b"},
	}, { // Test 6: And the step name, so a pipeline step can be found by what it is called.
		Name: "query on step name", Filter: ListFilter{Query: "provision"}, WantIDs: []string{"run_c"},
	}, { // Test 7: And the inventory.
		Name: "query on inventory", Filter: ListFilter{Query: "staging.ini"},
		WantIDs: []string{"run_b"},
	}, { // Test 8: A term nothing carries returns nothing rather than everything.
		Name: "query matches nothing", Filter: ListFilter{Query: "nonesuch"}, WantIDs: nil,
	}, { // Test 9: The status filter is an exact match.
		Name: "status", Filter: ListFilter{Status: string(StatusFailed)}, WantIDs: []string{"run_b"},
	}, { // Test 10: An unknown status matches nothing.
		Name: "unknown status", Filter: ListFilter{Status: "invented"}, WantIDs: nil,
	}, { // Test 11: The tool filter normalizes, so asking for ansible finds the runs stored with an
		// empty tool, which is its historical form and most of the run history on an old install.
		Name: "tool ansible matches empty", Filter: ListFilter{Tool: ToolAnsible},
		WantIDs: []string{"run_a"},
	}, { // Test 12: A named tool matches only itself.
		Name: "tool bash", Filter: ListFilter{Tool: ToolBash}, WantIDs: []string{"run_b"},
	}, { // Test 13: The after bound includes a run created exactly on it.
		Name: "after is inclusive", Filter: ListFilter{After: filterBase.Add(time.Minute)},
		WantIDs: []string{"run_c", "run_b"},
	}, { // Test 14: The before bound excludes a run created exactly on it.
		Name: "before is exclusive", Filter: ListFilter{Before: filterBase.Add(time.Minute)},
		WantIDs: []string{"run_a"},
	}, { // Test 15: The two together name a span.
		Name: "a window", Filter: ListFilter{
			After: filterBase.Add(time.Minute), Before: filterBase.Add(2 * time.Minute)},
		WantIDs: []string{"run_b"},
	}, { // Test 16: A window with the bounds reversed matches nothing rather than everything.
		Name: "reversed window", Filter: ListFilter{
			After: filterBase.Add(2 * time.Minute), Before: filterBase},
		WantIDs: nil,
	}, { // Test 17: The source filter.
		Name: "source", Filter: ListFilter{Source: "schedule"}, WantIDs: []string{"run_b"},
	}, { // Test 18: The specific object behind the source.
		Name: "source id", Filter: ListFilter{SourceID: "tpl_1"}, WantIDs: []string{"run_a"},
	}, { // Test 19: The actor who fired it.
		Name: "actor", Filter: ListFilter{Actor: "bob"}, WantIDs: []string{"run_b"},
	}, { // Test 20: The worker that executed it, so a worker's row opens the work it did.
		Name: "claimed by", Filter: ListFilter{ClaimedBy: "worker-1"}, WantIDs: []string{"run_a"},
	}, { // Test 21: The approval rule that held it, which is a historical record.
		Name: "held by", Filter: ListFilter{HeldBy: "prod gate"}, WantIDs: []string{"run_a"},
	}, { // Test 22: A label pair.
		Name: "label pair", Filter: ListFilter{LabelKey: "env", LabelValue: "prod"},
		WantIDs: []string{"run_a"},
	}, { // Test 23: A label key a run does not carry does not match it against an empty value.
		// Comparing the map lookup directly matched every run with no such label, which neither SQL
		// store does, so the same filter answered differently depending on the backend.
		Name: "label key with an empty value", Filter: ListFilter{LabelKey: "env"}, WantIDs: nil,
	}, { // Test 24: A label key nothing carries matches nothing.
		Name: "unknown label key", Filter: ListFilter{LabelKey: "nope", LabelValue: "x"},
		WantIDs: nil,
	}, { // Test 25: The host filter resolves through the stored summaries.
		Name: "host", Filter: ListFilter{Host: "web01"}, WantIDs: []string{"run_c"},
	}, { // Test 26: A host nothing touched matches nothing.
		Name: "unknown host", Filter: ListFilter{Host: "db99"}, WantIDs: nil,
	}, { // Test 27: Filters combine, and a combination nothing satisfies returns nothing.
		Name:   "combined and contradictory",
		Filter: ListFilter{Status: string(StatusFailed), Tool: ToolTerraform}, WantIDs: nil,
	}, { // Test 28: A combination one run satisfies returns it.
		Name:    "combined and satisfied",
		Filter:  ListFilter{Status: string(StatusFailed), Tool: ToolBash, Actor: "bob"},
		WantIDs: []string{"run_b"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := seedFilterRuns(t)
			got, err := store.ListPage(context.Background(), test.Filter, 0, 0)
			if err != nil {
				t.Fatalf("ListPage() error = %v", err)
			}
			var ids []string
			for _, r := range got {
				ids = append(ids, r.ID)
			}
			if diff := cmp.Diff(test.WantIDs, ids, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ListPage(%+v) (-want +got):\n%s", test.Filter, diff)
			}
		})
	}
}

// TestListPagePagesOnlyWhenItIsGivenALimit pins that an offset with no limit returns everything.
//
// Both SQL stores emit their OFFSET clause inside the same branch as LIMIT, so an unlimited page
// there ignores the offset. Applying it unconditionally here made this store skip rows the other
// two returned, and this is the store every dispatch test runs against, so the divergence would
// have been discovered in a deployment rather than in a test.
func TestListPagePagesOnlyWhenItIsGivenALimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Limit   int
		Offset  int
		WantIDs []string
	}{
		{Name: "no limit, no offset", WantIDs: []string{"run_c", "run_b", "run_a"}}, // Test 0.
		{Name: "no limit with an offset", Offset: 2,
			WantIDs: []string{"run_c", "run_b", "run_a"}}, // Test 1: The offset is ignored.
		{Name: "negative limit", Limit: -1, Offset: 1,
			WantIDs: []string{"run_c", "run_b", "run_a"}}, // Test 2: Same.
		// Test 3: The newest page.
		{Name: "first page", Limit: 2, WantIDs: []string{"run_c", "run_b"}},
		// Test 4: The remainder.
		{Name: "second page", Limit: 2, Offset: 2, WantIDs: []string{"run_a"}},
		// Test 5: An offset at the end is an empty page, not the last one again.
		{Name: "offset at the end", Limit: 2, Offset: 3, WantIDs: nil},
		// Test 6: An offset past the end is likewise empty.
		{Name: "offset past the end", Limit: 2, Offset: 99, WantIDs: nil},
		// Test 7: A limit larger than the fleet returns the fleet.
		{Name: "limit past the end", Limit: 99, WantIDs: []string{"run_c", "run_b", "run_a"}},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := seedFilterRuns(t)
			got, err := store.ListPage(context.Background(), ListFilter{}, test.Limit, test.Offset)
			if err != nil {
				t.Fatalf("ListPage() error = %v", err)
			}
			var ids []string
			for _, r := range got {
				ids = append(ids, r.ID)
			}
			if diff := cmp.Diff(test.WantIDs, ids, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ListPage(limit=%d, offset=%d) (-want +got):\n%s",
					test.Limit, test.Offset, diff)
			}
		})
	}
}

// TestRunsSharingAnInstantOrderStably pins that two runs created in the same nanosecond come back
// in the same order every time, in the listing and in the metrics read.
//
// The runs are gathered from a map, which has no order, so without a tie-break a page refreshed
// twice reorders itself and a paged listing can show one run twice and another not at all.
func TestRunsSharingAnInstantOrderStably(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	for _, id := range []string{"run_a", "run_b", "run_c"} {
		saveRun(t, store, &Run{
			ID: id, Playbook: "site.yml", Status: StatusSucceeded, CreatedAt: filterBase,
		})
	}
	want := []string{"run_c", "run_b", "run_a"}
	for range 5 {
		page, err := store.ListPage(ctx, ListFilter{}, 0, 0)
		if err != nil {
			t.Fatalf("ListPage() error = %v", err)
		}
		var ids []string
		for _, r := range page {
			ids = append(ids, r.ID)
		}
		if diff := cmp.Diff(want, ids, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("listing order (-want +got):\n%s", diff)
		}
		timings, err := store.RunTimings(ctx, 0)
		if err != nil {
			t.Fatalf("RunTimings() error = %v", err)
		}
		ids = nil
		for _, tmg := range timings {
			ids = append(ids, tmg.ID)
		}
		if diff := cmp.Diff(want, ids, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("timing order (-want +got):\n%s", diff)
		}
	}
}

// TestATerminalRunTakesNoMoreWrites pins that a run that has ended refuses the appends and summary
// writes a worker might still be making.
//
// A worker whose lease was reclaimed keeps executing until it notices, so it keeps streaming output
// and events for a run the control node has already settled. Accepting them would append output to
// a finished run's log, which is evidence somebody may already have read and signed, and would let
// a stale summary overwrite the final one the fleet views read.
func TestATerminalRunTakesNoMoreWrites(t *testing.T) {
	t.Parallel()
	for testNum, status := range []Status{
		StatusSucceeded, StatusFailed, StatusCanceled, StatusInterrupted, StatusRejected,
	} {
		t.Run(fmt.Sprintf("test %d %s", testNum, status), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			saveRun(t, store, &Run{
				ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
			})
			if err := store.AppendLog(ctx, "run_1", []byte("real output")); err != nil {
				t.Fatalf("AppendLog() error = %v", err)
			}
			if err := store.AppendEvents(ctx, "run_1",
				[]event.Event{{Type: event.TypeTaskStart, Task: "real"}}); err != nil {
				t.Fatalf("AppendEvents() error = %v", err)
			}
			if err := store.SaveHostSummary(ctx, "run_1", []HostSummary{
				{Host: "web01", Worst: "ok"},
			}); err != nil {
				t.Fatalf("SaveHostSummary() error = %v", err)
			}
			if err := store.SaveTaskSummary(ctx, "run_1", []TaskSummary{
				{Task: "real", Seconds: 1},
			}); err != nil {
				t.Fatalf("SaveTaskSummary() error = %v", err)
			}
			saveRun(t, store, &Run{
				ID: "run_1", Playbook: "site.yml", Status: status, CreatedAt: time.Now(),
			})

			// Every late write is accepted without error and changes nothing, so a stale worker is
			// not made to retry forever against a run that has ended.
			if err := store.AppendLog(ctx, "run_1", []byte("late output")); err != nil {
				t.Errorf("AppendLog() on a %s run error = %v, want nil", status, err)
			}
			if err := store.AppendEvents(ctx, "run_1",
				[]event.Event{{Type: event.TypeTaskStart, Task: "late"}}); err != nil {
				t.Errorf("AppendEvents() on a %s run error = %v, want nil", status, err)
			}
			if err := store.SaveHostSummary(ctx, "run_1", []HostSummary{
				{Host: "web99", Worst: "failed"},
			}); err != nil {
				t.Errorf("SaveHostSummary() on a %s run error = %v, want nil", status, err)
			}
			if err := store.SaveTaskSummary(ctx, "run_1", []TaskSummary{
				{Task: "late", Seconds: 99},
			}); err != nil {
				t.Errorf("SaveTaskSummary() on a %s run error = %v, want nil", status, err)
			}
			appender, ok := store.(SummaryAppender)
			if !ok {
				t.Fatal("the memory store no longer appends summaries")
			}
			if err := appender.AppendHostSummary(ctx, "run_1", []HostSummary{
				{Host: "web99", Worst: "failed"},
			}); err != nil {
				t.Errorf("AppendHostSummary() on a %s run error = %v, want nil", status, err)
			}
			if err := appender.AppendTaskSummary(ctx, "run_1", []TaskSummary{
				{Task: "late", Seconds: 99},
			}); err != nil {
				t.Errorf("AppendTaskSummary() on a %s run error = %v, want nil", status, err)
			}

			logged, err := store.Log(ctx, "run_1")
			if err != nil {
				t.Fatalf("Log() error = %v", err)
			}
			if string(logged) != "real output" {
				t.Errorf("log = %q, want the finished run's own output only", logged)
			}
			events, err := store.Events(ctx, "run_1")
			if err != nil {
				t.Fatalf("Events() error = %v", err)
			}
			if len(events) != 1 || events[0].Task != "real" {
				t.Errorf("events = %+v, want the finished run's own events only", events)
			}
			hosts, err := store.RunHostSummaries(ctx, "run_1")
			if err != nil {
				t.Fatalf("RunHostSummaries() error = %v", err)
			}
			if len(hosts) != 1 || hosts[0].Host != "web01" {
				t.Errorf("host summaries = %+v, want the final ones kept", hosts)
			}
			tasks, err := store.RunTaskSummaries(ctx, "run_1")
			if err != nil {
				t.Fatalf("RunTaskSummaries() error = %v", err)
			}
			if len(tasks) != 1 || tasks[0].Task != "real" {
				t.Errorf("task summaries = %+v, want the final ones kept", tasks)
			}
		})
	}
}

// TestTheLogStopsAtTheCaptureLimitAndSaysSo pins the bound on how much output one run may
// accumulate, and that the run explains itself once it is hit.
//
// Nothing bounded the total. A request carrying output is capped, but the accumulation was not, so
// a run that prints without stopping grew the database until the disk did not take another byte.
// That is reachable by accident from a playbook in a loop, and deliberately by anyone who can start
// a run or hold a worker token. The audit chain lives in the same database, so filling it takes the
// evidence down with the product. The warning matters because a reader who saw the output stop
// would otherwise think the run went quiet.
func TestTheLogStopsAtTheCaptureLimitAndSaysSo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: "site.yml", Status: StatusRunning, CreatedAt: time.Now(),
	})
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = 'x'
	}
	for written := 0; written < MaxLogBytes+2<<20; written += len(chunk) {
		if err := store.AppendLog(ctx, "run_1", chunk); err != nil {
			t.Fatalf("AppendLog() error = %v", err)
		}
	}
	logged, err := store.Log(ctx, "run_1")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if len(logged) > MaxLogBytes+len(chunk) {
		t.Errorf("the log grew to %d bytes, past the %d cap plus one chunk", len(logged),
			MaxLogBytes)
	}
	if len(logged) < MaxLogBytes {
		t.Errorf("the log stopped at %d bytes, short of the %d cap, so real output was lost early",
			len(logged), MaxLogBytes)
	}
	got := getRun(t, store, "run_1")
	if got.Warning != LogTruncatedWarning {
		t.Errorf("warning = %q, want %q: a reader must be told the log is incomplete rather than "+
			"left to think the run went quiet", got.Warning, LogTruncatedWarning)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %s, want the run to carry on: truncating output is a degradation, not "+
			"a failure", got.Status)
	}
}

// TestTheStoreIsSafeUnderConcurrentUse pins that the store survives the shape of traffic it
// actually sees: writers finishing runs and streaming output while readers page the runs view and
// the janitor sweeps.
//
// Implementations must be safe for concurrent use, and the failure mode is not a wrong answer but a
// fatal concurrent map access that takes the whole control node down. Run under the race detector,
// this is what catches a read path that takes no lock.
func TestTheStoreIsSafeUnderConcurrentUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	const runs = 20
	for i := range runs {
		saveRun(t, store, &Run{
			ID: fmt.Sprintf("run_%02d", i), Playbook: "site.yml", Status: StatusRunning,
			CreatedAt: filterBase.Add(time.Duration(i) * time.Second), ClaimedBy: "worker-a",
		})
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	worker := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				fn(i)
			}
		}()
	}
	id := func(i int) string { return fmt.Sprintf("run_%02d", i%runs) }

	worker(func(i int) {
		_ = store.AppendLog(ctx, id(i), []byte("output "))
	})
	worker(func(i int) {
		_ = store.AppendEvents(ctx, id(i), []event.Event{{Type: event.TypeRunnerOK, Host: "web01"}})
	})
	worker(func(i int) {
		_ = store.SaveHostSummary(ctx, id(i), []HostSummary{
			{Host: "web01", OK: i, Worst: "ok", RanAt: filterBase},
		})
	})
	worker(func(i int) {
		_, _ = store.ApplyRunningProgress(ctx, id(i), "worker-a",
			Progress{Warning: "still going", Outputs: map[string]any{"n": i}})
	})
	worker(func(i int) {
		_ = store.Heartbeat(ctx, id(i), "worker-a")
	})
	worker(func(int) {
		_, _ = store.ReclaimStale(ctx, time.Hour)
	})
	worker(func(i int) {
		_, _ = store.ListPage(ctx, ListFilter{Host: "web01"}, 5, i%runs)
	})
	worker(func(int) {
		_, _ = store.RunStatusCounts(ctx)
	})
	worker(func(int) {
		_, _ = store.RunTimings(ctx, 10)
	})
	worker(func(int) {
		_, _ = store.FleetHealth(ctx, 10)
	})
	worker(func(int) {
		_, _ = store.Workers(ctx)
	})
	worker(func(i int) {
		_, _ = store.Get(ctx, id(i))
	})
	worker(func(i int) {
		_, _ = store.LogAfter(ctx, id(i), 0, 0)
	})
	worker(func(i int) {
		_, _ = store.EventsAfter(ctx, id(i), 0, 10)
	})
	worker(func(int) {
		_, _ = store.Claim(ctx, "worker-b", []string{""})
	})

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	// The store is still readable and still consistent afterwards.
	if _, err := store.List(ctx); err != nil {
		t.Fatalf("List() after the concurrent run error = %v", err)
	}
}

// TestPurgeReleasesTheIdempotencyKey pins that deleting a run frees the key it held, so the same
// key can be used again afterwards rather than resolving forever to a run that is gone.
func TestPurgeReleasesTheIdempotencyKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	old := filterBase.Add(-time.Hour)
	saveRun(t, store, &Run{
		ID: "run_old", Playbook: "site.yml", Status: StatusSucceeded, CreatedAt: old,
		IdempotencyKey: "nightly",
	})
	if _, err := store.PurgeRunsBefore(ctx, filterBase); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	saveRun(t, store, &Run{
		ID: "run_new", Playbook: "site.yml", Status: StatusPending, CreatedAt: filterBase,
		IdempotencyKey: "nightly",
	})
	got, err := store.ByIdempotencyKey(ctx, "nightly")
	if err != nil {
		t.Fatalf("ByIdempotencyKey() error = %v", err)
	}
	if got.ID != "run_new" {
		t.Errorf("the key resolves to %q, want the new run: the purged run's claim on it was not "+
			"released", got.ID)
	}
}

// TestListPageStatusFilterIsExactNotAPrefix pins that the status filter compares the whole word.
//
// Every status the fixtures carry is spelled out in full elsewhere in this file, so a comparison
// loosened to a prefix match still answers all of those correctly and only diverges on a partial
// word. The filter reaches ListPage straight from a query string, so a partial word is exactly what
// arrives from a truncated link or a typed URL, and matching it would put runs an operator did not
// ask for into an incident review.
func TestListPageStatusFilterIsExactNotAPrefix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := seedFilterRuns(t)
	tests := []struct {
		Name    string
		Status  string
		WantIDs []string
	}{{ // Test 0: The full word selects its one run.
		Name: "the whole status", Status: string(StatusRunning), WantIDs: []string{"run_c"},
	}, { // Test 1: A prefix of a real status selects nothing.
		Name: "a prefix of running", Status: "run", WantIDs: nil,
	}, { // Test 2: A single leading letter selects nothing either.
		Name: "one letter of succeeded", Status: "s", WantIDs: nil,
	}, { // Test 3: A status no run carries selects nothing.
		Name: "an unknown status", Status: "runningx", WantIDs: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := store.ListPage(ctx, ListFilter{Status: test.Status}, 0, 0)
			if err != nil {
				t.Fatalf("ListPage() error = %v", err)
			}
			ids := make([]string, 0, len(got))
			for _, r := range got {
				ids = append(ids, r.ID)
			}
			if diff := cmp.Diff(test.WantIDs, ids, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("status filter %q returned the wrong runs (-want +got):\n%s",
					test.Status, diff)
			}
		})
	}
}
