package sqlitestore

import (
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// TestEverySelectedColumnIsDeclared holds each shared select-list constant against the CREATE of the
// table it reads. A column in a select list but not in the CREATE is exactly the org_id defect: fresh
// databases carry it only if some other statement adds it, upgraded databases may not carry it at
// all, and the first read after an upgrade fails. This test would have failed the suite the day
// runs.org_id entered runColumns, and it caught credentials.settings living only in a hand ALTER.
func TestEverySelectedColumnIsDeclared(t *testing.T) {
	t.Parallel()
	parsed := sqlutil.ParseSchemaColumns(schema)
	declared := func(table string) map[string]bool {
		out := map[string]bool{}
		for _, c := range parsed[table] {
			out[c.Name] = true
		}
		return out
	}
	tests := []struct {
		Table   string
		Columns string
	}{
		{"runs", runColumns},
		{"run_host_summary", hostSummaryColumns},
		{"credentials", credentialColumns},
		{"grants", grantColumns},
		{"credential_types", credTypeColumns},
		{"inventories", inventoryColumns},
		{"inventory_sources", invSourceColumns},
		{"projects", projectColumns},
		{"policies", policyColumns},
		{"schedules", scheduleColumns},
		{"templates", templateColumns},
	}
	for _, tc := range tests {
		have := declared(tc.Table)
		if len(have) == 0 {
			t.Errorf("the schema parse found no columns for %s, so nothing about it can be checked "+
				"or healed", tc.Table)
			continue
		}
		for _, raw := range strings.Split(tc.Columns, ",") {
			col := strings.TrimSpace(raw)
			if col == "" || have[col] {
				continue
			}
			t.Errorf("%s selects %q, which its CREATE does not declare: a fresh database gets it "+
				"only by accident and an upgraded one may never, which is the org_id defect again",
				tc.Table, col)
		}
	}
}
