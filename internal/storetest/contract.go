// Package storetest provides a shared behavior contract for run.Store implementations so that
// different backends, such as the in memory store and the SQLite store, cannot drift apart.
package storetest

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

// Contract runs the full run.Store contract against a fresh store from newStore. Each subtest gets
// its own store so they do not share state.
func Contract(t *testing.T, newStore func() run.Store) {
	t.Helper()
	t.Run("save and get", func(t *testing.T) { testSaveGet(t, newStore()) })
	t.Run("provenance round trip", func(t *testing.T) { testProvenance(t, newStore()) })
	t.Run("warning round trip", func(t *testing.T) { testWarning(t, newStore()) })
	t.Run("host facts", func(t *testing.T) { testHostFacts(t, newStore()) })
	t.Run("pipeline steps round trip", func(t *testing.T) { testPipelineSteps(t, newStore()) })
	t.Run("get missing", func(t *testing.T) { testGetNotFound(t, newStore()) })
	t.Run("idempotency key dedup", func(t *testing.T) { testByIdempotencyKey(t, newStore()) })
	t.Run("save updates existing", func(t *testing.T) { testSaveUpdate(t, newStore()) })
	t.Run("list newest first", func(t *testing.T) { testList(t, newStore()) })
	t.Run("list page and status counts", func(t *testing.T) { testListPage(t, newStore()) })
	t.Run("log append and read", func(t *testing.T) { testLog(t, newStore()) })
	t.Run("log after cursor", func(t *testing.T) { testLogAfter(t, newStore()) })
	t.Run("events append and read", func(t *testing.T) { testEvents(t, newStore()) })
	t.Run("events after cursor", func(t *testing.T) { testEventsAfter(t, newStore()) })
	t.Run("shards excluded from list", func(t *testing.T) { testShards(t, newStore()) })
	t.Run("pipeline steps ordered", func(t *testing.T) { testSteps(t, newStore()) })
	t.Run("non-terminal runs", func(t *testing.T) { testNonTerminal(t, newStore()) })
	t.Run("fleet health ranking", func(t *testing.T) { testFleetHealth(t, newStore()) })
	t.Run("drift status", func(t *testing.T) { testDriftStatus(t, newStore()) })
	t.Run("host costs", func(t *testing.T) { testHostCosts(t, newStore()) })
	t.Run("flaky detection", func(t *testing.T) { testFlaky(t, newStore()) })
	t.Run("host history", func(t *testing.T) { testHostHistory(t, newStore()) })
	t.Run("task trends", func(t *testing.T) { testTaskTrends(t, newStore()) })
	t.Run("claim leases oldest", func(t *testing.T) { testClaim(t, newStore()) })
	t.Run("claim respects queue", func(t *testing.T) { testClaimQueue(t, newStore()) })
	t.Run("heartbeat and reclaim", func(t *testing.T) { testLeaseLifecycle(t, newStore()) })
	t.Run("reclaim resolves orphaned children", func(t *testing.T) { testReclaimOrphans(t, newStore()) })
	t.Run("reclaim settles abandoned parents", func(t *testing.T) { testReclaimAbandonedParents(t, newStore()) })
	t.Run("reclaim settles an approved parent with no coordinator", func(t *testing.T) {
		testReclaimApprovedParentWithNoCoordinator(t, newStore())
	})
	t.Run("reclaim leaves a coordinated parent alone", func(t *testing.T) {
		testReclaimLeavesACoordinatedParentAlone(t, newStore())
	})
	t.Run("cancel request", func(t *testing.T) { testRequestCancel(t, newStore()) })
	t.Run("cancel pending", func(t *testing.T) { testCancelPending(t, newStore()) })
	t.Run("save keeps cancel sticky", func(t *testing.T) { testSaveKeepsCancel(t, newStore()) })
	t.Run("claim skips cancel requested", func(t *testing.T) { testClaimSkipsCancel(t, newStore()) })
	t.Run("transition status", func(t *testing.T) { testTransitionStatus(t, newStore()) })
	t.Run("workers", func(t *testing.T) { testWorkers(t, newStore()) })
	t.Run("retention purge", func(t *testing.T) { testPurge(t, newStore()) })
	t.Run("terminal run fences writes", func(t *testing.T) { testTerminalFence(t, newStore()) })
}

// sampleRun returns a fully populated terminal run with deterministic times.
func sampleRun(id string) *run.Run {
	created := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	started := created.Add(time.Second)
	ended := created.Add(2 * time.Second)
	code := 0
	retryOf := "run_prior"
	return &run.Run{
		ID: id, Playbook: "play.yml", Inventory: "inventory.ini",
		Status: run.StatusSucceeded, ExitCode: &code,
		CreatedAt: created, StartedAt: &started, EndedAt: &ended,
		RetryOf:   &retryOf,
		ExtraVars: map[string]any{"version": "1.2.3"},
		Outputs:   map[string]any{"built": true, "count": float64(2)},
		Tool:      "bash", Command: "echo hi", DryRun: true,
		ProposedFrom: "run_check", Intent: "echo hello on the box",
		IdempotencyKey: "idem_sample",
		Timeout:        3600,
		Notifications: []run.NotifyTarget{
			{Kind: run.NotifySlack, URL: "https://hooks.example.com/team", OnFailure: true},
		},
	}
}

// testTransitionStatus checks the atomic status move: it changes a row only from the expected
// status, and a second attempt from a status the run has already left changes nothing, so two
// racing approvers cannot both win.
func testTransitionStatus(t *testing.T, store run.Store) {
	ctx := context.Background()
	r := sampleRun("run_t")
	r.Status = run.StatusPendingApproval
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	ok, err := store.TransitionStatus(ctx, "run_t", run.StatusPendingApproval, run.StatusPending)
	if err != nil {
		t.Fatalf("TransitionStatus() error = %v", err)
	}
	if !ok {
		t.Fatal("first transition changed nothing, want it to move the run")
	}
	got, err := store.Get(ctx, "run_t")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	ok, err = store.TransitionStatus(ctx, "run_t", run.StatusPendingApproval, run.StatusPending)
	if err != nil {
		t.Fatalf("TransitionStatus() error = %v", err)
	}
	if ok {
		t.Error("second transition changed a row, want a no-op")
	}
	if ok, err := store.TransitionStatus(ctx, "run_missing", run.StatusPending, run.StatusRejected); err != nil || ok {
		t.Errorf("missing run transition = (%v, %v), want (false, nil)", ok, err)
	}
}

// testSaveGet verifies a run round trips and that returned values are independent copies.
func testSaveGet(t *testing.T, store run.Store) {
	ctx := context.Background()
	want := sampleRun("run_1")
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Get() mismatch (-want +got):\n%s", diff)
	}

	got.Playbook = "mutated"
	again, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Playbook != "play.yml" {
		t.Error("mutating the returned run changed stored state")
	}
}

// testGetNotFound verifies a missing run reports run.ErrNotFound across all read methods.
func testGetNotFound(t *testing.T, store run.Store) {
	ctx := context.Background()
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Get() = %v, want ErrNotFound", err)
	}
	if _, err := store.Log(ctx, "missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Log() = %v, want ErrNotFound", err)
	}
	if _, err := store.Events(ctx, "missing"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Events() = %v, want ErrNotFound", err)
	}
	if err := store.AppendLog(ctx, "missing", []byte("x")); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("AppendLog() = %v, want ErrNotFound", err)
	}
	batch := []event.Event{{Type: event.TypePlayStart}}
	if err := store.AppendEvents(ctx, "missing", batch); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("AppendEvents() = %v, want ErrNotFound", err)
	}
}

