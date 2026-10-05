package pgstore

import (
	"os"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// TestEveryTableHoldingARunIdIsCheckedBeforeDroppingItsAuthorization guards the one way the
// readability cleanup can do damage on Postgres.
//
// A retained readability decision may only be dropped once nothing is left that it governs. The
// cleanup decides that by checking runAuthReferents, and a table missing from it is not an error: its
// rows lose the decision that makes them readable and go quietly invisible to every grant-restricted
// caller. A new table holding a run id is the likely way that happens, so the schema is read rather
// than trusted.
func TestEveryTableHoldingARunIdIsCheckedBeforeDroppingItsAuthorization(t *testing.T) {
	t.Parallel()
	checked := map[string]bool{}
	for _, table := range runAuthReferents {
		checked[table] = true
	}
	tables := sqlutil.ParseSchemaColumns(schema)
	var missing []string
	found := 0
	for table, cols := range tables {
		if table == "run_auth" {
			continue
		}
		holdsRunID := false
		for _, col := range cols {
			// The runs table holds its own id rather than a run_id, and is checked under that name.
			if col.Name == "run_id" || (table == "runs" && col.Name == "id") {
				holdsRunID = true
			}
		}
		if !holdsRunID {
			continue
		}
		found++
		if !checked[table] {
			missing = append(missing, table)
		}
	}
	if found == 0 {
		t.Fatal("no tables holding a run id were found, so this guard is asserting nothing. The " +
			"schema's shape changed and this has to change with it")
	}
	if len(missing) > 0 {
		t.Errorf("%d table(s) hold a run id and are not checked before a readability decision is "+
			"dropped: %s\nRows in them would become invisible to every grant-restricted caller, "+
			"silently. Add them to runAuthReferents.", len(missing), strings.Join(missing, ", "))
	}
	for _, table := range runAuthReferents {
		if _, ok := tables[table]; !ok {
			t.Errorf("runAuthReferents names %q, which is not in the schema", table)
		}
	}
	src, err := os.ReadFile("runauth_purge.go")
	if err != nil {
		t.Fatalf("read the cleanup's source: %v", err)
	}
	if !strings.Contains(string(src), "range runAuthReferents") {
		t.Error("the cleanup no longer builds its query from runAuthReferents, so this guard " +
			"checks a list nothing reads")
	}
}
