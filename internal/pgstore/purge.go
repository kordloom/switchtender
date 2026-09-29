package pgstore

import (
	"context"
	"fmt"
	"time"
)

// PurgeEventsBefore drops the events and logs of terminal runs created before cutoff, keeping the
// run records and their summaries. It returns how many runs were trimmed.
func (s *store) PurgeEventsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	cut := formatTime(cutoff)
	const sel = `run_id IN (
		SELECT id FROM runs WHERE status NOT IN ('pending','running') AND created_at < $1
	)`
	if _, err := s.db.ExecContext(ctx, "DELETE FROM run_events WHERE "+sel, cut); err != nil {
		return 0, fmt.Errorf("purge events: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM run_logs WHERE "+sel, cut); err != nil {
		return 0, fmt.Errorf("purge logs: %w", err)
	}
	var trimmed int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM runs WHERE status NOT IN ('pending','running') AND created_at < $1`, cut).
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
	const sel = `run_id IN (
		SELECT id FROM runs WHERE status NOT IN ('pending','running') AND created_at < $1
	)`
	if _, err := s.db.ExecContext(ctx, "DELETE FROM run_events WHERE "+sel, cut); err != nil {
		return 0, fmt.Errorf("purge run events: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM run_logs WHERE "+sel, cut); err != nil {
		return 0, fmt.Errorf("purge run logs: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
DELETE FROM runs WHERE status NOT IN ('pending','running') AND created_at < $1`, cut)
	if err != nil {
		return 0, fmt.Errorf("purge runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge runs: %w", err)
	}
	return int(n), nil
}
