package schedule

import (
	"context"
	"testing"
	"time"
)

// TestScheduleWithoutTimezoneFiresTwiceAcrossReplicasInDifferentZones runs a highly available pair
// over one store, the shape two serve processes on one PostgreSQL have, where one replica's local
// zone is UTC, as a container's is, and the other's is America/Chicago, as a host set to the team's
// zone is. Both tick every fifteen seconds through one day, and the Chicago replica happens to tick
// first.
//
// A schedule that names no timezone reads its cron expression in "the server's local time", and
// the highly available pair promises that a due schedule is claimed by a compare-and-set so exactly
// one server fires it. The compare-and-set guards one occurrence at a time, but each winner
// computes the next fire in its own zone, so a claim won on the Chicago replica moves the 09:00 UTC
// occurrence to 09:00 Chicago the same day, and the daily job runs twice in one day. Nothing at
// creation pins the zone the schedule was written in, and nothing refuses or warns about replicas
// that disagree.
func TestScheduleWithoutTimezoneFiresTwiceAcrossReplicasInDifferentZones(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	chicago := schZone(t, "America/Chicago")
	day := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	// Created on the UTC replica, which computed the first fire as 09:00 UTC.
	due := day.Add(9 * time.Hour)
	store := NewMemStore()
	if err := store.Save(ctx, &Schedule{
		ID: "sch_local", Name: "daily report", Cron: "0 9 * * *", Playbook: "report.yml",
		Enabled: true, CreatedAt: day.Add(-time.Hour), NextRunAt: &due,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sub := &schCounter{}
	inUTC := NewScheduler(store, sub, nil)
	inChicago := NewScheduler(store, sub, nil)
	t.Cleanup(inUTC.Close)
	t.Cleanup(inChicago.Close)
	var fires []string
	for at := day; at.Before(day.Add(24 * time.Hour)); at = at.Add(DefaultInterval) {
		before := sub.fired.Load()
		// A replica's ticker hands it times in its own local zone, which is the zone the cron
		// library reads an expression with no zone descriptor in.
		inChicago.tick(at.In(chicago))
		inUTC.tick(at.In(time.UTC))
		if sub.fired.Load() != before {
			fires = append(fires, at.Format(time.RFC3339))
		}
	}
	if len(fires) != 1 {
		t.Errorf("a daily schedule fired %d times in one day across the pair, at %v, want once",
			len(fires), fires)
	}
}
