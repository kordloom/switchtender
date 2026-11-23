package sqlitestore

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// testTemplatePathsByAdmin proves a template's PathsByAdmin survives Save and Get, and survives an
// Update that flips it from true to false.
func testTemplatePathsByAdmin(t *testing.T, store template.Store) {
	ctx := context.Background()
	tests := []struct {
		// ID is the template identifier.
		ID string
		// Saved is the fact on Save.
		Saved bool
	}{{ // Test 0: A template an admin saved.
		ID: "tpl_by_admin", Saved: true,
	}, { // Test 1: A template anyone else saved.
		ID: "tpl_by_other", Saved: false,
	}}
	for testNum, test := range tests {
		tpl := &template.Template{ID: test.ID, Name: "deploy", Playbook: "site.yml",
			PathsByAdmin: test.Saved, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
		if err := store.Save(ctx, tpl); err != nil {
			t.Fatalf("test %d: Save() error = %v", testNum, err)
		}
		got, err := store.Get(ctx, test.ID)
		if err != nil {
			t.Fatalf("test %d: Get() error = %v", testNum, err)
		}
		if got.PathsByAdmin != test.Saved {
			t.Errorf("test %d: after Save PathsByAdmin = %t, want %t", testNum, got.PathsByAdmin, test.Saved)
		}
		tpl.PathsByAdmin = !test.Saved
		if err := store.Update(ctx, tpl); err != nil {
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

// testSchedulePathsByAdmin proves a schedule's PathsByAdmin survives Save and Get, and survives an
// Update that flips it from true to false.
func testSchedulePathsByAdmin(t *testing.T, store schedule.Store) {
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
		sc := &schedule.Schedule{ID: test.ID, Name: "nightly", Cron: "0 9 * * *", Timezone: "UTC",
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

// TestStoresKeepPathsByAdmin runs the template and schedule round trips on SQLite.
func TestStoresKeepPathsByAdmin(t *testing.T) {
	t.Parallel()
	db, err := Open(t.TempDir() + "/st.db")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	testTemplatePathsByAdmin(t, db.Templates())
	testSchedulePathsByAdmin(t, db.Schedules())
}
