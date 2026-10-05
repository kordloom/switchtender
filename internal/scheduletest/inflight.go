package scheduletest

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/schedule"
)

// testInFlight verifies the in-flight mark a fire's claim leaves: the claim is the same
// compare-and-set as ClaimDue and ClaimFinal and marks the occurrence it took, the mark is
// invisible to every read and survives an edit, a row marking another occurrence is not claimed
// until that one settles, a sweep takes a mark only once it is old enough and takes it once, and a
// settle clears only the occurrence it names.
func testInFlight(t *testing.T, store schedule.Store) {
	ctx := context.Background()
	due := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	next := due.Add(5 * time.Minute)
	for _, id := range []string{"sch_fire", "sch_final", "sch_plain"} {
		at := due
		if err := store.Save(ctx, &schedule.Schedule{
			ID: id, Cron: "*/5 * * * *", Timezone: "UTC", Playbook: "p.yml", Enabled: true,
			CreatedAt: due.Add(-time.Hour), NextRunAt: &at,
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}
	if won, err := store.ClaimFire(ctx, "sch_fire", due.Add(time.Minute), &next); err != nil || won {
		t.Errorf("ClaimFire() against a time the row does not hold = %v, %v, want lost", won, err)
	}
	if won, err := store.ClaimFire(ctx, "sch_fire", due, &next); err != nil || !won {
		t.Fatalf("ClaimFire() = %v, %v, want a won claim", won, err)
	}
	if won, err := store.ClaimFire(ctx, "sch_fire", due, &next); err != nil || won {
		t.Errorf("ClaimFire() of the same occurrence again = %v, %v, want lost", won, err)
	}
	if won, err := store.ClaimFire(ctx, "sch_final", due, nil); err != nil || !won {
		t.Fatalf("ClaimFire() of a last occurrence = %v, %v, want a won claim", won, err)
	}
	if won, err := store.ClaimFire(ctx, "sch_gone", due, &next); err != nil || won {
		t.Errorf("ClaimFire() of a missing row = %v, %v, want lost without an error", won, err)
	}
	if won, err := store.ClaimDue(ctx, "sch_plain", due, next); err != nil || !won {
		t.Fatalf("ClaimDue() = %v, %v, want a won claim", won, err)
	}
	for id, want := range map[string]*time.Time{"sch_fire": &next, "sch_final": nil} {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		switch {
		case want == nil && got.NextRunAt != nil:
			t.Errorf("%s NextRunAt = %v, want none after a final claim", id, got.NextRunAt)
		case want != nil && (got.NextRunAt == nil || !got.NextRunAt.Equal(*want)):
			t.Errorf("%s NextRunAt = %v, want %v", id, got.NextRunAt, *want)
		}
		// An edit rewrites the row and keeps the mark: the occurrence it took is still owed.
		got.Name = "edited"
		if err := store.Update(ctx, got); err != nil {
			t.Fatalf("Update(%s) error = %v", id, err)
		}
	}

	if early, err := store.TakeInFlight(ctx, time.Hour); err != nil || len(early) != 0 {
		t.Errorf("TakeInFlight() an hour of grace after the claims = %v, %v, want none", early, err)
	}
	taken, err := store.TakeInFlight(ctx, 0)
	if err != nil {
		t.Fatalf("TakeInFlight() error = %v", err)
	}
	want := []schedule.InFlight{{ScheduleID: "sch_final", Occurrence: due},
		{ScheduleID: "sch_fire", Occurrence: due}}
	if diff := cmp.Diff(want, taken, cmp.Comparer(func(a, b time.Time) bool {
		return a.Equal(b)
	})); diff != "" {
		t.Errorf("TakeInFlight() mismatch (-want +got):\n%s", diff)
	}
	// Taking re-marks what it took, so a second sweep in the same instant takes nothing.
	if again, err := store.TakeInFlight(ctx, time.Second); err != nil || len(again) != 0 {
		t.Errorf("TakeInFlight() right after a take = %v, %v, want none", again, err)
	}

	// The next occurrence cannot be claimed while the one before it is still in flight.
	later := next.Add(5 * time.Minute)
	if won, err := store.ClaimFire(ctx, "sch_fire", next, &later); err != nil || won {
		t.Errorf("ClaimFire() of the next occurrence while one is in flight = %v, %v, want lost",
			won, err)
	}
	for _, settle := range []struct {
		ID string
		At time.Time
	}{{ID: "sch_fire", At: next}, {ID: "sch_gone", At: due}, {ID: "sch_fire", At: due},
		{ID: "sch_fire", At: due}, {ID: "sch_final", At: due}} {
		if err := store.SettleInFlight(ctx, settle.ID, settle.At); err != nil {
			t.Fatalf("SettleInFlight(%s, %v) error = %v", settle.ID, settle.At, err)
		}
	}
	if won, err := store.ClaimFire(ctx, "sch_fire", next, &later); err != nil || !won {
		t.Errorf("ClaimFire() of the next occurrence once the one before settled = %v, %v, want won",
			won, err)
	}
	taken, err = store.TakeInFlight(ctx, 0)
	if err != nil {
		t.Fatalf("TakeInFlight() error = %v", err)
	}
	want = []schedule.InFlight{{ScheduleID: "sch_fire", Occurrence: next}}
	if diff := cmp.Diff(want, taken, cmp.Comparer(func(a, b time.Time) bool {
		return a.Equal(b)
	})); diff != "" {
		t.Errorf("TakeInFlight() after settling mismatch (-want +got):\n%s", diff)
	}
	// A deleted schedule takes its mark with it.
	if err := store.Delete(ctx, "sch_fire"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if left, err := store.TakeInFlight(ctx, 0); err != nil || len(left) != 0 {
		t.Errorf("TakeInFlight() after the delete = %v, %v, want none", left, err)
	}
}
