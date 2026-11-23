package schedule

import (
	"context"
	"testing"
	"time"
)

// testSchedulePathsByAdmin proves a schedule's PathsByAdmin survives Save and Get, and survives an
// Update that flips it from true to false.
func testSchedulePathsByAdmin(t *testing.T, store Store) {
	ctx := context.Background()
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		// ID is the schedule identifier.
		ID string
		// Saved is the fact on Save.
		Saved bool
	}{{ // Test 0: A schedule an admin saved.
		ID: "sch_by_admin", Saved: true,
	}, { // Test 1: A schedule anyone else saved.
		ID: "sch_by_other", Saved: false,
	}}
	for testNum, test := range tests {
		sc := &Schedule{ID: test.ID, Name: "nightly", Cron: "0 9 * * *", Timezone: "UTC",
			Playbook: "site.yml", Enabled: true, PathsByAdmin: test.Saved, CreatedAt: at, NextRunAt: &at}
		if err := store.Save(ctx, sc); err != nil {
			t.Fatalf("test %d: Save() error = %v", testNum, err)
		}
		got, err := store.Get(ctx, test.ID)
		if err != nil {
			t.Fatalf("test %d: Get() error = %v", testNum, err)
		}
		if got.PathsByAdmin != test.Saved {
			t.Errorf("test %d: after Save PathsByAdmin = %t, want %t", testNum, got.PathsByAdmin, test.Saved)
		}
		sc.PathsByAdmin = !test.Saved
		if err := store.Update(ctx, sc); err != nil {
			t.Fatalf("test %d: Update() error = %v", testNum, err)
		}
		got, err = store.Get(ctx, test.ID)
		if err != nil {
			t.Fatalf("test %d: Get() after Update error = %v", testNum, err)
		}
		if got.PathsByAdmin != !test.Saved {
			t.Errorf("test %d: after Update PathsByAdmin = %t, want %t", testNum, got.PathsByAdmin,
				!test.Saved)
		}
	}
}

// TestMemStoreKeepsSchedulePathsByAdmin runs the round trip on the memory store.
func TestMemStoreKeepsSchedulePathsByAdmin(t *testing.T) {
	t.Parallel()
	testSchedulePathsByAdmin(t, NewMemStore())
}
