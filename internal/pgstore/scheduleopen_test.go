package pgstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/schedule"
)

// TestOpenPinsTheZoneOfAScheduleThatNamesNone stores schedules the way an earlier release did,
// with no timezone, and opens the store again, on PostgreSQL, where a highly available pair shares
// the rows.
//
// A schedule naming no zone was read in the zone of whichever server evaluated it, so two servers
// in different zones fired one schedule at two different times. Open writes schedule.UnnamedZone
// onto every such row, and leaves alone a schedule that names its zone in any of the three places
// one can.
func TestOpenPinsTheZoneOfAScheduleThatNamesNone(t *testing.T) {
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	dsn := freshDatabase(t, shared)
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
	first, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	for testNum, test := range tests {
		sc := test.Schedule
		sc.ID, sc.Playbook, sc.Enabled = fmt.Sprintf("sch_%d", testNum), "p.yml", true
		sc.CreatedAt, sc.NextRunAt = at.Add(time.Duration(testNum)*time.Minute), &at
		if err := first.Schedules().Save(ctx, sc); err != nil {
			t.Fatalf("Save(%s) error = %v", sc.ID, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	db, err := Open(dsn)
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
