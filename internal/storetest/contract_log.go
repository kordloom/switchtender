package storetest

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
	"github.com/kordloom/switchtender/internal/run"
)

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
	// An old terminal run that never recorded events or logs. It is eligible for trimming but has
	// nothing to remove, so it must not inflate the trimmed count. This pins the one definition of
	// trimmed across backends: counting every eligible run would report two here, counting only
	// runs whose data was removed reports one, and all three stores must agree on the latter.
	if err := store.Save(ctx, &run.Run{
		ID: "bare", Status: run.StatusSucceeded, CreatedAt: old,
	}); err != nil {
		t.Fatalf("Save(bare) error = %v", err)
	}

	// Trimming events keeps the run record but drops its events. Only "old" held data, so "bare" is
	// not counted even though it is an eligible terminal-old run.
	trimmed, err := store.PurgeEventsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeEventsBefore() error = %v", err)
	}
	if trimmed != 1 {
		t.Errorf("PurgeEventsBefore() trimmed = %d, want 1", trimmed)
	}
	if _, err := store.Get(ctx, "bare"); err != nil {
		t.Errorf("bare run gone after event purge: %v", err)
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
	// Both "old" and the event-free "bare" run are terminal and older than cutoff, so both go.
	deleted, err := store.PurgeRunsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	if deleted != 2 {
		t.Errorf("PurgeRunsBefore() deleted = %d, want 2", deleted)
	}
	if _, err := store.Get(ctx, "old"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Get(old) error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, "bare"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Get(bare) error = %v, want ErrNotFound", err)
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

// testLogCap pins that captured output stops at run.MaxLogBytes and that the run says it was cut.
//
// The request carrying one chunk is capped by middleware, but the accumulated total was not, so a
// run printing in a loop grew the database until the disk was full. The audit chain lives in the
// same database, so this took the evidence down with the product, and it was reachable by accident
// from an ordinary playbook. Truncating without saying so would be its own fault: a log that simply
// stops reads as a run that went quiet.
func testLogCap(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	r := &run.Run{ID: "run_cap", Playbook: "p.yml", Status: run.StatusRunning, CreatedAt: time.Now()}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// Chunked rather than one huge write, since chunks are how a run actually produces output and
	// the cap has to hold across them rather than inside one.
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for written := 0; written < run.MaxLogBytes+(4<<20); written += len(chunk) {
		if err := store.AppendLog(ctx, "run_cap", chunk); err != nil {
			t.Fatalf("AppendLog() error = %v", err)
		}
	}
	got, err := store.Log(ctx, "run_cap")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	// One chunk of overshoot is allowed: the write that crosses the line is admitted whole rather
	// than split, which keeps the fence a single statement.
	if len(got) > run.MaxLogBytes+len(chunk) {
		t.Errorf("stored log is %d bytes, want no more than %d",
			len(got), run.MaxLogBytes+len(chunk))
	}
	if len(got) == 0 {
		t.Error("the cap swallowed the whole log, so nothing was captured at all")
	}
	after, err := store.Get(ctx, "run_cap")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if after.Warning != run.LogTruncatedWarning {
		t.Errorf("run warning = %q, want the truncation notice so a reader knows the log is "+
			"incomplete", after.Warning)
	}
}
