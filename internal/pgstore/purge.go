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

// owedOutcomeHold is the SQL condition on a row of runs that keeps it from retention: the run owes
// its outcome to the audit chain, or its parent owes one. The janitor builds an outcome from the
// run's record, its log, and its host and task summaries, and a parent's outcome from each child's
// record and log as well, so a purge of any of them before the outcome is committed leaves the
// janitor to commit a record of something other than what ran.
const owedOutcomeHold = `(runs.outcome_owed_ms > 0 OR EXISTS (SELECT 1 FROM runs AS owner
	WHERE owner.id = runs.parent_id AND owner.outcome_owed_ms > 0))`

// purgeableRun is the SQL predicate for a run retention may remove once it is older than the
// cutoff: a finished run that no owed outcome is built from.
const purgeableRun = terminalRun + " AND NOT " + owedOutcomeHold

// PurgeEventsBefore drops the events and logs of terminal runs created before cutoff, keeping the
// run records and their summaries. It returns how many runs were trimmed, counting only runs that
// actually held events or logs to remove. The count is taken before the deletes, since afterward
// the rows are gone, and each EXISTS rides the run_id index so it stays a single cheap query. A run
// an owed outcome is built from keeps them. See owedOutcomeHold.
func (s *store) PurgeEventsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	cut := sqlutil.FormatTime(cutoff)
	var trimmed int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM runs WHERE `+purgeableRun+` AND created_at < $1
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
// keeping the per host and per task summaries. It returns how many runs were deleted. A run an
// owed outcome is built from stays, with everything it holds. See owedOutcomeHold.
func (s *store) PurgeRunsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	cut := sqlutil.FormatTime(cutoff)
	if err := s.deleteBatched(ctx, "run_events", cut); err != nil {
		return 0, fmt.Errorf("purge run events: %w", err)
	}
	if err := s.deleteBatched(ctx, "run_logs", cut); err != nil {
		return 0, fmt.Errorf("purge run logs: %w", err)
	}
	if err := s.purgeNotifications(ctx, cut); err != nil {
		return 0, err
	}
	// An approver's reason is audit data held as long as its run is, so it goes with the run. The
	// commitment to it stays on the chain, which retention never touches, and with the random value
	// gone it can never be opened again, the same as a redaction.
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM run_decisions WHERE run_id IN (
	SELECT id FROM runs WHERE `+purgeableRun+` AND created_at < $1
)`, cut); err != nil {
		return 0, fmt.Errorf("purge decision records: %w", err)
	}
	// The readability decision is kept before the rows go, because the derived rows this run
	// governs outlive it. Without this a purge silently made every summary, drift row, and state
	// reading it governs unreadable to a grant-restricted caller: not refused, not explained, just
	// absent. ON CONFLICT DO NOTHING, so a re-run of a partially completed purge is harmless.
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO run_auth (run_id, org_id, project_id, inventory_id, pull_credential_id, credential_ids)
SELECT id, org_id, project_id, inventory_id, pull_credential_id, credential_ids
FROM runs WHERE `+purgeableRun+` AND created_at < $1
ON CONFLICT (run_id) DO NOTHING`, cut); err != nil {
		return 0, fmt.Errorf("retain run authorization: %w", err)
	}
	if err := s.purgeReviewReports(ctx, cut); err != nil {
		return 0, err
	}
	deleted := 0
	for {
		res, err := s.db.ExecContext(ctx, `
DELETE FROM runs WHERE id IN (
	SELECT id FROM runs WHERE `+purgeableRun+` AND created_at < $1 LIMIT $2
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
			break
		}
	}
	// The rows of a run already gone go now as well. See purgeOrphans.
	if err := s.purgeOrphans(ctx, cutoff); err != nil {
		return deleted, err
	}
	return deleted, nil
}

// runOwned names a table whose rows go with their run, and the condition that dates a row older
// than the purge's cutoff.
type runOwned struct {
	// table is the table, a fixed internal name and never caller input.
	table string
	// older is the condition a row must meet to be older than the cutoff, which it takes as its one
	// argument.
	older string
	// millis reports whether the cutoff is passed as Unix milliseconds rather than as stored time
	// text.
	millis bool
}

// runOwnedTables are the tables whose rows retention removes with their run, other than the events
// and logs, which every release deletes before the run itself. Each one is also swept for the rows
// of runs that no longer exist. See purgeOrphans.
var runOwnedTables = []runOwned{
	{table: "run_decisions", older: "recorded_at < $1"},
	{table: "notification_events", older: "created_ms < $1", millis: true},
	{table: "notification_deliveries", older: "created_ms < $1", millis: true},
	// An end still owed for a run retention removed has nothing left to announce.
	{table: "run_ends_owed", older: "owed_ms < $1", millis: true},
	// So does a start or hold still owed.
	{table: "run_events_owed", older: "owed_ms < $1", millis: true},
	// A refusal's record names no run and is bounded by its own rule in purgeReviewReports.
	{table: "review_reports", older: "run_id <> '' AND created_at < $1"},
}

// purgeOrphans removes, from every table in runOwnedTables, the rows of runs that no longer exist,
// keeping any row written after cutoff.
//
// A purge removes a table's rows by joining to the runs it is about to delete, which reaches only
// rows whose run is still there. A release that predates a table does not know to delete from it,
// and it serves the same database during a rolling update or after a rollback, so its purge deletes
// the run and leaves the table's rows behind. No later join can reach them: the reason text an
// approver gave, which goes with its run, and the redacted run snapshot in a notification stayed
// forever, and their run id kept the run's retained readability decision alive with them. The
// cutoff keeps the sweep away from a row written before its run is, a window a fresh row can be
// in and a row older than the retention window cannot.
func (s *store) purgeOrphans(ctx context.Context, cutoff time.Time) error {
	for _, owned := range runOwnedTables {
		arg := any(sqlutil.FormatTime(cutoff))
		if owned.millis {
			arg = cutoff.UnixMilli()
		}
		q := "DELETE FROM " + owned.table + " WHERE " + owned.older +
			" AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.id = " + owned.table + ".run_id)"
		if _, err := s.db.ExecContext(ctx, q, arg); err != nil {
			return fmt.Errorf("purge %s of runs that no longer exist: %w", owned.table, err)
		}
	}
	return nil
}

// purgeReviewReports deletes the pull request review report records of the runs a purge is about
// to delete, and the finished records of refusals, which have no run, created before cut, so the
// table is bounded by the same retention as the history it describes. A record still owed a report
// is kept.
func (s *store) purgeReviewReports(ctx context.Context, cut string) error {
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM review_reports WHERE run_id IN (
	SELECT id FROM runs WHERE `+purgeableRun+` AND created_at < $1
) OR (run_id = '' AND done = 1 AND created_at < $1)`, cut); err != nil {
		return fmt.Errorf("purge review reports: %w", err)
	}
	return nil
}

// deleteBatched removes the child rows of purgeable runs older than cut from table in bounded
// batches, so no single statement holds a table lock for long. table is a fixed internal name,
// not caller input.
func (s *store) deleteBatched(ctx context.Context, table, cut string) error {
	q := fmt.Sprintf(`
DELETE FROM %s WHERE seq IN (
	SELECT seq FROM %s WHERE run_id IN (
		SELECT id FROM runs WHERE `+purgeableRun+` AND created_at < $1
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

// TrimSummaries keeps the newest keep summaries for each host and each task and deletes the rest,
// except the summaries of a run an owed outcome is built from. See owedOutcomeHold.
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
	ORDER BY `+sqlutil.TimeOrder+` DESC, run_id COLLATE "C" DESC LIMIT $2
) AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.id = %s.run_id AND `+owedOutcomeHold+`)`,
		trim.table, trim.column, trim.column, trim.column, trim.table, trim.column, trim.table)
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
