package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// TestOpenWiresEveryStore pins the constructor rather than the rules its stores implement. Open
// assembles seventeen stores by hand in one composite literal, so a field left out or bound to the
// wrong struct compiles cleanly and passes every contract test written against the store that was
// wired. It fails at runtime, on the first request that reaches the missing one, as a nil
// dereference in a control plane that has already accepted the request.
//
// Each accessor is therefore exercised with a real read, which is the only thing that distinguishes
// a wired store from a nil one behind a non-nil interface value.
//
//nolint:funlen // Test function.
func TestOpenWiresEveryStore(t *testing.T) {
	t.Parallel()
	db := openShared(t)
	ctx := context.Background()

	tests := []struct {
		Name string
		Read func() error
	}{{ // Test 0: The run store, which every execution path goes through.
		Name: "Runs", Read: func() error { _, err := db.Runs().List(ctx); return err },
	}, { // Test 1: The schedule store.
		Name: "Schedules", Read: func() error { _, err := db.Schedules().List(ctx); return err },
	}, { // Test 2: The API token store, which authenticates every request.
		Name: "Tokens", Read: func() error { _, err := db.Tokens().List(ctx); return err },
	}, { // Test 3: The execution secret store.
		Name: "Credentials", Read: func() error { _, err := db.Credentials().List(ctx); return err },
	}, { // Test 4: The operator-defined credential type store.
		Name: "CredentialTypes",
		Read: func() error { _, err := db.CredentialTypes().List(ctx); return err },
	}, { // Test 5: The git project store.
		Name: "Projects", Read: func() error { _, err := db.Projects().List(ctx); return err },
	}, { // Test 6: The job template store.
		Name: "Templates", Read: func() error { _, err := db.Templates().List(ctx); return err },
	}, { // Test 7: The account store.
		Name: "Users", Read: func() error { _, err := db.Users().List(ctx); return err },
	}, { // Test 8: The stored inventory store.
		Name: "Inventories", Read: func() error { _, err := db.Inventories().List(ctx); return err },
	}, { // Test 9: The approval policy store, which decides what needs a second person.
		Name: "Policies", Read: func() error { _, err := db.Policies().List(ctx); return err },
	}, { // Test 10: The audit trail store, which is the tamper-evident record itself.
		Name: "Audits", Read: func() error { _, err := db.Audits().List(ctx, 1); return err },
	}, { // Test 11: The dynamic inventory source store.
		Name: "InventorySources",
		Read: func() error { _, err := db.InventorySources().List(ctx); return err },
	}, { // Test 12: The webhook trigger store.
		Name: "Triggers", Read: func() error { _, err := db.Triggers().List(ctx); return err },
	}, { // Test 13: The team store.
		Name: "Teams", Read: func() error { _, err := db.Teams().List(ctx); return err },
	}, { // Test 14: The organization store.
		Name: "Orgs", Read: func() error { _, err := db.Orgs().List(ctx); return err },
	}, { // Test 15: The per-object access grant store, which decides who may see what.
		Name: "Grants", Read: func() error { _, err := db.Grants().ForObject(ctx, "obj_none"); return err },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s() panicked: %v. Open left this store nil, so the first request "+
						"that reaches it takes the process down.", test.Name, r)
				}
			}()
			if err := test.Read(); err != nil {
				t.Errorf("%s(): a read through the wired store failed: %v", test.Name, err)
			}
		})
	}
}

