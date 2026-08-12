// Package scheduletest provides a shared behavior contract for schedule.Store implementations so the
// in memory, SQLite, and PostgreSQL backends cannot drift apart.
package scheduletest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// Contract runs the full schedule.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() schedule.Store) {
	t.Helper()
	t.Run("save and get", func(t *testing.T) { testSaveGet(t, newStore()) })
	t.Run("get missing", func(t *testing.T) { testGetMissing(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
	t.Run("delete", func(t *testing.T) { testDelete(t, newStore()) })
	t.Run("claim due", func(t *testing.T) { testClaimDue(t, newStore()) })
	t.Run("claim due holds under concurrency", func(t *testing.T) {
		testClaimDueUnderConcurrency(t, newStore())
	})
	t.Run("record fire never resurrects a deleted schedule", func(t *testing.T) {
		testRecordFire(t, newStore())
	})
	t.Run("update refuses a deleted schedule", func(t *testing.T) { testUpdate(t, newStore()) })
	t.Run("missing row and zero value", func(t *testing.T) { testMissingAndZero(t, newStore()) })
	t.Run("claim due on a missing row is a lost race", func(t *testing.T) {
		testClaimDueMissing(t, newStore())
	})
	t.Run("empty list is non-nil", func(t *testing.T) {
		got, err := newStore().List(context.Background())
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if got == nil {
			t.Error("List() on an empty store = nil, want a non-nil empty slice")
		}
	})
}

// testSaveGet verifies a schedule round trips including steps and returns independent copies.
func testSaveGet(t *testing.T, store schedule.Store) {
	ctx := context.Background()
	next := time.Date(2026, 7, 6, 1, 0, 0, 0, time.UTC)
	// The owning organization round trips like every other field. It is what scopes an inline
	// schedule to a tenant, so a backend that drops it hands another organization's crontab import,
	// command lines and all, to anybody who asks.
	want := &schedule.Schedule{
		ID: "sch_1", Name: "nightly", Cron: "0 2 * * *", Timezone: "America/New_York", Inventory: "hosts",
		Steps:      []run.PipelineStep{{Name: "one", Playbook: "one.yml", ContinueOnFailure: true}},
		TemplateID: "tpl_x", OrgID: "org_owner",
		Enabled: true, CreatedAt: time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), NextRunAt: &next,
	}
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "sch_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Get() mismatch (-want +got):\n%s", diff)
	}

	got.Name = "mutated"
	got.Steps[0].Name = "mutated"
	again, err := store.Get(ctx, "sch_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Name != "nightly" || again.Steps[0].Name != "one" {
		t.Error("mutating the returned schedule changed stored state")
	}
}

// testGetMissing verifies a missing schedule reports ErrNotFound on Get and Delete.
func testGetMissing(t *testing.T, store schedule.Store) {
	ctx := context.Background()
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, schedule.ErrNotFound) {
		t.Errorf("Get() = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "missing"); !errors.Is(err, schedule.ErrNotFound) {
		t.Errorf("Delete() = %v, want ErrNotFound", err)
	}
}

