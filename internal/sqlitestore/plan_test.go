package sqlitestore_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestHotQueryPlans pins the access path SQLite chooses for the reads on the run list and the
// dispatcher sweep.
//
// It asserts on the plan rather than on a duration because the failure it guards against is not a
// slow query, it is a query whose cost is the size of the whole table. Each of these has an index
// that answers both its filter and its ordering, and when one goes missing SQLite still returns the
// right rows, by walking every row and sorting them, so nothing fails and the install simply gets
// slower as it fills up. A temp b-tree in a plan behind a LIMIT is that failure, and naming the
// index the plan must use also stops a future index from quietly taking the query somewhere worse.
func TestHotQueryPlans(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "plans.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	ctx := t.Context()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Enough rows, and enough children among them, that the planner is choosing between real
	// alternatives rather than treating the table as too small to matter.
	for i := range 500 {
		id := fmt.Sprintf("run-%05d", i)
		r := &run.Run{
			ID: id, Playbook: "site.yml", Inventory: "inventory/prod",
			Status:    run.StatusSucceeded,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			Actor:     fmt.Sprintf("user-%d", i%13),
		}
		if err := db.Runs().Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if i%10 != 0 {
			continue
		}
		parent, idx := id, 0
		child := &run.Run{
			ID: id + "-s0", Playbook: "site.yml", Inventory: "inventory/prod",
			Status: run.StatusRunning, CreatedAt: r.CreatedAt, ParentID: &parent, ShardIndex: &idx,
		}
		if err := db.Runs().Save(ctx, child); err != nil {
			t.Fatalf("Save(shard) error = %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	tests := []struct {
		// Name identifies the read the query stands for.
		Name string
		// Query is the shape the store issues, with its bind values inlined.
		Query string
		// WantIndex is the index the plan must name.
		WantIndex string
	}{{ // Test 0: The runs view's page, the read every dashboard load makes.
		Name: "run list page",
		Query: "SELECT id FROM runs WHERE parent_id IS NULL " +
			"ORDER BY created_at DESC, id DESC LIMIT 50",
		WantIndex: "idx_runs_toplevel_created",
	}, { // Test 1: The same page filtered by the status chips beside it.
		Name: "run list page by status",
		Query: "SELECT id FROM runs WHERE parent_id IS NULL AND status = 'succeeded' " +
			"ORDER BY created_at DESC, id DESC LIMIT 50",
		WantIndex: "idx_runs_toplevel_status",
	}, { // Test 2: The oldest-first page, which reads the same index backwards.
		Name: "run list page oldest first",
		Query: "SELECT id FROM runs WHERE parent_id IS NULL " +
			"ORDER BY created_at ASC, id ASC LIMIT 50",
		WantIndex: "idx_runs_toplevel_created",
	}, { // Test 3: The status chip counts, answered from an index without reading a run row. The
		// narrow (status, parent_id) index wins here over the wider top-level one, which is right:
		// both cover the count and the narrow one reads fewer pages to do it.
		Name:      "run status counts",
		Query:     "SELECT status, COUNT(*) FROM runs WHERE parent_id IS NULL GROUP BY status",
		WantIndex: "COVERING INDEX idx_runs_status_parent",
	}, { // Test 4: The dispatcher's sweep over work still in flight.
		Name: "non-terminal sweep",
		Query: "SELECT id FROM runs WHERE status NOT IN " +
			"('succeeded', 'failed', 'canceled', 'interrupted', 'rejected')",
		WantIndex: "idx_runs_live",
	}, { // Test 5: A parent's children, which must still ride the parent index.
		Name:      "shards of a parent",
		Query:     "SELECT id FROM runs WHERE parent_id = 'run-00010'",
		WantIndex: "idx_runs_child_parent",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := queryPlan(ctx, raw, test.Query)
			if err != nil {
				t.Fatalf("%s: query plan error = %v", test.Name, err)
			}
			if !strings.Contains(plan, test.WantIndex) {
				t.Errorf("%s plan = %q, want it to use %s", test.Name, plan, test.WantIndex)
			}
			if strings.Contains(plan, "TEMP B-TREE") {
				t.Errorf("%s plan = %q, want no sort: the index must supply the order", test.Name, plan)
			}
		})
	}
}

// queryPlan returns SQLite's plan for a query as one line, for asserting on the access path.
func queryPlan(ctx context.Context, db *sql.DB, query string) (string, error) {
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		return "", fmt.Errorf("explain query plan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var steps []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			return "", fmt.Errorf("explain query plan: %w", err)
		}
		steps = append(steps, detail)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("explain query plan: %w", err)
	}
	return strings.Join(steps, "; "), nil
}