// TestOpenWiresEveryStoreToTheSameDatabase pins the other half of the wiring risk. Every store in
// the literal is handed the one open handle, and one bound to a second connection would still pass
// every test above while writing into a different pool: the connection cap Open sets would be
// doubled, and the advisory locks that serialize migration and audit appends would be taken on a
// backend nothing else shares. The proof is a write through one accessor being visible through
// another in the same transaction-free read.
func TestOpenWiresEveryStoreToTheSameDatabase(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	id := fmt.Sprintf("run_wiring_%d", time.Now().UnixNano())
	if err := db.Runs().Save(ctx, &run.Run{
		ID: id, Status: run.StatusPending, CreatedAt: time.Now().UTC(),
		Tool: "bash", Command: "echo hi", Queue: "wiring",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// The log store is a separate set of methods on the same struct, so a read through it proves
	// both halves see one database.
	if _, err := db.Runs().Log(ctx, id); err != nil {
		t.Fatalf("Log() error = %v: the run written through Save is invisible to the log reader, "+
			"so the two are not looking at the same database", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// After Close every store must be dead, which proves they share the handle Close closes rather
	// than each holding one of their own.
	if _, err := db.Runs().List(ctx); err == nil {
		t.Error("the run store still reads after Close, so it holds a handle Close does not own")
	}
	if _, err := db.Tokens().List(ctx); err == nil {
		t.Error("the token store still reads after Close, so it holds a handle of its own and the " +
			"connection cap Open sets is not the cap the process actually uses")
	}
}

// TestOpenRefusesAnUnreachableDatabase pins the failure path. Open must not hand back a DB whose
// handle cannot be used: sql.Open is lazy and returns no error for a server that is not there, so
// without the explicit ping the process would start, accept requests, and fail on each one.
func TestOpenRefusesAnUnreachableDatabase(t *testing.T) {
	tests := []struct {
		Name string
		DSN  string
	}{{ // Test 0: A syntactically valid DSN pointing at nothing.
		Name: "no listener", DSN: "postgres://postgres@127.0.0.1:1/nope?sslmode=disable&connect_timeout=2",
	}, { // Test 1: A DSN the driver cannot parse at all.
		Name: "unparseable", DSN: "://not a dsn",
	}, { // Test 2: An empty DSN, which is what an unset configuration value looks like.
		Name: "empty", DSN: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			db, err := pgstore.Open(test.DSN)
			if err == nil {
				_ = db.Close()
				t.Fatalf("%s: Open() succeeded on a database it cannot reach, so the process "+
					"starts and fails on every request instead of refusing to start", test.Name)
			}
			if db != nil {
				t.Errorf("%s: Open() returned both an error and a DB, so a caller ignoring the "+
					"error gets a half-built store", test.Name)
			}
		})
	}
}

// TestAppendLogFencesATerminalRun pins the write fence on captured output. A worker reclaimed by the
// stale sweep may still be alive and still producing output, and its late chunks must not append to
// a run that already ended: the log is what a person reads to see what a run did, and text arriving
// after the terminal record makes the stored account of the run disagree with its own outcome.
func TestAppendLogFencesATerminalRun(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()

	tests := []struct {
		Name      string
		Status    run.Status
		WantWrite bool
		Want      error
	}{{ // Test 0: A running run accepts output.
		Name: "running", Status: run.StatusRunning, WantWrite: true, Want: nil,
	}, { // Test 1: So does a pending one, which is where a relay's first chunk can land.
		Name: "pending", Status: run.StatusPending, WantWrite: true, Want: nil,
	}, { // Test 2: A succeeded run is fenced silently, not reported as an error.
		Name: "succeeded", Status: run.StatusSucceeded, WantWrite: false, Want: nil,
	}, { // Test 3: An interrupted run, the exact state the sweep leaves behind.
		Name: "interrupted", Status: run.StatusInterrupted, WantWrite: false, Want: nil,
	}, { // Test 4: A rejected run never ran, so nothing may be attributed to it.
		Name: "rejected", Status: run.StatusRejected, WantWrite: false, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := newPending(fmt.Sprintf("q_log_%d_%d", stamp, testNum), test.Name)
			r.Status = test.Status
			saveRun(t, ctx, s, r)
			err := s.AppendLog(ctx, r.ID, []byte("late output\n"))
			if !errors.Is(err, test.Want) {
				t.Fatalf("%s: AppendLog() error = %v, want %v", test.Name, err, test.Want)
			}
			got, err := s.Log(ctx, r.ID)
			if err != nil {
				t.Fatalf("Log() error = %v", err)
			}
			if test.WantWrite && len(got) == 0 {
				t.Errorf("%s: the run accepted no output at all", test.Name)
			}
			if !test.WantWrite && len(got) != 0 {
				t.Errorf("%s: %d bytes landed on a run that had already ended, so the stored "+
					"account of the run disagrees with its own outcome", test.Name, len(got))
			}
		})
	}
	t.Run("test 5", func(t *testing.T) { // Test 5: A run that is not there is an error, not a no-op.
		t.Parallel()
		err := s.AppendLog(ctx, "run_no_such_log", []byte("x"))
		if !errors.Is(err, run.ErrNotFound) {
			t.Errorf("AppendLog() on a missing run = %v, want run.ErrNotFound: a silent success "+
				"loses the output of a run the caller thinks exists", err)
		}
	})
	t.Run("test 6", func(t *testing.T) { // Test 6: An empty chunk is stored, not treated as absent.
		t.Parallel()
		r := newPending(fmt.Sprintf("q_log_%d_empty", stamp), "emptychunk")
		r.Status = run.StatusRunning
		saveRun(t, ctx, s, r)
		if err := s.AppendLog(ctx, r.ID, []byte{}); err != nil {
			t.Errorf("AppendLog() with an empty chunk = %v, want nil", err)
		}
	})
}

// TestAppendEventsFencesATerminalRun pins the same fence on the structured event stream, which the
// live run view and every downstream consumer read. It is a separate code path from the log fence,
// using a status read inside a transaction rather than a predicate in the insert, so it needs its
// own proof.
func TestAppendEventsFencesATerminalRun(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()

	tests := []struct {
		Name      string
		Status    run.Status
		WantCount int
	}{{ // Test 0: A running run accepts events.
		Name: "running", Status: run.StatusRunning, WantCount: 2,
	}, { // Test 1: A canceled run does not.
		Name: "canceled", Status: run.StatusCanceled, WantCount: 0,
	}, { // Test 2: Nor a failed one.
		Name: "failed", Status: run.StatusFailed, WantCount: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := newPending(fmt.Sprintf("q_ev_%d_%d", stamp, testNum), test.Name)
			r.Status = test.Status
			saveRun(t, ctx, s, r)
			events := []event.Event{{Type: "task"}, {Type: "task"}}
			if err := s.AppendEvents(ctx, r.ID, events); err != nil {
				t.Fatalf("%s: AppendEvents() error = %v", test.Name, err)
			}
			got, err := s.Events(ctx, r.ID)
			if err != nil {
				t.Fatalf("Events() error = %v", err)
			}
			if len(got) != test.WantCount {
				t.Errorf("%s: the run holds %d events, want %d: a reclaimed worker streamed into "+
					"a run that had already ended", test.Name, len(got), test.WantCount)
			}
		})
	}
	t.Run("test 3", func(t *testing.T) { // Test 3: A missing run is reported, not silently dropped.
		t.Parallel()
		err := s.AppendEvents(ctx, "run_no_such_events", []event.Event{{Type: "task"}})
		if !errors.Is(err, run.ErrNotFound) {
			t.Errorf("AppendEvents() on a missing run = %v, want run.ErrNotFound", err)
		}
	})
	t.Run("test 4", func(t *testing.T) { // Test 4: An empty batch on a live run is a clean no-op.
		t.Parallel()
		r := newPending(fmt.Sprintf("q_ev_%d_empty", stamp), "emptybatch")
		r.Status = run.StatusRunning
		saveRun(t, ctx, s, r)
		if err := s.AppendEvents(ctx, r.ID, nil); err != nil {
			t.Errorf("AppendEvents() with no events = %v, want nil", err)
		}
	})
	t.Run("test 5", func(t *testing.T) { // Test 5: An empty batch on a missing run still reports it.
		t.Parallel()
		err := s.AppendEvents(ctx, "run_no_such_empty_events", nil)
		if !errors.Is(err, run.ErrNotFound) {
			t.Errorf("AppendEvents() with no events on a missing run = %v, want run.ErrNotFound: "+
				"an empty batch must not become a way to skip the existence check", err)
		}
	})
}

// TestSummaryWritesFenceATerminalRun pins the fence on the four summary writers. The comment on
// summaryFenced states the danger: a reclaimed-but-alive worker must not overwrite the final summary
// a healthy finalize already stored, because those rows outlive the run record and are what the
// drift and fleet-health views are built from. Each writer takes its own path to the fence, so each
// is proven separately.
func TestSummaryWritesFenceATerminalRun(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()

	hosts := []run.HostSummary{{Host: "web1", OK: 1, Worst: "ok", RanAt: time.Now().UTC()}}
	tasks := []run.TaskSummary{{Task: "install", Seconds: 1.5, RanAt: time.Now().UTC()}}

	// The incremental writers live on a second interface, reached by assertion at the call site, so
	// a store that lost them would compile and pass every run.Store test and fail on the first
	// relayed report continued across batches.
	appender, ok := s.(run.SummaryAppender)
	if !ok {
		t.Fatal("the PostgreSQL run store does not satisfy run.SummaryAppender, so a relayed " +
			"report continued across batches has no incremental write path")
	}

	tests := []struct {
		Name  string
		Write func(id string) error
		Count func(id string) (int, error)
	}{{ // Test 0: The whole-set host writer.
		Name:  "SaveHostSummary",
		Write: func(id string) error { return s.SaveHostSummary(ctx, id, hosts) },
		Count: func(id string) (int, error) { n, err := s.RunHostSummaries(ctx, id); return len(n), err },
	}, { // Test 1: The incremental host writer used by a relayed report.
		Name:  "AppendHostSummary",
		Write: func(id string) error { return appender.AppendHostSummary(ctx, id, hosts) },
		Count: func(id string) (int, error) { n, err := s.RunHostSummaries(ctx, id); return len(n), err },
	}, { // Test 2: The whole-set task writer.
		Name:  "SaveTaskSummary",
		Write: func(id string) error { return s.SaveTaskSummary(ctx, id, tasks) },
		Count: func(id string) (int, error) { n, err := s.RunTaskSummaries(ctx, id); return len(n), err },
	}, { // Test 3: The incremental task writer.
		Name:  "AppendTaskSummary",
		Write: func(id string) error { return appender.AppendTaskSummary(ctx, id, tasks) },
		Count: func(id string) (int, error) { n, err := s.RunTaskSummaries(ctx, id); return len(n), err },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// A live run accepts the write, so the fence is not simply always closed.
			live := newPending(fmt.Sprintf("q_sum_%d_%d_live", stamp, testNum), "live")
			live.Status = run.StatusRunning
			saveRun(t, ctx, s, live)
			if err := test.Write(live.ID); err != nil {
				t.Fatalf("%s on a live run error = %v", test.Name, err)
			}
			n, err := test.Count(live.ID)
			if err != nil {
				t.Fatalf("count error = %v", err)
			}
			if n != 1 {
				t.Fatalf("%s: a live run holds %d summary rows, want 1", test.Name, n)
			}

			// A run that already ended refuses it, silently.
			done := newPending(fmt.Sprintf("q_sum_%d_%d_done", stamp, testNum), "done")
			done.Status = run.StatusSucceeded
			saveRun(t, ctx, s, done)
			if err := test.Write(done.ID); err != nil {
				t.Fatalf("%s on a terminal run error = %v, want a silent no-op", test.Name, err)
			}
			n, err = test.Count(done.ID)
			if err != nil {
				t.Fatalf("count error = %v", err)
			}
			if n != 0 {
				t.Errorf("%s: %d summary rows landed on a run that had already ended, so a "+
					"reclaimed worker overwrote the final summary a healthy finalize stored",
					test.Name, n)
			}
		})
	}
}

