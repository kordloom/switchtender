package pgstore

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// TestOpenStillRepairsWhatItSkips is the control on the migration skip.
//
// Open no longer migrates when the catalog says the schema is already current, which is what stops
// every ordinary start from taking AccessExclusiveLock on every table and deadlocking the writes of
// nodes that are already running. The danger in that is obvious and total: a currency check that
// answers yes when it should answer no turns off migration for the whole product, silently, and the
// first sign is an upgraded binary reading a column that was never added.
//
// So every condition the check asks about is broken here in turn, and Open has to notice and repair
// each one. A check stuck on yes fails every one.
//
// It runs against a database of its own, created and dropped here, because it damages the schema and
// the rest of the suite shares one.
func TestOpenStillRepairsWhatItSkips(t *testing.T) {
	dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if dsn == "" {
		// The same ratchet the rest of this package uses: a release gate that demanded the full
		// suite must not pass because the one control on the migration skip quietly stood down.
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN " +
				"is not: the full suite was demanded and the migration skip has no control on it")
		}
		t.Skip("SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	own := freshDatabase(t, dsn)

	db, err := Open(own)
	if err != nil {
		t.Fatalf("Open() on a new database error = %v", err)
	}
	if current, cerr := schemaIsCurrent(db.db); cerr != nil || !current {
		t.Fatalf("schemaIsCurrent() = %v, %v right after the schema was applied, so every start "+
			"will migrate and nothing is saved", current, cerr)
	}

	table, column := anAddableColumn(t)
	indexes := sqlutil.ParseSchemaIndexes(schema)
	if len(indexes.Created) == 0 || len(indexes.Dropped) == 0 {
		t.Fatal("the schema declares no index to create or none to drop, so this control cannot " +
			"cover those two conditions")
	}

	damage := []struct {
		// What says which condition is being broken.
		What string
		// SQL breaks it.
		SQL string
		// Check reports whether Open put it back.
		Check func(*sql.DB) (bool, error)
	}{{
		What: "a column an ALTER can add",
		SQL:  fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s CASCADE", table, column),
		Check: func(raw *sql.DB) (bool, error) {
			return has(raw, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2)`,
				table, column)
		},
	}, {
		What: "an index the schema declares",
		SQL:  "DROP INDEX " + indexes.Created[0],
		Check: func(raw *sql.DB) (bool, error) {
			return has(raw, `SELECT EXISTS (SELECT 1 FROM pg_indexes
				WHERE schemaname = current_schema() AND indexname = $1)`, indexes.Created[0])
		},
	}, {
		What:  "the trigger that owes a finished run's outcome to the chain",
		SQL:   "DROP TRIGGER runs_outcome_owed ON runs",
		Check: func(raw *sql.DB) (bool, error) { return hasRunTrigger(raw, "runs_outcome_owed") },
	}, {
		What:  "the trigger that owes a finished run's end to its notification targets",
		SQL:   "DROP TRIGGER runs_owe_end ON runs",
		Check: func(raw *sql.DB) (bool, error) { return hasRunTrigger(raw, "runs_owe_end") },
	}}

	for _, d := range damage {
		t.Run(d.What, func(t *testing.T) {
			raw := rawHandle(t, own)
			if _, err := raw.Exec(d.SQL); err != nil {
				t.Fatalf("break %s: %v", d.What, err)
			}
			current, cerr := schemaIsCurrent(raw)
			if cerr != nil {
				t.Fatalf("schemaIsCurrent() error = %v", cerr)
			}
			if current {
				t.Fatalf("schemaIsCurrent() = true with %s missing, so Open will skip the "+
					"migration and the database stays broken", d.What)
			}
			again, err := Open(own)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer func() { _ = again.Close() }()
			ok, err := d.Check(raw)
			if err != nil {
				t.Fatalf("check %s: %v", d.What, err)
			}
			if !ok {
				t.Errorf("Open() did not restore %s", d.What)
			}
		})
	}

	// The other direction: an index the schema removes, put back by hand. A check that only ever
	// looks for things that should be there would call this current and leave it in place.
	t.Run("an index the schema drops", func(t *testing.T) {
		raw := rawHandle(t, own)
		gone := indexes.Dropped[0]
		// Built over the table and column derived above rather than the one the removed index was
		// originally on, because what the schema's DROP targets is the name. Hardcoding a table
		// stood this control down the first time it ran: the index is called
		// idx_host_summary_host and the table is run_host_summary. A setup that cannot run is a
		// failure here and not a skip, since a control that quietly declines to run is the thing
		// this whole file exists to catch.
		if _, err := raw.Exec(fmt.Sprintf("CREATE INDEX %s ON %s (%s)", gone, table, column)); err != nil {
			t.Fatalf("recreate %s on %s(%s): %v", gone, table, column, err)
		}
		current, cerr := schemaIsCurrent(raw)
		if cerr != nil {
			t.Fatalf("schemaIsCurrent() error = %v", cerr)
		}
		if current {
			t.Fatalf("schemaIsCurrent() = true with %s back, so Open will skip the migration and "+
				"the index the schema removes stays", gone)
		}
		again, err := Open(own)
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		defer func() { _ = again.Close() }()
		still, err := has(raw, `SELECT EXISTS (SELECT 1 FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = $1)`, gone)
		if err != nil {
			t.Fatalf("check %s: %v", gone, err)
		}
		if still {
			t.Errorf("Open() left %s in place", gone)
		}
	})

	_ = db.Close()
}

// anAddableColumn returns a table and one column on it that the migration's ALTER statements can
// add, which is what makes it safe to drop and expect back.
func anAddableColumn(t *testing.T) (string, string) {
	t.Helper()
	for table, cols := range sqlutil.ParseSchemaColumns(schema) {
		for _, col := range cols {
			if col.Addable() {
				return table, col.Name
			}
		}
	}
	t.Fatal("the schema declares no column an ALTER can add, so this control cannot cover that " +
		"condition")
	return "", ""
}

// freshDatabase creates a database of this test's own on the server the DSN names and returns a DSN
// for it, dropping it when the test ends. It creates through the DSN's own connection, since CREATE
// DATABASE works from any database, and swaps the name with url.Parse, so a DSN naming any database
// works. Deriving an admin DSN by replacing "/switchtender?" skipped silently whenever the database
// had another name, including under the ratchet that exists to refuse exactly that.
//
// The name carries random bytes as well as the time. Parallel callers named by the clock alone
// collided, and the loser skipped, so a test that proved nothing reported as one that could not run.
// A database that cannot be created fails the test for the same reason: a caller reaches here only
// with a DSN set, so the database it asked for is the test, not an optional extra.
func freshDatabase(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse SWITCHTENDER_TEST_POSTGRES_DSN: %v", err)
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer func() { _ = conn.Close() }()
	name := freshDatabaseName(t)
	if _, err := conn.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("cannot create a database of this test's own, %s: %v", name, err)
	}
	t.Cleanup(func() {
		c, cerr := sql.Open("pgx", dsn)
		if cerr != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u.Path = "/" + name
	return u.String()
}

// freshDatabaseName returns a database name no other caller can be handed: the time and eight random
// bytes, in the lowercase letters, digits, and underscores an unquoted identifier allows.
func freshDatabaseName(t *testing.T) string {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("read random bytes for a database name: %v", err)
	}
	return fmt.Sprintf("st_fresh_%d_%s", time.Now().UnixNano(), hex.EncodeToString(suffix[:]))
}

// skipOrFail skips the test, unless SWITCHTENDER_REQUIRE_FULL_SUITE demands the full suite, in
// which case the condition that would have skipped it fails it: a gate that asked for everything
// must not be satisfied by a test that quietly ran nothing.
func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// rawHandle opens a plain connection to the test's own database, for the DDL that breaks the schema
// and the catalog reads that check it.
func rawHandle(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open a raw handle: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

// has runs a one-column EXISTS query.
func has(raw *sql.DB, q string, args ...any) (bool, error) {
	var out bool
	if err := raw.QueryRow(q, args...).Scan(&out); err != nil {
		return false, err
	}
	return out, nil
}

// hasRunTrigger reports whether the runs table carries the trigger called name.
func hasRunTrigger(raw *sql.DB, name string) (bool, error) {
	return has(raw, `SELECT EXISTS (SELECT 1 FROM pg_trigger
		WHERE tgname = $1 AND tgrelid = to_regclass('runs'))`, name)
}

// TestFreshDatabaseNamesNeverCollide pins the helper's names: many taken at once, as parallel tests
// take them, are all different and all valid unquoted identifiers. Named by the clock alone, two
// callers in the same instant got one name, and the second's CREATE DATABASE failed.
func TestFreshDatabaseNamesNeverCollide(t *testing.T) {
	t.Parallel()
	valid := regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	const callers = 64
	names := make(chan string, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			names <- freshDatabaseName(t)
		}()
	}
	wg.Wait()
	close(names)
	seen := map[string]bool{}
	for name := range names {
		if seen[name] {
			t.Errorf("two callers were handed %s", name)
		}
		seen[name] = true
		if !valid.MatchString(name) {
			t.Errorf("%s is not a valid unquoted Postgres identifier of at most 63 bytes", name)
		}
	}
}
