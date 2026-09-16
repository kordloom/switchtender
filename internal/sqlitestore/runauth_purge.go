package sqlitestore

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
)

// runAuthReferents are every table that can hold a run id and outlive the run itself.
//
// A retained readability decision may only be dropped once nothing is left that it governs. Miss a
// table here and its rows lose the decision that makes them readable, which does not fail, error,
// or log: it makes those rows quietly invisible to every grant-restricted caller, which is the
// precise failure retaining the decision exists to prevent.
//
// run_events and run_logs are listed even though a run purge takes them with it. They cost nothing
// to check, and the list is safer as "everything holding a run id" than as "everything I reasoned
// outlives a run".
var runAuthReferents = []string{
	"runs", "run_host_summary", "run_task_summary", "host_facts", "host_facts_history",
	"run_events", "run_logs",
}

//go:embed runauth_purge.go
var purgeSourceForTest string

// PurgeRunAuth drops retained readability decisions that no longer govern anything, returning how
// many were removed.
//
// The decisions are small, a handful of ids each, but one is written for every run retention
// deletes and a busy fleet deletes runs forever. This is what stops that being a table that only
// grows.
func (s *store) PurgeRunAuth(ctx context.Context) (int, error) {
	var conds []string
	for _, table := range runAuthReferents {
		column := "run_id"
		if table == "runs" {
			column = "id"
		}
		conds = append(conds, fmt.Sprintf("run_id NOT IN (SELECT %s FROM %s)", column, table))
	}
	q := fmt.Sprintf(`
DELETE FROM run_auth WHERE run_id IN (
	SELECT run_id FROM run_auth WHERE %s LIMIT ?
)`, strings.Join(conds, " AND "))

	deleted := 0
	for {
		res, err := s.db.ExecContext(ctx, q, purgeBatch)
		if err != nil {
			return deleted, fmt.Errorf("purge run authorization: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, fmt.Errorf("purge run authorization: %w", err)
		}
		deleted += int(n)
		if n < int64(purgeBatch) {
			return deleted, nil
		}
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
	}
}