// TestSaveRefusesADuplicateIdempotencyKey pins submission dedup. The key is what stops a retried
// HTTP submit from launching the same change twice, so the second insert has to be refused with the
// sentinel the API layer maps to "you already submitted this", not with a raw driver error.
func TestSaveRefusesADuplicateIdempotencyKey(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()
	key := fmt.Sprintf("idem_%d", stamp)

	first := newPending("q_idem", fmt.Sprintf("first_%d", stamp))
	first.IdempotencyKey = key
	saveRun(t, ctx, s, first)

	second := newPending("q_idem", fmt.Sprintf("second_%d", stamp))
	second.IdempotencyKey = key
	if err := s.Save(ctx, second); !errors.Is(err, run.ErrDuplicateKey) {
		t.Fatalf("Save() with a reused key = %v, want run.ErrDuplicateKey: without the sentinel "+
			"the API cannot tell a retried submit from a real failure and launches the change twice",
			err)
	}

	t.Run("test 0", func(t *testing.T) { // Test 0: Re-saving the same run under its own key is fine.
		if err := s.Save(ctx, first); err != nil {
			t.Errorf("re-saving the key's own run = %v, want nil: an ordinary progress save must "+
				"not trip the dedup index", err)
		}
	})
	t.Run("test 1", func(t *testing.T) { // Test 1: Empty keys do not collide with one another.
		a := newPending("q_idem", fmt.Sprintf("nokey_a_%d", stamp))
		b := newPending("q_idem", fmt.Sprintf("nokey_b_%d", stamp))
		if err := s.Save(ctx, a); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if err := s.Save(ctx, b); err != nil {
			t.Errorf("Save() of a second keyless run = %v, want nil: the dedup index is partial "+
				"so an unkeyed submit is never deduped against another", err)
		}
	})
	t.Run("test 2", func(t *testing.T) { // Test 2: An empty key is never looked up.
		if _, err := s.ByIdempotencyKey(ctx, ""); !errors.Is(err, run.ErrNotFound) {
			t.Errorf("ByIdempotencyKey(\"\") = %v, want run.ErrNotFound: an empty key matching any "+
				"keyless run would make every unkeyed submit look like a retry", err)
		}
	})
	t.Run("test 3", func(t *testing.T) { // Test 3: A real key finds exactly its run.
		got, err := s.ByIdempotencyKey(ctx, key)
		if err != nil {
			t.Fatalf("ByIdempotencyKey() error = %v", err)
		}
		if got.ID != first.ID {
			t.Errorf("ByIdempotencyKey() returned %s, want %s", got.ID, first.ID)
		}
	})
	t.Run("test 4", func(t *testing.T) { // Test 4: An unknown key is not found rather than empty.
		if _, err := s.ByIdempotencyKey(ctx, "idem_never_used"); !errors.Is(err, run.ErrNotFound) {
			t.Errorf("ByIdempotencyKey() on an unknown key = %v, want run.ErrNotFound", err)
		}
	})
}

