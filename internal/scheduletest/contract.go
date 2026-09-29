// Package scheduletest provides a shared behavior contract for schedule.Store implementations so the
// in memory and SQLite backends cannot drift apart.
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
	want := &schedule.Schedule{
		ID: "sch_1", Name: "nightly", Cron: "0 2 * * *", Inventory: "hosts",
		Steps:      []run.PipelineStep{{Name: "one", Playbook: "one.yml", ContinueOnFailure: true}},
		TemplateID: "tpl_x",
		Enabled:    true, CreatedAt: time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), NextRunAt: &next,
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
