package demo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestSeedConfigPredatesTheNightlyHistory holds every seeded configuration object to a creation
// time older than the oldest run the seed can place, the first nightly audit, two weeks and the
// run window before now. A schedule created three days ago does not fire two weeks of audits, and
// a template whose history is older than itself is a contradiction a careful reader catches. The
// inventory source's last sync is a reading, not a creation, and stays recent, which is the
// control.
func TestSeedConfigPredatesTheNightlyHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	before := time.Now()
	seedConfig(ctx, stores.deps(), zap.NewNop())
	oldestRun := before.Add(-seedRunWindow - seedHistorySpan)

	var created []struct {
		// Name is the object.
		Name string
		// At is when it was created.
		At time.Time
	}
	add := func(kind, name string, at time.Time) {
		created = append(created, struct {
			Name string
			At   time.Time
		}{Name: kind + " " + name, At: at})
	}
	templates, _ := stores.Templates.List(ctx)
	for _, x := range templates {
		add("template", x.Name, x.CreatedAt)
	}
	schedules, _ := stores.Schedules.List(ctx)
	for _, x := range schedules {
		add("schedule", x.Name, x.CreatedAt)
	}
	policies, _ := stores.Policies.List(ctx)
	for _, x := range policies {
		add("policy", x.Name, x.CreatedAt)
	}
	users, _ := stores.Users.List(ctx)
	for _, x := range users {
		add("user", x.Username, x.CreatedAt)
	}
	projects, _ := stores.Projects.List(ctx)
	for _, x := range projects {
		add("project", x.Name, x.CreatedAt)
	}
	if len(created) < 15 {
		t.Fatalf("only %d objects were read, want the whole seeded catalog", len(created))
	}
	for i, c := range created {
		t.Run(fmt.Sprintf("test %d %s", i, c.Name), func(t *testing.T) {
			t.Parallel()
			if !c.At.Before(oldestRun) {
				t.Errorf("%s was created %v, after the oldest run the seed places at %v", c.Name,
					c.At, oldestRun)
			}
		})
	}
	sources, _ := stores.InvSources.List(ctx)
	for _, src := range sources {
		if src.SyncedAt == nil || src.SyncedAt.Before(before.Add(-2*time.Hour)) {
			t.Errorf("source %s last synced %v, want a recent reading", src.Name, src.SyncedAt)
		}
	}
}
