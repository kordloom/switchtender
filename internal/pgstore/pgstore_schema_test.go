package pgstore

import (
	"database/sql"
	"errors"
	"os"
	"testing"
)

// TestOpenRefusesAnotherApplicationsRunsTable pins the schema probe. A table named runs was taken as
// this product's schema, so a database holding another application's failed the migration with an
// error about a column nobody had heard of. The columns identify the schema, and a foreign table is
// refused by name before anything is touched.
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
