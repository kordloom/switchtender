package sqlitestore_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// baseTime is a fixed instant the ordering tests build from, so nothing depends on the wall clock.
var baseTime = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// saveRuns stores every run or fails the test, so the setup of an ordering case stays one line.
func saveRuns(t *testing.T, store run.Store, runs ...*run.Run) {
	t.Helper()
	ctx := context.Background()
	for _, r := range runs {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
}

// runIDs returns the ids of a run list, for comparing an order without comparing whole rows.
func runIDs(runs []*run.Run) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

// TestByIdempotencyKeyNeverMatchesTheEmptyKey pins the refusal that keeps submission dedup from
// collapsing. Most runs store an empty key, so a lookup that treated empty as a value would match
// the first unkeyed run in the table and hand a caller somebody else's run as their own
// already-accepted submission. The key comparison is also exact, not folded, because a key is
// opaque caller-chosen text.
func TestByIdempotencyKeyNeverMatchesTheEmptyKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	long := strings.Repeat("k", 4096)
	saveRuns(t, store,
		&run.Run{ID: "run_unkeyed_a", Status: run.StatusPending, CreatedAt: baseTime},
		&run.Run{ID: "run_unkeyed_b", Status: run.StatusPending, CreatedAt: baseTime},
		&run.Run{ID: "run_keyed", Status: run.StatusPending, CreatedAt: baseTime,
			IdempotencyKey: "Deploy-2026"},
		&run.Run{ID: "run_unicode", Status: run.StatusPending, CreatedAt: baseTime,
			IdempotencyKey: "клавиша-🔑"},
		&run.Run{ID: "run_long", Status: run.StatusPending, CreatedAt: baseTime,
			IdempotencyKey: long},
	)

	tests := []struct {
		Name   string
		Key    string
		WantID string
		Want   error
	}{{ // Test 0: The empty key never matches, however many unkeyed runs are stored.
		Name: "empty", Key: "", Want: run.ErrNotFound,
	}, { // Test 1: An exact key finds its run.
		Name: "exact", Key: "Deploy-2026", WantID: "run_keyed",
	}, { // Test 2: The comparison is case sensitive, since a key is opaque caller text.
		Name: "wrong case", Key: "deploy-2026", Want: run.ErrNotFound,
	}, { // Test 3: A key that is a prefix of a stored one does not match it.
		Name: "prefix", Key: "Deploy", Want: run.ErrNotFound,
	}, { // Test 4: A LIKE wildcard is a literal here, not a pattern.
		Name: "wildcard", Key: "%", Want: run.ErrNotFound,
	}, { // Test 5: Unicode keys round-trip byte for byte.
		Name: "unicode", Key: "клавиша-🔑", WantID: "run_unicode",
	}, { // Test 6: A very long key is matched whole.
		Name: "long", Key: long, WantID: "run_long",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := store.ByIdempotencyKey(ctx, test.Key)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ByIdempotencyKey(%s) error = %v, want %v", test.Name, err, test.Want)
			}
			if test.Want != nil {
				return
			}
			if got.ID != test.WantID {
				t.Errorf("ByIdempotencyKey(%s) = %s, want %s", test.Name, got.ID, test.WantID)
			}
		})
	}
}

// TestDuplicateIdempotencyKeyIsRefusedAndTheEmptyOneIsNot pins both halves of the partial unique
// index. Two runs claiming one key is the collision dedup exists to catch, and it has to come back
// as ErrDuplicateKey rather than as a generic write failure. The empty key is deliberately outside
// the index, because almost every run carries one and a unique constraint over them would refuse
// the second run an install ever submitted.
func TestDuplicateIdempotencyKeyIsRefusedAndTheEmptyOneIsNot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	first := &run.Run{ID: "run_1", Status: run.StatusPending, CreatedAt: baseTime,
		IdempotencyKey: "same"}
	saveRuns(t, store, first)

	// A different run reaching for the held key is refused by name.
	second := &run.Run{ID: "run_2", Status: run.StatusPending, CreatedAt: baseTime,
		IdempotencyKey: "same"}
	if err := store.Save(ctx, second); !errors.Is(err, run.ErrDuplicateKey) {
		t.Errorf("Save(second holder of the key) = %v, want ErrDuplicateKey", err)
	}
	if _, err := store.Get(ctx, "run_2"); !errors.Is(err, run.ErrNotFound) {
		t.Error("the refused run was written anyway, so the refusal is only a message")
	}

	// The holder can be saved again, which is what every status transition does.
	first.Status = run.StatusRunning
	if err := store.Save(ctx, first); err != nil {
		t.Errorf("re-saving the key's own run = %v, want the update to go through", err)
	}

	// Unkeyed runs are unaffected however many there are.
	for i := 0; i < 3; i++ {
		if err := store.Save(ctx, &run.Run{
			ID: fmt.Sprintf("run_unkeyed_%d", i), Status: run.StatusPending, CreatedAt: baseTime,
		}); err != nil {
			t.Fatalf("Save(unkeyed %d) = %v, want the empty key to be outside the index", i, err)
		}
	}
}

// TestSaveKeepsACancelAStaleSnapshotWouldErase pins the MAX merge on the cancel flag. A worker
// holds an in-memory copy of a run and writes it back whole when the status changes. If a person
// cancels between the read and the write, a plain replace would drop the request on the floor and
// the run would carry on executing on real hosts with nobody able to explain why the cancel did
// nothing.
func TestSaveKeepsACancelAStaleSnapshotWouldErase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	tests := []struct {
		Name       string
		Stored     bool
		Incoming   bool
		WantCancel bool
	}{{ // Test 0: A stale snapshot cannot clear a cancel that landed after it was read.
		Name: "stale write over a cancel", Stored: true, Incoming: false, WantCancel: true,
	}, { // Test 1: A cancel in the write sticks.
		Name: "cancel arrives in the write", Stored: false, Incoming: true, WantCancel: true,
	}, { // Test 2: Both set stays set.
		Name: "both", Stored: true, Incoming: true, WantCancel: true,
	}, { // Test 3: Neither set stays clear, so the merge does not invent a cancel.
		Name: "neither", Stored: false, Incoming: false, WantCancel: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id := fmt.Sprintf("run_%d", testNum)
			saveRuns(t, store, &run.Run{ID: id, Status: run.StatusRunning, CreatedAt: baseTime,
				CancelRequested: test.Stored})
			saveRuns(t, store, &run.Run{ID: id, Status: run.StatusRunning, CreatedAt: baseTime,
				CancelRequested: test.Incoming})
			got, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.CancelRequested != test.WantCancel {
				t.Errorf("%s: cancel_requested = %v, want %v", test.Name, got.CancelRequested,
					test.WantCancel)
			}
		})
	}
}

