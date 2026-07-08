package pgstore_test

import (
	"database/sql"
	"github.com/dcadolph/yardmaster/internal/audit"
	"github.com/dcadolph/yardmaster/internal/audittest"
	"github.com/dcadolph/yardmaster/internal/auth"
	"github.com/dcadolph/yardmaster/internal/authtest"
	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/credtest"
	"github.com/dcadolph/yardmaster/internal/grant"
	"github.com/dcadolph/yardmaster/internal/granttest"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/inventorytest"
	"github.com/dcadolph/yardmaster/internal/invsource"
	"github.com/dcadolph/yardmaster/internal/invsourcetest"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/projecttest"
	"github.com/dcadolph/yardmaster/internal/team"
	"github.com/dcadolph/yardmaster/internal/teamtest"
	"github.com/dcadolph/yardmaster/internal/template"
	"github.com/dcadolph/yardmaster/internal/templatetest"
	"github.com/dcadolph/yardmaster/internal/trigger"
	"github.com/dcadolph/yardmaster/internal/triggertest"
	"github.com/dcadolph/yardmaster/internal/user"
	"github.com/dcadolph/yardmaster/internal/usertest"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/dcadolph/yardmaster/internal/pgstore"
	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/schedule"
	"github.com/dcadolph/yardmaster/internal/scheduletest"
	"github.com/dcadolph/yardmaster/internal/storetest"
)

// dsnEnv names the environment variable that provides the test database. Without it the contract
// is skipped, so the default suite needs no PostgreSQL; CI provides one as a service container.
const dsnEnv = "YARDMASTER_POSTGRES_DSN"

// testDSN returns the test database DSN or skips the test.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s to run the PostgreSQL contract", dsnEnv)
	}
	return dsn
}

// truncateAll clears every table so each contract subtest starts from an empty store.
func truncateAll(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	const q = `TRUNCATE runs, run_logs, run_events, run_host_summary, run_task_summary, schedules`
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func TestStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	storetest.Contract(t, func() run.Store {
		truncateAll(t, dsn)
		return db.Runs()
	})
}

func TestScheduleStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	scheduletest.Contract(t, func() schedule.Store {
		truncateAll(t, dsn)
		return db.Schedules()
	})
}

func TestTokenStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	authtest.Contract(t, func() auth.Store {
		truncateTokens(t, dsn)
		return db.Tokens()
	})
}

// truncateTokens clears the tokens table between contract subtests.
func truncateTokens(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE tokens"); err != nil {
		t.Fatalf("truncate tokens: %v", err)
	}
}

func TestCredentialStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	credtest.Contract(t, func() credential.Store {
		truncateCredentials(t, dsn)
		return db.Credentials()
	})
}

// truncateCredentials clears the credentials table between contract subtests.
func truncateCredentials(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE credentials"); err != nil {
		t.Fatalf("truncate credentials: %v", err)
	}
}

func TestProjectStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	projecttest.Contract(t, func() project.Store {
		truncateProjects(t, dsn)
		return db.Projects()
	})
}

// truncateProjects clears the projects table between contract subtests.
func truncateProjects(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE projects"); err != nil {
		t.Fatalf("truncate projects: %v", err)
	}
}

func TestInventoryStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	inventorytest.Contract(t, func() inventory.Store {
		truncateInventories(t, dsn)
		return db.Inventories()
	})
}

// truncateInventories clears the inventories table between contract subtests.
func truncateInventories(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE inventories"); err != nil {
		t.Fatalf("truncate inventories: %v", err)
	}
}

func TestTemplateStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	templatetest.Contract(t, func() template.Store {
		truncateTemplates(t, dsn)
		return db.Templates()
	})
}

// truncateTemplates clears the templates table between contract subtests.
func truncateTemplates(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE templates"); err != nil {
		t.Fatalf("truncate templates: %v", err)
	}
}

func TestUserStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	usertest.Contract(t, func() user.Store {
		truncateUsers(t, dsn)
		return db.Users()
	})
}

// truncateUsers clears the users table between contract subtests.
func truncateUsers(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE users"); err != nil {
		t.Fatalf("truncate users: %v", err)
	}
}

func TestAuditStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	audittest.Contract(t, func() audit.Store {
		truncateAudit(t, dsn)
		return db.Audits()
	})
}

// truncateAudit clears the audit table between contract subtests.
func truncateAudit(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE audit_entries"); err != nil {
		t.Fatalf("truncate audit_entries: %v", err)
	}
}

func TestInvSourceStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	invsourcetest.Contract(t, func() invsource.Store {
		truncateSources(t, dsn)
		return db.InventorySources()
	})
}

// truncateSources clears the inventory source table between contract subtests.
func truncateSources(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE inventory_sources"); err != nil {
		t.Fatalf("truncate inventory_sources: %v", err)
	}
}

func TestTriggerStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	triggertest.Contract(t, func() trigger.Store {
		truncateTriggers(t, dsn)
		return db.Triggers()
	})
}

// truncateTriggers clears the triggers table between contract subtests.
func truncateTriggers(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE triggers"); err != nil {
		t.Fatalf("truncate triggers: %v", err)
	}
}

func TestTeamStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	teamtest.Contract(t, func() team.Store {
		truncateTeams(t, dsn)
		return db.Teams()
	})
}

// truncateTeams clears the team tables between contract subtests.
func truncateTeams(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE teams, team_members"); err != nil {
		t.Fatalf("truncate teams: %v", err)
	}
}

func TestGrantStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	granttest.Contract(t, func() grant.Store {
		truncateGrants(t, dsn)
		return db.Grants()
	})
}

// truncateGrants clears the grants table between contract subtests.
func truncateGrants(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("TRUNCATE grants"); err != nil {
		t.Fatalf("truncate grants: %v", err)
	}
}
