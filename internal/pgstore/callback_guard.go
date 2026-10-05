package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// callbackLiveIndex names the unique index holding at most one unfinished provisioning callback run
// per template and host, which run.LiveCallback describes. It is the guard against two replicas
// launching one host's callback twice, so it lives in the database rather than in any one process.
const callbackLiveIndex = "idx_runs_callback_live"

// isCallbackConflict reports whether a write failed because it would be a second unfinished
// callback run for one template and host.
func isCallbackConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation &&
		pgErr.ConstraintName == callbackLiveIndex
}

// SpendBudget records one use of key's allowance in the window open at now, opening one when none
// is, and returns how many uses that window holds. The upsert is one statement, so two replicas
// spending at once each count, and opening a window also deletes every closed one, so the table
// holds roughly one row per caller active in the last window.
func (s *store) SpendBudget(ctx context.Context, key string, window time.Duration,
	now time.Time) (int, error) {
	const q = `INSERT INTO budgets (key, window_end, spent) VALUES ($1, $2, 1)
ON CONFLICT(key) DO UPDATE SET
	spent = CASE WHEN budgets.window_end < $3 THEN 1 ELSE budgets.spent + 1 END,
	window_end = CASE WHEN budgets.window_end < $3 THEN excluded.window_end ELSE budgets.window_end END
RETURNING spent`
	at := now.UnixNano()
	var spent int
	if err := s.db.QueryRowContext(ctx, q, key, now.Add(window).UnixNano(), at).Scan(&spent); err != nil {
		return 0, fmt.Errorf("spend budget: %w", err)
	}
	if spent == 1 {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM budgets WHERE window_end < $1", at); err != nil {
			return 0, fmt.Errorf("prune budgets: %w", err)
		}
	}
	return spent, nil
}

// BudgetSpent returns how many uses key's window open at now holds, zero when none is.
func (s *store) BudgetSpent(ctx context.Context, key string, now time.Time) (int, error) {
	var spent int
	err := s.db.QueryRowContext(ctx, "SELECT spent FROM budgets WHERE key=$1 AND window_end >= $2",
		key, now.UnixNano()).Scan(&spent)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read budget: %w", err)
	}
	return spent, nil
}