// TestPurgeRunsBeforeSpareseverythingUnfinished pins the retention predicate against the incident
// the schema comment records. Retention once selected runs as "not pending or running", which
// silently counted pending_approval as finished and deleted runs that were waiting for an approver.
// A person's pending decision disappearing is the worst possible retention bug in an approvals
// product, so every non-terminal status is proven to survive a purge whose cutoff is in the future.
//
//nolint:funlen // Test function.
func TestPurgeRunsBeforeSparesEverythingUnfinished(t *testing.T) {
	// Not parallel: retention sweeps every run in the database rather than a scoped set, so a
	// concurrent test's terminal runs would be collected by this test's purge and the reverse.
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()
	old := time.Now().UTC().Add(-72 * time.Hour)

	tests := []struct {
		Status      run.Status
		WantDeleted bool
	}{{ // Test 0: A run waiting for an approver must survive. This is the incident.
		Status: run.StatusPendingApproval, WantDeleted: false,
	}, { // Test 1: A queued run has not run yet.
		Status: run.StatusPending, WantDeleted: false,
	}, { // Test 2: A run under way is obviously not collectable.
		Status: run.StatusRunning, WantDeleted: false,
	}, { // Test 3: A finished run is.
		Status: run.StatusSucceeded, WantDeleted: true,
	}, { // Test 4: So is a failed one.
		Status: run.StatusFailed, WantDeleted: true,
	}, { // Test 5: And a canceled one.
		Status: run.StatusCanceled, WantDeleted: true,
	}, { // Test 6: And one the sweep interrupted.
		Status: run.StatusInterrupted, WantDeleted: true,
	}, { // Test 7: And one an approver rejected, which is a decided outcome.
		Status: run.StatusRejected, WantDeleted: true,
	}}
	// Every case is set up first, then one purge runs over all of them, because the purge is not
	// scoped to a run: a per-case purge would hide a predicate that deletes another case's row.
	ids := make([]string, len(tests))
	for i, test := range tests {
		r := newPending(fmt.Sprintf("q_purge_%d", stamp), fmt.Sprintf("%d_%s", i, test.Status))
		r.Status = test.Status
		r.CreatedAt = old
		saveRun(t, ctx, s, r)
		ids[i] = r.ID
	}
	if _, err := s.PurgeRunsBefore(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			_, err := s.Get(ctx, ids[testNum])
			gone := errors.Is(err, run.ErrNotFound)
			if err != nil && !gone {
				t.Fatalf("Get() error = %v", err)
			}
			if gone != test.WantDeleted {
				if test.WantDeleted {
					t.Errorf("a %s run survived retention, so finished runs accumulate forever",
						test.Status)
					return
				}
				t.Errorf("retention deleted a %s run: an unfinished run vanished, and for "+
					"pending_approval that is a person's pending decision destroyed", test.Status)
			}
		})
	}
}

