package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"

	"github.com/kordloom/switchtender/internal/run"
)

// callbackLiveIndex holds at most one unfinished provisioning callback run per template and host,
// which run.LiveCallback describes. It is the guard against two replicas launching one host's
// callback twice, so it lives in the database rather than in any one process.
const callbackLiveIndex = `CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_callback_live
	ON runs(source_id, limit_pattern)
	WHERE source = '` + run.SourceCallback + `' AND parent_id IS NULL AND ` + nonTerminalRun

// callbackLiveColumns is how SQLite names the columns of callbackLiveIndex in the message of a
// unique constraint it violates.
const callbackLiveColumns = "runs.source_id, runs.limit_pattern"

// isCallbackConflict reports whether a write failed because it would be a second unfinished
// callback run for one template and host.
func isCallbackConflict(err error) bool {
	var serr *sqlite.Error
	return errors.As(err, &serr) && serr.Code() == sqliteConstraintUnique &&
		strings.Contains(serr.Error(), callbackLiveColumns)
}

// SpendBudget records one use of key's allowance in the window open at now, opening one when none
// is, and returns how many uses that window holds. Opening a window also deletes every closed one,
// so the table holds roughly one row per caller active in the last window.
func (s *store) SpendBudget(ctx context.Context, key string, window time.Duration,
	now time.Time) (int, error) {
	const q = `INSERT INTO budgets (key, window_end, spent) VALUES (?, ?, 1)
ON CONFLICT(key) DO UPDATE SET
	spent = CASE WHEN budgets.window_end < ? THEN 1 ELSE budgets.spent + 1 END,
	window_end = CASE WHEN budgets.window_end < ? THEN excluded.window_end ELSE budgets.window_end END
RETURNING spent`
	at := now.UnixNano()
	var spent int
	if err := s.db.writeQueryRowContext(ctx, q, key, now.Add(window).UnixNano(), at,
		at).Scan(&spent); err != nil {
		return 0, fmt.Errorf("spend budget: %w", err)
	}
	if spent == 1 {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM budgets WHERE window_end < ?", at); err != nil {
			return 0, fmt.Errorf("prune budgets: %w", err)
		}
	}
	return spent, nil
}

// BudgetSpent returns how many uses key's window open at now holds, zero when none is.
func (s *store) BudgetSpent(ctx context.Context, key string, now time.Time) (int, error) {
	var spent int
	err := s.db.writeQueryRowContext(ctx,
		"SELECT spent FROM budgets WHERE key=? AND window_end >= ?", key, now.UnixNano()).Scan(&spent)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read budget: %w", err)
	}
	return spent, nil
}
