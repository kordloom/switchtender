package sqlitestore

import (
	"context"
	"fmt"
	"time"
)

// purgeBatch is how many child rows one delete statement removes. Retention deletes loop in
// batches so the first sweep on a mature database never locks the single writer for minutes on
// one monolithic statement, which would time out every worker heartbeat in flight.
const purgeBatch = 2000

// PurgeEventsBefore drops the events and logs of terminal runs created before cutoff, keeping the
// run records and their summaries. It returns how many runs were trimmed.
func (s *store) PurgeEventsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	cut := formatTime(cutoff)
	if err := s.deleteBatched(ctx, "run_events", cut); err != nil {
		return 0, fmt.Errorf("purge events: %w", err)
	}
	if err := s.deleteBatched(ctx, "run_logs", cut); err != nil {
		return 0, fmt.Errorf("purge logs: %w", err)
	}
	var trimmed int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM runs WHERE status NOT IN ('pending','running') AND created_at < ?`, cut).
		Scan(&trimmed)
	if err != nil {
		return 0, fmt.Errorf("purge events: %w", err)
	}
	return trimmed, nil
}

// PurgeRunsBefore deletes terminal runs created before cutoff along with their events and logs,
// keeping the per host and per task summaries. It returns how many runs were deleted.
func (s *store) PurgeRunsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	cut := formatTime(cutoff)
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
	SELECT id FROM runs WHERE status NOT IN ('pending','running') AND created_at < ? LIMIT ?
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
// batches, so no single statement holds the writer for long. table is a fixed internal name, not
// caller input.
func (s *store) deleteBatched(ctx context.Context, table, cut string) error {
	q := fmt.Sprintf(`
DELETE FROM %s WHERE seq IN (
	SELECT seq FROM %s WHERE run_id IN (
		SELECT id FROM runs WHERE status NOT IN ('pending','running') AND created_at < ?
	) LIMIT ?
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
