package sqlitestore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestScheduleWrittenByPaddedRelease covers a row written by a release that stored the fractional
// second padded to nine digits, read by a build that writes it trimmed.
//
// Reverting the format was not enough on its own: rows written while the padded build was deployed
// still could not be claimed, so schedules stayed broken for exactly the installs that had taken the
// upgrade. Open normalizes stored times so a claim does not depend on which release wrote the row.
func TestScheduleWrittenByPaddedRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	next := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	if err := db.Schedules().Save(ctx, &schedule.Schedule{
		ID: "sc_1", Name: "n", Cron: "0 0 * * *", Playbook: "s.yml", Enabled: true, NextRunAt: &next,
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// What v1.34.0 and v1.34.1 wrote: nine fractional digits, always present.
	raw, _ := sql.Open("sqlite", path)
	if _, err := raw.Exec("UPDATE schedules SET next_run_at=?", "2026-07-31T00:00:00.000000000Z"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	db2, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	got, err := db2.Schedules().Get(ctx, "sc_1")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := db2.Schedules().ClaimDue(ctx, "sc_1", *got.NextRunAt, got.NextRunAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("A SCHEDULE WRITTEN BY v1.34.x CANNOT BE CLAIMED BY v1.35.0")
	}
}
