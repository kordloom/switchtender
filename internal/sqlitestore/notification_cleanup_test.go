package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/notificationtest"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/template"
)

// TestDeleteTakesNotificationAttachments runs the attachment cleanup contract against SQLite, for
// every kind of object a notification target can be attached to.
func TestDeleteTakesNotificationAttachments(t *testing.T) {
	t.Parallel()
	notificationtest.CleanupContract(t, func(t *testing.T) (notification.Store,
		map[string]notificationtest.Attachable) {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Notifications(), sqliteAttachables(db)
	})
}

// sqliteAttachables returns the attachable kinds as the SQLite stores provide them.
func sqliteAttachables(db *sqlitestore.DB) map[string]notificationtest.Attachable {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	recorded := func(store any) notification.RecordedDeleter {
		rd, _ := store.(notification.RecordedDeleter)
		return rd
	}
	return map[string]notificationtest.Attachable{
		notification.KindTemplate: {
			Create: func(ctx context.Context, id string) error {
				return db.Templates().Save(ctx, &template.Template{ID: id, Name: id,
					Playbook: "site.yml", CreatedAt: at})
			},
			Delete: db.Templates().Delete, Recorded: recorded(db.Templates()),
		},
		notification.KindSchedule: {
			Create: func(ctx context.Context, id string) error {
				return db.Schedules().Save(ctx, &schedule.Schedule{ID: id, Name: id,
					Cron: "0 2 * * *", Playbook: "site.yml", CreatedAt: at})
			},
			Delete: db.Schedules().Delete, Recorded: recorded(db.Schedules()),
		},
		notification.KindProject: {
			Create: func(ctx context.Context, id string) error {
				return db.Projects().Save(ctx, &project.Project{ID: id, Name: id,
					RepoURL: "https://git.example.com/infra.git", CreatedAt: at})
			},
			Delete: db.Projects().Delete, Recorded: recorded(db.Projects()),
		},
		notification.KindOrg: {
			Create: func(ctx context.Context, id string) error {
				return db.Orgs().Save(ctx, &org.Org{ID: id, Name: id, CreatedAt: at})
			},
			Delete: db.Orgs().Delete, Recorded: recorded(db.Orgs()),
		},
	}
}

// TestRunPurgeTakesItsNotifications pins that retention removes a purged run's notification events
// and deliveries with it, and leaves a kept run's alone.
func TestRunPurgeTakesItsNotifications(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Now().UTC().Truncate(time.Second)
	for id, at := range map[string]time.Time{"run_old": old, "run_new": recent} {
		ended := at.Add(time.Minute)
		if err := db.Runs().Save(ctx, &run.Run{ID: id, Playbook: "site.yml", Inventory: "hosts",
			Status: run.StatusSucceeded, CreatedAt: at, EndedAt: &ended}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
		ok, err := db.Notifications().Record(ctx, &notification.RunEvent{RunID: id,
			Event: "success", Snapshot: []byte(`{"id":"` + id + `"}`), CreatedAt: at},
			[]notification.Recipient{{NotificationID: "ntf_a", Name: "a", Kind: "email"}})
		if err != nil || !ok {
			t.Fatalf("Record(%s) = %v, %v", id, ok, err)
		}
	}
	if _, err := db.Runs().PurgeRunsBefore(ctx, old.Add(24*time.Hour)); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	for id, want := range map[string]int{"run_old": 0, "run_new": 1} {
		list, err := db.Notifications().Deliveries(ctx, notification.DeliveryFilter{RunID: id})
		if err != nil {
			t.Fatalf("Deliveries(%s) error = %v", id, err)
		}
		if len(list) != want {
			t.Errorf("deliveries of %s after the purge = %d, want %d", id, len(list), want)
		}
	}
}
