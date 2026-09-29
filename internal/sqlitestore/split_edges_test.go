package sqlitestore

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenReadPoolFallsBackForEveryPathThatCannotCarryASecondHandle pins the exact set of paths the
// read pool declines. A second handle onto an in-memory database opens a different database
// entirely, and a path already carrying its own DSN options would have the pool's options appended
// to a query string it does not own. Getting this wrong does not fail loudly: reads would land on a
// database with none of the writes in it, or on a connection whose pragmas were silently dropped.
func TestOpenReadPoolFallsBackForEveryUnsuitablePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tests := []struct {
		Name     string
		Path     string
		WantPool bool
	}{{ // Test 0: A plain file path is the ordinary case and does get its own read pool.
		Name: "plain file", Path: filepath.Join(dir, "plain.db"), WantPool: true,
	}, { // Test 1: A bare in-memory database: a second handle would be a different database.
		Name: "memory", Path: ":memory:", WantPool: false,
	}, { // Test 2: A named in-memory database, same reasoning.
		Name: "named memory", Path: "file:named?mode=memory&cache=shared", WantPool: false,
	}, { // Test 3: A path carrying its own options, which the pool must not append to.
		Name: "path with options", Path: filepath.Join(dir, "opts.db") + "?_pragma=foo(1)",
		WantPool: false,
	}, { // Test 4: A DSN in file: form, which the pool must not wrap in another file: prefix.
		Name: "file url", Path: "file:" + filepath.Join(dir, "url.db"), WantPool: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			pool, err := openReadPool(test.Path)
			if err != nil {
				t.Fatalf("openReadPool(%s) error = %v", test.Name, err)
			}
			if pool != nil {
				defer func() { _ = pool.Close() }()
			}
			if got := pool != nil; got != test.WantPool {
				t.Errorf("openReadPool(%s) opened a pool = %v, want %v", test.Name, got, test.WantPool)
			}
		})
	}
}

// TestReadPoolCarriesItsPragmas pins that the read pool's connections really are query_only, rather
// than the DSN options being silently ignored. The whole safety of splitting reads off the writer
// rests on a misrouted write failing loudly instead of racing the writer into SQLite's
// read-to-write upgrade deadlock, and that guarantee is one unnoticed DSN typo away from being
// nothing at all.
func TestReadPoolCarriesItsPragmas(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "pragma.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()
	if d.db.r == d.db.w {
		t.Fatal("no read pool was opened for a plain file path")
	}

	var readOnly, writable int
	if err := d.db.r.QueryRow("PRAGMA query_only").Scan(&readOnly); err != nil {
		t.Fatalf("read pool PRAGMA query_only: %v", err)
	}
	if readOnly != 1 {
		t.Error("the read pool is not query_only, so a misrouted write races the single writer " +
			"instead of failing")
	}
	if err := d.db.w.QueryRow("PRAGMA query_only").Scan(&writable); err != nil {
		t.Fatalf("write connection PRAGMA query_only: %v", err)
	}
	if writable != 0 {
		t.Error("the write connection is query_only, so nothing can be written at all")
	}
}

// TestEveryWriteOnTheReadPoolIsRefused pins the refusal across statement kinds, not just the one
// INSERT an earlier test tried. A pool that refused inserts but accepted an UPDATE or a DELETE
// would be a hole exactly where it matters, since the store's fenced updates are the statements
// whose correctness depends on running on the single writer.
func TestEveryWriteOnTheReadPoolIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "refuse.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()
	if d.db.r == d.db.w {
		t.Fatal("no read pool was opened for a plain file path")
	}
	if _, err := d.db.w.ExecContext(ctx,
		"INSERT INTO orgs (id, name, created_at) VALUES ('org_1','ops','2026-01-01T00:00:00Z')"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	tests := []struct {
		Name string
		SQL  string
	}{{ // Test 0: An insert.
		Name: "insert",
		SQL:  "INSERT INTO orgs (id, name, created_at) VALUES ('org_2','x','2026-01-01T00:00:00Z')",
	}, { // Test 1: An update, the shape every fenced transition uses.
		Name: "update", SQL: "UPDATE orgs SET name='changed' WHERE id='org_1'",
	}, { // Test 2: A delete.
		Name: "delete", SQL: "DELETE FROM orgs WHERE id='org_1'",
	}, { // Test 3: A schema change.
		Name: "ddl", SQL: "CREATE TABLE sneaky (id TEXT)",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := d.db.r.ExecContext(ctx, test.SQL); err == nil {
				t.Errorf("a %s on the read pool succeeded, want a query_only refusal", test.Name)
			}
		})
	}

	// The seeded row is untouched, which proves none of the above quietly landed.
	var name string
	if err := d.db.w.QueryRowContext(ctx, "SELECT name FROM orgs WHERE id='org_1'").Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "ops" {
		t.Errorf("the seeded row reads %q, so a refused write landed anyway", name)
	}
}

// TestRunSearchClauseBindsOneArgumentPerColumn pins the shape of the generated search predicate.
// The clause and its argument list are built separately and appended to a query that already has
// arguments of its own, so a mismatch between the two counts binds the wrong value to the wrong
// placeholder rather than failing, which is how a search silently starts matching the wrong rows.
func TestRunSearchClauseBindsOneArgumentPerColumn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Query     string
		WantEmpty bool
		WantArg   string
	}{{ // Test 0: A blank term produces no clause and no arguments.
		Name: "empty", Query: "", WantEmpty: true,
	}, { // Test 1: Whitespace only is also blank, so a stray space does not filter everything out.
		Name: "whitespace", Query: "   \t\n ", WantEmpty: true,
	}, { // Test 2: A term is lowercased and surrounded, matching the lower() on each column.
		Name: "mixed case", Query: "  SiTe.YML  ", WantArg: "%site.yml%",
	}, { // Test 3: Unicode survives the fold rather than being mangled.
		Name: "unicode", Query: "Ünïcode", WantArg: "%ünïcode%",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			clause, args := runSearchClause(test.Query)
			if test.WantEmpty {
				if clause != "" || len(args) != 0 {
					t.Fatalf("runSearchClause(%q) = (%q, %v), want no clause and no args",
						test.Query, clause, args)
				}
				return
			}
			if got := strings.Count(clause, "?"); got != len(args) {
				t.Errorf("clause holds %d placeholders but %d args are bound, so every later "+
					"filter binds to the wrong placeholder: %q", got, len(args), clause)
			}
			if len(args) != len(runSearchColumns) {
				t.Errorf("bound %d args for %d searched columns", len(args), len(runSearchColumns))
			}
			for i, a := range args {
				if a != test.WantArg {
					t.Errorf("arg %d = %v, want %q", i, a, test.WantArg)
				}
			}
		})
	}
}