// TestChildOrderingPutsUnindexedChildrenLast pins the NULL ordering both child listings state
// explicitly. SQLite sorts NULLs first and PostgreSQL sorts them last, so a listing that relied on
// the default returned a different order on each backend for the same rows. Steps are the case that
// actually happens: the steps endpoint is not gated on kind, so calling it on a split parent lists
// shard children, every one of which has no step index at all.
func TestChildOrderingPutsUnindexedChildrenLast(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	parent := "run_parent"
	idx := func(i int) *int { return &i }
	saveRuns(t, store,
		&run.Run{ID: parent, Status: run.StatusRunning, CreatedAt: baseTime, Kind: run.KindSplit},
		&run.Run{ID: "child_none", Status: run.StatusPending, CreatedAt: baseTime, ParentID: &parent},
		&run.Run{ID: "child_2", Status: run.StatusPending, CreatedAt: baseTime, ParentID: &parent,
			ShardIndex: idx(2), StepIndex: idx(2)},
		&run.Run{ID: "child_0", Status: run.StatusPending, CreatedAt: baseTime, ParentID: &parent,
			ShardIndex: idx(0), StepIndex: idx(0)},
		&run.Run{ID: "child_1", Status: run.StatusPending, CreatedAt: baseTime, ParentID: &parent,
			ShardIndex: idx(1), StepIndex: idx(1)},
	)

	want := []string{"child_0", "child_1", "child_2", "child_none"}
	shards, err := store.Shards(ctx, parent)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if diff := cmp.Diff(want, runIDs(shards)); diff != "" {
		t.Errorf("Shards() order (-want +got):\n%s", diff)
	}
	steps, err := store.Steps(ctx, parent)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	if diff := cmp.Diff(want, runIDs(steps)); diff != "" {
		t.Errorf("Steps() order (-want +got):\n%s", diff)
	}

	// A parent with no children lists nothing rather than failing.
	if got, err := store.Shards(ctx, "run_nobody"); err != nil || len(got) != 0 {
		t.Errorf("Shards(unknown parent) = (%d runs, %v), want none and no error", len(got), err)
	}
}

// TestStepsOrderRetriesWithinAStep pins that two attempts at the same step come back oldest attempt
// first. The step listing is what the workflow view renders, and an attempt order that flips makes
// a retry look like it ran before the failure it was retrying.
func TestStepsOrderRetriesWithinAStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	parent := "run_pipeline"
	idx := func(i int) *int { return &i }
	saveRuns(t, store,
		&run.Run{ID: parent, Status: run.StatusRunning, CreatedAt: baseTime, Kind: run.KindPipeline},
		&run.Run{ID: "step0_try1", Status: run.StatusFailed, CreatedAt: baseTime, ParentID: &parent,
			StepIndex: idx(0), Attempt: 1},
		&run.Run{ID: "step0_try0", Status: run.StatusFailed, CreatedAt: baseTime, ParentID: &parent,
			StepIndex: idx(0), Attempt: 0},
		&run.Run{ID: "step1_try0", Status: run.StatusPending, CreatedAt: baseTime, ParentID: &parent,
			StepIndex: idx(1), Attempt: 0},
	)
	steps, err := store.Steps(ctx, parent)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	want := []string{"step0_try0", "step0_try1", "step1_try0"}
	if diff := cmp.Diff(want, runIDs(steps)); diff != "" {
		t.Errorf("Steps() order (-want +got):\n%s", diff)
	}
}

