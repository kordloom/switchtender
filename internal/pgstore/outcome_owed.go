package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// OwedOutcomes returns the finished top-level runs that owe their outcome to the chain, owed at
// least age by the database's clock, the one the runs_outcome_owed trigger stamps with, longest
// owed first.
func (s *store) OwedOutcomes(ctx context.Context, age time.Duration, limit int) ([]string, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs
WHERE outcome_owed_ms > 0
	AND outcome_owed_ms <= (extract(epoch FROM clock_timestamp()) * 1000)::bigint - $1
ORDER BY outcome_owed_ms, id LIMIT $2`, age.Milliseconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("owed outcomes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("owed outcomes: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("owed outcomes: %w", err)
	}
	return ids, nil
}

// OutcomeOwed reports whether the run owes its outcome to the chain.
func (s *store) OutcomeOwed(ctx context.Context, id string) (bool, error) {
	var owed int64
	err := s.db.QueryRowContext(ctx, "SELECT outcome_owed_ms FROM runs WHERE id = $1", id).Scan(&owed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("outcome owed: %w", err)
	}
	return owed > 0, nil
}

// SettleOutcome records that the run's outcome is on the chain.
func (s *store) SettleOutcome(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE runs SET outcome_owed_ms = 0 WHERE id = $1 AND outcome_owed_ms > 0", id); err != nil {
		return fmt.Errorf("settle outcome: %w", err)
	}
	return nil
}
