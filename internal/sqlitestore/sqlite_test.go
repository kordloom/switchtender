package sqlitestore_test

import (
	"context"
	"github.com/dcadolph/switchtender/internal/audit"
	"github.com/dcadolph/switchtender/internal/audittest"
	"github.com/dcadolph/switchtender/internal/auth"
	"github.com/dcadolph/switchtender/internal/authtest"
	"github.com/dcadolph/switchtender/internal/credential"
	"github.com/dcadolph/switchtender/internal/credtest"
	"github.com/dcadolph/switchtender/internal/grant"
	"github.com/dcadolph/switchtender/internal/granttest"
	"github.com/dcadolph/switchtender/internal/inventory"
	"github.com/dcadolph/switchtender/internal/inventorytest"
	"github.com/dcadolph/switchtender/internal/invsource"
	"github.com/dcadolph/switchtender/internal/invsourcetest"
	"github.com/dcadolph/switchtender/internal/policy"
	"github.com/dcadolph/switchtender/internal/policytest"
	"github.com/dcadolph/switchtender/internal/project"
	"github.com/dcadolph/switchtender/internal/projecttest"
	"github.com/dcadolph/switchtender/internal/team"
	"github.com/dcadolph/switchtender/internal/teamtest"
	"github.com/dcadolph/switchtender/internal/template"
	"github.com/dcadolph/switchtender/internal/templatetest"
	"github.com/dcadolph/switchtender/internal/trigger"
	"github.com/dcadolph/switchtender/internal/triggertest"
	"github.com/dcadolph/switchtender/internal/user"
	"github.com/dcadolph/switchtender/internal/usertest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dcadolph/switchtender/internal/run"
	"github.com/dcadolph/switchtender/internal/schedule"
	"github.com/dcadolph/switchtender/internal/scheduletest"
	"github.com/dcadolph/switchtender/internal/sqlitestore"
	"github.com/dcadolph/switchtender/internal/storetest"
)

func TestStoreContract(t *testing.T) {
	t.Parallel()
	storetest.Contract(t, func() run.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Runs()
	})
}

func TestScheduleStoreContract(t *testing.T) {
	t.Parallel()
	scheduletest.Contract(t, func() schedule.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Schedules()
	})
}

func TestTokenStoreContract(t *testing.T) {
	t.Parallel()
	authtest.Contract(t, func() auth.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Tokens()
	})
}

func TestCredentialStoreContract(t *testing.T) {
	t.Parallel()
	credtest.Contract(t, func() credential.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Credentials()
	})
}

func TestProjectStoreContract(t *testing.T) {
	t.Parallel()
	projecttest.Contract(t, func() project.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Projects()
	})
}

func TestTemplateStoreContract(t *testing.T) {
	t.Parallel()
	templatetest.Contract(t, func() template.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Templates()
	})
}

func TestUserStoreContract(t *testing.T) {
	t.Parallel()
	usertest.Contract(t, func() user.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Users()
	})
}

func TestInventoryStoreContract(t *testing.T) {
	t.Parallel()
	inventorytest.Contract(t, func() inventory.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Inventories()
	})
}

func TestPolicyStoreContract(t *testing.T) {
	t.Parallel()
	policytest.Contract(t, func() policy.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Policies()
	})
}

func TestAuditStoreContract(t *testing.T) {
	t.Parallel()
	audittest.Contract(t, func() audit.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Audits()
	})
}

func TestInvSourceStoreContract(t *testing.T) {
	t.Parallel()
	invsourcetest.Contract(t, func() invsource.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.InventorySources()
	})
}

func TestTriggerStoreContract(t *testing.T) {
	t.Parallel()
	triggertest.Contract(t, func() trigger.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Triggers()
	})
}

func TestTeamStoreContract(t *testing.T) {
	t.Parallel()
	teamtest.Contract(t, func() team.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Teams()
	})
}

func TestTeamForeignKeyEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	teams := db.Teams()

	// With foreign keys enforced, a membership cannot reference a team that does not exist.
	if err := teams.AddMember(ctx, "team_ghost", "user_1"); err == nil {
		t.Error("AddMember to a nonexistent team succeeded, want a foreign key error")
	}

	// A member of a real team is dropped when the team is deleted, leaving no orphan row.
	if err := teams.Save(ctx, &team.Team{ID: "team_1", Name: "ops", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := teams.AddMember(ctx, "team_1", "user_1"); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	if err := teams.Delete(ctx, "team_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got, _ := teams.TeamsForUser(ctx, "user_1"); len(got) != 0 {
		t.Errorf("TeamsForUser after delete = %v, want none", got)
	}
}

func TestGrantStoreContract(t *testing.T) {
	t.Parallel()
	granttest.Contract(t, func() grant.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Grants()
	})
}
