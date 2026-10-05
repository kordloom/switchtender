package sqlitestore

import (
	"context"
	"fmt"
	"time"
)

// nowMillis is this database's clock in Unix milliseconds, the clock an owed end is stamped and
// aged with. SQLite reads the clock of the process running the statement, which is the store clock.
const nowMillis = "CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)"

// runEndsSchema is the ledger of the ends of top-level runs still owed to their named notification
// targets, part of schema. The trigger writes a row in the same statement that moves a run to a
// terminal status, whichever path moves it, the executor's finalize, a relay worker's report, the
// lease sweep, a cancel, or a decision, so the end of a run whose process stopped before announcing
// it is never lost. The process that records the end for the run's targets deletes the row, and the
// sweep of owed ends records whatever is left. A run inserted already finished, as a restore or an
// import writes one, and a child of a split or pipeline, which its parent's end speaks for, owe
// nothing.
const runEndsSchema = `
-- The ends of top-level runs owed to their named notification targets, by when they became owed.
CREATE TABLE IF NOT EXISTS run_ends_owed (
	run_id  TEXT PRIMARY KEY,
	owed_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_run_ends_owed_at ON run_ends_owed(owed_ms, run_id);
CREATE TRIGGER IF NOT EXISTS runs_owe_end AFTER UPDATE OF status ON runs FOR EACH ROW
WHEN NEW.parent_id IS NULL AND NEW.` + terminalRun + ` AND NOT OLD.` + terminalRun + `
BEGIN
	INSERT OR IGNORE INTO run_ends_owed (run_id, owed_ms) VALUES (NEW.id, ` + nowMillis + `);
END;
`

// OwedEnds returns the runs whose end has been owed for at least grace, oldest first.
func (s *store) OwedEnds(ctx context.Context, grace time.Duration, limit int) ([]string, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id FROM run_ends_owed
WHERE owed_ms <= `+nowMillis+` - ? ORDER BY owed_ms, run_id LIMIT ?`, grace.Milliseconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("list owed run ends: %w", err)
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("list owed run ends: %w", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// SettleEnd marks a run's end announced.
func (s *store) SettleEnd(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM run_ends_owed WHERE run_id=?", id); err != nil {
		return fmt.Errorf("settle run end: %w", err)
	}
	return nil
}
