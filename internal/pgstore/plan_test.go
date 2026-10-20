package pgstore_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/pgstore"
)

// TestRunKeyPlans pins that the run reads which order or range on the stored time order key are
// served by an index on that key.
//
// PostgreSQL serves an expression only from an index on the same expression and collation, so a
// query that orders by the key with no matching index still returns the right rows, by sorting
// every matching row, and nothing fails while the install gets slower as it fills. Sequential and
// bitmap scans are turned off for the plan so that an empty test table cannot make the sort look
// cheap: with them off, the only plan without a Sort node is one that reads the key index in order.
func TestRunKeyPlans(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	truncateAll(t, dsn)
	t.Cleanup(func() { truncateAll(t, dsn) })
	// Enough history, analyzed, that the planner weighs a sort against a real table. On an empty one
	// it estimates a single row and sorts it, whichever indexes exist.
	if _, err := raw.ExecContext(t.Context(), `
INSERT INTO runs (id, playbook, inventory, status, created_at, actor, source, source_id, queue,
	claimed_by, claimed_at)
SELECT 'run-' || i, 'site.yml', 'inventory/prod', CASE WHEN i % 500 = 0 THEN 'pending' ELSE 'succeeded' END,
	to_char(timestamp '2025-01-01' + i * interval '1 second', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	'user-' || (i % 400), CASE WHEN i % 7 = 0 THEN 'webhook' ELSE 'manual' END, 'src-' || (i % 50),
	'default', CASE WHEN i % 500 = 0 THEN '' ELSE 'worker-' || (i % 5) END,
	CASE WHEN i % 500 = 0 THEN NULL
		ELSE to_char(timestamp '2025-01-01' + i * interval '1 second', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') END
FROM generate_series(1, 20000) AS i`); err != nil {
		t.Fatalf("seed runs: %v", err)
	}
	if _, err := raw.ExecContext(t.Context(), "ANALYZE runs"); err != nil {
		t.Fatalf("analyze runs: %v", err)
	}

	const key = `rtrim(created_at, 'Z') COLLATE "C"`
	tests := []struct {
		// Name identifies the read the query stands for.
		Name string
		// Query is the shape the store issues, with its bind values inlined.
		Query string
		// WantIndex is the index the plan must name.
		WantIndex string
		// Sorts reports that the plan may sort, for a grouped read no index can hand over in order.
		Sorts bool
	}{{ // Test 0: The runs view's page.
		Name: "run list page",
		Query: "SELECT id FROM runs WHERE parent_id IS NULL ORDER BY " + key +
			` DESC, id COLLATE "C" DESC LIMIT 50`,
		WantIndex: "idx_runs_created_key",
	}, { // Test 1: The oldest-first page, which reads the same index backwards.
		Name: "run list page oldest first",
		Query: "SELECT id FROM runs WHERE parent_id IS NULL ORDER BY " + key +
			` ASC, id COLLATE "C" ASC LIMIT 50`,
		WantIndex: "idx_runs_created_key",
	}, { // Test 2: The page between two times, which must range over the key.
		Name: "run list page between two times",
		Query: "SELECT id FROM runs WHERE parent_id IS NULL AND " + key + " >= '2025-01-01T05:00:00' " +
			"AND " + key + " < '2025-01-01T05:10:00' ORDER BY " + key +
			` DESC, id COLLATE "C" DESC LIMIT 50`,
		WantIndex: "idx_runs_created_key",
	}, { // Test 3: The runs view's search by who started a run.
		Name: "run list page by actor",
		Query: "SELECT id FROM runs WHERE parent_id IS NULL AND actor = 'user-1' ORDER BY " + key +
			` DESC, id COLLATE "C" DESC LIMIT 50`,
		WantIndex: "idx_runs_actor_key",
	}, { // Test 4: The runs view's search by what started a run.
		Name: "run list page by source",
		Query: "SELECT id FROM runs WHERE parent_id IS NULL AND source = 'webhook' AND source_id = 'x' " +
			"ORDER BY " + key + ` DESC, id COLLATE "C" DESC LIMIT 50`,
		WantIndex: "idx_runs_source_key",
	}, { // Test 5: The executor's claim, oldest pending run first within its queue.
		Name: "claim",
		Query: "SELECT id FROM runs WHERE status='pending' AND claimed_by='' AND kind='' " +
			"AND cancel_requested=0 AND queue IN ('default') ORDER BY " + key + `, id COLLATE "C" LIMIT 1`,
		WantIndex: "idx_runs_pending_claim_key",
	}, { // Test 6: The workers page, which must range over recent leases only.
		Name: "workers seen recently",
		Query: `SELECT claimed_by, MAX(rtrim(claimed_at, 'Z') COLLATE "C") FROM runs ` +
			`WHERE claimed_by != '' AND claimed_at IS NOT NULL ` +
			`AND rtrim(claimed_at, 'Z') COLLATE "C" >= '2025-01-01T05:00:00' AND parent_id IS NULL ` +
			`GROUP BY claimed_by ORDER BY MAX(rtrim(claimed_at, 'Z') COLLATE "C") DESC, claimed_by COLLATE "C"`,
		WantIndex: "idx_runs_leased_key",
		Sorts:     true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			plan, err := runPlan(t, raw, test.Query)
			if err != nil {
				t.Fatalf("%s: query plan error = %v", test.Name, err)
			}
			if !strings.Contains(plan, test.WantIndex) {
				t.Errorf("%s plan = %q, want it to use %s", test.Name, plan, test.WantIndex)
			}
			if !test.Sorts && strings.Contains(plan, "Sort") {
				t.Errorf("%s plan = %q, want no sort: the index must supply the order", test.Name, plan)
			}
		})
	}
}

// runPlan returns PostgreSQL's plan for a query as one line, with sequential and bitmap scans off.
func runPlan(t *testing.T, db *sql.DB, query string) (string, error) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, set := range []string{"SET LOCAL enable_seqscan = off", "SET LOCAL enable_bitmapscan = off"} {
		if _, err := tx.Exec(set); err != nil {
			return "", fmt.Errorf("%s: %w", set, err)
		}
	}
	rows, err := tx.Query("EXPLAIN " + query)
	if err != nil {
		return "", fmt.Errorf("explain: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var steps []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("explain: %w", err)
		}
		steps = append(steps, strings.TrimSpace(line))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("explain: %w", err)
	}
	return strings.Join(steps, "; "), nil
}
