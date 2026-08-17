package sqlutil

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestParseSchemaColumns holds the parser to the layout the schemas are written in, including the
// shapes that would silently corrupt a heal if misread: commas inside a composite key, parentheses
// inside a body, constraint lines that are not columns, and comments.
func TestParseSchemaColumns(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id            TEXT PRIMARY KEY,
	org_id        TEXT NOT NULL DEFAULT '',
	-- a comment between columns must not become a column.
	attempt       INTEGER NOT NULL DEFAULT 0,
	claimed_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_runs ON runs(org_id);
CREATE TABLE IF NOT EXISTS org_members (
	org_id  TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL,
	PRIMARY KEY (org_id, user_id)
);
ALTER TABLE runs ADD COLUMN IF NOT EXISTS later TEXT NOT NULL DEFAULT '';
`
	got := ParseSchemaColumns(schema)
	want := map[string][]SchemaColumn{
		"runs": {
			{Name: "id", Clause: "TEXT PRIMARY KEY"},
			{Name: "org_id", Clause: "TEXT NOT NULL DEFAULT ''"},
			{Name: "attempt", Clause: "INTEGER NOT NULL DEFAULT 0"},
			{Name: "claimed_at", Clause: "TEXT"},
		},
		"org_members": {
			{Name: "org_id", Clause: "TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE"},
			{Name: "user_id", Clause: "TEXT NOT NULL"},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseSchemaColumns mismatch (-want +got):\n%s", diff)
	}

	// Addable is what keeps the heal from attempting the impossible: a primary key member and a
	// NOT NULL column with no default cannot be added to an existing table, and both are original-era
	// columns a created table always has.
	tests := []struct {
		Col  SchemaColumn
		Want bool
	}{ // Test 0: An ordinary defaulted column is addable.
		{SchemaColumn{Name: "org_id", Clause: "TEXT NOT NULL DEFAULT ''"}, true},
		// Test 1: A primary key is not.
		{SchemaColumn{Name: "id", Clause: "TEXT PRIMARY KEY"}, false},
		// Test 2: NOT NULL with no default is not.
		{SchemaColumn{Name: "user_id", Clause: "TEXT NOT NULL"}, false},
		// Test 3: A nullable column with no default is.
		{SchemaColumn{Name: "claimed_at", Clause: "TEXT"}, true},
	}
	for i, tc := range tests {
		if got := tc.Col.Addable(); got != tc.Want {
			t.Errorf("test %d: Addable(%s %s) = %v, want %v", i, tc.Col.Name, tc.Col.Clause, got, tc.Want)
		}
	}
}

// TestParseSchemaColumnsHardening pins the ways the scan itself was shown to go wrong: a marker
// phrase inside a comment minting a phantom table, a comment's unbalanced parenthesis corrupting the
// depth count that finds the end of a CREATE body, a constraint keyword matching the front of a real
// column's name, and a non-identifier table key reaching the ALTER splice.
func TestParseSchemaColumnsHardening(t *testing.T) {
	t.Parallel()
	const schema = `
-- This comment says CREATE TABLE IF NOT EXISTS is a no-op on an existing table, and the phrase in
-- prose must not become a parse.
CREATE TABLE IF NOT EXISTS runs (
	id       TEXT PRIMARY KEY,
	checksum TEXT NOT NULL DEFAULT '',
	-- a comment with an unbalanced parenthesis (see the docs
	unique_key TEXT NOT NULL DEFAULT '',
	constraint_ref TEXT NOT NULL DEFAULT '',
	checked_at TEXT,
	UNIQUE (checksum, unique_key)
);
ALTER TABLE runs ADD COLUMN IF NOT EXISTS later TEXT NOT NULL DEFAULT '';
`
	got := ParseSchemaColumns(schema)
	if len(got) != 1 {
		t.Fatalf("parsed tables = %v, want exactly runs: a phantom key here becomes a junk ALTER "+
			"or a silently unhealed table", keysOf(got))
	}
	var names []string
	for _, c := range got["runs"] {
		names = append(names, c.Name)
	}
	want := []string{"id", "checksum", "unique_key", "constraint_ref", "checked_at"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("runs columns (-want +got):\n%s\nA missing name here is a column every upgraded "+
			"database silently never gains.", diff)
	}
}

// keysOf lists a parse result's table names for a failure message.
func keysOf(m map[string][]SchemaColumn) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