// testByIdempotencyKey verifies submission dedup at the store: a saved key looks up its run, an
// unused or empty key reports ErrNotFound, re-saving the same run under its key is an ordinary
// update, and a different run claiming a used key is rejected with ErrDuplicateKey without landing,
// the partial unique index that backstops a concurrent retry. An empty key never dedupes.
func testByIdempotencyKey(t *testing.T, store run.Store) {
	ctx := context.Background()

	// An unused key and the empty key are never found.
	if _, err := store.ByIdempotencyKey(ctx, "idem_unused"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("ByIdempotencyKey(unused) = %v, want ErrNotFound", err)
	}
	if _, err := store.ByIdempotencyKey(ctx, ""); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("ByIdempotencyKey(empty) = %v, want ErrNotFound", err)
	}

	// A saved run is found by its key.
	first := &run.Run{
		ID: "run_a", Playbook: "p", Status: run.StatusPending,
		CreatedAt: time.Now(), IdempotencyKey: "idem_1",
	}
	if err := store.Save(ctx, first); err != nil {
		t.Fatalf("Save(first) error = %v", err)
	}
	got, err := store.ByIdempotencyKey(ctx, "idem_1")
	if err != nil {
		t.Fatalf("ByIdempotencyKey() error = %v", err)
	}
	if got.ID != "run_a" {
		t.Errorf("ByIdempotencyKey() id = %q, want run_a", got.ID)
	}

	// Re-saving the same run under the same key is an ordinary update, not a conflict.
	first.Status = run.StatusRunning
	if err := store.Save(ctx, first); err != nil {
		t.Errorf("re-Save(first) error = %v, want nil", err)
	}

	// A different run claiming the used key is rejected, the backstop for a concurrent retry.
	second := &run.Run{
		ID: "run_b", Playbook: "p", Status: run.StatusPending,
		CreatedAt: time.Now(), IdempotencyKey: "idem_1",
	}
	if err := store.Save(ctx, second); !errors.Is(err, run.ErrDuplicateKey) {
		t.Errorf("Save(second) = %v, want ErrDuplicateKey", err)
	}
	// The loser never landed: the key still resolves to the original winner, and run_b is absent.
	if got, err := store.ByIdempotencyKey(ctx, "idem_1"); err != nil || got.ID != "run_a" {
		t.Errorf("ByIdempotencyKey() after conflict = (%v, %v), want run_a", got, err)
	}
	if _, err := store.Get(ctx, "run_b"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Get(run_b) = %v, want ErrNotFound, the losing run must not persist", err)
	}

	// An empty key never dedupes: keyless runs coexist freely.
	for _, id := range []string{"run_c", "run_d"} {
		keyless := &run.Run{ID: id, Playbook: "p", Status: run.StatusPending, CreatedAt: time.Now()}
		if err := store.Save(ctx, keyless); err != nil {
			t.Errorf("Save(%s) keyless error = %v, want nil", id, err)
		}
	}
}

// testSaveUpdate verifies that saving an existing id replaces the stored run.
func testSaveUpdate(t *testing.T, store run.Store) {
	ctx := context.Background()
	r := &run.Run{ID: "run_1", Playbook: "p", Status: run.StatusPending, CreatedAt: time.Now()}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	code := 2
	r.Status = run.StatusFailed
	r.ExitCode = &code
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusFailed || got.ExitCode == nil || *got.ExitCode != 2 {
		t.Errorf("after update got status=%q exit=%v, want failed exit=2", got.Status, got.ExitCode)
	}
}

