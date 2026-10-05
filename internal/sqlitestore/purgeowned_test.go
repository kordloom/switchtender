package sqlitestore

import (
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// purgedBeforeRun are the tables every release's purge deletes from before it deletes the run, so
// no release leaves their rows behind. They are large, and sweeping them for rows of runs that no
// longer exist would scan them whole on every pass for rows that cannot be there.
var purgedBeforeRun = []string{"run_events", "run_logs"}

// outliveRun are the tables whose rows outlive their run on purpose: the summaries and facts that
// power the views across runs, stream tickets that expire on their own clock, the retained
// readability decision, the runs table itself, and the records of minted secrets, which the sweep
// deletes only once it has revoked the secret, a run that is gone being one reason it does.
var outliveRun = []string{
	"runs", "run_auth", "run_host_summary", "run_task_summary", "host_facts", "host_facts_history",
	"host_fact_cache", "stream_tickets", "secret_leases",
}

// TestEveryTableHoldingARunIdIsPurgedOrKeptOnPurpose guards the class of defect behind rows that
// outlive the run retention deleted.
//
// A table holding a run's rows either goes with its run, in which case it belongs in
// runOwnedTables and is swept for the rows of runs that no longer exist, or it outlives its run on
// purpose. A new table nobody classified is the likely way a run's reason text or redacted snapshot
// outlives it forever, since whoever adds one is thinking about the feature it serves rather than
// about retention, so the schema is read rather than trusted.
func TestEveryTableHoldingARunIdIsPurgedOrKeptOnPurpose(t *testing.T) {
	t.Parallel()
	classified := map[string]bool{}
	for _, owned := range runOwnedTables {
		classified[owned.table] = true
	}
	for _, table := range append(append([]string{}, purgedBeforeRun...), outliveRun...) {
		classified[table] = true
	}
	tables := sqlutil.ParseSchemaColumns(schema)
	var unclassified []string
	for table, cols := range tables {
		for _, col := range cols {
			if col.Name == "run_id" && !classified[table] {
				unclassified = append(unclassified, table)
			}
		}
	}
	sort.Strings(unclassified)
	if diff := cmp.Diff([]string{}, unclassified, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("tables holding a run id that are neither purged with their run nor kept on "+
			"purpose (-want +got):\n%s\nAdd each to runOwnedTables, or to the lists here with the "+
			"reason it outlives its run.", strings.TrimSpace(diff))
	}
	for _, owned := range runOwnedTables {
		if _, ok := tables[owned.table]; !ok {
			t.Errorf("runOwnedTables names %q, which is not in the schema", owned.table)
		}
	}
}
