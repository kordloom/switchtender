package sqlitestore

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestKeyConflictMatchesOnlyDuplicateKeys pins that a keyed insert reports a duplicate key only for
// the constraint classes that actually mean one.
//
// Matching any constraint class turned a NOT NULL, CHECK, or foreign-key violation into "another run
// already holds this key". That is a wrong answer shaped like a normal race: the caller is handed
// somebody else's run instead of an error naming the real fault, and the Postgres backend, which
// matches 23505 specifically, answers differently on the same input.
//
// The errors are produced by tripping real constraints rather than synthesized, because the point is
// what the driver actually returns.
func TestKeyConflictMatchesOnlyDuplicateKeys(t *testing.T) {
	t.Parallel()
	// One connection: SQLite admits a single writer, and PRAGMA foreign_keys is per connection, so
	// a pool would both serialize badly and lose the pragma.
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "conflict.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON;
CREATE TABLE parent (id TEXT PRIMARY KEY);
CREATE TABLE t (
  id TEXT PRIMARY KEY,
  uniq TEXT UNIQUE,
  needed TEXT NOT NULL,
  small INTEGER CHECK (small < 10),
  parent_id TEXT REFERENCES parent(id)
);
INSERT INTO t (id, uniq, needed, small) VALUES ('a', 'taken', 'x', 1);`); err != nil {
		t.Fatalf("schema error = %v", err)
	}

	tests := []struct {
		Name string
		SQL  string
		Want bool
	}{
		{"unique", `INSERT INTO t (id, uniq, needed) VALUES ('b', 'taken', 'x')`, true},
		{"primary key", `INSERT INTO t (id, uniq, needed) VALUES ('a', 'other', 'x')`, true},
		{"not null", `INSERT INTO t (id, uniq) VALUES ('c', 'c')`, false},
		{"check", `INSERT INTO t (id, uniq, needed, small) VALUES ('d', 'd', 'x', 99)`, false},
		{"foreign key", `INSERT INTO t (id, uniq, needed, parent_id) VALUES ('e', 'e', 'x', 'nope')`, false},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			_, err := db.Exec(test.SQL)
			if err == nil {
				t.Fatalf("%s did not trip its constraint", test.Name)
			}
			if got := isKeyConflict(err); got != test.Want {
				t.Errorf("isKeyConflict(%s violation) = %v, want %v; error was %v",
					test.Name, got, test.Want, err)
			}
		})
	}
	if isKeyConflict(errors.New("some other failure")) {
		t.Error("a non-sqlite error was reported as a duplicate key")
	}
}