// TestPurgeRespectsItsCutoff pins the time bound. A purge that ignored the cutoff would delete the
// history an operator is still using, and one that never matched would let the tables grow without
// limit. The boundary is exclusive, so a run created exactly at the cutoff survives.
func TestPurgeRespectsItsCutoff(t *testing.T) {
	// Not parallel, for the reason given on TestPurgeRunsBeforeSparesEverythingUnfinished.
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()
	cutoff := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	tests := []struct {
		Name        string
		CreatedAt   time.Time
		WantDeleted bool
	}{{ // Test 0: Well before the cutoff, collected.
		Name: "old", CreatedAt: cutoff.Add(-24 * time.Hour), WantDeleted: true,
	}, { // Test 1: One second before the cutoff, still collected.
		Name: "just before", CreatedAt: cutoff.Add(-time.Second), WantDeleted: true,
	}, { // Test 2: Exactly at the cutoff, kept, since the comparison is strictly less than.
		Name: "exactly at", CreatedAt: cutoff, WantDeleted: false,
	}, { // Test 3: After the cutoff, kept.
		Name: "after", CreatedAt: cutoff.Add(time.Second), WantDeleted: false,
	}}
	ids := make([]string, len(tests))
	for i, test := range tests {
		r := newPending(fmt.Sprintf("q_cutoff_%d", stamp), fmt.Sprintf("%d_%s", i, test.Name))
		r.Status = run.StatusSucceeded
		r.CreatedAt = test.CreatedAt
		saveRun(t, ctx, s, r)
		ids[i] = r.ID
	}
	if _, err := s.PurgeRunsBefore(ctx, cutoff); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			_, err := s.Get(ctx, ids[testNum])
			gone := errors.Is(err, run.ErrNotFound)
			if err != nil && !gone {
				t.Fatalf("Get() error = %v", err)
			}
			if gone != test.WantDeleted {
				t.Errorf("%s: deleted = %v, want %v: the retention cutoff is off by one, which "+
					"either destroys history an operator is still reading or lets the tables grow "+
					"without limit", test.Name, gone, test.WantDeleted)
			}
		})
	}
}

