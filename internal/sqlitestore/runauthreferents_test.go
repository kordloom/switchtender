package sqlitestore

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// TestEveryTableHoldingARunIdIsCheckedBeforeDroppingItsAuthorization guards the one way this
// cleanup can do damage.
//
// A retained readability decision may only be dropped once nothing is left that it governs. The
// cleanup decides that by checking a list of tables, and a table missing from that list is not an
// error, a warning, or a crash: its rows lose the decision that makes them readable, and they go
// quietly invisible to every grant-restricted caller. That is the exact failure retaining the
// decision was built to prevent, reintroduced by omission.
//
// A new table holding a run id is the likely way that happens, since whoever adds one is thinking
// about the feature it serves rather than about retention. So the schema is read rather than
// trusted.
func TestEveryTableHoldingARunIdIsCheckedBeforeDroppingItsAuthorization(t *testing.T) {
	t.Parallel()
	checked := map[string]bool{}
	for _, table := range runAuthReferents {
		checked[table] = true
	}

	var missing []string
	var found int
	for table, cols := range sqlutil.ParseSchemaColumns(schema) {
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

	// And the other direction: a listed table that no longer exists would make the anti-join fail
	// at runtime, where it reads as retention being broken rather than as a stale list.
	tables := sqlutil.ParseSchemaColumns(schema)
	for _, table := range runAuthReferents {
		if _, ok := tables[table]; !ok {
			t.Errorf("runAuthReferents names %q, which is not in the schema", table)
		}
	}
	// The guard is only meaningful if the list is actually what builds the query.
	if !regexp.MustCompile(`runAuthReferents`).MatchString(purgeSourceForTest) {
		t.Error("the cleanup no longer builds its query from runAuthReferents, so this guard " +
			"checks a list nothing reads")
	}
}