// testList verifies runs come back newest first.
func testList(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range []*run.Run{
		{ID: "a", Status: run.StatusSucceeded, CreatedAt: base},
		{ID: "b", Status: run.StatusSucceeded, CreatedAt: base.Add(time.Second)},
		{ID: "c", Status: run.StatusSucceeded, CreatedAt: base.Add(2 * time.Second)},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	runs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	gotIDs := make([]string, len(runs))
	for i, r := range runs {
		gotIDs[i] = r.ID
	}
	if diff := cmp.Diff([]string{"c", "b", "a"}, gotIDs); diff != "" {
		t.Errorf("List() order mismatch (-want +got):\n%s", diff)
	}
}

// testListPage verifies paging returns newest first, honors limit and offset, and that status
// counts tally every top-level run.
func testListPage(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seed := []*run.Run{
		{ID: "a", Status: run.StatusSucceeded, Playbook: "deploy.yml", CreatedAt: base},
		{ID: "b", Status: run.StatusFailed, Tool: run.ToolBash, Command: "echo hi", CreatedAt: base.Add(time.Second)},
		{ID: "c", Status: run.StatusRunning, Playbook: "migrate.yml", CreatedAt: base.Add(2 * time.Second)},
		{ID: "d", Status: run.StatusSucceeded, Playbook: "deploy.yml", CreatedAt: base.Add(3 * time.Second)},
	}
	for _, r := range seed {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	ids := func(runs []*run.Run) []string {
		out := make([]string, len(runs))
		for i, r := range runs {
			out[i] = r.ID
		}
		return out
	}

	first, err := store.ListPage(ctx, run.ListFilter{}, 2, 0)
	if err != nil {
		t.Fatalf("ListPage() error = %v", err)
	}
	if diff := cmp.Diff([]string{"d", "c"}, ids(first)); diff != "" {
		t.Errorf("ListPage(2,0) mismatch (-want +got):\n%s", diff)
	}
	next, err := store.ListPage(ctx, run.ListFilter{}, 2, 2)
	if err != nil {
		t.Fatalf("ListPage() error = %v", err)
	}
	if diff := cmp.Diff([]string{"b", "a"}, ids(next)); diff != "" {
		t.Errorf("ListPage(2,2) mismatch (-want +got):\n%s", diff)
	}
	if past, _ := store.ListPage(ctx, run.ListFilter{}, 2, 10); len(past) != 0 {
		t.Errorf("ListPage past end = %v, want empty", ids(past))
	}
	if all, _ := store.ListPage(ctx, run.ListFilter{}, 0, 0); len(all) != 4 {
		t.Errorf("ListPage(0,0) len = %d, want 4", len(all))
	}

	// A non-empty query filters case-insensitively across the runs-view fields, newest first, and
	// composes with paging.
	if hit, _ := store.ListPage(ctx, run.ListFilter{Query: "deploy"}, 0, 0); cmp.Diff([]string{"d", "a"}, ids(hit)) != "" {
		t.Errorf("ListPage search deploy = %v, want [d a]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Query: "BASH"}, 0, 0); len(hit) != 1 || hit[0].ID != "b" {
		t.Errorf("ListPage search BASH = %v, want [b]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Query: "failed"}, 0, 0); len(hit) != 1 || hit[0].ID != "b" {
		t.Errorf("ListPage search failed = %v, want [b]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Query: "nomatch"}, 0, 0); len(hit) != 0 {
		t.Errorf("ListPage search nomatch = %v, want empty", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Query: "deploy"}, 1, 0); len(hit) != 1 || hit[0].ID != "d" {
		t.Errorf("ListPage search with page = %v, want [d]", ids(hit))
	}

	// The status and tool filters are exact, unlike the fuzzy query, and the ansible tool matches
	// runs stored with an empty tool, its historical form.
	if hit, _ := store.ListPage(ctx, run.ListFilter{Status: "failed"}, 0, 0); len(hit) != 1 || hit[0].ID != "b" {
		t.Errorf("ListPage status failed = %v, want [b]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Status: "succeeded"}, 0, 0); cmp.Diff([]string{"d", "a"}, ids(hit)) != "" {
		t.Errorf("ListPage status succeeded = %v, want [d a]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Tool: "bash"}, 0, 0); len(hit) != 1 || hit[0].ID != "b" {
		t.Errorf("ListPage tool bash = %v, want [b]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Tool: "ansible"}, 0, 0); cmp.Diff([]string{"d", "c", "a"}, ids(hit)) != "" {
		t.Errorf("ListPage tool ansible = %v, want [d c a]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Status: "succeeded", Query: "deploy"}, 0, 0); cmp.Diff([]string{"d", "a"}, ids(hit)) != "" {
		t.Errorf("ListPage status plus query = %v, want [d a]", ids(hit))
	}

	// A created-at window keeps only runs inside it: half-open, after inclusive, before exclusive.
	if got, _ := store.ListPage(ctx, run.ListFilter{
		After:  base.Add(1 * time.Second),
		Before: base.Add(3 * time.Second),
	}, 0, 0); len(got) != 2 {
		t.Errorf("date window = %d runs, want 2", len(got))
	}

	// OldestFirst flips the default ordering.
	if all, _ := store.ListPage(ctx, run.ListFilter{OldestFirst: true}, 0, 0); cmp.Diff([]string{"a", "b", "c", "d"}, ids(all)) != "" {
		t.Errorf("ListPage oldest first = %v, want [a b c d]", ids(all))
	}

	counts, err := store.RunStatusCounts(ctx)
	if err != nil {
		t.Fatalf("RunStatusCounts() error = %v", err)
	}
	want := map[run.Status]int{run.StatusSucceeded: 2, run.StatusFailed: 1, run.StatusRunning: 1}
	if diff := cmp.Diff(want, counts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("RunStatusCounts() mismatch (-want +got):\n%s", diff)
	}

	// Provenance and label filters compose with the rest. Summaries attach while the run is
	// still live, since the terminal fence rejects summary writes after a run finishes.
	live := &run.Run{
		ID: "e", Playbook: "tag.yml", Status: run.StatusRunning, CreatedAt: base.Add(4 * time.Second),
		Source: "schedule", SourceID: "sch_9", Actor: "night-cron",
		Labels: map[string]string{"env": "prod"},
	}
	if err := store.Save(ctx, live); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.SaveHostSummary(ctx, "e", []run.HostSummary{{Host: "web09", Worst: "ok", RanAt: base}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	live.Status = run.StatusSucceeded
	if err := store.Save(ctx, live); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Source: "schedule"}, 0, 0); len(hit) != 1 || hit[0].ID != "e" {
		t.Errorf("source filter = %v, want [e]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Actor: "night-cron"}, 0, 0); len(hit) != 1 || hit[0].ID != "e" {
		t.Errorf("actor filter = %v, want [e]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{SourceID: "sch_9"}, 0, 0); len(hit) != 1 || hit[0].ID != "e" {
		t.Errorf("source id filter = %v, want [e]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{LabelKey: "env", LabelValue: "prod"}, 0, 0); len(hit) != 1 || hit[0].ID != "e" {
		t.Errorf("label filter = %v, want [e]", ids(hit))
	}
	if hit, _ := store.ListPage(ctx, run.ListFilter{Host: "web09"}, 0, 0); len(hit) != 1 || hit[0].ID != "e" {
		t.Errorf("host filter = %v, want [e]", ids(hit))
	}
}

// testLog verifies log append, read, ordering, and copy independence.
func testLog(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{ID: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.AppendLog(ctx, "x", []byte("hello ")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := store.AppendLog(ctx, "x", []byte("world")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}

	body, err := store.Log(ctx, "x")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if string(body) != "hello world" {
		t.Errorf("Log() = %q, want %q", body, "hello world")
	}

	body[0] = 'X'
	again, err := store.Log(ctx, "x")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if len(again) == 0 || again[0] != 'h' {
		t.Error("mutating the returned log changed stored state")
	}
}

// testEvents verifies event append, read, ordering, and copy independence.
func testEvents(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{ID: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.AppendEvents(ctx, "x",
		[]event.Event{{Type: event.TypePlayStart, Time: at, Play: "demo"}}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	if err := store.AppendEvents(ctx, "x",
		[]event.Event{{Type: event.TypeRunnerOK, Time: at, Play: "demo", Task: "t", Host: "h"}}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}

	got, err := store.Events(ctx, "x")
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	want := []event.Event{
		{Type: event.TypePlayStart, Time: at, Play: "demo"},
		{Type: event.TypeRunnerOK, Time: at, Play: "demo", Task: "t", Host: "h"},
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Events() mismatch (-want +got):\n%s", diff)
	}

	got[0].Play = "mutated"
	again, err := store.Events(ctx, "x")
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	if again[0].Play != "demo" {
		t.Error("mutating the returned events changed stored state")
	}
}

// testEventsAfter verifies the seq cursor: events come back after the cursor, in order, with
// a positive strictly increasing Seq, honoring the limit, and paging by the last Seq walks
// the whole log. It does not assume specific Seq values, since those differ across stores.
func testEventsAfter(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{ID: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const total = 5
	for i := 0; i < total; i++ {
		if err := store.AppendEvents(ctx, "x",
			[]event.Event{{Type: event.TypeTaskStart, Time: at, Task: fmt.Sprintf("t%d", i)}}); err != nil {
			t.Fatalf("AppendEvents() error = %v", err)
		}
	}

	// Missing run is ErrNotFound.
	if _, err := store.EventsAfter(ctx, "missing", 0, 0); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("EventsAfter(missing) = %v, want ErrNotFound", err)
	}

	// From the start returns all, in order, with a positive strictly increasing Seq.
	all, err := store.EventsAfter(ctx, "x", 0, 0)
	if err != nil {
		t.Fatalf("EventsAfter() error = %v", err)
	}
	if len(all) != total {
		t.Fatalf("EventsAfter(0,0) len = %d, want %d", len(all), total)
	}
	var prev int64
	for i, e := range all {
		if e.Seq <= prev {
			t.Errorf("event %d Seq = %d, want > %d", i, e.Seq, prev)
		}
		if e.Task != fmt.Sprintf("t%d", i) {
			t.Errorf("event %d Task = %q, want t%d", i, e.Task, i)
		}
		prev = e.Seq
	}

	// The limit caps the batch.
	if got, _ := store.EventsAfter(ctx, "x", 0, 2); len(got) != 2 {
		t.Errorf("EventsAfter(0,2) len = %d, want 2", len(got))
	}

	// The cursor skips everything at or before it.
	tail, err := store.EventsAfter(ctx, "x", all[1].Seq, 0)
	if err != nil {
		t.Fatalf("EventsAfter(cursor) error = %v", err)
	}
	if len(tail) != total-2 || tail[0].Task != "t2" {
		t.Errorf("EventsAfter(after t1) = %d events, first %q, want 3 starting t2", len(tail), tail[0].Task)
	}

	// Paging by the last Seq walks the whole log exactly once.
	var paged []event.Event
	cursor := int64(0)
	for {
		batch, err := store.EventsAfter(ctx, "x", cursor, 2)
		if err != nil {
			t.Fatalf("paging EventsAfter() error = %v", err)
		}
		if len(batch) == 0 {
			break
		}
		paged = append(paged, batch...)
		cursor = batch[len(batch)-1].Seq
	}
	if diff := cmp.Diff(all, paged, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("paged walk mismatch (-want +got):\n%s", diff)
	}
}

// testShards verifies that shard runs are excluded from List and returned by Shards in order.
func testShards(t *testing.T, store run.Store) {
	ctx := context.Background()
	parentID := "run_parent"
	if err := store.Save(ctx,
		&run.Run{ID: parentID, Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for i := range 2 {
		idx, count := i, 2
		child := &run.Run{
			ID: fmt.Sprintf("run_child_%d", i), Status: run.StatusSucceeded, CreatedAt: time.Now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &count, Limit: "host" + fmt.Sprint(i),
		}
		if err := store.Save(ctx, child); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	top, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	parentSeen := false
	for _, r := range top {
		if r.ParentID != nil {
			t.Errorf("List returned shard run %s", r.ID)
		}
		if r.ID == parentID {
			parentSeen = true
		}
	}
	if !parentSeen {
		t.Error("List did not return the parent run")
	}

	shards, err := store.Shards(ctx, parentID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("Shards len = %d, want 2", len(shards))
	}
	if shards[0].ShardIndex == nil || *shards[0].ShardIndex != 0 {
		t.Errorf("first shard index = %v, want 0", shards[0].ShardIndex)
	}
	if shards[0].Limit != "host0" {
		t.Errorf("shard limit = %q, want host0", shards[0].Limit)
	}
}

// testSteps verifies pipeline step runs are ordered by step index and excluded from List.
func testSteps(t *testing.T, store run.Store) {
	ctx := context.Background()
	parentID := "run_pipeline"
	if err := store.Save(ctx, &run.Run{
		ID: parentID, Kind: run.KindPipeline, Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for i := range 2 {
		idx := i
		if err := store.Save(ctx, &run.Run{
			ID: fmt.Sprintf("run_step_%d", i), Playbook: fmt.Sprintf("step%d.yml", i),
			Status: run.StatusSucceeded, CreatedAt: time.Now(),
			ParentID: &parentID, StepIndex: &idx, StepName: fmt.Sprintf("step-%d", i),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	steps, err := store.Steps(ctx, parentID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(steps))
	}
	if steps[0].StepName != "step-0" || steps[0].StepIndex == nil || *steps[0].StepIndex != 0 {
		t.Errorf("first step = %+v, want name step-0 index 0", steps[0])
	}

	parent, err := store.Get(ctx, parentID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if parent.Kind != run.KindPipeline {
		t.Errorf("parent kind = %q, want %q", parent.Kind, run.KindPipeline)
	}

	top, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, r := range top {
		if r.ParentID != nil {
			t.Errorf("List returned step run %s", r.ID)
		}
	}
}

// testNonTerminal verifies only pending and running runs are returned, including shards.
func testNonTerminal(t *testing.T, store run.Store) {
	ctx := context.Background()
	parentID := "p"
	idx, count := 0, 1
	for _, r := range []*run.Run{
		{ID: "pending", Status: run.StatusPending, CreatedAt: time.Now()},
		{ID: "running", Status: run.StatusRunning, CreatedAt: time.Now()},
		{ID: "done", Status: run.StatusSucceeded, CreatedAt: time.Now()},
		{ID: "gone", Status: run.StatusInterrupted, CreatedAt: time.Now()},
		{
			ID: "shard", Status: run.StatusRunning, CreatedAt: time.Now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
		},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	got, err := store.NonTerminal(ctx)
	if err != nil {
		t.Fatalf("NonTerminal() error = %v", err)
	}
	seen := make(map[string]bool, len(got))
	for _, r := range got {
		seen[r.ID] = true
	}
	if !seen["pending"] || !seen["running"] || !seen["shard"] {
		t.Errorf("NonTerminal missing active runs, got %v", seen)
	}
	if seen["done"] || seen["gone"] {
		t.Error("NonTerminal returned a terminal run")
	}
}

// testFleetHealth verifies host summaries persist and rank hosts by recent failures with windowing.
func testFleetHealth(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	save := func(runID string, at time.Time, hosts map[string]string) {
		var sums []run.HostSummary
		for host, worst := range hosts {
			sums = append(sums, run.HostSummary{Host: host, Worst: worst, RanAt: at})
		}
		if err := store.SaveHostSummary(ctx, runID, sums); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
	}
	save("r1", base, map[string]string{"db01": "failed", "web01": "ok"})
	save("r2", base.Add(time.Hour), map[string]string{"db01": "failed", "web01": "ok"})
	save("r3", base.Add(2*time.Hour), map[string]string{"db01": "ok", "web01": "changed"})

	health, err := store.FleetHealth(ctx, 10)
	if err != nil {
		t.Fatalf("FleetHealth() error = %v", err)
	}
	byHost := make(map[string]run.HostHealth, len(health))
	for _, h := range health {
		byHost[h.Host] = h
	}
	if db := byHost["db01"]; db.Failures != 2 || db.Total != 3 || db.LastOutcome != "ok" {
		t.Errorf("db01 = %+v, want failures 2 total 3 last ok", db)
	}
	if web := byHost["web01"]; web.Failures != 0 {
		t.Errorf("web01 failures = %d, want 0", web.Failures)
	}
	if len(health) < 2 || health[0].Host != "db01" {
		t.Errorf("ranking = %+v, want db01 first", health)
	}

	windowed, err := store.FleetHealth(ctx, 1)
	if err != nil {
		t.Fatalf("FleetHealth() error = %v", err)
	}
	for _, h := range windowed {
		if h.Total != 1 {
			t.Errorf("window 1 total for %s = %d, want 1", h.Host, h.Total)
		}
		if h.Host == "db01" && h.Failures != 0 {
			t.Errorf("db01 window 1 failures = %d, want 0 since most recent run was ok", h.Failures)
		}
	}
}

// testDriftStatus verifies drift comes only from dry-run checks and reports the latest check per host,
// so a real run's changes and a stale check do not distort the current drift.
func testDriftStatus(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	saveRun := func(id string, at time.Time, dry bool, changed map[string]int) {
		// The summary is written while the run is still running, as a real run does, then the run
		// finalizes, since the store fences summary writes to a terminal run.
		if err := store.Save(ctx, &run.Run{
			ID: id, Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: at, DryRun: dry,
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		var sums []run.HostSummary
		for host, c := range changed {
			sums = append(sums, run.HostSummary{Host: host, Changed: c, Worst: "changed", RanAt: at})
		}
		if err := store.SaveHostSummary(ctx, id, sums); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
		if err := store.Save(ctx, &run.Run{
			ID: id, Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: at, DryRun: dry,
		}); err != nil {
			t.Fatalf("Save() finalize error = %v", err)
		}
	}
	// An older check finds drift on web01. A real run then changes it. The newest check finds it near
	// clean. db01 has one check that found it in sync.
	saveRun("chk1", base, true, map[string]int{"web01": 3, "db01": 0})
	saveRun("apply1", base.Add(time.Hour), false, map[string]int{"web01": 5})
	saveRun("chk2", base.Add(2*time.Hour), true, map[string]int{"web01": 1})

	drift, err := store.DriftStatus(ctx)
	if err != nil {
		t.Fatalf("DriftStatus() error = %v", err)
	}
	byHost := make(map[string]run.HostDrift, len(drift))
	for _, d := range drift {
		byHost[d.Host] = d
	}
	// web01's current drift is its latest check, chk2 with one drifted task, not the real run's five
	// or the older check's three.
	if w := byHost["web01"]; w.DriftedTasks != 1 || w.RunID != "chk2" {
		t.Errorf("web01 drift = %+v, want 1 drifted task from chk2", w)
	}
	// db01's only check found it in sync.
	if d := byHost["db01"]; d.DriftedTasks != 0 || d.RunID != "chk1" {
		t.Errorf("db01 drift = %+v, want 0 drifted tasks from chk1", d)
	}
	// The most drifted host ranks first.
	if len(drift) < 1 || drift[0].Host != "web01" {
		t.Errorf("drift order = %+v, want web01 first", drift)
	}
}

// testFlaky verifies flip counting marks intermittent hosts flaky and steady hosts not.
func testFlaky(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	save := func(runID string, at time.Time, hosts map[string]string) {
		var sums []run.HostSummary
		for host, worst := range hosts {
			sums = append(sums, run.HostSummary{Host: host, Worst: worst, RanAt: at})
		}
		if err := store.SaveHostSummary(ctx, runID, sums); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
	}
	// flappy alternates, fixed fails then recovers once, solid never fails.
	save("r1", base, map[string]string{"flappy": "failed", "fixed": "failed", "solid": "ok"})
	save("r2", base.Add(time.Hour), map[string]string{"flappy": "ok", "fixed": "failed", "solid": "ok"})
	save("r3", base.Add(2*time.Hour), map[string]string{"flappy": "failed", "fixed": "ok", "solid": "changed"})
	save("r4", base.Add(3*time.Hour), map[string]string{"flappy": "ok", "fixed": "ok", "solid": "ok"})

	health, err := store.FleetHealth(ctx, 10)
	if err != nil {
		t.Fatalf("FleetHealth() error = %v", err)
	}
	byHost := make(map[string]run.HostHealth, len(health))
	for _, h := range health {
		byHost[h.Host] = h
	}
	if h := byHost["flappy"]; h.Flips != 3 || !h.Flaky {
		t.Errorf("flappy = flips %d flaky %v, want 3 true", h.Flips, h.Flaky)
	}
	wantRecent := []string{"ok", "failed", "ok", "failed"}
	if diff := cmp.Diff(wantRecent, byHost["flappy"].Recent); diff != "" {
		t.Errorf("flappy recent mismatch (-want +got):\n%s", diff)
	}
	wantRecentRuns := []string{"r4", "r3", "r2", "r1"}
	if diff := cmp.Diff(wantRecentRuns, byHost["flappy"].RecentRuns); diff != "" {
		t.Errorf("flappy recent runs mismatch (-want +got):\n%s", diff)
	}
	if h := byHost["fixed"]; h.Flips != 1 || h.Flaky {
		t.Errorf("fixed = flips %d flaky %v, want 1 false", h.Flips, h.Flaky)
	}
	if h := byHost["solid"]; h.Flips != 0 || h.Flaky {
		t.Errorf("solid = flips %d flaky %v, want 0 false", h.Flips, h.Flaky)
	}
}

// testHostHistory verifies per host history comes back newest first with run ids and windowing.
func testHostHistory(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, worst := range []string{"ok", "failed", "changed"} {
		sums := []run.HostSummary{
			{Host: "db01", Worst: worst, DurationSeconds: float64(i + 1), RanAt: base.Add(time.Duration(i) * time.Hour)},
			{Host: "web01", Worst: "ok", RanAt: base.Add(time.Duration(i) * time.Hour)},
		}
		if err := store.SaveHostSummary(ctx, fmt.Sprintf("r%d", i), sums); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
	}

	history, err := store.HostHistory(ctx, "db01", 10)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history len = %d, want 3", len(history))
	}
	if history[0].RunID != "r2" || history[0].Worst != "changed" {
		t.Errorf("newest = %+v, want run r2 worst changed", history[0])
	}
	for _, hs := range history {
		if hs.Host != "db01" {
			t.Errorf("history returned host %q", hs.Host)
		}
		if hs.RunID == "" {
			t.Error("history entry missing run id")
		}
	}

	limited, err := store.HostHistory(ctx, "db01", 1)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	if len(limited) != 1 || limited[0].RunID != "r2" {
		t.Errorf("limited = %+v, want only r2", limited)
	}

	empty, err := store.HostHistory(ctx, "ghost", 5)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("unknown host history = %+v, want empty", empty)
	}
}

// testTaskTrends verifies task durations persist and aggregate over the recent window.
func testTaskTrends(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	save := func(runID string, at time.Time, tasks map[string]float64) {
		var sums []run.TaskSummary
		for task, seconds := range tasks {
			sums = append(sums, run.TaskSummary{Task: task, Seconds: seconds, RanAt: at})
		}
		if err := store.SaveTaskSummary(ctx, runID, sums); err != nil {
			t.Fatalf("SaveTaskSummary() error = %v", err)
		}
	}
	save("r1", base, map[string]float64{"install": 10, "restart": 1})
	save("r2", base.Add(time.Hour), map[string]float64{"install": 20, "restart": 1})
	save("r3", base.Add(2*time.Hour), map[string]float64{"install": 30})

	trends, err := store.TaskTrends(ctx, 10)
	if err != nil {
		t.Fatalf("TaskTrends() error = %v", err)
	}
	byTask := make(map[string]run.TaskTrend, len(trends))
	for _, tr := range trends {
		byTask[tr.Task] = tr
	}
	install := byTask["install"]
	if install.Runs != 3 || install.AvgSeconds != 20 || install.LastSeconds != 30 {
		t.Errorf("install = %+v, want runs 3 avg 20 last 30", install)
	}
	restart := byTask["restart"]
	if restart.Runs != 2 || restart.AvgSeconds != 1 {
		t.Errorf("restart = %+v, want runs 2 avg 1", restart)
	}

	windowed, err := store.TaskTrends(ctx, 1)
	if err != nil {
		t.Fatalf("TaskTrends() error = %v", err)
	}
	byTask = make(map[string]run.TaskTrend, len(windowed))
	for _, tr := range windowed {
		byTask[tr.Task] = tr
	}
	if w := byTask["install"]; w.Runs != 1 || w.AvgSeconds != 30 {
		t.Errorf("windowed install = %+v, want runs 1 avg 30", w)
	}
}

// testClaim verifies claiming takes the oldest unclaimed plain run exactly once and skips
// children, parents, and non-pending runs.
func testClaim(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	parentID := "run_split"
	idx, count := 0, 1
	for _, r := range []*run.Run{
		{ID: "run_new", Playbook: "p", Status: run.StatusPending, CreatedAt: base.Add(time.Minute)},
		{ID: "run_old", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "run_done", Playbook: "p", Status: run.StatusSucceeded, CreatedAt: base},
		{ID: parentID, Playbook: "p", Kind: run.KindSplit, Status: run.StatusPending, CreatedAt: base},
		{
			ID: "run_shard", Playbook: "p", Status: run.StatusPending,
			CreatedAt: base.Add(30 * time.Minute),
			ParentID:  &parentID, ShardIndex: &idx, ShardCount: &count,
		},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	first, err := store.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if first.ID != "run_old" || first.ClaimedBy != "worker-a" || first.ClaimedAt == nil {
		t.Errorf("first claim = %+v, want run_old leased by worker-a", first)
	}

	second, err := store.Claim(ctx, "worker-b", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if second.ID != "run_new" {
		t.Errorf("second claim = %s, want run_new", second.ID)
	}

	third, err := store.Claim(ctx, "worker-c", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if third.ID != "run_shard" {
		t.Errorf("third claim = %s, want run_shard, children are executable", third.ID)
	}

	if _, err := store.Claim(ctx, "worker-d", []string{""}); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("fourth claim error = %v, want ErrNonePending", err)
	}
}

// testClaimQueue verifies a queued run is only claimable by an executor serving that queue.
func testClaimQueue(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	for _, r := range []*run.Run{
		{ID: "run_default", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "run_dmz", Playbook: "p", Status: run.StatusPending, CreatedAt: base, Queue: "dmz"},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	// A default executor claims the default run and never the dmz run.
	got, err := store.Claim(ctx, "serve", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if got.ID != "run_default" {
		t.Errorf("default executor claimed %s, want run_default", got.ID)
	}
	if _, err := store.Claim(ctx, "serve", []string{""}); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("default executor second claim = %v, want ErrNonePending, the dmz run is off limits", err)
	}

	// A dmz worker claims the dmz run.
	dmz, err := store.Claim(ctx, "dmz-worker", []string{"dmz"})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if dmz.ID != "run_dmz" {
		t.Errorf("dmz worker claimed %s, want run_dmz", dmz.ID)
	}
}

// testLeaseLifecycle verifies heartbeats renew a lease and stale leases requeue pending runs and
// interrupt running ones.
func testLeaseLifecycle(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{
		ID: "run_q", Playbook: "p", Status: run.StatusPending, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	claimed, err := store.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}

	if err := store.Heartbeat(ctx, claimed.ID, "worker-a"); err != nil {
		t.Errorf("Heartbeat() error = %v", err)
	}
	if err := store.Heartbeat(ctx, claimed.ID, "impostor"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Heartbeat() wrong owner error = %v, want ErrNotFound", err)
	}

	// A fresh lease survives a sweep that only reclaims leases older than a minute.
	n, err := store.ReclaimStale(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	if n != 0 {
		t.Errorf("ReclaimStale() = %d, want 0 while the lease is fresh", n)
	}

	// A zero age makes every held lease stale: the pending run goes back in the queue.
	n, err = store.ReclaimStale(ctx, 0)
	if err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	if n != 1 {
		t.Errorf("ReclaimStale() = %d, want 1 requeued", n)
	}
	back, err := store.Get(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if back.ClaimedBy != "" || back.Status != run.StatusPending {
		t.Errorf("requeued run = %+v, want unclaimed pending", back)
	}

	// A stale lease on a running run interrupts it instead.
	reclaimed, err := store.Claim(ctx, "worker-b", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	reclaimed.Status = run.StatusRunning
	if err := store.Save(ctx, reclaimed); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if n, err = store.ReclaimStale(ctx, 0); err != nil || n != 1 {
		t.Fatalf("ReclaimStale() = %d, %v, want 1 interrupted", n, err)
	}
	gone, err := store.Get(ctx, reclaimed.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if gone.Status != run.StatusInterrupted || gone.EndedAt == nil {
		t.Errorf("stale running run = %+v, want interrupted with an end time", gone)
	}
	// The interrupt also releases the lease so the reclaimed worker's heartbeat stops matching and
	// the run cannot be resurrected through its stale owner.
	if gone.ClaimedBy != "" || gone.ClaimedAt != nil {
		t.Errorf("interrupted run still leased = %+v, want claimed_by and claimed_at cleared", gone)
	}
	if err := store.Heartbeat(ctx, gone.ID, "worker-b"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Heartbeat() on interrupted run = %v, want ErrNotFound", err)
	}
}

// testReclaimOrphans verifies a dead coordinator does not leave its children stranded. When a stale
// split or pipeline parent is interrupted, nothing is left to roll its children up, so a child no
// executor started must not stay pending and claimable, and one already executing must be told to
// stop. Without this a killed coordinator's shards run on with no parent to report them.
func testReclaimOrphans(t *testing.T, store run.Store) {
	ctx := context.Background()
	stale := time.Now().Add(-time.Hour)
	fresh := time.Now()
	parentID := "run_orphan_parent"
	parent := &run.Run{
		ID: parentID, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusRunning,
		ClaimedBy: "dead-coordinator", ClaimedAt: &stale, CreatedAt: stale,
	}
	idx0, idx1, count := 0, 1, 2
	queued := &run.Run{
		ID: "run_orphan_queued", Playbook: "site.yml", Status: run.StatusPending,
		ParentID: &parentID, ShardIndex: &idx0, ShardCount: &count, CreatedAt: stale,
	}
	executing := &run.Run{
		ID: "run_orphan_running", Playbook: "site.yml", Status: run.StatusRunning,
		ParentID: &parentID, ShardIndex: &idx1, ShardCount: &count, CreatedAt: stale,
		ClaimedBy: "live-worker", ClaimedAt: &fresh,
	}
	for _, r := range []*run.Run{parent, queued, executing} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	// Only the parent's lease is stale, so the executing child is swept as an orphan rather than
	// as a lease that expired on its own.
	if _, err := store.ReclaimStale(ctx, 30*time.Minute); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}

	gotParent, err := store.Get(ctx, parentID)
	if err != nil {
		t.Fatalf("Get(parent) error = %v", err)
	}
	if gotParent.Status != run.StatusInterrupted {
		t.Fatalf("parent status = %q, want interrupted", gotParent.Status)
	}
	gotQueued, err := store.Get(ctx, queued.ID)
	if err != nil {
		t.Fatalf("Get(queued) error = %v", err)
	}
	if gotQueued.Status != run.StatusCanceled {
		t.Errorf("queued child status = %q, want canceled, not left claimable", gotQueued.Status)
	}
	if gotQueued.EndedAt == nil {
		t.Error("queued child has no end time, so it never finished")
	}
	if gotQueued.Error != run.OrphanError() {
		t.Errorf("queued child error = %q, want %q", gotQueued.Error, run.OrphanError())
	}
	gotRunning, err := store.Get(ctx, executing.ID)
	if err != nil {
		t.Fatalf("Get(executing) error = %v", err)
	}
	if !gotRunning.CancelRequested {
		t.Error("executing child was not asked to stop, so it runs on with no parent to report it")
	}

	// The queued child is no longer work: a claim must not hand it out.
	if claimed, err := store.Claim(ctx, "worker-x", []string{""}); err == nil {
		t.Errorf("Claim() returned %s, want nothing claimable after the sweep", claimed.ID)
	} else if !errors.Is(err, run.ErrNonePending) {
		t.Errorf("Claim() error = %v, want ErrNonePending", err)
	}
}

// testRequestCancel verifies the cancel flag round trips and unknown runs report ErrNotFound.
func testRequestCancel(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{
		ID: "run_c", Playbook: "p", Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.RequestCancel(ctx, "run_c"); err != nil {
		t.Fatalf("RequestCancel() error = %v", err)
	}
	got, err := store.Get(ctx, "run_c")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.CancelRequested {
		t.Error("CancelRequested not set after RequestCancel")
	}
	if err := store.RequestCancel(ctx, "ghost"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("RequestCancel(ghost) error = %v, want ErrNotFound", err)
	}
}

// testWorkers verifies executors are listed from their leases with active counts and freshness,
// and that a lease older than run.WorkerWindow is excluded.
func testWorkers(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	older, newer := base, base.Add(time.Minute)
	stale := base.Add(-run.WorkerWindow)
	for _, r := range []*run.Run{
		{ID: "r1", Playbook: "p", Status: run.StatusRunning, CreatedAt: base, ClaimedBy: "goat-1", ClaimedAt: &newer},
		{ID: "r2", Playbook: "p", Status: run.StatusSucceeded, CreatedAt: base, ClaimedBy: "goat-1", ClaimedAt: &older},
		{ID: "r3", Playbook: "p", Status: run.StatusRunning, CreatedAt: base, ClaimedBy: "serve-1", ClaimedAt: &older},
		{ID: "r4", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "r5", Playbook: "p", Status: run.StatusSucceeded, CreatedAt: stale, ClaimedBy: "ghost-1", ClaimedAt: &stale},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	workers, err := store.Workers(ctx)
	if err != nil {
		t.Fatalf("Workers() error = %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(workers))
	}
	if workers[0].Owner != "goat-1" || workers[0].Active != 1 || workers[0].Completed != 1 || !workers[0].LastSeen.Equal(newer) {
		t.Errorf("first worker = %+v, want goat-1 active 1 completed 1 seen %v", workers[0], newer)
	}
	if workers[1].Owner != "serve-1" || workers[1].Active != 1 {
		t.Errorf("second worker = %+v, want serve-1 active 1", workers[1])
	}
}

// testLogAfter verifies the log cursor: a read from zero returns the whole log, a cursor taken at
// any read boundary resumes with exactly the bytes appended after it, and a cursor at the end
// returns nothing. Chunk boundaries are a store detail, so only concatenations are asserted.
func testLogAfter(t *testing.T, store run.Store) {
	ctx := context.Background()
	lcRun := sampleRun("run_lc")
	lcRun.Status = run.StatusRunning
	if err := store.Save(ctx, lcRun); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.AppendLog(ctx, "run_lc", []byte("one")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}

	first, err := store.LogAfter(ctx, "run_lc", 0, 0)
	if err != nil {
		t.Fatalf("LogAfter() error = %v", err)
	}
	if got := concatChunks(first); got != "one" {
		t.Errorf("LogAfter(0) = %q, want %q", got, "one")
	}
	cursor := first[len(first)-1].Seq
	if last, err := store.LastLogSeq(ctx, "run_lc"); err != nil || last != cursor {
		t.Errorf("LastLogSeq() = (%d, %v), want (%d, nil)", last, err, cursor)
	}

	if err := store.AppendLog(ctx, "run_lc", []byte("two")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	rest, err := store.LogAfter(ctx, "run_lc", cursor, 0)
	if err != nil {
		t.Fatalf("LogAfter(cursor) error = %v", err)
	}
	if got := concatChunks(rest); got != "two" {
		t.Errorf("LogAfter(cursor) = %q, want %q", got, "two")
	}

	end, err := store.LastLogSeq(ctx, "run_lc")
	if err != nil {
		t.Fatalf("LastLogSeq() error = %v", err)
	}
	if tail, err := store.LogAfter(ctx, "run_lc", end, 0); err != nil || len(tail) != 0 {
		t.Errorf("LogAfter(end) = (%d chunks, %v), want none", len(tail), err)
	}

	if _, err := store.LogAfter(ctx, "ghost", 0, 0); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("LogAfter(ghost) error = %v, want ErrNotFound", err)
	}
	if _, err := store.LastLogSeq(ctx, "ghost"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("LastLogSeq(ghost) error = %v, want ErrNotFound", err)
	}
}

// concatChunks joins chunk bytes in order for comparing log contents.
func concatChunks(chunks []run.LogChunk) string {
	var out []byte
	for _, c := range chunks {
		out = append(out, c.Data...)
	}
	return string(out)
}

// testCancelPending verifies the unclaimed-run cancel: it terminalizes a waiting pending or
// pending_approval run, refuses a claimed, executing, terminal, or missing run, and stamps the
// end time.
func testCancelPending(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	claimed := base.Add(time.Minute)
	for _, r := range []*run.Run{
		{ID: "run_wait", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "run_held", Playbook: "p", Status: run.StatusPendingApproval, CreatedAt: base},
		{ID: "run_taken", Playbook: "p", Status: run.StatusPending, CreatedAt: base, ClaimedBy: "w1", ClaimedAt: &claimed},
		{ID: "run_live", Playbook: "p", Status: run.StatusRunning, CreatedAt: base, ClaimedBy: "w1", ClaimedAt: &claimed},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	ok, err := store.CancelPending(ctx, "run_wait")
	if err != nil || !ok {
		t.Fatalf("CancelPending(run_wait) = (%v, %v), want (true, nil)", ok, err)
	}
	got, err := store.Get(ctx, "run_wait")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusCanceled || got.EndedAt == nil {
		t.Errorf("canceled run = %q ended %v, want canceled with an end time", got.Status, got.EndedAt)
	}
	if ok, err := store.CancelPending(ctx, "run_wait"); err != nil || ok {
		t.Errorf("second CancelPending = (%v, %v), want (false, nil)", ok, err)
	}

	if ok, err := store.CancelPending(ctx, "run_held"); err != nil || !ok {
		t.Errorf("CancelPending(run_held) = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := store.CancelPending(ctx, "run_taken"); err != nil || ok {
		t.Errorf("CancelPending(run_taken) = (%v, %v), want (false, nil)", ok, err)
	}
	if r, _ := store.Get(ctx, "run_taken"); r.Status != run.StatusPending {
		t.Errorf("claimed run status = %q, want pending untouched", r.Status)
	}
	if ok, err := store.CancelPending(ctx, "run_live"); err != nil || ok {
		t.Errorf("CancelPending(run_live) = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := store.CancelPending(ctx, "ghost"); err != nil || ok {
		t.Errorf("CancelPending(ghost) = (%v, %v), want (false, nil)", ok, err)
	}
}

// testSaveKeepsCancel verifies the sticky cancel flag: replacing a run from a snapshot taken
// before the cancel was requested must not erase the stored flag.
func testSaveKeepsCancel(t *testing.T, store run.Store) {
	ctx := context.Background()
	r := &run.Run{ID: "run_sc", Playbook: "p", Status: run.StatusPending,
		CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.RequestCancel(ctx, "run_sc"); err != nil {
		t.Fatalf("RequestCancel() error = %v", err)
	}

	stale := r.Clone()
	stale.Status = run.StatusRunning
	stale.CancelRequested = false
	if err := store.Save(ctx, stale); err != nil {
		t.Fatalf("Save(stale) error = %v", err)
	}

	got, err := store.Get(ctx, "run_sc")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.CancelRequested {
		t.Error("cancel flag erased by a stale save, want it kept")
	}
	if got.Status != run.StatusRunning {
		t.Errorf("status = %q, want running from the save", got.Status)
	}
}

// testClaimSkipsCancel verifies a pending run whose cancel was requested is never claimed.
func testClaimSkipsCancel(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range []*run.Run{
		{ID: "run_stop", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "run_go", Playbook: "p", Status: run.StatusPending, CreatedAt: base.Add(time.Minute)},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	if err := store.RequestCancel(ctx, "run_stop"); err != nil {
		t.Fatalf("RequestCancel() error = %v", err)
	}

	got, err := store.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if got.ID != "run_go" {
		t.Errorf("claimed %s, want run_go; the cancel-requested run must be skipped", got.ID)
	}
	if _, err := store.Claim(ctx, "worker-b", []string{""}); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("second Claim() error = %v, want ErrNonePending", err)
	}
}

// testHostCosts verifies per host durations persist and average over the recent window.
func testHostCosts(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	save := func(runID string, at time.Time, durations map[string]float64) {
		var sums []run.HostSummary
		for host, d := range durations {
			sums = append(sums, run.HostSummary{Host: host, Worst: "ok", DurationSeconds: d, RanAt: at})
		}
		if err := store.SaveHostSummary(ctx, runID, sums); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
	}
	save("r1", base, map[string]float64{"db01": 30, "web01": 4})
	save("r2", base.Add(time.Hour), map[string]float64{"db01": 20, "web01": 2})
	save("r3", base.Add(2*time.Hour), map[string]float64{"db01": 10})

	costs, err := store.HostCosts(ctx, 10)
	if err != nil {
		t.Fatalf("HostCosts() error = %v", err)
	}
	want := map[string]float64{"db01": 20, "web01": 3}
	if diff := cmp.Diff(want, costs, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("HostCosts() mismatch (-want +got):\n%s", diff)
	}

	windowed, err := store.HostCosts(ctx, 1)
	if err != nil {
		t.Fatalf("HostCosts() error = %v", err)
	}
	wantWindowed := map[string]float64{"db01": 10, "web01": 2}
	if diff := cmp.Diff(wantWindowed, windowed, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("HostCosts(window 1) mismatch (-want +got):\n%s", diff)
	}

	empty, err := store.HostCosts(ctx, 0)
	if err != nil {
		t.Fatalf("HostCosts() error = %v", err)
	}
	if diff := cmp.Diff(wantWindowed, empty, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("HostCosts(window 0) should clamp to 1 (-want +got):\n%s", diff)
	}
}

// testPurge verifies retention: events and runs older than the cutoff are removed while newer runs,
// non-terminal runs, and the summaries that power cross-run views survive.
func testPurge(t *testing.T, store run.Store) {
	ctx := context.Background()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// An old terminal run with events, a log, and a summary. Its output is recorded while it is still
	// running, as a real run does, then it finalizes, since the store fences writes to a terminal run.
	if err := store.Save(ctx, &run.Run{ID: "old", Status: run.StatusRunning, CreatedAt: old}); err != nil {
		t.Fatalf("Save(old) error = %v", err)
	}
	if err := store.AppendEvents(ctx, "old",
		[]event.Event{{Type: event.TypePlayStart, Time: old, Play: "p"}}); err != nil {
		t.Fatalf("AppendEvents(old) error = %v", err)
	}
	if err := store.AppendLog(ctx, "old", []byte("old output")); err != nil {
		t.Fatalf("AppendLog(old) error = %v", err)
	}
	if err := store.SaveHostSummary(ctx, "old",
		[]run.HostSummary{{Host: "h1", Worst: "ok", RanAt: old}}); err != nil {
		t.Fatalf("SaveHostSummary(old) error = %v", err)
	}
	if err := store.Save(ctx, &run.Run{ID: "old", Status: run.StatusSucceeded, CreatedAt: old}); err != nil {
		t.Fatalf("Save(old) finalize error = %v", err)
	}
	// A recent terminal run and an old run still running.
	if err := store.Save(ctx, &run.Run{ID: "recent", Status: run.StatusRunning, CreatedAt: recent}); err != nil {
		t.Fatalf("Save(recent) error = %v", err)
	}
	if err := store.AppendEvents(ctx, "recent",
		[]event.Event{{Type: event.TypePlayStart, Time: recent, Play: "p"}}); err != nil {
		t.Fatalf("AppendEvents(recent) error = %v", err)
	}
	if err := store.Save(ctx, &run.Run{ID: "recent", Status: run.StatusSucceeded, CreatedAt: recent}); err != nil {
		t.Fatalf("Save(recent) finalize error = %v", err)
	}
	if err := store.Save(ctx, &run.Run{ID: "running", Status: run.StatusRunning, CreatedAt: old}); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	// An old run waiting for an approver. It is not terminal, so retention must leave it alone: a
	// hold can outlive any window, and deleting one silently discards work someone still has to
	// decide on.
	if err := store.Save(ctx, &run.Run{
		ID: "held", Status: run.StatusPendingApproval, CreatedAt: old,
	}); err != nil {
		t.Fatalf("Save(held) error = %v", err)
	}

	// Trimming events keeps the run record but drops its events.
	trimmed, err := store.PurgeEventsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeEventsBefore() error = %v", err)
	}
	if trimmed != 1 {
		t.Errorf("PurgeEventsBefore() trimmed = %d, want 1", trimmed)
	}
	if evs, err := store.Events(ctx, "old"); err != nil || len(evs) != 0 {
		t.Errorf("old events = %v (err %v), want empty", evs, err)
	}
	if _, err := store.Get(ctx, "old"); err != nil {
		t.Errorf("old run gone after event purge: %v", err)
	}
	if evs, _ := store.Events(ctx, "recent"); len(evs) != 1 {
		t.Errorf("recent events = %v, want kept", evs)
	}

	// Deleting old runs removes the record but keeps its summary and never touches newer or running.
	deleted, err := store.PurgeRunsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	if deleted != 1 {
		t.Errorf("PurgeRunsBefore() deleted = %d, want 1", deleted)
	}
	if _, err := store.Get(ctx, "old"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Get(old) error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, "held"); err != nil {
		t.Errorf("run awaiting approval was purged: %v", err)
	}
	if _, err := store.Get(ctx, "recent"); err != nil {
		t.Errorf("recent run deleted: %v", err)
	}
	if _, err := store.Get(ctx, "running"); err != nil {
		t.Errorf("running run deleted despite being non-terminal: %v", err)
	}
	history, err := store.HostHistory(ctx, "h1", 10)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	if len(history) != 1 {
		t.Errorf("HostHistory() = %v, want the summary kept after run purge", history)
	}
}

// testTerminalFence verifies the store fences auxiliary writes to a terminal run: a reclaimed-but-alive
// worker's late logs, events, and summaries are dropped rather than appended or overwritten, and the
// run is not resurrected. The writes return no error, so a benign late write does not look like a
// failure.
func testTerminalFence(t *testing.T, store run.Store) {
	ctx := context.Background()
	created := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	// The run records its output while running.
	if err := store.Save(ctx, &run.Run{ID: "fx", Status: run.StatusRunning, CreatedAt: created}); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	if err := store.AppendLog(ctx, "fx", []byte("early")); err != nil {
		t.Fatalf("AppendLog(early) error = %v", err)
	}
	if err := store.AppendEvents(ctx, "fx",
		[]event.Event{{Type: event.TypePlayStart, Time: created, Play: "p"}}); err != nil {
		t.Fatalf("AppendEvents(early) error = %v", err)
	}

	// It finalizes.
	if err := store.Save(ctx, &run.Run{ID: "fx", Status: run.StatusSucceeded, CreatedAt: created}); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}

	// A reclaimed-but-alive worker's late writes are dropped, not errors.
	if err := store.AppendLog(ctx, "fx", []byte("late")); err != nil {
		t.Errorf("AppendLog(late) error = %v, want a silent no-op", err)
	}
	if err := store.AppendEvents(ctx, "fx",
		[]event.Event{{Type: event.TypePlayStart, Time: created, Play: "zombie"}}); err != nil {
		t.Errorf("AppendEvents(late) error = %v, want a silent no-op", err)
	}
	if err := store.SaveHostSummary(ctx, "fx",
		[]run.HostSummary{{Host: "zombie", Worst: "failures", RanAt: created}}); err != nil {
		t.Errorf("SaveHostSummary(late) error = %v, want a silent no-op", err)
	}
	if err := store.SaveTaskSummary(ctx, "fx",
		[]run.TaskSummary{{Task: "zombie", Seconds: 9}}); err != nil {
		t.Errorf("SaveTaskSummary(late) error = %v, want a silent no-op", err)
	}

	// The late writes landed nowhere and the run keeps its terminal state.
	if body, err := store.Log(ctx, "fx"); err != nil || string(body) != "early" {
		t.Errorf("Log = %q (err %v), want %q with the late write dropped", body, err, "early")
	}
	if evs, err := store.Events(ctx, "fx"); err != nil || len(evs) != 1 {
		t.Errorf("Events len = %d (err %v), want only the pre-terminal event", len(evs), err)
	}
	if got, err := store.Get(ctx, "fx"); err != nil || got.Status != run.StatusSucceeded {
		t.Errorf("run status = %v (err %v), want succeeded, not resurrected", got.Status, err)
	}
}

// testProvenance verifies the provenance and label fields round trip through Save and Get.
func testProvenance(t *testing.T, store run.Store) {
	ctx := context.Background()
	saved := &run.Run{
		ID: "run_prov", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		Source:    "template", SourceID: "tpl_9", Actor: "douglas",
		RerunOf: "run_prev", Labels: map[string]string{"env": "prod", "ticket": "OPS-1"},
	}
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "run_prov")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Source != "template" || got.SourceID != "tpl_9" || got.Actor != "douglas" ||
		got.RerunOf != "run_prev" {
		t.Errorf("provenance = %q %q %q %q, want template tpl_9 douglas run_prev",
			got.Source, got.SourceID, got.Actor, got.RerunOf)
	}
	if diff := cmp.Diff(saved.Labels, got.Labels); diff != "" {
		t.Errorf("labels mismatch (-want +got):\n%s", diff)
	}
}

// testWarning verifies a run's warning round trips through Save and Get without touching its status.
// A run whose event capture failed finishes succeeded but has nothing to show, so the warning is the
// only thing that explains the empty matrix and it has to survive the store.
func testWarning(t *testing.T, store run.Store) {
	ctx := context.Background()
	saved := &run.Run{
		ID: "run_warned", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		Warning:   "event capture unavailable: no space left on device",
	}
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "run_warned")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(saved.Warning, got.Warning); diff != "" {
		t.Errorf("warning mismatch (-want +got):\n%s", diff)
	}
	if got.Status != run.StatusSucceeded {
		t.Errorf("status = %q, want succeeded left alone by the warning", got.Status)
	}
}

// testPipelineSteps verifies a pipeline's step graph round trips through Save and Get, including the
// dependencies between steps. A pipeline held for approval is executed from the stored graph, so
// losing it would mean an approved workflow could no longer run, and an ordinary run must store no
// steps at all rather than an empty list.
func testPipelineSteps(t *testing.T, store run.Store) {
	ctx := context.Background()
	saved := &run.Run{
		ID: "run_pipe", Playbook: "release", Kind: run.KindPipeline,
		Status: run.StatusPendingApproval, CreatedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		Steps: []run.PipelineStep{
			{Name: "plan", Tool: "terraform", Command: "terraform plan"},
			{Name: "apply", Tool: "terraform", Command: "terraform apply", DependsOn: []string{"plan"},
				Retries: 2, ContinueOnFailure: true},
		},
	}
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "run_pipe")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(saved.Steps, got.Steps); diff != "" {
		t.Errorf("steps mismatch (-want +got):\n%s", diff)
	}

	plain := &run.Run{
		ID: "run_plain", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}
	if err := store.Save(ctx, plain); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	gotPlain, err := store.Get(ctx, "run_plain")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(gotPlain.Steps) != 0 {
		t.Errorf("plain run steps = %v, want none", gotPlain.Steps)
	}
}

// testHostFacts verifies gathered facts round trip per host, that a later gather replaces an
// earlier one, and that a host nobody has gathered reports not found rather than an empty record.
func testHostFacts(t *testing.T, store run.Store) {
	ctx := context.Background()
	at := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)

	if _, err := store.HostFactsFor(ctx, "never-gathered"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("HostFactsFor(unknown) error = %v, want ErrNotFound", err)
	}

	first := []run.HostFacts{
		{Host: "web01", Facts: map[string]string{"distribution": "Debian", "kernel": "6.1.0"}, GatheredAt: at},
		{Host: "db01", Facts: map[string]string{"distribution": "Ubuntu"}, GatheredAt: at},
	}
	if err := store.SaveHostFacts(ctx, "run_1", first); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}
	got, err := store.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if got.Facts["distribution"] != "Debian" || got.Facts["kernel"] != "6.1.0" || got.RunID != "run_1" {
		t.Errorf("facts = %+v, want the Debian gather from run_1", got)
	}

	// A later gather replaces the earlier one, since the newest is the truth about a host.
	later := at.Add(time.Hour)
	if err := store.SaveHostFacts(ctx, "run_2", []run.HostFacts{
		{Host: "web01", Facts: map[string]string{"distribution": "Debian", "kernel": "6.6.0"}, GatheredAt: later},
	}); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}
	got, err = store.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if got.Facts["kernel"] != "6.6.0" || got.RunID != "run_2" {
		t.Errorf("facts after regather = %+v, want the run_2 gather", got)
	}

	// The other host is untouched by that replacement.
	other, err := store.HostFactsFor(ctx, "db01")
	if err != nil || other.Facts["distribution"] != "Ubuntu" {
		t.Errorf("db01 facts = %+v, err %v, want the original Ubuntu gather", other, err)
	}

	// An empty set is a no-op rather than an error, so a run that gathered nothing is fine.
	if err := store.SaveHostFacts(ctx, "run_3", nil); err != nil {
		t.Errorf("SaveHostFacts(nil) error = %v, want nil", err)
	}
}

// testReclaimAbandonedParents verifies a split or pipeline parent that no coordinator ever started
// does not strand its children.
//
// A parent is saved before its children, and the coordinator that would run it starts only after
// every child is written. A child save that fails, or a process that dies in that window, leaves the
// parent pending with no lease. Nothing claims a run with a kind, so no worker will ever take it,
// and orphan resolution only fires for an interrupted parent, so the parent sat pending forever
// while its children stayed claimable and ran with nothing to roll them up.
//
// A parent awaiting approval is the case this must not touch. It is resting for as long as a person
// takes to decide, and Approve starts its coordinator, so sweeping it would cancel every gated split
// and workflow that outlived one sweep interval.
func testReclaimAbandonedParents(t *testing.T, store run.Store) {
	ctx := context.Background()
	stale := time.Now().Add(-time.Hour)
	abandonedID, heldID, freshID := "run_abandoned", "run_held", "run_fresh_parent"
	idx, count := 0, 1
	child := func(id, parent string, status run.Status) *run.Run {
		p := parent
		return &run.Run{
			ID: id, Playbook: "site.yml", Status: status, ParentID: &p,
			ShardIndex: &idx, ShardCount: &count, CreatedAt: stale,
		}
	}
	saved := []*run.Run{
		// A parent whose coordinator never started, old enough to be past any sweep cutoff.
		{ID: abandonedID, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusPending,
			CreatedAt: stale},
		child("run_abandoned_shard", abandonedID, run.StatusPending),
		// A parent held for an approver, equally old. It is waiting on a person, not abandoned.
		{ID: heldID, Playbook: "site.yml", Kind: run.KindSplit,
			Status: run.StatusPendingApproval, CreatedAt: stale},
		child("run_held_shard", heldID, run.StatusPendingApproval),
		// A parent submitted just now, whose coordinator is about to save it running.
		{ID: freshID, Playbook: "site.yml", Kind: run.KindPipeline, Status: run.StatusPending,
			CreatedAt: time.Now()},
	}
	for _, r := range saved {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	if _, err := store.ReclaimStale(ctx, 30*time.Minute); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}

	gotAbandoned, err := store.Get(ctx, abandonedID)
	if err != nil {
		t.Fatalf("Get(abandoned) error = %v", err)
	}
	if gotAbandoned.Status != run.StatusInterrupted {
		t.Errorf("abandoned parent status = %q, want interrupted: nothing claims a run with a "+
			"kind, so it waits forever while its children stay claimable", gotAbandoned.Status)
	}
	if gotAbandoned.EndedAt == nil {
		t.Error("abandoned parent has no end time, so it never finished")
	}
	if gotAbandoned.Error != run.AbandonedParentError() {
		t.Errorf("abandoned parent error = %q, want %q", gotAbandoned.Error,
			run.AbandonedParentError())
	}
	// Interrupting the parent is only worth doing because it settles the children in the same sweep.
	gotShard, err := store.Get(ctx, "run_abandoned_shard")
	if err != nil {
		t.Fatalf("Get(abandoned shard) error = %v", err)
	}
	if gotShard.Status != run.StatusCanceled {
		t.Errorf("shard of an abandoned parent status = %q, want canceled: a pending shard is "+
			"claimable and runs with nothing to roll it up", gotShard.Status)
	}

	gotHeld, err := store.Get(ctx, heldID)
	if err != nil {
		t.Fatalf("Get(held) error = %v", err)
	}
	if gotHeld.Status != run.StatusPendingApproval {
		t.Errorf("held parent status = %q, want pending_approval: a run awaiting a person was "+
			"canceled for taking longer than one sweep interval", gotHeld.Status)
	}
	gotHeldShard, err := store.Get(ctx, "run_held_shard")
	if err != nil {
		t.Fatalf("Get(held shard) error = %v", err)
	}
	if gotHeldShard.Status != run.StatusPendingApproval {
		t.Errorf("shard of a held parent status = %q, want pending_approval", gotHeldShard.Status)
	}

	gotFresh, err := store.Get(ctx, freshID)
	if err != nil {
		t.Fatalf("Get(fresh) error = %v", err)
	}
	if gotFresh.Status != run.StatusPending {
		t.Errorf("freshly submitted parent status = %q, want pending: its coordinator was still "+
			"starting", gotFresh.Status)
	}
}

// testReclaimApprovedParentWithNoCoordinator verifies a parent released by an approval, whose
// coordinator never arrived, is still settled.
//
// An approved parent goes straight to running so the sweep cannot catch it in the instant before its
// coordinator claims it. That leaves running-and-unclaimed as the state meaning the coordinator
// never arrived, and neither the lease sweep, which only looks at leased runs, nor a pending-only
// abandoned rule covers it. Without this the fix for one race created a parent nothing would finish.
func testReclaimApprovedParentWithNoCoordinator(t *testing.T, store run.Store) {
	ctx := context.Background()
	stale := time.Now().Add(-time.Hour)
	parentID := "run_approved_no_coordinator"
	idx, count := 0, 1
	child := parentID
	saved := []*run.Run{
		// Released by an approval, then the process handling it went away.
		{ID: parentID, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusRunning,
			CreatedAt: stale},
		{ID: "run_approved_shard", Playbook: "site.yml", Status: run.StatusPending,
			ParentID: &child, ShardIndex: &idx, ShardCount: &count, CreatedAt: stale},
	}
	for _, r := range saved {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	if _, err := store.ReclaimStale(ctx, 30*time.Minute); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	got, err := store.Get(ctx, parentID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusInterrupted {
		t.Errorf("parent status = %q, want interrupted: running with no lease means no coordinator "+
			"ever claimed it, and nothing else sweeps that state", got.Status)
	}
	shard, err := store.Get(ctx, "run_approved_shard")
	if err != nil {
		t.Fatalf("Get(shard) error = %v", err)
	}
	if shard.Status != run.StatusCanceled {
		t.Errorf("shard status = %q, want canceled: it is claimable under a parent with no "+
			"coordinator", shard.Status)
	}
}

// testReclaimLeavesACoordinatedParentAlone verifies the widened rule does not touch a parent whose
// coordinator holds it, which is every healthy split and pipeline.
func testReclaimLeavesACoordinatedParentAlone(t *testing.T, store run.Store) {
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	now := time.Now()
	healthy := &run.Run{
		ID: "run_coordinated", Playbook: "site.yml", Kind: run.KindSplit,
		Status: run.StatusRunning, CreatedAt: old, ClaimedBy: "coordinator-a", ClaimedAt: &now,
	}
	if err := store.Save(ctx, healthy); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := store.ReclaimStale(ctx, 30*time.Minute); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	got, err := store.Get(ctx, "run_coordinated")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRunning {
		t.Errorf("a coordinated parent was swept: status = %q. Its lease is fresh, so it is being "+
			"actively coordinated", got.Status)
	}
}
