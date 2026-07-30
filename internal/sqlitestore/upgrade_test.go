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

// TestScheduleSurvivesUpgrade writes a schedule row in the timestamp form an older release stored,
// then claims it the way the scheduler does.
//
// ClaimDue is a compare and swap on next_run_at as text. A release that changed how timestamps are
// written, without migrating the rows already there, made every existing schedule unclaimable: the
// stored text no longer equaled the text the new code compared against. The scheduler treats a lost
// claim as another node having won, so nothing was logged and every schedule simply stopped firing.
// Any future change to the stored form has to keep this passing or carry a migration.
func TestScheduleSurvivesUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	next := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	if err := db.Schedules().Save(ctx, &schedule.Schedule{
		ID: "sc_1", Name: "nightly", Cron: "0 0 * * *", Playbook: "site.yml",
		Enabled: true, NextRunAt: &next,
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// Rewrite next_run_at in the pre-v1.34.0 trimmed form, which is what an upgraded install holds.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE schedules SET next_run_at=?", "2026-07-31T00:00:00Z"); err != nil {
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
	claimed, err := db2.Schedules().ClaimDue(ctx, "sc_1", *got.NextRunAt, got.NextRunAt.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("SCHEDULE DID NOT FIRE: a pre-upgrade row can never be claimed, so it stops firing forever")
	}
	t.Log("claimed, the schedule still fires after upgrade")
}
