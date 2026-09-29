package pgstore

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestOpenRefusesANewSchemaOnCommunity covers the one licensed act in this package: initializing a
// schema that does not exist yet. It needs a real server, so it runs exactly when the rest of the
// suite does. Not parallel: it swaps the process license out and back.
func TestOpenRefusesANewSchemaOnCommunity(t *testing.T) {
	dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if dsn == "" {
		// The same ratchet as the store contract: the release gate demands the full suite, and a
		// licensing gate that quietly skipped there would green-light a release the paid tier's
		// own refusal never ran on.
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN " +
				"is not: the full suite was demanded and the license gate cannot run")
		}
		t.Skip("SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	team := license.Current()
	license.Set(nil)
	defer license.Set(team)

	// The DSN's database already holds a schema from the suite around this test, so the refusal
	// has to be proven against a database that does not: one of this test's own, created empty.
	db, err := Open(freshDatabase(t, dsn))
	if err == nil {
		_ = db.Close()
		t.Fatal("Open() initialized a new schema with no license")
	}
	if !strings.Contains(err.Error(), "Team license") {
		t.Errorf("refusal does not name the tier: %v", err)
	}
}

// TestOpenRefusesAnotherApplicationsRunsTable pins the schema probe. A table named runs was taken as
// this product's schema, so a database holding another application's skipped the license gate and
// then failed the migration with an error about a column nobody had heard of. The columns identify
// the schema, and a foreign table is refused by name before anything is touched.
func TestOpenRefusesAnotherApplicationsRunsTable(t *testing.T) {
	dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN is not")
		}
		t.Skip("SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	target := freshDatabase(t, dsn)
	conn, err := sql.Open("pgx", target)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := conn.Exec("CREATE TABLE runs (id serial PRIMARY KEY, lap_seconds int)"); err != nil {
		t.Fatalf("create the other application's table: %v", err)
	}
	_ = conn.Close()

	db, err := Open(target)
	if err == nil {
		_ = db.Close()
		t.Fatal("Open() accepted a database whose runs table belongs to another application")
	}
	if !errors.Is(err, ErrForeignSchema) {
		t.Errorf("Open() error = %v, want ErrForeignSchema", err)
	}
}
