package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
)

// TestUpgradedDatabaseHealsTheScheduleSkipColumns pins that a database from before schedules
// counted skipped fires or chose what a spring-forward night does opens, takes a skip, and reads
// both back. Every schedule read names all three columns, so an upgrade that missed any one would
// stop the scheduler from listing anything at all.
func TestUpgradedDatabaseHealsTheScheduleSkipColumns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db := openStoreAt(t, path)
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	rawExec(t, path,
		"ALTER TABLE schedules DROP COLUMN last_skip",
		"ALTER TABLE schedules DROP COLUMN skipped_fires",
		"ALTER TABLE schedules DROP COLUMN spring_forward")

	store := openStoreAt(t, path).Schedules()
	next := baseTime.Add(time.Hour)
	if err := store.Save(ctx, &schedule.Schedule{
		ID: "sch_old", Name: "old", Cron: "0 * * * *", TemplateID: "tpl_x", Enabled: true,
		CreatedAt: baseTime, NextRunAt: &next, SpringForward: schedule.SpringForwardSkip,
	}); err != nil {
		t.Fatalf("Save() on a healed database error = %v", err)
	}
	if err := store.RecordSkip(ctx, "sch_old", baseTime, schedule.SkipNoHosts); err != nil {
		t.Fatalf("RecordSkip() on a healed database error = %v", err)
	}
	got, err := store.Get(ctx, "sch_old")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LastSkip != schedule.SkipNoHosts || got.SkippedFires != 1 {
		t.Errorf("healed schedule skip = %q x%d, want %q x1", got.LastSkip, got.SkippedFires,
			schedule.SkipNoHosts)
	}
	if got.SpringForward != schedule.SpringForwardSkip {
		t.Errorf("healed schedule spring forward = %q, want %q", got.SpringForward,
			schedule.SpringForwardSkip)
	}
}
