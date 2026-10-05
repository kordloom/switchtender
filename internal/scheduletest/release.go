package scheduletest

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
)

// testRelease verifies an occurrence a claim took is handed back only while the row still holds
// what the claim wrote, for an advancing claim and for the claim of a last occurrence alike, so an
// edit or another server's claim made in between is never undone.
func testRelease(t *testing.T, store schedule.Store) {
	ctx := context.Background()
	due := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	next := due.Add(5 * time.Minute)
	for _, id := range []string{"sch_cycle", "sch_last"} {
		at := due
		if err := store.Save(ctx, &schedule.Schedule{
			ID: id, Cron: "*/5 * * * *", Timezone: "UTC", Playbook: "p.yml", Enabled: true,
			CreatedAt: due.Add(-time.Hour), NextRunAt: &at,
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}
	if won, err := store.ClaimDue(ctx, "sch_cycle", due, next); err != nil || !won {
		t.Fatalf("ClaimDue() = %v, %v, want a won claim", won, err)
	}
	if won, err := store.ClaimFinal(ctx, "sch_last", due); err != nil || !won {
		t.Fatalf("ClaimFinal() = %v, %v, want a won claim", won, err)
	}
	other := due.Add(10 * time.Minute)
	tests := []struct {
		Claimed  *time.Time
		ID       string
		WantNext *time.Time
		WantBack bool
	}{{ // Test 0: A claim that wrote a different time is not the one that moved the row.
		ID: "sch_cycle", Claimed: &other, WantBack: false, WantNext: &next,
	}, { // Test 1: A final claim's release does not touch a row that has a next fire.
		ID: "sch_cycle", Claimed: nil, WantBack: false, WantNext: &next,
	}, { // Test 2: The claim's own write is handed back to the occurrence it took.
		ID: "sch_cycle", Claimed: &next, WantBack: true, WantNext: &due,
	}, { // Test 3: A second hand back finds the occurrence already back and changes nothing.
		ID: "sch_cycle", Claimed: &next, WantBack: false, WantNext: &due,
	}, { // Test 4: An advancing claim's release does not touch a row a final claim cleared.
		ID: "sch_last", Claimed: &next, WantBack: false, WantNext: nil,
	}, { // Test 5: The last occurrence a final claim took is handed back.
		ID: "sch_last", Claimed: nil, WantBack: true, WantNext: &due,
	}, { // Test 6: A row that is gone reports no hand back and no error.
		ID: "sch_gone", Claimed: nil, WantBack: false,
	}}
	for testNum, test := range tests {
		back, err := store.Release(ctx, test.ID, test.Claimed, due)
		if err != nil {
			t.Fatalf("test %d: Release() error = %v", testNum, err)
		}
		if back != test.WantBack {
			t.Errorf("test %d: Release() = %v, want %v", testNum, back, test.WantBack)
		}
		if test.ID == "sch_gone" {
			continue
		}
		got, err := store.Get(ctx, test.ID)
		if err != nil {
			t.Fatalf("test %d: Get() error = %v", testNum, err)
		}
		switch {
		case test.WantNext == nil && got.NextRunAt != nil:
			t.Errorf("test %d: NextRunAt = %v, want none", testNum, got.NextRunAt)
		case test.WantNext != nil && (got.NextRunAt == nil || !got.NextRunAt.Equal(*test.WantNext)):
			t.Errorf("test %d: NextRunAt = %v, want %v", testNum, got.NextRunAt, *test.WantNext)
		}
	}
	// A handed back occurrence is due again, and claiming it works exactly as the first time did.
	if won, err := store.ClaimDue(ctx, "sch_cycle", due, next); err != nil || !won {
		t.Errorf("ClaimDue() after a hand back = %v, %v, want a won claim", won, err)
	}
}