// TestListPageTimeBoundsAreExactlyAsDocumented pins the two window edges. After is inclusive and
// Before is exclusive, so consecutive windows tile the timeline without dropping or double
// counting a run that sits exactly on a boundary. Getting one of them backward is invisible until
// somebody reconciles two adjacent reports and finds a run in both or in neither.
func TestListPageTimeBoundsAreExactlyAsDocumented(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	early := baseTime.Add(-time.Hour)
	late := baseTime.Add(time.Hour)
	saveRuns(t, store,
		&run.Run{ID: "run_early", Status: run.StatusSucceeded, CreatedAt: early},
		&run.Run{ID: "run_edge", Status: run.StatusSucceeded, CreatedAt: baseTime},
		&run.Run{ID: "run_late", Status: run.StatusSucceeded, CreatedAt: late},
	)

	tests := []struct {
		Name    string
		Filter  run.ListFilter
		WantIDs []string
	}{{ // Test 0: After includes a run created at exactly that instant.
		Name: "after is inclusive", Filter: run.ListFilter{After: baseTime},
		WantIDs: []string{"run_late", "run_edge"},
	}, { // Test 1: Before excludes a run created at exactly that instant.
		Name: "before is exclusive", Filter: run.ListFilter{Before: baseTime},
		WantIDs: []string{"run_early"},
	}, { // Test 2: The pair tiles without overlap, so the edge run lands in exactly one window.
		Name:   "half open window",
		Filter: run.ListFilter{After: baseTime, Before: late}, WantIDs: []string{"run_edge"},
	}, { // Test 3: An empty window returns nothing rather than everything.
		Name:   "empty window",
		Filter: run.ListFilter{After: late.Add(time.Hour), Before: late.Add(2 * time.Hour)},
	}, { // Test 4: Oldest first flips the order and nothing else.
		Name: "oldest first", Filter: run.ListFilter{OldestFirst: true},
		WantIDs: []string{"run_early", "run_edge", "run_late"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := store.ListPage(ctx, test.Filter, 0, 0)
			if err != nil {
				t.Fatalf("ListPage(%s) error = %v", test.Name, err)
			}
			if diff := cmp.Diff(test.WantIDs, runIDs(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ListPage(%s) (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestListPageMatchesLabelKeysThatLookLikePaths pins the literal key lookup. Compiling the key into
// a JSON path treats a dot as a step, so an ordinary label like app.tier or k8s.io/name matched
// nothing on SQLite while matching correctly on PostgreSQL, and the list came back empty with no
// error at all: the worst shape a filter bug can take, since it reads as "no runs" rather than as a
// failure.
func TestListPageMatchesLabelKeysThatLookLikePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store,
		&run.Run{ID: "run_dotted", Status: run.StatusSucceeded, CreatedAt: baseTime,
			Labels: map[string]string{"app.tier": "web"}},
		&run.Run{ID: "run_slashed", Status: run.StatusSucceeded, CreatedAt: baseTime,
			Labels: map[string]string{"k8s.io/name": "api"}},
		&run.Run{ID: "run_bracketed", Status: run.StatusSucceeded, CreatedAt: baseTime,
			Labels: map[string]string{"weird[0]": "yes"}},
		&run.Run{ID: "run_unicode", Status: run.StatusSucceeded, CreatedAt: baseTime,
			Labels: map[string]string{"среда": "прод"}},
		&run.Run{ID: "run_empty_value", Status: run.StatusSucceeded, CreatedAt: baseTime,
			Labels: map[string]string{"marker": ""}},
		&run.Run{ID: "run_none", Status: run.StatusSucceeded, CreatedAt: baseTime},
	)

	tests := []struct {
		Name    string
		Key     string
		Value   string
		WantIDs []string
	}{{ // Test 0: A key holding a dot is looked up literally.
		Name: "dotted", Key: "app.tier", Value: "web", WantIDs: []string{"run_dotted"},
	}, { // Test 1: A key holding a dot and a slash, the shape Kubernetes labels take.
		Name: "slashed", Key: "k8s.io/name", Value: "api", WantIDs: []string{"run_slashed"},
	}, { // Test 2: A key holding JSON path brackets is still literal text.
		Name: "bracketed", Key: "weird[0]", Value: "yes", WantIDs: []string{"run_bracketed"},
	}, { // Test 3: Unicode keys and values.
		Name: "unicode", Key: "среда", Value: "прод", WantIDs: []string{"run_unicode"},
	}, { // Test 4: A label whose value is empty is found by asking for the empty value.
		Name: "empty value", Key: "marker", Value: "", WantIDs: []string{"run_empty_value"},
	}, { // Test 5: The right key with the wrong value matches nothing, not the key alone.
		Name: "wrong value", Key: "app.tier", Value: "db",
	}, { // Test 6: A key nothing carries matches nothing.
		Name: "absent key", Key: "nope", Value: "x",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := store.ListPage(ctx,
				run.ListFilter{LabelKey: test.Key, LabelValue: test.Value}, 0, 0)
			if err != nil {
				t.Fatalf("ListPage(%s) error = %v", test.Name, err)
			}
			if diff := cmp.Diff(test.WantIDs, runIDs(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ListPage(label %s) (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestListPageNormalizesTheHistoricalAnsibleTool pins the tool filter's COALESCE. Runs from before
// the tool column was populated store an empty tool and are Ansible runs, so filtering the runs
// view by Ansible has to find them. Trusting the column instead would make an install's whole
// history disappear from its own filter.
func TestListPageNormalizesTheHistoricalAnsibleTool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store,
		&run.Run{ID: "run_historical", Status: run.StatusSucceeded, CreatedAt: baseTime},
		&run.Run{ID: "run_named", Status: run.StatusSucceeded, CreatedAt: baseTime, Tool: "ansible"},
		&run.Run{ID: "run_tf", Status: run.StatusSucceeded, CreatedAt: baseTime, Tool: "terraform"},
	)

	tests := []struct {
		Name    string
		Tool    string
		WantIDs []string
	}{{ // Test 0: Ansible finds both the named form and the historical empty one.
		Name: "ansible", Tool: "ansible", WantIDs: []string{"run_named", "run_historical"},
	}, { // Test 1: Another tool matches only itself.
		Name: "terraform", Tool: "terraform", WantIDs: []string{"run_tf"},
	}, { // Test 2: A tool nothing used matches nothing.
		Name: "unknown", Tool: "cobol",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := store.ListPage(ctx, run.ListFilter{Tool: test.Tool}, 0, 0)
			if err != nil {
				t.Fatalf("ListPage(%s) error = %v", test.Name, err)
			}
			if diff := cmp.Diff(test.WantIDs, runIDs(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ListPage(tool %s) (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestListPageCombinesEveryFilterWithAnd pins that the filters narrow together rather than one of
// them quietly winning. Each clause is appended to the same statement with its own arguments, so a
// clause added in the wrong order or bound to the wrong placeholder produces a result that looks
// plausible and is wrong. A run has to satisfy all of them to come back.
func TestListPageCombinesEveryFilterWithAnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openStore(t)
	store := db.Runs()

	match := &run.Run{
		ID: "run_match", Status: run.StatusRunning, CreatedAt: baseTime, Playbook: "site.yml",
		Tool: "ansible", Source: "schedule", SourceID: "sch_1", Actor: "deploy-bot",
		ClaimedBy: "worker-1", HeldByPolicy: "prod apply",
		Labels: map[string]string{"env": "prod"},
	}
	saveRuns(t, store, match,
		// Each of these differs from the match in exactly one filtered field.
		&run.Run{ID: "run_other_actor", Status: run.StatusRunning, CreatedAt: baseTime,
			Playbook: "site.yml", Tool: "ansible", Source: "schedule", SourceID: "sch_1",
			Actor: "somebody-else", ClaimedBy: "worker-1", HeldByPolicy: "prod apply",
			Labels: map[string]string{"env": "prod"}},
		&run.Run{ID: "run_other_worker", Status: run.StatusRunning, CreatedAt: baseTime,
			Playbook: "site.yml", Tool: "ansible", Source: "schedule", SourceID: "sch_1",
			Actor: "deploy-bot", ClaimedBy: "worker-2", HeldByPolicy: "prod apply",
			Labels: map[string]string{"env": "prod"}},
		&run.Run{ID: "run_other_label", Status: run.StatusRunning, CreatedAt: baseTime,
			Playbook: "site.yml", Tool: "ansible", Source: "schedule", SourceID: "sch_1",
			Actor: "deploy-bot", ClaimedBy: "worker-1", HeldByPolicy: "prod apply",
			Labels: map[string]string{"env": "stg"}},
	)
	if err := store.SaveHostSummary(ctx, "run_match", []run.HostSummary{
		{Host: "web01", OK: 3, Worst: "ok", RanAt: baseTime},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}

	full := run.ListFilter{
		Query: "SITE", Status: string(run.StatusRunning), Tool: "ansible",
		After: baseTime.Add(-time.Minute), Before: baseTime.Add(time.Minute),
		Source: "schedule", SourceID: "sch_1", Actor: "deploy-bot", Host: "web01",
		ClaimedBy: "worker-1", HeldBy: "prod apply", LabelKey: "env", LabelValue: "prod",
	}
	got, err := store.ListPage(ctx, full, 0, 0)
	if err != nil {
		t.Fatalf("ListPage(all filters) error = %v", err)
	}
	if diff := cmp.Diff([]string{"run_match"}, runIDs(got)); diff != "" {
		t.Errorf("every filter together (-want +got):\n%s", diff)
	}

	// Flip one field at a time and the match must disappear, which proves each clause is live
	// rather than being dropped by a build error nobody noticed.
	flips := map[string]func(f *run.ListFilter){
		"status":     func(f *run.ListFilter) { f.Status = string(run.StatusFailed) },
		"tool":       func(f *run.ListFilter) { f.Tool = "terraform" },
		"source":     func(f *run.ListFilter) { f.Source = "api" },
		"source id":  func(f *run.ListFilter) { f.SourceID = "sch_2" },
		"actor":      func(f *run.ListFilter) { f.Actor = "nobody" },
		"host":       func(f *run.ListFilter) { f.Host = "db01" },
		"claimed by": func(f *run.ListFilter) { f.ClaimedBy = "worker-9" },
		"held by":    func(f *run.ListFilter) { f.HeldBy = "some other rule" },
		"label":      func(f *run.ListFilter) { f.LabelValue = "stg" },
		"query":      func(f *run.ListFilter) { f.Query = "nothing-like-this" },
		"after":      func(f *run.ListFilter) { f.After = baseTime.Add(time.Hour) },
		"before":     func(f *run.ListFilter) { f.Before = baseTime.Add(-time.Hour) },
	}
	for name, flip := range flips {
		narrowed := full
		flip(&narrowed)
		hits, err := store.ListPage(ctx, narrowed, 0, 0)
		if err != nil {
			t.Fatalf("ListPage(flipped %s) error = %v", name, err)
		}
		if len(hits) != 0 {
			t.Errorf("flipping %s still returned %v, so that clause is not filtering", name,
				runIDs(hits))
		}
	}
}

// TestListPageLimitAndOffsetPageWithoutGapsOrRepeats pins the paging arithmetic. The runs view
// pages with a limit and an offset over a tie-broken order, so a page boundary that repeats or
// skips a row is what an operator sees as a run that exists on two pages or on none.
func TestListPageLimitAndOffsetPageWithoutGapsOrRepeats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	const total = 7
	for i := 0; i < total; i++ {
		saveRuns(t, store, &run.Run{
			ID: fmt.Sprintf("run_%02d", i), Status: run.StatusSucceeded,
			CreatedAt: baseTime.Add(time.Duration(i) * time.Second),
		})
	}

	tests := []struct {
		Name    string
		Limit   int
		Offset  int
		WantIDs []string
	}{{ // Test 0: No limit returns everything newest first.
		Name: "unlimited", Limit: 0, Offset: 0,
		WantIDs: []string{"run_06", "run_05", "run_04", "run_03", "run_02", "run_01", "run_00"},
	}, { // Test 1: The first page.
		Name: "first page", Limit: 3, WantIDs: []string{"run_06", "run_05", "run_04"},
	}, { // Test 2: The second page continues without repeating.
		Name: "second page", Limit: 3, Offset: 3, WantIDs: []string{"run_03", "run_02", "run_01"},
	}, { // Test 3: The last page is short rather than padded.
		Name: "short last page", Limit: 3, Offset: 6, WantIDs: []string{"run_00"},
	}, { // Test 4: An offset past the end returns nothing.
		Name: "past the end", Limit: 3, Offset: 99,
	}, { // Test 5: A limit of one is a valid page.
		Name: "single", Limit: 1, Offset: 2, WantIDs: []string{"run_04"},
	}, { // Test 6: A negative limit is treated as no limit, not as an empty page.
		Name: "negative limit", Limit: -1, Offset: 0,
		WantIDs: []string{"run_06", "run_05", "run_04", "run_03", "run_02", "run_01", "run_00"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := store.ListPage(ctx, run.ListFilter{}, test.Limit, test.Offset)
			if err != nil {
				t.Fatalf("ListPage(%s) error = %v", test.Name, err)
			}
			if diff := cmp.Diff(test.WantIDs, runIDs(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ListPage(%s) (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestRunSearchIsCaseInsensitiveOverEveryColumnItClaims pins the free-text search. It is what an
// operator types into the runs view, and each column the memory store searches has to be searched
// here too or the same query answers differently depending on which backend is behind it.
func TestRunSearchIsCaseInsensitiveOverEveryColumnItClaims(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store,
		&run.Run{ID: "run_playbook", Status: run.StatusSucceeded, CreatedAt: baseTime,
			Playbook: "Deploy/Site.YML"},
		&run.Run{ID: "run_command", Status: run.StatusSucceeded, CreatedAt: baseTime,
			Tool: "bash", Command: "systemctl Restart Nginx"},
		&run.Run{ID: "run_step", Status: run.StatusSucceeded, CreatedAt: baseTime,
			StepName: "Migrate-Database"},
		&run.Run{ID: "run_inventory", Status: run.StatusSucceeded, CreatedAt: baseTime,
			Inventory: "PROD-hosts.ini"},
	)

	tests := []struct {
		Name    string
		Query   string
		WantIDs []string
	}{{ // Test 0: The playbook column, matched with the opposite case.
		Name: "playbook", Query: "site.yml", WantIDs: []string{"run_playbook"},
	}, { // Test 1: The command column.
		Name: "command", Query: "RESTART", WantIDs: []string{"run_command"},
	}, { // Test 2: The step name column.
		Name: "step name", Query: "migrate", WantIDs: []string{"run_step"},
	}, { // Test 3: The inventory column.
		Name: "inventory", Query: "prod-hosts", WantIDs: []string{"run_inventory"},
	}, { // Test 4: The id column, so pasting a run id finds it.
		Name: "id", Query: "run_step", WantIDs: []string{"run_step"},
	}, { // Test 5: The tool column.
		Name: "tool", Query: "bash", WantIDs: []string{"run_command"},
	}, { // Test 6: The status column.
		Name: "status", Query: "succeeded",
		WantIDs: []string{"run_playbook", "run_command", "run_step", "run_inventory"},
	}, { // Test 7: A term nothing holds matches nothing rather than everything.
		Name: "no match", Query: "definitely-not-here",
	}, { // Test 8: Surrounding whitespace is trimmed, so a pasted term still matches.
		Name: "padded", Query: "  MIGRATE  ", WantIDs: []string{"run_step"},
	}, { // Test 9: A blank term is not a filter, so every run comes back.
		Name: "blank", Query: "   ",
		WantIDs: []string{"run_playbook", "run_command", "run_step", "run_inventory"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := store.ListPage(ctx, run.ListFilter{Query: test.Query}, 0, 0)
			if err != nil {
				t.Fatalf("ListPage(query %s) error = %v", test.Name, err)
			}
			gotIDs := runIDs(got)
			if len(gotIDs) != len(test.WantIDs) {
				t.Fatalf("query %q matched %v, want %v", test.Query, gotIDs, test.WantIDs)
			}
			for _, want := range test.WantIDs {
				found := false
				for _, id := range gotIDs {
					if id == want {
						found = true
					}
				}
				if !found {
					t.Errorf("query %q did not match %s; matched %v", test.Query, want, gotIDs)
				}
			}
		})
	}
}

// TestRunSearchTreatsLikeMetacharactersAsPatterns records what a search containing a LIKE
// metacharacter does today. The term is interpolated into a LIKE pattern without escaping, so a
// percent sign matches every run and an underscore matches any single character. It is recorded
// rather than asserted as correct: an operator pasting a run id like run_0001 is matching a pattern
// they did not write, and a literal search for a percentage in a command finds everything.
func TestRunSearchTreatsLikeMetacharactersAsPatterns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store,
		&run.Run{ID: "runA0001", Status: run.StatusSucceeded, CreatedAt: baseTime,
			Tool: "bash", Command: "echo done"},
		&run.Run{ID: "runB0001", Status: run.StatusSucceeded, CreatedAt: baseTime,
			Tool: "bash", Command: "df -h | grep 50%"},
	)

	// A percent sign is a wildcard, so a literal search for it returns everything rather than the
	// one run whose command contains it.
	pct, err := store.ListPage(ctx, run.ListFilter{Query: "50%"}, 0, 0)
	if err != nil {
		t.Fatalf("ListPage(percent) error = %v", err)
	}
	if len(pct) != 1 {
		t.Logf("searching for %q matched %v: the term is a LIKE pattern, not literal text",
			"50%", runIDs(pct))
	}

	// An underscore matches any single character, so an id-shaped term is a pattern too.
	under, err := store.ListPage(ctx, run.ListFilter{Query: "runA_001"}, 0, 0)
	if err != nil {
		t.Fatalf("ListPage(underscore) error = %v", err)
	}
	if len(under) == 0 {
		t.Error("an underscore term matched nothing at all, which is neither literal nor a pattern")
	}
}

// TestStampApprovedSpecIsNarrowAndReportsAMissingRun pins the write an approval makes. The digest
// records exactly what an approver decided on, and it is stamped while the run may be being claimed
// or canceled by somebody else, so the statement must touch nothing but its own column. A missing
// run has to be an error rather than a silent success, or an approval can be recorded against
// nothing at all.
func TestStampApprovedSpecIsNarrowAndReportsAMissingRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	before := &run.Run{
		ID: "run_held", Status: run.StatusPendingApproval, CreatedAt: baseTime,
		Playbook: "site.yml", Actor: "casey", ActorType: "session", ActorUserID: "user_casey",
		HeldByPolicy: "prod apply", RequireDistinctApprover: true, CancelRequested: true,
		ClaimedBy: "worker-1", Labels: map[string]string{"env": "prod"},
	}
	saveRuns(t, store, before)

	if err := store.StampApprovedSpec(ctx, "run_held", "sha256:abc"); err != nil {
		t.Fatalf("StampApprovedSpec() error = %v", err)
	}
	after, err := store.Get(ctx, "run_held")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if after.ApprovedSpecDigest != "sha256:abc" {
		t.Errorf("ApprovedSpecDigest = %q, want the digest the approver decided on",
			after.ApprovedSpecDigest)
	}
	// Everything else the run carried is untouched, so the stamp cannot clobber a concurrent
	// claim, cancel, or the separation-of-duties requirement it is being checked against.
	stamped := *after
	stamped.ApprovedSpecDigest = ""
	if diff := cmp.Diff(before, &stamped, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the stamp changed columns it does not own (-before +after):\n%s", diff)
	}

	if err := store.StampApprovedSpec(ctx, "run_ghost", "sha256:abc"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("StampApprovedSpec(missing run) = %v, want ErrNotFound", err)
	}
	// An empty digest is a real value here, not a no-op, so clearing one is reported as done.
	if err := store.StampApprovedSpec(ctx, "run_held", ""); err != nil {
		t.Errorf("StampApprovedSpec(empty digest) = %v, want the clear to be accepted", err)
	}
}

// TestTransitionStatusAndClaimRefusesACanceledRun pins the fence that keeps a canceled pipeline
// from executing. Cancel is a flag rather than a status, so a compare-and-swap that looks only at
// the status cannot see one: a pipeline canceled after approval and before its coordinator picked
// it up still read as pending_approval, won the swap, and ran on real hosts. The flag belongs in
// the predicate, not in a check beside it.
func TestTransitionStatusAndClaimRefusesACanceledRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	tests := []struct {
		Name     string
		Status   run.Status
		Cancel   bool
		From     run.Status
		WantMove bool
	}{{ // Test 0: The ordinary case, an approved run a coordinator takes.
		Name: "approved", Status: run.StatusPendingApproval, From: run.StatusPendingApproval,
		WantMove: true,
	}, { // Test 1: The same run with a cancel already requested is refused.
		Name: "approved then canceled", Status: run.StatusPendingApproval, Cancel: true,
		From: run.StatusPendingApproval, WantMove: false,
	}, { // Test 2: A status that does not match the expected one is refused.
		Name: "wrong from", Status: run.StatusPending, From: run.StatusPendingApproval,
		WantMove: false,
	}, { // Test 3: A terminal run cannot be moved back into running.
		Name: "terminal", Status: run.StatusCanceled, From: run.StatusPendingApproval,
		WantMove: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id := fmt.Sprintf("run_%d", testNum)
			saveRuns(t, store, &run.Run{ID: id, Status: test.Status, CreatedAt: baseTime,
				CancelRequested: test.Cancel, Kind: run.KindPipeline})
			moved, err := store.TransitionStatusAndClaim(ctx, id, test.From, run.StatusRunning,
				"coordinator-1")
			if err != nil {
				t.Fatalf("TransitionStatusAndClaim(%s) error = %v", test.Name, err)
			}
			if moved != test.WantMove {
				t.Fatalf("TransitionStatusAndClaim(%s) = %v, want %v", test.Name, moved,
					test.WantMove)
			}
			got, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !test.WantMove {
				if got.Status == run.StatusRunning || got.ClaimedBy != "" {
					t.Errorf("%s: the refused run is %q held by %q, so the fence let it through",
						test.Name, got.Status, got.ClaimedBy)
				}
				return
			}
			if got.Status != run.StatusRunning || got.ClaimedBy != "coordinator-1" {
				t.Errorf("%s: the run is %q held by %q, want running under the coordinator",
					test.Name, got.Status, got.ClaimedBy)
			}
			if got.StartedAt == nil {
				t.Error("the run started with no start time, so it has no duration")
			}
		})
	}
}

// TestTransitionStatusAndClaimNeverMovesAStartBackward pins the COALESCE on started_at. A run
// restarted through the same fence, which a shard retry does, would otherwise have its start time
// rewritten to the later attempt and lose the duration of everything before it.
func TestTransitionStatusAndClaimNeverMovesAStartBackward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	started := baseTime.Add(-time.Hour)
	saveRuns(t, store, &run.Run{
		ID: "run_1", Status: run.StatusPending, CreatedAt: baseTime.Add(-2 * time.Hour),
		StartedAt: &started,
	})
	moved, err := store.TransitionStatusAndClaim(ctx, "run_1", run.StatusPending,
		run.StatusRunning, "worker-1")
	if err != nil || !moved {
		t.Fatalf("TransitionStatusAndClaim() = (%v, %v), want the move to happen", moved, err)
	}
	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want the original %v kept", got.StartedAt, started)
	}
}

// TestFinalizeRunningIsFencedOnStatusAndOwner pins both halves of the terminal write's fence. A
// second dispatcher on a shared database whose heartbeats lapsed is still alive and will try to
// finalize a run the sweep already reassigned; without the owner test it would overwrite the
// outcome of whatever is running under the new holder.
func TestFinalizeRunningIsFencedOnStatusAndOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	exit := 0
	fin := func(owner string) run.Finalization {
		return run.Finalization{
			Status: run.StatusSucceeded, ExitCode: &exit, EndedAt: baseTime.Add(time.Minute),
			Owner: owner, Image: "ghcr.io/acme/ee:9", CommitSHA: "abc123",
		}
	}

	tests := []struct {
		Name       string
		Status     run.Status
		Holder     string
		Owner      string
		WantChange bool
	}{{ // Test 0: The holder finalizing its own running run.
		Name: "holder", Status: run.StatusRunning, Holder: "worker-1", Owner: "worker-1",
		WantChange: true,
	}, { // Test 1: A different worker is refused, so a stale process cannot end somebody's run.
		Name: "wrong owner", Status: run.StatusRunning, Holder: "worker-1", Owner: "worker-2",
	}, { // Test 2: An empty owner is the relay path, already authorized upstream.
		Name: "no owner claimed", Status: run.StatusRunning, Holder: "worker-1", Owner: "",
		WantChange: true,
	}, { // Test 3: A run that is not running cannot be finalized at all.
		Name: "not running", Status: run.StatusPending, Holder: "worker-1", Owner: "worker-1",
	}, { // Test 4: An already terminal run stays as it ended.
		Name: "already terminal", Status: run.StatusFailed, Holder: "worker-1", Owner: "worker-1",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id := fmt.Sprintf("run_%d", testNum)
			saveRuns(t, store, &run.Run{ID: id, Status: test.Status, CreatedAt: baseTime,
				ClaimedBy: test.Holder, Error: "original"})
			changed, err := store.FinalizeRunning(ctx, id, fin(test.Owner))
			if err != nil {
				t.Fatalf("FinalizeRunning(%s) error = %v", test.Name, err)
			}
			if changed != test.WantChange {
				t.Fatalf("FinalizeRunning(%s) = %v, want %v", test.Name, changed, test.WantChange)
			}
			got, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !test.WantChange {
				if got.Status != test.Status || got.EndedAt != nil {
					t.Errorf("%s: the refused finalize landed anyway: %q ended %v", test.Name,
						got.Status, got.EndedAt)
				}
				return
			}
			// The facts that explain the outcome land in the same statement as the status, or a
			// run is terminal with no record of how it ended.
			if got.Status != run.StatusSucceeded || got.ExitCode == nil || *got.ExitCode != 0 ||
				got.EndedAt == nil || got.Image != "ghcr.io/acme/ee:9" || got.CommitSHA != "abc123" {
				t.Errorf("%s: finalized run = %+v, want the outcome and its explaining facts",
					test.Name, got)
			}
		})
	}
}

// TestApplyRunningProgressKeepsWhatAReportDoesNotCarry pins the CASE arms in the progress write.
// One statement stands in for a read-modify-write, so a report with no warning and no outputs must
// leave the stored ones alone rather than blanking them. Blanking would lose the set_stats a
// previous batch published and the note a worker already raised.
func TestApplyRunningProgressKeepsWhatAReportDoesNotCarry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	started := baseTime
	saveRuns(t, store, &run.Run{
		ID: "run_1", Status: run.StatusRunning, CreatedAt: baseTime, ClaimedBy: "worker-1",
		StartedAt: &started, Warning: "kept warning",
		Outputs: map[string]any{"stage": "one"},
	})

	// A report carrying nothing must not erase either field.
	later := baseTime.Add(time.Hour)
	applied, err := store.ApplyRunningProgress(ctx, "run_1", "worker-1",
		run.Progress{StartedAt: &later})
	if err != nil || !applied {
		t.Fatalf("ApplyRunningProgress(empty) = (%v, %v), want it applied", applied, err)
	}
	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Warning != "kept warning" {
		t.Errorf("Warning = %q, want the stored note kept by an empty report", got.Warning)
	}
	if diff := cmp.Diff(map[string]any{"stage": "one"}, got.Outputs); diff != "" {
		t.Errorf("Outputs (-want +got):\n%s", diff)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want the original %v: a repeated report must not move it",
			got.StartedAt, started)
	}

	// A report that does carry them replaces both.
	applied, err = store.ApplyRunningProgress(ctx, "run_1", "worker-1", run.Progress{
		Warning: "new warning", Outputs: map[string]any{"stage": "two"},
	})
	if err != nil || !applied {
		t.Fatalf("ApplyRunningProgress(full) = (%v, %v), want it applied", applied, err)
	}
	got, err = store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Warning != "new warning" {
		t.Errorf("Warning = %q, want the reported one", got.Warning)
	}
	if diff := cmp.Diff(map[string]any{"stage": "two"}, got.Outputs); diff != "" {
		t.Errorf("Outputs after a full report (-want +got):\n%s", diff)
	}

	// The fence: a stranger and a settled run are both refused.
	if applied, err := store.ApplyRunningProgress(ctx, "run_1", "worker-2",
		run.Progress{Warning: "intruder"}); err != nil || applied {
		t.Errorf("ApplyRunningProgress(stranger) = (%v, %v), want it refused", applied, err)
	}
	saveRuns(t, store, &run.Run{ID: "run_done", Status: run.StatusSucceeded, CreatedAt: baseTime,
		ClaimedBy: "worker-1"})
	if applied, err := store.ApplyRunningProgress(ctx, "run_done", "worker-1",
		run.Progress{Warning: "late"}); err != nil || applied {
		t.Errorf("ApplyRunningProgress(terminal) = (%v, %v), want it refused", applied, err)
	}
}

// TestClaimSkipsEveryRunItMustNotTake pins the claim predicate one clause at a time. Each skipped
// shape is a run that would execute on real hosts if the clause were dropped, and the failure is
// silent: the claim simply succeeds and the work runs. The claimable run is stored last so a query
// that ignored the ordering would still have to pick it out on merit.
func TestClaimSkipsEveryRunItMustNotTake(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	pendingParent := "run_parent_pending"
	runningParent := "run_parent_running"
	canceledParent := "run_parent_canceled"
	saveRuns(t, store,
		&run.Run{ID: pendingParent, Status: run.StatusPending, CreatedAt: baseTime,
			Kind: run.KindSplit},
		&run.Run{ID: runningParent, Status: run.StatusRunning, CreatedAt: baseTime,
			Kind: run.KindSplit},
		&run.Run{ID: canceledParent, Status: run.StatusRunning, CreatedAt: baseTime,
			Kind: run.KindSplit, CancelRequested: true},

		// Not claimable, oldest first so an unfenced query would take one of them.
		&run.Run{ID: "skip_canceled", Status: run.StatusPending, CreatedAt: baseTime.Add(1),
			CancelRequested: true},
		&run.Run{ID: "skip_held", Status: run.StatusPendingApproval, CreatedAt: baseTime.Add(2)},
		&run.Run{ID: "skip_claimed", Status: run.StatusPending, CreatedAt: baseTime.Add(3),
			ClaimedBy: "somebody"},
		&run.Run{ID: "skip_coordinator", Status: run.StatusPending, CreatedAt: baseTime.Add(4),
			Kind: run.KindSplit},
		&run.Run{ID: "skip_other_queue", Status: run.StatusPending, CreatedAt: baseTime.Add(5),
			Queue: "batch"},
		&run.Run{ID: "skip_parent_pending", Status: run.StatusPending, CreatedAt: baseTime.Add(6),
			ParentID: &pendingParent},
		&run.Run{ID: "skip_parent_canceled", Status: run.StatusPending, CreatedAt: baseTime.Add(7),
			ParentID: &canceledParent},
		&run.Run{ID: "skip_running", Status: run.StatusRunning, CreatedAt: baseTime.Add(8)},

		// The one that may be taken, newest of all.
		&run.Run{ID: "take_me", Status: run.StatusPending, CreatedAt: baseTime.Add(9),
			ParentID: &runningParent},
	)

	got, err := store.Claim(ctx, "worker-1", nil)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if got.ID != "take_me" {
		t.Fatalf("Claim() took %q, want take_me: everything older is fenced off for a reason",
			got.ID)
	}
	if got.ClaimSecret == "" {
		t.Error("the claimed run carries no capability, so the worker cannot prove its lease")
	}
	if got.ClaimedBy != "worker-1" || got.ClaimedAt == nil {
		t.Errorf("the claimed run reads holder %q at %v, want the lease stamped", got.ClaimedBy,
			got.ClaimedAt)
	}

	// Nothing else is claimable now, so no fenced run leaks out on a second poll.
	if _, err := store.Claim(ctx, "worker-2", nil); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("a second Claim() = %v, want ErrNonePending: every remaining run is fenced", err)
	}
}

// TestClaimIsExclusiveUnderConcurrency pins that a run is leased once and only once. Several
// executors poll the same database, and a claim that can be won twice means one playbook running
// twice on the same hosts at the same time, which for an apply is the failure mode this product
// exists to prevent.
func TestClaimIsExclusiveUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	const runCount = 40
	for i := 0; i < runCount; i++ {
		saveRuns(t, store, &run.Run{
			ID: fmt.Sprintf("run_%03d", i), Status: run.StatusPending,
			CreatedAt: baseTime.Add(time.Duration(i) * time.Second),
		})
	}

	const workers = 8
	var (
		mu      sync.Mutex
		claimed = map[string]string{}
		secrets = map[string]bool{}
		wg      sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			owner := fmt.Sprintf("worker-%d", w)
			for {
				r, err := store.Claim(ctx, owner, nil)
				if errors.Is(err, run.ErrNonePending) {
					return
				}
				if err != nil {
					t.Errorf("Claim() error = %v", err)
					return
				}
				mu.Lock()
				if prev, dup := claimed[r.ID]; dup {
					t.Errorf("%s was claimed by both %s and %s, so one playbook runs twice",
						r.ID, prev, owner)
				}
				claimed[r.ID] = owner
				if r.ClaimSecret == "" {
					t.Errorf("%s was claimed with no capability", r.ID)
				}
				if secrets[r.ClaimSecret] {
					t.Errorf("%s reused a claim secret, so one worker's capability opens another "+
						"worker's run", r.ID)
				}
				secrets[r.ClaimSecret] = true
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if len(claimed) != runCount {
		t.Errorf("claimed %d runs, want all %d: work was left unclaimed", len(claimed), runCount)
	}
}

// TestNonTerminalReturnsExactlyTheUnfinishedStatuses pins the status set the SQL predicate names.
// It is written out as literals rather than derived from the Go helper, so a status added to one
// and not the other is what this catches: a new terminal status missing from the list would make
// finished runs look unfinished to the resume path, which restarts them.
func TestNonTerminalReturnsExactlyTheUnfinishedStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	statuses := []run.Status{
		run.StatusPending, run.StatusRunning, run.StatusPendingApproval,
		run.StatusSucceeded, run.StatusFailed, run.StatusCanceled, run.StatusInterrupted,
		run.StatusRejected,
	}
	var want []string
	for i, s := range statuses {
		id := fmt.Sprintf("run_%s", s)
		saveRuns(t, store, &run.Run{ID: id, Status: s,
			CreatedAt: baseTime.Add(time.Duration(i) * time.Second)})
		if !s.Terminal() {
			want = append(want, id)
		}
	}

	got, err := store.NonTerminal(ctx)
	if err != nil {
		t.Fatalf("NonTerminal() error = %v", err)
	}
	gotIDs := runIDs(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("NonTerminal() = %v, want %v: the SQL status list and Status.Terminal disagree",
			gotIDs, want)
	}
	for _, id := range want {
		found := false
		for _, g := range gotIDs {
			if g == id {
				found = true
			}
		}
		if !found {
			t.Errorf("NonTerminal() missed %s, so a run nobody finished looks finished", id)
		}
	}
}

// TestRunStatusCountsCountOnlyTopLevelRuns pins that shard and step children do not inflate the
// status chips. A count that includes children disagrees with the run list beside it, and the
// number an operator reads to decide whether the queue is backing up would be a multiple of the
// truth on any install that splits its work.
func TestRunStatusCountsCountOnlyTopLevelRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	parent := "run_parent"
	saveRuns(t, store,
		&run.Run{ID: parent, Status: run.StatusRunning, CreatedAt: baseTime, Kind: run.KindSplit},
		&run.Run{ID: "run_top_2", Status: run.StatusRunning, CreatedAt: baseTime},
		&run.Run{ID: "run_top_3", Status: run.StatusFailed, CreatedAt: baseTime},
		&run.Run{ID: "child_1", Status: run.StatusRunning, CreatedAt: baseTime, ParentID: &parent},
		&run.Run{ID: "child_2", Status: run.StatusRunning, CreatedAt: baseTime, ParentID: &parent},
		&run.Run{ID: "child_3", Status: run.StatusFailed, CreatedAt: baseTime, ParentID: &parent},
	)

	got, err := store.RunStatusCounts(ctx)
	if err != nil {
		t.Fatalf("RunStatusCounts() error = %v", err)
	}
	want := map[run.Status]int{run.StatusRunning: 2, run.StatusFailed: 1}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RunStatusCounts() (-want +got):\n%s", diff)
	}
}

// TestScanRunCarriesEveryNullBackAsAbsent pins the difference between a column that holds a zero and
// one that holds nothing. A run with no exit code is not a run that exited zero, and a top-level
// run is one whose parent is null rather than empty. Collapsing the two turns "never produced an
// outcome" into "succeeded", which is the wrong answer in the direction that matters.
func TestScanRunCarriesEveryNullBackAsAbsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store, &run.Run{ID: "run_bare", Status: run.StatusPending, CreatedAt: baseTime})
	got, err := store.Get(ctx, "run_bare")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil: a run that never exited did not exit zero", *got.ExitCode)
	}
	for name, ptr := range map[string]any{
		"ParentID": got.ParentID, "ShardIndex": got.ShardIndex, "ShardCount": got.ShardCount,
		"StepIndex": got.StepIndex, "RetryOf": got.RetryOf,
	} {
		if !isNilPointer(ptr) {
			t.Errorf("%s = %v, want nil on a plain run", name, ptr)
		}
	}
	for name, at := range map[string]*time.Time{
		"StartedAt": got.StartedAt, "EndedAt": got.EndedAt, "ClaimedAt": got.ClaimedAt,
	} {
		if at != nil {
			t.Errorf("%s = %v, want nil on a run that has not reached that point", name, at)
		}
	}
	if got.PolicySet != nil {
		t.Error("PolicySet is set on a run nothing gated: no rules and not recorded are " +
			"different facts and must not be confused")
	}
	if len(got.Labels) != 0 || len(got.Steps) != 0 || len(got.Tags) != 0 ||
		len(got.CredentialIDs) != 0 || len(got.Notifications) != 0 || len(got.ExtraVars) != 0 {
		t.Errorf("a bare run came back with collections filled in: %+v", got)
	}

	// A zero exit code is stored and read back as a real zero, not as absent.
	zero := 0
	saveRuns(t, store, &run.Run{ID: "run_zero", Status: run.StatusSucceeded, CreatedAt: baseTime,
		ExitCode: &zero})
	got, err = store.Get(ctx, "run_zero")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want a stored zero", got.ExitCode)
	}
}

// isNilPointer reports whether a pointer held in an interface is nil, for checking several
// differently typed optional fields in one loop.
func isNilPointer(v any) bool {
	switch p := v.(type) {
	case *string:
		return p == nil
	case *int:
		return p == nil
	default:
		return v == nil
	}
}

// TestRunRoundTripsHostileText pins that whatever a tool printed survives a save and a read. The
// text arrives from somebody else's playbook, inventory, or container output, so it carries
// whatever bytes that produced. A store that mangles it loses the failure detail on exactly the
// runs whose failure detail matters.
func TestRunRoundTripsHostileText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	long := strings.Repeat("verbose output line\n", 5000)
	tests := []struct {
		Name  string
		Run   *run.Run
		Check func(t *testing.T, got *run.Run)
	}{{ // Test 0: Unicode across every text field a run carries.
		Name: "unicode",
		Run: &run.Run{ID: "run_unicode", Status: run.StatusFailed, CreatedAt: baseTime,
			Playbook: "déploiement/site.yml", Command: "echo 'ünïcode 🔧 中文'",
			Error: "échec: 中文 error", Inventory: "hôtes.ini", StepName: "миграция",
			Labels: map[string]string{"среда": "прод"}},
		Check: func(t *testing.T, got *run.Run) {
			t.Helper()
			if got.Playbook != "déploiement/site.yml" || got.Error != "échec: 中文 error" {
				t.Errorf("unicode was mangled: %+v", got)
			}
			if got.Labels["среда"] != "прод" {
				t.Errorf("unicode label was mangled: %v", got.Labels)
			}
		},
	}, { // Test 1: Text a SQL author would fear, stored as data because it is bound, not spliced.
		Name: "sql shaped",
		Run: &run.Run{ID: "run_sql", Status: run.StatusFailed, CreatedAt: baseTime,
			Command: "'; DROP TABLE runs; --", Error: `" OR 1=1 --`,
			Playbook: `a'b"c\d%e_f`},
		Check: func(t *testing.T, got *run.Run) {
			t.Helper()
			if got.Command != "'; DROP TABLE runs; --" || got.Playbook != `a'b"c\d%e_f` {
				t.Errorf("quoted text was altered: %+v", got)
			}
		},
	}, { // Test 2: A very long field, which is what a chatty tool's error looks like.
		Name: "very long",
		Run: &run.Run{ID: "run_long", Status: run.StatusFailed, CreatedAt: baseTime,
			Error: long},
		Check: func(t *testing.T, got *run.Run) {
			t.Helper()
			if got.Error != long {
				t.Errorf("a long error came back %d bytes, want %d", len(got.Error), len(long))
			}
		},
	}, { // Test 3: Bytes no text column can hold, which the store cleans identically everywhere.
		Name: "invalid utf8 and nul",
		Run: &run.Run{ID: "run_bytes", Status: run.StatusFailed, CreatedAt: baseTime,
			Error: "before\x00after\xff\xfeend"},
		Check: func(t *testing.T, got *run.Run) {
			t.Helper()
			if strings.ContainsRune(got.Error, 0) {
				t.Error("a NUL byte survived into the stored error, which PostgreSQL would refuse")
			}
			if !strings.Contains(got.Error, "before") || !strings.Contains(got.Error, "end") {
				t.Errorf("cleaning the error threw away readable text: %q", got.Error)
			}
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			saveRuns(t, store, test.Run)
			got, err := store.Get(ctx, test.Run.ID)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", test.Name, err)
			}
			test.Check(t, got)
		})
	}
}

