package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	_ "modernc.org/sqlite"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/audittest"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/authtest"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/credtest"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/granttest"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/inventorytest"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/invsourcetest"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/orgtest"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/policytest"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/projecttest"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/scheduletest"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/storetest"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/teamtest"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/templatetest"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/triggertest"
	"github.com/kordloom/switchtender/internal/user"
	"github.com/kordloom/switchtender/internal/usertest"
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

// TestStoreMigratesIdempotencyKey proves the on-open migration recovers a database created before
// the idempotency key existed. CREATE TABLE IF NOT EXISTS is a no-op on such a database, so the
// column and its unique index must be added on open or submission dedup silently would not work.
func TestStoreMigratesIdempotencyKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switchtender.db")

	// Open once to build the current schema, then strip the column and index to mimic an older
	// database, the exact state an upgrade must heal.
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	for _, stmt := range []string{
		"DROP INDEX IF EXISTS idx_runs_idempotency_key",
		"ALTER TABLE runs DROP COLUMN idempotency_key",
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("simulate old schema %q: %v", stmt, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	// Reopening runs the migration, which must re-add the column and its unique index so dedup works.
	migrated, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("reopen after downgrade error = %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })
	store := migrated.Runs()

	first := &run.Run{
		ID: "run_1", Playbook: "p", Status: run.StatusPending,
		CreatedAt: time.Now(), IdempotencyKey: "idem",
	}
	if err := store.Save(ctx, first); err != nil {
		t.Fatalf("Save() after migration error = %v", err)
	}
	if got, err := store.ByIdempotencyKey(ctx, "idem"); err != nil || got.ID != "run_1" {
		t.Fatalf("ByIdempotencyKey() after migration = (%v, %v), want run_1", got, err)
	}
	second := &run.Run{
		ID: "run_2", Playbook: "p", Status: run.StatusPending,
		CreatedAt: time.Now(), IdempotencyKey: "idem",
	}
	if err := store.Save(ctx, second); !errors.Is(err, run.ErrDuplicateKey) {
		t.Errorf("Save(second) after migration = %v, want ErrDuplicateKey, the unique index must be rebuilt", err)
	}
}

// TestStoreMigratesTemplateTimeout proves the on-open migration heals a templates table created
// before a template could cap its own run length. CREATE TABLE IF NOT EXISTS is a no-op on such a
// database, so without the migration every read and write of a template would fail on the missing
// column and the whole templates feature would break on upgrade.
func TestStoreMigratesTemplateTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switchtender.db")

	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := raw.Exec("ALTER TABLE templates DROP COLUMN timeout"); err != nil {
		t.Fatalf("simulate old schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	migrated, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("reopen after downgrade error = %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })
	store := migrated.Templates()

	want := &template.Template{
		ID: "tpl_1", Name: "deploy", Playbook: "site.yml",
		Timeout: 600, CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save() after migration error = %v", err)
	}
	got, err := store.Get(ctx, "tpl_1")
	if err != nil {
		t.Fatalf("Get() after migration error = %v", err)
	}
	if got.Timeout != want.Timeout {
		t.Errorf("Timeout after migration = %d, want %d", got.Timeout, want.Timeout)
	}
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
	credtest.TypeContract(t, func() credential.TypeStore {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.CredentialTypes()
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

func TestOrgStoreContract(t *testing.T) {
	t.Parallel()
	orgtest.Contract(t, func() org.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Orgs()
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

// TestStoreMigratesProvenance verifies an upgrade heals a database created before run provenance
// existed. The columns are added on open, and a run written afterward round trips its source,
// actor, lineage, and labels rather than failing on a missing column.
func TestStoreMigratesProvenance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switchtender.db")

	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Strip the provenance columns to mimic a database from before they existed.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	for _, column := range []string{"source", "source_id", "actor", "rerun_of", "labels", "steps"} {
		if _, err := raw.Exec("ALTER TABLE runs DROP COLUMN " + column); err != nil {
			t.Fatalf("simulate old schema, drop %s: %v", column, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	migrated, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("reopen after downgrade error = %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })
	store := migrated.Runs()

	saved := &run.Run{
		ID: "run_1", Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: time.Now(),
		Source: "schedule", SourceID: "sch_1", Actor: "deploy-bot", RerunOf: "run_0",
		Labels: map[string]string{"env": "prod"},
		Steps: []run.PipelineStep{
			{Name: "plan", Tool: "terraform", Command: "terraform plan"},
			{Name: "apply", Tool: "terraform", DependsOn: []string{"plan"}},
		},
	}
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save() after migration error = %v", err)
	}
	got, err := store.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Source != "schedule" || got.SourceID != "sch_1" || got.Actor != "deploy-bot" ||
		got.RerunOf != "run_0" || got.Labels["env"] != "prod" {
		t.Errorf("provenance after migration = %+v, want the saved values", got)
	}
	// A workflow held for approval is run from its stored graph, so an upgraded database has to
	// carry it or an approval that arrives after the upgrade would have nothing left to run.
	if diff := cmp.Diff(saved.Steps, got.Steps); diff != "" {
		t.Errorf("steps after migration mismatch (-want +got):\n%s", diff)
	}

	// The filters that read those columns must work against the healed schema.
	hits, err := store.ListPage(ctx, run.ListFilter{Source: "schedule"}, 0, 0)
	if err != nil || len(hits) != 1 {
		t.Errorf("ListPage(source) = %d runs, err %v, want 1 run", len(hits), err)
	}
	labeled, err := store.ListPage(ctx, run.ListFilter{LabelKey: "env", LabelValue: "prod"}, 0, 0)
	if err != nil || len(labeled) != 1 {
		t.Errorf("ListPage(label) = %d runs, err %v, want 1 run", len(labeled), err)
	}
}