// testList verifies schedules come back oldest first.
func testList(t *testing.T, store schedule.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	for _, s := range []*schedule.Schedule{
		{ID: "c", Cron: "* * * * *", Playbook: "p", CreatedAt: base.Add(2 * time.Hour)},
		{ID: "a", Cron: "* * * * *", Playbook: "p", CreatedAt: base},
		{ID: "b", Cron: "* * * * *", Playbook: "p", CreatedAt: base.Add(time.Hour)},
	} {
		if err := store.Save(ctx, s); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	gotIDs := []string{list[0].ID, list[1].ID, list[2].ID}
	if diff := cmp.Diff([]string{"a", "b", "c"}, gotIDs); diff != "" {
		t.Errorf("List() order mismatch (-want +got):\n%s", diff)
	}
}

// testDelete verifies a deleted schedule is gone.
func testDelete(t *testing.T, store schedule.Store) {
	ctx := context.Background()
	if err := store.Save(ctx,
		&schedule.Schedule{ID: "sch_1", Cron: "* * * * *", Playbook: "p", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Delete(ctx, "sch_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, "sch_1"); !errors.Is(err, schedule.ErrNotFound) {
		t.Errorf("Get() after delete = %v, want ErrNotFound", err)
	}
}

// testClaimDueUnderConcurrency verifies the compare-and-swap actually serializes, rather than
// appearing to because the test ran one call at a time.
//
// ClaimDue is what stops a schedule firing twice. Several control nodes run the same scheduler
// against one database, so at the moment a schedule comes due every one of them tries to advance it,
// and exactly one must win. A sequential test passes on a store where that is not true.
//
// This one is a genuine compare-and-swap because both statements update the same row on the same
// condition, so they contend for the row and the loser re-evaluates against the updated value. That
// is worth knowing by measurement rather than by reading, because the same reasoning applied to a
// predicate over a different row is false, and getting that wrong here fires somebody's production
// deploy twice.
func testClaimDueUnderConcurrency(t *testing.T, store schedule.Store) {
	ctx := context.Background()
	const racers = 8
	base := time.Date(2026, 8, 2, 3, 0, 0, 0, time.UTC)
	for round := range 40 {
		id := fmt.Sprintf("sch_race_%d", round)
		next := base.Add(time.Duration(round) * time.Hour)
		if err := store.Save(ctx, &schedule.Schedule{
			ID: id, Name: id, Cron: "0 3 * * *", Playbook: "site.yml",
			Enabled: true, NextRunAt: &next, CreatedAt: base,
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
		var wins atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				won, err := store.ClaimDue(ctx, id, next, next.Add(24*time.Hour))
				if err != nil {
					t.Errorf("ClaimDue(%s) error = %v", id, err)
					return
				}
				if won {
					wins.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := wins.Load(); got != 1 {
			t.Fatalf("round %d: %d of %d schedulers claimed the same due schedule, so the run it "+
				"fires happens %d times", round, got, racers, got)
		}
	}
}

// testClaimDue verifies exactly one caller wins the advance of a due schedule.
func testClaimDue(t *testing.T, store schedule.Store) {
	ctx := context.Background()
	oldNext := time.Date(2026, 7, 6, 1, 0, 0, 0, time.UTC)
	newNext := oldNext.Add(time.Hour)
	if err := store.Save(ctx, &schedule.Schedule{
		ID: "sch_cas", Cron: "@hourly", Playbook: "p.yml", Enabled: true,
		CreatedAt: time.Now(), NextRunAt: &oldNext,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	won, err := store.ClaimDue(ctx, "sch_cas", oldNext, newNext)
	if err != nil {
		t.Fatalf("ClaimDue() error = %v", err)
	}
	if !won {
		t.Fatal("first claim lost, want it to win")
	}
	again, err := store.ClaimDue(ctx, "sch_cas", oldNext, newNext.Add(time.Hour))
	if err != nil {
		t.Fatalf("ClaimDue() second error = %v", err)
	}
	if again {
		t.Error("second claim with the stale next time won, want it to lose")
	}
	got, err := store.Get(ctx, "sch_cas")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.NextRunAt == nil || !got.NextRunAt.Equal(newNext) {
		t.Errorf("NextRunAt = %v, want the winner's %v", got.NextRunAt, newNext)
	}
}

// testRecordFire pins that recording a fire writes only what a fire owns and never creates a row.
//
// The scheduler used to write the whole schedule back after firing, from a snapshot taken before the
// run. A delete landing in between was undone: the row came back enabled with a live next run time
// and kept firing, which is the opposite of what deleting a schedule means. A disable and an edit
// were reverted the same way.
func testRecordFire(t *testing.T, store schedule.Store) {
	t.Helper()
	ctx := context.Background()
	created := time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)
	next := created.Add(time.Hour)
	fired := created.Add(30 * time.Minute)

	seed := func(id string) *schedule.Schedule {
		sc := &schedule.Schedule{
			ID: id, Name: id, Cron: "0 * * * *", Playbook: "p.yml", Enabled: true,
			CreatedAt: created, NextRunAt: &next, LastRunID: "run_first",
		}
		if err := store.Save(ctx, sc); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
		return sc
	}
	seed("sch_a")
	seed("sch_b")

	// A fire touches only the firing schedule. Without this, a cross-row write such as a stray
	// "OR 1=1" or a loop over every schedule compiles and passes every other assertion here.
	if err := store.RecordFire(ctx, "sch_a", fired, "run_second"); err != nil {
		t.Fatalf("RecordFire() error = %v", err)
	}
	other, err := store.Get(ctx, "sch_b")
	if err != nil {
		t.Fatalf("Get(sch_b) error = %v", err)
	}
	if other.LastRunID != "run_first" || other.LastRunAt != nil {
		t.Errorf("recording a fire on one schedule wrote to another: last_run_id=%q last_run_at=%v",
			other.LastRunID, other.LastRunAt)
	}

	got, err := store.Get(ctx, "sch_a")
	if err != nil {
		t.Fatalf("Get(sch_a) error = %v", err)
	}
	if got.LastRunID != "run_second" {
		t.Errorf("LastRunID = %q, want run_second", got.LastRunID)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(fired) {
		t.Errorf("LastRunAt = %v, want %v", got.LastRunAt, fired)
	}
	// Everything a fire does not own must survive it, or a fire silently reverts an operator's edit.
	if !got.Enabled || got.NextRunAt == nil || !got.NextRunAt.Equal(next) || got.Playbook != "p.yml" {
		t.Errorf("a fire rewrote fields it does not own: enabled=%v next=%v playbook=%q",
			got.Enabled, got.NextRunAt, got.Playbook)
	}

	// A fire that created no run keeps the run id already stored.
	if err := store.RecordFire(ctx, "sch_a", fired.Add(time.Hour), ""); err != nil {
		t.Fatalf("RecordFire(no run) error = %v", err)
	}
	if got, err = store.Get(ctx, "sch_a"); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LastRunID != "run_second" {
		t.Errorf("LastRunID = %q, want the previous run id kept when a fire created none", got.LastRunID)
	}

	// The case this method exists for.
	if err := store.Delete(ctx, "sch_a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.RecordFire(ctx, "sch_a", fired, "run_third"); err != nil {
		t.Errorf("RecordFire() on a deleted schedule error = %v, want nil; the record is a note "+
			"about a run that already happened", err)
	}
	if _, err := store.Get(ctx, "sch_a"); !errors.Is(err, schedule.ErrNotFound) {
		t.Errorf("Get() after delete then fire = %v, want ErrNotFound; the schedule came back", err)
	}
}

// testMissingAndZero sweeps every method against a row that is not there, an empty store, and a
// zero-value argument, and pins the single answer all backends have to give.
//
// Nothing in the interface forces a backend to agree here, so each one used to answer for itself.
// The split is between the two kinds of failure a caller has to tell apart: an error means the store
// could not carry out the request and somebody should look, while a normal negative answer means the
// request was carried out and the row was not there. A read or an edit that names a row that is gone
// is the caller working from a stale id, so it reports ErrNotFound. Recording a fire is a note about
// a run that already happened, so a missing row is nothing to report. Claiming is a lost race, which
// testClaimDueMissing covers on its own.
func testMissingAndZero(t *testing.T, store schedule.Store) {
	ctx := context.Background()

	tests := []struct {
		Call func(schedule.Store) error
		Want error
		Name string
	}{{ // Test 0: Get names a row that is not there.
		Name: "get missing",
		Call: func(s schedule.Store) error { _, err := s.Get(ctx, "nope"); return err },
		Want: schedule.ErrNotFound,
	}, { // Test 1: Get with the zero-value id.
		Name: "get empty id",
		Call: func(s schedule.Store) error { _, err := s.Get(ctx, ""); return err },
		Want: schedule.ErrNotFound,
	}, { // Test 2: Delete names a row that is not there.
		Name: "delete missing",
		Call: func(s schedule.Store) error { return s.Delete(ctx, "nope") },
		Want: schedule.ErrNotFound,
	}, { // Test 3: Delete with the zero-value id.
		Name: "delete empty id",
		Call: func(s schedule.Store) error { return s.Delete(ctx, "") },
		Want: schedule.ErrNotFound,
	}, { // Test 4: Update names a row that is not there.
		Name: "update missing",
		Call: func(s schedule.Store) error {
			return s.Update(ctx, &schedule.Schedule{ID: "nope", Cron: "0 * * * *", Playbook: "p.yml"})
		},
		Want: schedule.ErrNotFound,
	}, { // Test 5: Update with a zero-value schedule, whose id is empty.
		Name: "update zero value schedule",
		Call: func(s schedule.Store) error { return s.Update(ctx, &schedule.Schedule{}) },
		Want: schedule.ErrNotFound,
	}, { // Test 6: RecordFire names a row that is not there.
		Name: "record fire missing",
		Call: func(s schedule.Store) error { return s.RecordFire(ctx, "nope", time.Now(), "run_1") },
		Want: nil,
	}, { // Test 7: RecordFire with a zero-value id, time, and run id.
		Name: "record fire zero values",
		Call: func(s schedule.Store) error { return s.RecordFire(ctx, "", time.Time{}, "") },
		Want: nil,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			if err := test.Call(store); !errors.Is(err, test.Want) {
				t.Errorf("%s = %v, want %v", test.Name, err, test.Want)
			}
		})
	}

	// None of the above may have created anything. A backend that upserts where it should update
	// passes every assertion so far and leaves a schedule behind that fires.
	left, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(left) != 0 {
		t.Errorf("calls against missing rows left %d schedule(s) behind, want an empty store", len(left))
	}

	// A zero-value schedule is storable and reads back as itself, including the zero created time.
	// A backend that cannot round trip the zero time reports a schedule the others do not.
	if err := store.Save(ctx, &schedule.Schedule{}); err != nil {
		t.Fatalf("Save(zero value) error = %v", err)
	}
	got, err := store.Get(ctx, "")
	if err != nil {
		t.Fatalf("Get() after saving a zero-value schedule error = %v", err)
	}
	if diff := cmp.Diff(&schedule.Schedule{}, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("zero-value schedule round trip mismatch (-want +got):\n%s", diff)
	}
}

// testClaimDueMissing pins that claiming a row that is not there reports a lost race rather than an
// error, on every backend.
//
// ClaimDue is a compare-and-swap the scheduler runs after listing, so a schedule deleted in the
// window between the list and the claim is unavoidable and expected. It is not a fault: the caller's
// right move is the same one it makes when another scheduler node got there first, which is to skip
// the schedule and carry on. Reporting it as an error conflates "I could not tell you the outcome"
// with "the outcome is no", and only the first deserves a log line or a retry. The SQL backends also
// cannot tell a deleted row from a row another node already advanced without a second query, since
// both are zero rows affected, so the error would cost a round trip to manufacture a distinction the
// caller must not act on anyway. A claim against a row that is gone loses, quietly.
func testClaimDueMissing(t *testing.T, store schedule.Store) {
	ctx := context.Background()
	next := time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC)

	won, err := store.ClaimDue(ctx, "never_existed", next, next.Add(time.Hour))
	if err != nil {
		t.Errorf("ClaimDue() on a row that never existed error = %v, want nil; a schedule that is "+
			"not there is a lost race, not a fault", err)
	}
	if won {
		t.Error("ClaimDue() on a row that never existed won, want it to lose")
	}
	if won, err = store.ClaimDue(ctx, "", time.Time{}, time.Time{}); err != nil || won {
		t.Errorf("ClaimDue() with zero-value arguments = (%v, %v), want (false, nil)", won, err)
	}

	// The same claim against a schedule deleted after it was listed, which is the case the scheduler
	// actually hits. Nothing may be re-created by the losing claim.
	sc := &schedule.Schedule{
		ID: "sch_gone", Name: "gone", Cron: "0 * * * *", Playbook: "p.yml", Enabled: true,
		CreatedAt: time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC), NextRunAt: &next,
	}
	if err := store.Save(ctx, sc); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Delete(ctx, "sch_gone"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	won, err = store.ClaimDue(ctx, "sch_gone", next, next.Add(time.Hour))
	if err != nil {
		t.Errorf("ClaimDue() on a schedule deleted after it was listed error = %v, want nil", err)
	}
	if won {
		t.Error("ClaimDue() on a deleted schedule won, so the scheduler would fire a deleted schedule")
	}
	if _, err := store.Get(ctx, "sch_gone"); !errors.Is(err, schedule.ErrNotFound) {
		t.Errorf("Get() after claiming a deleted schedule = %v, want ErrNotFound; the claim "+
			"re-created it", err)
	}

	// A claim against a live schedule with no next run time must also lose rather than match a null.
	live := &schedule.Schedule{
		ID: "sch_idle", Name: "idle", Cron: "0 * * * *", Playbook: "p.yml", Enabled: true,
		CreatedAt: time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC),
	}
	if err := store.Save(ctx, live); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if won, err = store.ClaimDue(ctx, "sch_idle", time.Time{}, next); err != nil || won {
		t.Errorf("ClaimDue() against an unscheduled row = (%v, %v), want (false, nil)", won, err)
	}
	idle, err := store.Get(ctx, "sch_idle")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if idle.NextRunAt != nil {
		t.Errorf("a lost claim advanced NextRunAt to %v, want it left unset", idle.NextRunAt)
	}
}

// testUpdate pins that updating a schedule refuses a row that is gone, so an edit racing a delete
// cannot re-create it. Save stays an upsert, which is what the create path needs.
func testUpdate(t *testing.T, store schedule.Store) {
	t.Helper()
	ctx := context.Background()
	created := time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)
	sc := &schedule.Schedule{
		ID: "sch_1", Name: "nightly", Cron: "0 * * * *", Playbook: "p.yml", Enabled: true,
		CreatedAt: created, OrgID: "org_a",
	}
	if err := store.Save(ctx, sc); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	sc.Playbook = "edited.yml"
	// The owning organization is written by an update like any other column. A backend that leaves
	// it out of the statement keeps whatever was there, so a schedule handed to another tenant, or
	// held back from its own, would read as correct in the store and wrong to the authorizer.
	sc.OrgID = "org_b"
	if err := store.Update(ctx, sc); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "sch_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Playbook != "edited.yml" {
		t.Errorf("Playbook = %q, want edited.yml", got.Playbook)
	}
	if got.OrgID != "org_b" {
		t.Errorf("OrgID = %q, want org_b; an update did not write the owning organization", got.OrgID)
	}

	if err := store.Delete(ctx, "sch_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Update(ctx, sc); !errors.Is(err, schedule.ErrNotFound) {
		t.Errorf("Update() on a deleted schedule = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, "sch_1"); !errors.Is(err, schedule.ErrNotFound) {
		t.Error("an update re-created a deleted schedule")
	}
}
