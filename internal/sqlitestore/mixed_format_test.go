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

// TestListSurvivesOneUnreadableScheduleStamp is the SQLite half of the bound pgstore holds, since the
// two stores read these stamps identically and a fix in one is no fix at all for an install on the
// other.
//
// One schedule with a next_run_at nothing can parse used to fail the scan, and List turns any scan
// error into a failure for the whole query, so every schedule in the install disappeared behind one
// error: the schedules page errored and the scheduler enumerated nothing, so nothing fired. The
// migration that normalizes these stamps leaves one it cannot read in place on purpose, reasoning
// that the schedule is already broken, and this is what keeps that cost on the one row.
func TestListSurvivesOneUnreadableScheduleStamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	next := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"sc_healthy", "sc_damaged"} {
		if err := db.Schedules().Save(ctx, &schedule.Schedule{
			ID: id, Name: id, Cron: "* * * * *", Playbook: "s.yml", Enabled: true, NextRunAt: &next,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}
	_ = db.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE schedules SET next_run_at=? WHERE id=?",
		"not a timestamp", "sc_damaged"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	db2, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db2.Close() }()
	got, err := db2.Schedules().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v.\n\nOne schedule with an unreadable next_run_at took down the "+
			"listing for every schedule in the install, so the schedules page errors and the "+
			"scheduler enumerates nothing: no schedule fires at all, not just the damaged one.", err)
	}
	seen := map[string]*schedule.Schedule{}
	for _, sc := range got {
		seen[sc.ID] = sc
	}
	if seen["sc_healthy"] == nil {
		t.Error("the healthy schedule is missing, so the damaged row still cost more than itself")
	}
	if seen["sc_healthy"] != nil && seen["sc_healthy"].NextRunAt == nil {
		t.Error("the healthy schedule lost its next run, so it will not fire either")
	}
	// The damaged row is listed rather than hidden. A schedule that vanishes from the page reads as
	// one somebody deleted, and an operator cannot repair what they cannot see.
	if seen["sc_damaged"] == nil {
		t.Error("the damaged schedule was dropped from the listing instead of shown without a next run")
	}
	if seen["sc_damaged"] != nil && seen["sc_damaged"].NextRunAt != nil {
		t.Error("the damaged stamp was read as a time, so a value nothing can parse became one")
	}
}
