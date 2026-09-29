package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// purgeBatch is how many rows one delete statement removes. Retention deletes loop in batches so
// the first sweep on a mature database does not lock a table on one long running statement.
const purgeBatch = 5000

// PurgeEventsBefore drops the events and logs of terminal runs created before cutoff, keeping the
// run records and their summaries. It returns how many runs were trimmed, counting only runs that
// actually held events or logs to remove. The count is taken before the deletes, since afterward
// the rows are gone, and each EXISTS rides the run_id index so it stays a single cheap query.
func (s *store) PurgeEventsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	cut := sqlutil.FormatTime(cutoff)
	var trimmed int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM runs WHERE `+terminalRun+` AND created_at < $1
	AND (EXISTS (SELECT 1 FROM run_events WHERE run_events.run_id = runs.id)
	     OR EXISTS (SELECT 1 FROM run_logs WHERE run_logs.run_id = runs.id))`, cut).
		Scan(&trimmed)
	if err != nil {
		return 0, fmt.Errorf("purge events: %w", err)
	}
	if err := s.deleteBatched(ctx, "run_events", cut); err != nil {
		return 0, fmt.Errorf("purge events: %w", err)
	}
	if err := s.deleteBatched(ctx, "run_logs", cut); err != nil {
		return 0, fmt.Errorf("purge logs: %w", err)
	}
	return trimmed, nil
}

// PurgeRunsBefore deletes terminal runs created before cutoff along with their events and logs,
// keeping the per host and per task summaries. It returns how many runs were deleted.
func (s *store) PurgeRunsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	cut := sqlutil.FormatTime(cutoff)
	if err := s.deleteBatched(ctx, "run_events", cut); err != nil {
		return 0, fmt.Errorf("purge run events: %w", err)
	}
	if err := s.deleteBatched(ctx, "run_logs", cut); err != nil {
		return 0, fmt.Errorf("purge run logs: %w", err)
	}
	deleted := 0
	for {
		res, err := s.db.ExecContext(ctx, `
DELETE FROM runs WHERE id IN (
	SELECT id FROM runs WHERE `+terminalRun+` AND created_at < $1 LIMIT $2
)`, cut, purgeBatch)
		if err != nil {
			return deleted, fmt.Errorf("purge runs: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, fmt.Errorf("purge runs: %w", err)
		}
		deleted += int(n)
		if int(n) < purgeBatch {
			return deleted, nil
		}
	}
}

// deleteBatched removes the child rows of terminal runs older than cut from table in bounded
// batches, so no single statement holds a table lock for long. table is a fixed internal name,
// not caller input.
func (s *store) deleteBatched(ctx context.Context, table, cut string) error {
	q := fmt.Sprintf(`
DELETE FROM %s WHERE seq IN (
	SELECT seq FROM %s WHERE run_id IN (
		SELECT id FROM runs WHERE `+terminalRun+` AND created_at < $1
	) LIMIT $2
)`, table, table)
	for {
		res, err := s.db.ExecContext(ctx, q, cut, purgeBatch)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if int(n) < purgeBatch {
			return nil
		}
	}
}

// summaryTrim describes one summary table's trim: which table to prune and which column groups it.
type summaryTrim struct {
	// table is the summary table to prune.
	table string
	// column is the column the table's window partitions on, host or task.
	column string
}

// summaryTrims lists the two tables retention bounds, in the order they are pruned.
var summaryTrims = []summaryTrim{
	{table: "run_host_summary", column: "host"},
	{table: "run_task_summary", column: "task"},
}

// TrimSummaries keeps the newest keep summaries for each host and each task and deletes the rest.
//
// The excess is removed one host or task at a time rather than by one window function over the
// whole table. A single ranked delete has to be batched to avoid holding a lock on a large table,
// and every batch would re-rank every row, so clearing a large backlog would scan the table once
// per batch. Grouping first costs one index-only pass and then touches only the keys that are
// actually over the limit, and each delete rides the ordered index for one key.
func (s *store) TrimSummaries(ctx context.Context, keep int) (int, error) {
	if keep < 1 {
		keep = 1
	}
	deleted := 0
	for _, trim := range summaryTrims {
		n, err := s.trimSummaryTable(ctx, trim, keep)
		deleted += n
		if err != nil {
			return deleted, fmt.Errorf("trim %s: %w", trim.table, err)
		}
	}
	return deleted, nil
}

// trimSummaryTable prunes one summary table down to keep rows per group key, returning how many
// rows it deleted. The table and column names are fixed internal identifiers, not caller input.
func (s *store) trimSummaryTable(ctx context.Context, trim summaryTrim, keep int) (int, error) {
	over := fmt.Sprintf(
		"SELECT %s FROM %s GROUP BY %s HAVING COUNT(*) > $1", trim.column, trim.table, trim.column)
	rows, err := s.db.QueryContext(ctx, over, keep)
	if err != nil {
		return 0, err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return 0, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	del := fmt.Sprintf(`
DELETE FROM %s WHERE %s = $1 AND (run_id, %s) NOT IN (
	SELECT run_id, %s FROM %s WHERE %s = $1
	ORDER BY `+sqlutil.TimeOrder+` DESC, run_id DESC LIMIT $2
)`, trim.table, trim.column, trim.column, trim.column, trim.table, trim.column)
	deleted := 0
	for _, key := range keys {
		res, err := s.db.ExecContext(ctx, del, key, keep)
		if err != nil {
			return deleted, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += int(n)
	}
	return deleted, nil
}
