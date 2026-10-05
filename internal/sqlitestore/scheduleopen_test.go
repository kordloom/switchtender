package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// openScheduleTestDB opens a fresh SQLite store holding the given schedules and returns its path.
func openScheduleTestDB(t *testing.T, schedules ...*schedule.Schedule) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "switchtender.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	for _, sc := range schedules {
		if err := db.Schedules().Save(context.Background(), sc); err != nil {
			t.Fatalf("Save(%s) error = %v", sc.ID, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return path
}

// rawScheduleDB opens a plain handle on a store's file for writes an earlier release made.
func rawScheduleDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open a raw handle: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

// TestScheduleTimeRewriteKeepsAClaimMadeAfterItsRead reads the stamps an earlier release wrote,
// lets a server of that release claim the due occurrence, and only then writes the canonical forms.
//
// The rewrite used to be keyed by id alone, so it wrote the stamp it read back over the claim and
// the occurrence that had just fired was due again. It now changes a row only while the row still
// holds the stamp that was read.
func TestScheduleTimeRewriteKeepsAClaimMadeAfterItsRead(t *testing.T) {
	t.Parallel()
	due := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	path := openScheduleTestDB(t, &schedule.Schedule{
		ID: "sch_norm", Name: "every five minutes", Cron: "*/5 * * * *", Timezone: "UTC",
		Playbook: "sync.yml", Enabled: true, CreatedAt: due.Add(-time.Hour), NextRunAt: &due,
	})
	raw := rawScheduleDB(t, path)
	const oldWidth, oldAdvanced = "2026-10-04T10:00:00.000000000Z", "2026-10-04T10:05:00.000000000Z"
	if _, err := raw.Exec("UPDATE schedules SET next_run_at=? WHERE id='sch_norm'",
		oldWidth); err != nil {
		t.Fatalf("store the earlier release's stamp: %v", err)
	}
	stale, err := staleScheduleTimes(raw)
	if err != nil {
		t.Fatalf("staleScheduleTimes() error = %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("staleScheduleTimes() = %d rows, want the one the earlier release wrote", len(stale))
	}
	if _, err := raw.Exec("UPDATE schedules SET next_run_at=? WHERE id='sch_norm' AND "+
		"next_run_at=?", oldAdvanced, oldWidth); err != nil {
		t.Fatalf("claim the due occurrence: %v", err)
	}
	if err := rewriteScheduleTimes(raw, stale); err != nil {
		t.Fatalf("rewriteScheduleTimes() error = %v", err)
	}
	var stored string
	if err := raw.QueryRow("SELECT next_run_at FROM schedules WHERE id='sch_norm'").
		Scan(&stored); err != nil {
		t.Fatalf("read the next fire: %v", err)
	}
	got, err := sqlutil.ParseTime(stored)
	if err != nil {
		t.Fatalf("parse %q: %v", stored, err)
	}
	if want := due.Add(5 * time.Minute); !got.Equal(want) {
		t.Errorf("next fire after the claim = %s, want %s: the rewrite put the occurrence that "+
			"was just fired back as due", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestOpenPinsTheZoneOfAScheduleThatNamesNone stores schedules the way an earlier release did,
// with no timezone, and opens the store again.
//
// A schedule naming no zone was read in the zone of whichever server evaluated it, so two servers
// in different zones fired one schedule at two different times. Open writes schedule.UnnamedZone
// onto every such row, and leaves alone a schedule that names its zone in any of the three places
// one can.
func TestOpenPinsTheZoneOfAScheduleThatNamesNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		Schedule *schedule.Schedule
		WantZone string
	}{{ // Test 0: A cron expression with no zone anywhere is pinned.
		Schedule: &schedule.Schedule{Cron: "0 9 * * *"}, WantZone: schedule.UnnamedZone,
	}, { // Test 1: A recurrence with a floating DTSTART is pinned.
		Schedule: &schedule.Schedule{RRule: "DTSTART:20261001T090000 RRULE:FREQ=DAILY"},
		WantZone: schedule.UnnamedZone,
	}, { // Test 2: A cron expression carrying its own descriptor is left as written.
		Schedule: &schedule.Schedule{Cron: "CRON_TZ=Asia/Tokyo 0 9 * * *"}, WantZone: "",
	}, { // Test 3: A recurrence whose DTSTART names its zone is left as written.
		Schedule: &schedule.Schedule{
			RRule: "DTSTART;TZID=Europe/Berlin:20261001T090000 RRULE:FREQ=DAILY",
		},
		WantZone: "",
	}, { // Test 4: A schedule that names its timezone keeps it.
		Schedule: &schedule.Schedule{Cron: "0 9 * * *", Timezone: "America/New_York"},
		WantZone: "America/New_York",
	}}
	var stored []*schedule.Schedule
	for testNum, test := range tests {
		sc := test.Schedule
		sc.ID, sc.Playbook, sc.Enabled = fmt.Sprintf("sch_%d", testNum), "p.yml", true
		sc.CreatedAt, sc.NextRunAt = at.Add(time.Duration(testNum)*time.Minute), &at
		stored = append(stored, sc)
	}
	path := openScheduleTestDB(t, stored...)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var want, got []string
	for testNum, test := range tests {
		sc, err := db.Schedules().Get(ctx, fmt.Sprintf("sch_%d", testNum))
		if err != nil {
			t.Fatalf("Get(sch_%d) error = %v", testNum, err)
		}
		want = append(want, test.WantZone)
		got = append(got, sc.Timezone)
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("zones after opening the store (-want +got):\n%s", diff)
	}
}