// TestRunTagsWithASeparatorDoNotRoundTrip records that a tag containing a comma comes back as two
// tags. Tags are stored as one comma-joined column and split on read, so the separator is not
// escapable. Ansible's own tag syntax is comma separated, which is why the storage was chosen, but
// the value arrives from an API body that permits any string.
func TestRunTagsWithASeparatorDoNotRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	saveRuns(t, store, &run.Run{ID: "run_1", Status: run.StatusPending, CreatedAt: baseTime,
		Tags: []string{"deploy,web"}, SkipTags: []string{"slow"}})
	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff([]string{"deploy", "web"}, got.Tags); diff != "" {
		t.Errorf("a comma-bearing tag round-tripped as (-recorded +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"slow"}, got.SkipTags); diff != "" {
		t.Errorf("skip tags (-want +got):\n%s", diff)
	}

	// The ordinary case, which the split is built for, is unaffected.
	saveRuns(t, store, &run.Run{ID: "run_2", Status: run.StatusPending, CreatedAt: baseTime,
		Tags: []string{"deploy", "web"}})
	got, err = store.Get(ctx, "run_2")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff([]string{"deploy", "web"}, got.Tags); diff != "" {
		t.Errorf("plain tags (-want +got):\n%s", diff)
	}
}

// TestATamperedPolicySetReadsAsNotRecorded demonstrates a fail-open on evidence.
//
// The rule set in force at submit is stored as JSON and is what lets a receipt be checked without
// asking this server what a digest meant. An empty column means "recorded before the set existed",
// which the code is careful to distinguish from "no rules". A column holding bytes that are not
// JSON is neither of those, and it is decoded to the same nil with no error at all, so a corrupted
// or edited rule set reads as an old run rather than as a run whose evidence cannot be trusted. The
// labels and steps columns beside it do report a decode failure.
func TestATamperedPolicySetReadsAsNotRecorded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tamper.db")
	db := openStoreAt(t, path)
	saveRuns(t, db.Runs(), &run.Run{
		ID: "run_1", Status: run.StatusPendingApproval, CreatedAt: baseTime,
		PolicySet: &run.PolicySet{Digest: "d1", Count: 1, Rules: []string{"prod apply"}},
	})
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Somebody with the database file edits the recorded rule set into something that is not JSON.
	rawExec(t, path, "UPDATE runs SET policy_set='{tampered' WHERE id='run_1'")

	reopened := openStoreAt(t, path)
	got, err := reopened.Runs().Get(ctx, "run_1")
	if err != nil {
		return // Reporting the decode failure is the outcome this test asks for.
	}
	if got.PolicySet == nil {
		t.Error("a rule set that is not decodable came back as nil with no error, which is the " +
			"same answer a run recorded before rule sets existed gives: tampered evidence and " +
			"absent evidence must not read the same")
	}
}

// TestATamperedLabelSetIsReported is the control beside the test above: the labels column, decoded
// a few lines away from the rule set, does report a decode failure rather than answering with an
// empty map. The two columns disagreeing about what an unreadable value means is the point.
func TestATamperedLabelSetIsReported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "labels.db")
	db := openStoreAt(t, path)
	saveRuns(t, db.Runs(), &run.Run{
		ID: "run_1", Status: run.StatusSucceeded, CreatedAt: baseTime,
		Labels: map[string]string{"env": "prod"},
	})
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	rawExec(t, path, "UPDATE runs SET labels='{tampered' WHERE id='run_1'")

	reopened := openStoreAt(t, path)
	if _, err := reopened.Runs().Get(ctx, "run_1"); err == nil {
		t.Error("an undecodable labels column read back cleanly, so a corrupt row is invisible")
	}
}
