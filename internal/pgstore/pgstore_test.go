package pgstore_test

import (
	"database/sql"
	"github.com/dcadolph/yardmaster/internal/auth"
	"github.com/dcadolph/yardmaster/internal/authtest"
	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/credtest"
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