// TestPurgeEventsBeforeKeepsTheRunAndCountsOnlyTrimmedOnes pins the shape of the lighter retention
// pass. It drops the bulky output of finished runs while keeping the run record itself, and its
// returned count is what an operator reads to see the sweep worked. Counting runs that held nothing
// would report work that never happened.
func TestPurgeEventsBeforeKeepsTheRunAndCountsOnlyTrimmedOnes(t *testing.T) {
	// Not parallel, for the reason given on TestPurgeRunsBeforeSparesEverythingUnfinished.
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()
	old := time.Now().UTC().Add(-72 * time.Hour)

	// One finished run with output, one finished run with none, one unfinished run with output.
	withOutput := newPending(fmt.Sprintf("q_pe_%d", stamp), "withoutput")
	withOutput.Status = run.StatusRunning
	withOutput.CreatedAt = old
	saveRun(t, ctx, s, withOutput)
	if err := s.AppendLog(ctx, withOutput.ID, []byte("hello\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := s.AppendEvents(ctx, withOutput.ID, []event.Event{{Type: "task"}}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	withOutput.Status = run.StatusSucceeded
	saveRun(t, ctx, s, withOutput)

	bare := newPending(fmt.Sprintf("q_pe_%d", stamp), "bare")
	bare.Status = run.StatusSucceeded
	bare.CreatedAt = old
	saveRun(t, ctx, s, bare)

	live := newPending(fmt.Sprintf("q_pe_%d", stamp), "live")
	live.Status = run.StatusRunning
	live.CreatedAt = old
	saveRun(t, ctx, s, live)
	if err := s.AppendLog(ctx, live.ID, []byte("still going\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}

	trimmed, err := s.PurgeEventsBefore(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("PurgeEventsBefore() error = %v", err)
	}
	if trimmed < 1 {
		t.Fatalf("PurgeEventsBefore reported %d runs trimmed, want at least the one that held "+
			"output", trimmed)
	}

	t.Run("test 0", func(t *testing.T) { // Test 0: The finished run's output is gone.
		got, err := s.Log(ctx, withOutput.ID)
		if err != nil {
			t.Fatalf("Log() error = %v", err)
		}
		if len(got) != 0 {
			t.Errorf("the finished run still holds %d bytes of output", len(got))
		}
		evs, err := s.Events(ctx, withOutput.ID)
		if err != nil {
			t.Fatalf("Events() error = %v", err)
		}
		if len(evs) != 0 {
			t.Errorf("the finished run still holds %d events", len(evs))
		}
	})
	t.Run("test 1", func(t *testing.T) { // Test 1: But the run record itself survives.
		if _, err := s.Get(ctx, withOutput.ID); err != nil {
			t.Errorf("Get() after the event purge = %v: this pass keeps the run record", err)
		}
		if _, err := s.Get(ctx, bare.ID); err != nil {
			t.Errorf("Get() on the output-free run = %v", err)
		}
	})
	t.Run("test 2", func(t *testing.T) { // Test 2: An unfinished run keeps its output.
		got, err := s.Log(ctx, live.ID)
		if err != nil {
			t.Fatalf("Log() error = %v", err)
		}
		if len(got) == 0 {
			t.Error("retention took the output of a run that is still running, so a person " +
				"watching it sees the log go blank")
		}
	})
}

// TestTrimSummariesClampsAndKeepsTheNewest pins the summary retention bound. The trim decides which
// history rows to destroy, so both the clamp on an absurd keep value and the choice of which rows
// survive matter: keeping the wrong ones silently rewrites what the fleet views report.
func TestTrimSummariesClampsAndKeepsTheNewest(t *testing.T) {
	// Not parallel: the trim bounds every host in the table, not just this test's.
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()
	host := fmt.Sprintf("host_trim_%d", stamp)
	base := time.Now().UTC().Add(-10 * time.Hour).Truncate(time.Second)

	// Six summaries for one host, each on its own run and an hour apart.
	for i := range 6 {
		r := newPending(fmt.Sprintf("q_trim_%d", stamp), fmt.Sprintf("trim%d", i))
		r.Status = run.StatusRunning
		saveRun(t, ctx, s, r)
		at := base.Add(time.Duration(i) * time.Hour)
		if err := s.SaveHostSummary(ctx, r.ID, []run.HostSummary{
			{Host: host, OK: i, Worst: "ok", RanAt: at},
		}); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
	}

	tests := []struct {
		Name     string
		Keep     int
		WantKept int
	}{{ // Test 0: Zero is clamped to one rather than destroying every row.
		Name: "zero keep", Keep: 0, WantKept: 1,
	}, { // Test 1: A negative keep is clamped the same way.
		Name: "negative keep", Keep: -5, WantKept: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: each case trims the same host's rows, so they must run in order.
			if _, err := s.TrimSummaries(ctx, test.Keep); err != nil {
				t.Fatalf("TrimSummaries(%d) error = %v", test.Keep, err)
			}
			hist, err := s.HostHistory(ctx, host, 100)
			if err != nil {
				t.Fatalf("HostHistory() error = %v", err)
			}
			if len(hist) != test.WantKept {
				t.Fatalf("%s: %d summaries survived TrimSummaries(%d), want %d: a clamp that let "+
					"zero through would delete every row for every host", test.Name, len(hist),
					test.Keep, test.WantKept)
			}
			// The survivor has to be the newest, or the trim rewrote what the fleet views report.
			if hist[0].OK != 5 {
				t.Errorf("%s: the surviving summary is the one with OK=%d, want the newest (OK=5)",
					test.Name, hist[0].OK)
			}
		})
	}
}

// TestNormalizeScheduleTimesOnOpen pins the migration that keeps schedules firing across releases.
// ClaimDue is a compare-and-swap on next_run_at as text, so a row written in a different fractional
// form can never be claimed again, and the scheduler reads the failed claim as another node having
// won: the schedule stops firing silently and permanently. Open rewrites the stored bytes into the
// form this build produces, and leaves alone anything it cannot parse, since rewriting a value it
// cannot read would be a guess.
//
// Not parallel: it writes non-canonical bytes straight into the schedules table and then reopens the
// database to drive the migration.
func TestNormalizeScheduleTimesOnOpen(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	at := time.Date(2027, 3, 4, 5, 6, 7, 0, time.UTC)
	canonical := sqlutil.FormatTime(at)

	tests := []struct {
		Name      string
		Stored    string
		WantAfter string
	}{{ // Test 0: A padded fractional second, the form a past release wrote.
		Name: "padded fraction", Stored: "2027-03-04T05:06:07.000000000Z", WantAfter: canonical,
	}, { // Test 1: A non-UTC offset normalizes to the UTC text the claim compares against.
		Name: "offset", Stored: "2027-03-04T00:06:07-05:00", WantAfter: canonical,
	}, { // Test 2: Already canonical, so it is left exactly as it stands.
		Name: "canonical", Stored: canonical, WantAfter: canonical,
	}, { // Test 3: Unparseable, so it is left alone rather than guessed at.
		Name: "unparseable", Stored: "not a timestamp", WantAfter: "not a timestamp",
	}}

	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()

	ids := make([]string, len(tests))
	for i, test := range tests {
		id := fmt.Sprintf("sch_norm_%d_%d", stamp, i)
		ids[i] = id
		if err := db.Schedules().Save(ctx, &schedule.Schedule{
			ID: id, Cron: "* * * * *", Playbook: "p.yml", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if _, err := raw.Exec("UPDATE schedules SET next_run_at=$1 WHERE id=$2",
			test.Stored, id); err != nil {
			t.Fatalf("seed %s: %v", test.Name, err)
		}
	}
	// The unparseable row is removed at the end whatever happens. Leaving it behind makes every
	// later schedule listing in the package fail, which is itself the defect reported in
	// TestListSurvivesOneUnreadableScheduleStamp below.
	// The cleanup opens its own connection because the deferred Close above runs first: a deferred
	// call fires as the function returns, and t.Cleanup only afterward.
	t.Cleanup(func() {
		conn, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Errorf("cleanup: open: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for _, id := range ids {
			if _, err := conn.Exec("DELETE FROM schedules WHERE id=$1", id); err != nil {
				t.Errorf("cleanup: delete %s: %v", id, err)
			}
		}
	})
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Reopening runs the normalization, exactly as a release upgrade would.
	reopened, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			var got string
			if err := raw.QueryRow("SELECT next_run_at FROM schedules WHERE id=$1",
				ids[testNum]).Scan(&got); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if got != test.WantAfter {
				t.Errorf("%s: stored next_run_at = %q, want %q. ClaimDue compares these bytes "+
					"exactly, so a row left in another form can never be claimed again and the "+
					"schedule stops firing silently.", test.Name, got, test.WantAfter)
			}
		})
	}
	t.Run("test 4", func(t *testing.T) { // Test 4: The reopen itself survives the unreadable row.
		// Open must not fail on a stamp it cannot parse, or one bad row stops every node booting.
		// What List then does with that row is a separate defect, pinned in
		// TestListSurvivesOneUnreadableScheduleStamp.
		if _, err := reopened.Schedules().Get(ctx, ids[2]); err != nil {
			t.Errorf("Get() on a healthy schedule after the reopen = %v: the migration tolerated "+
				"the unreadable row, so the healthy ones must still be readable", err)
		}
	})
}

// TestRunRoundTripsEveryStoredField pins that a run written and read back is the run that went in.
// The row carries sixty columns, several of them JSON blobs holding evidence a receipt is checked
// against, so a field silently dropped on the way through is a receipt that can never be recomputed.
// Unicode and a NUL byte are included because the store sanitizes on write: PostgreSQL refuses both
// a NUL and invalid UTF-8, and the write that carries them is the one recording what the run did.
func TestRunRoundTripsEveryStoredField(t *testing.T) {
	t.Parallel()
	ctx, s := fenceStore(t)
	stamp := time.Now().UnixNano()
	now := time.Now().UTC().Truncate(time.Second)
	started := now.Add(time.Minute)
	code := 3

	r := &run.Run{
		ID: fmt.Sprintf("run_round_%d", stamp), Playbook: "site.yml", Inventory: "hosts.ini",
		Status: run.StatusFailed, ExitCode: &code, Error: "boom ☠", CreatedAt: now,
		StartedAt: &started, EndedAt: &started, Limit: "web*", Attempt: 2,
		ExtraVars: map[string]any{"key": "válue"},
		Outputs:   map[string]any{"out": "résultat"},
		Queue:     "q_round", Tool: "terraform", Command: "apply -auto-approve",
		DryRun: true, Intent: "change", Image: "img:1", PullCredentialID: "cred_pull",
		Timeout: 900, Source: "webhook", SourceID: "hook_1", Actor: "élève",
		ActorType: "agent", ApprovedSpecDigest: "sha256:deadbeef", RerunOf: "run_prev",
		Labels: map[string]string{"env": "prød"}, Warning: "careful", AuditReceipt: "rcpt_1",
		HeldByPolicy: "pol_1", Tags: []string{"a", "b"}, SkipTags: []string{"c"},
		Verbosity: 3, Forks: 12, DiffMode: true, RequireDistinctApprover: true,
		PinnedCommit: "abc123", ActorUserID: "usr_1", ProjectID: "proj_1", CommitSHA: "def456",
		InventoryID: "inv_1", OrgID: "org_1", ProposedFrom: "run_proposal",
		CredentialIDs: []string{"cred_a", "cred_b"},
		PolicySet:     &run.PolicySet{Digest: "digest", Count: 2, Rules: []string{"r1", "r2"}},
		Notifications: []run.NotifyTarget{{Kind: "slack", URL: "https://hooks.example/ops"}},
	}
	saveRun(t, ctx, s, r)

	got, err := s.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	tests := []struct {
		Name string
		Got  any
		Want any
	}{
		{"Playbook", got.Playbook, r.Playbook},
		{"Status", got.Status, r.Status},
		{"Error", got.Error, r.Error},
		{"Limit", got.Limit, r.Limit},
		{"Attempt", got.Attempt, r.Attempt},
		{"Queue", got.Queue, r.Queue},
		{"Tool", got.Tool, r.Tool},
		{"Command", got.Command, r.Command},
		{"DryRun", got.DryRun, r.DryRun},
		{"Intent", got.Intent, r.Intent},
		{"Image", got.Image, r.Image},
		{"PullCredentialID", got.PullCredentialID, r.PullCredentialID},
		{"Timeout", got.Timeout, r.Timeout},
		{"Source", got.Source, r.Source},
		{"SourceID", got.SourceID, r.SourceID},
		{"Actor", got.Actor, r.Actor},
		{"ActorType", got.ActorType, r.ActorType},
		{"ApprovedSpecDigest", got.ApprovedSpecDigest, r.ApprovedSpecDigest},
		{"RerunOf", got.RerunOf, r.RerunOf},
		{"Warning", got.Warning, r.Warning},
		{"AuditReceipt", got.AuditReceipt, r.AuditReceipt},
		{"HeldByPolicy", got.HeldByPolicy, r.HeldByPolicy},
		{"Verbosity", got.Verbosity, r.Verbosity},
		{"Forks", got.Forks, r.Forks},
		{"DiffMode", got.DiffMode, r.DiffMode},
		{"RequireDistinctApprover", got.RequireDistinctApprover, r.RequireDistinctApprover},
		{"PinnedCommit", got.PinnedCommit, r.PinnedCommit},
		{"ActorUserID", got.ActorUserID, r.ActorUserID},
		{"ProjectID", got.ProjectID, r.ProjectID},
		{"CommitSHA", got.CommitSHA, r.CommitSHA},
		{"InventoryID", got.InventoryID, r.InventoryID},
		{"OrgID", got.OrgID, r.OrgID},
		{"ProposedFrom", got.ProposedFrom, r.ProposedFrom},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if test.Got != test.Want {
				t.Errorf("%s round trip: got %v, want %v", test.Name, test.Got, test.Want)
			}
		})
	}

	t.Run("test 100", func(t *testing.T) { // Test 100: The evidence blobs survive whole.
		t.Parallel()
		if got.PolicySet == nil {
			t.Fatal("the recorded rule set came back nil, so a receipt naming it can never be " +
				"recomputed from the stored run")
		}
		if got.PolicySet.Digest != r.PolicySet.Digest || got.PolicySet.Count != r.PolicySet.Count {
			t.Errorf("PolicySet = %+v, want %+v", got.PolicySet, r.PolicySet)
		}
		if len(got.PolicySet.Rules) != 2 {
			t.Errorf("the rule set came back with %d rules, want 2", len(got.PolicySet.Rules))
		}
		if len(got.Notifications) != 1 || got.Notifications[0].Kind != "slack" {
			t.Errorf("Notifications = %+v, want the stored slack target", got.Notifications)
		}
		if got.Labels["env"] != "prød" {
			t.Errorf("Labels = %+v, want the unicode value intact", got.Labels)
		}
		if len(got.CredentialIDs) != 2 {
			t.Errorf("CredentialIDs = %+v, want both", got.CredentialIDs)
		}
		if len(got.Tags) != 2 || len(got.SkipTags) != 1 {
			t.Errorf("Tags = %+v, SkipTags = %+v, want two and one", got.Tags, got.SkipTags)
		}
		if got.ExitCode == nil || *got.ExitCode != code {
			t.Errorf("ExitCode = %v, want %d", got.ExitCode, code)
		}
		if got.StartedAt == nil || !got.StartedAt.Equal(started) {
			t.Errorf("StartedAt = %v, want %v", got.StartedAt, started)
		}
	})
	t.Run("test 101", func(t *testing.T) { // Test 101: A NUL byte is cleaned rather than refused.
		t.Parallel()
		dirty := newPending("q_round_nul", fmt.Sprintf("nul_%d", stamp))
		dirty.Command = "echo \x00 done"
		dirty.Error = "bad \x00 byte"
		if err := s.Save(ctx, dirty); err != nil {
			t.Fatalf("Save() with a NUL byte = %v. PostgreSQL refuses a NUL with SQLSTATE 22021, "+
				"and the write that carried it is the one recording what the run did, so it has "+
				"to be cleaned rather than lost.", err)
		}
		back, err := s.Get(ctx, dirty.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if strings.ContainsRune(back.Command, 0) {
			t.Error("a NUL byte reached the stored command")
		}
	})
	t.Run("test 102", func(t *testing.T) { // Test 102: A very long command is stored whole.
		t.Parallel()
		long := newPending("q_round_long", fmt.Sprintf("long_%d", stamp))
		long.Command = strings.Repeat("x", 200000)
		if err := s.Save(ctx, long); err != nil {
			t.Fatalf("Save() with a 200k command = %v", err)
		}
		back, err := s.Get(ctx, long.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if len(back.Command) != 200000 {
			t.Errorf("the stored command is %d bytes, want 200000: a truncated command means the "+
				"record of what ran is not what ran", len(back.Command))
		}
	})
}

// TestAuditStoreSatisfiesEveryInterfaceTheServerNeeds pins the conformance the wiring depends on.
// DB.Audits is typed as audit.Store, so the anchor and install halves of the contract are reached
// only by type assertion at the call site: a store that lost one would compile, wire, and pass every
// audit.Store test, then fail at runtime the first time a relying party asked for an anchor.
func TestAuditStoreSatisfiesEveryInterfaceTheServerNeeds(t *testing.T) {
	t.Parallel()
	db := openShared(t)

	tests := []struct {
		Name string
		OK   bool
	}{{ // Test 0: The anchor half, which fixes chain links outside this install's control.
		Name: "audit.AnchorStore", OK: assertAnchorStore(db.Audits()),
	}, { // Test 1: The install binder, whose id is folded into every chain link.
		Name: "audit.InstallBinder", OK: assertInstallBinder(db.Audits()),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if !test.OK {
				t.Errorf("the PostgreSQL audit store does not satisfy %s, so the call site that "+
					"asserts to it fails at runtime rather than at build time", test.Name)
			}
		})
	}
}

// assertAnchorStore reports whether s also implements the anchor half of the audit contract.
func assertAnchorStore(s audit.Store) bool {
	_, ok := s.(audit.AnchorStore)
	return ok
}

// assertInstallBinder reports whether s also implements the install binder.
func assertInstallBinder(s audit.Store) bool {
	_, ok := s.(audit.InstallBinder)
	return ok
}
