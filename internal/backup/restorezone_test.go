package backup

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/schedule"
)

// TestRestorePinsTheZoneOfAScheduleThatNamesNone restores a backup an earlier release wrote, whose
// schedules name no zone, beside one that names its own.
//
// The restore wrote the row exactly as backed up, so every server reading the restored install
// read the zone-less schedule in a zone of its own, the double fire a highly available pair hit.
// The restore writes schedule.UnnamedZone onto it, the zone this release reads it in and the zone
// opening the store would write, and works out its next fire in that zone.
func TestRestorePinsTheZoneOfAScheduleThatNamesNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	src := freshStores()
	stale := time.Now().Add(-48 * time.Hour)
	for _, sc := range []*schedule.Schedule{
		{ID: "sch_unnamed", Cron: "0 2 * * *"},
		{ID: "sch_named", Cron: "0 2 * * *", Timezone: "America/New_York"},
		{ID: "sch_descriptor", Cron: "CRON_TZ=Asia/Tokyo 0 2 * * *"},
	} {
		sc.Name, sc.Playbook, sc.Enabled, sc.CreatedAt, sc.NextRunAt = sc.ID, "site.yml", true,
			stale, &stale
		if err := src.Schedules.Save(ctx, sc); err != nil {
			t.Fatalf("Save(%s) error = %v", sc.ID, err)
		}
	}
	var buf bytes.Buffer
	if _, err := Write(ctx, src, sealer, &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	dst := freshStores()
	if _, err := Read(ctx, dst, sealer, &buf); err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	tests := []struct {
		ID       string
		WantZone string
		WantHour string
	}{{ // Test 0: A schedule that names no zone comes back pinned, firing at 02:00 in that zone.
		ID: "sch_unnamed", WantZone: schedule.UnnamedZone, WantHour: "02:00 " + schedule.UnnamedZone,
	}, { // Test 1: A named zone is kept.
		ID: "sch_named", WantZone: "America/New_York", WantHour: "02:00 America/New_York",
	}, { // Test 2: A cron descriptor already names the zone, so nothing is written.
		ID: "sch_descriptor", WantZone: "", WantHour: "02:00 Asia/Tokyo",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := dst.Schedules.Get(ctx, test.ID)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", test.ID, err)
			}
			if diff := cmp.Diff(test.WantZone, got.Timezone); diff != "" {
				t.Errorf("restored zone mismatch (-want +got):\n%s", diff)
			}
			if got.NextRunAt == nil {
				t.Fatal("the restored schedule has no next fire")
			}
			zone := test.WantHour[len("02:00 "):]
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatalf("load %s: %v", zone, err)
			}
			if hour := got.NextRunAt.In(loc).Format("15:04") + " " + zone; hour != test.WantHour {
				t.Errorf("restored next fire = %s, want %s", hour, test.WantHour)
			}
		})
	}
}
