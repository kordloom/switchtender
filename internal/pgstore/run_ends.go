package pgstore

import (
	"context"
	"fmt"
	"time"
)

// nowMillis is this database's clock in Unix milliseconds, the clock an owed end is stamped and
// aged with, the same clock leases are stamped with.
const nowMillis = "(extract(epoch FROM now()) * 1000)::bigint"

// runEndsSchema is the ledger of the ends of top-level runs still owed to their named notification
// targets, part of schema and so created by the same migration that creates the table. The trigger
// writes a row in the same statement that moves a run to a terminal status, whichever path moves
// it, the executor's finalize, a relay worker's report, the lease sweep, a cancel, or a decision,
// so the end of a run whose process stopped before announcing it is never lost. The process that
// records the end for the run's targets deletes the row, and the sweep of owed ends records
// whatever is left. A run inserted already finished, as a restore or an import writes one, and a
// child of a split or pipeline, which its parent's end speaks for, owe nothing.
const runEndsSchema = `
-- The ends of top-level runs owed to their named notification targets, by when they became owed.
CREATE TABLE IF NOT EXISTS run_ends_owed (
	run_id  TEXT PRIMARY KEY,
	owed_ms BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_run_ends_owed_at ON run_ends_owed(owed_ms, run_id);
CREATE OR REPLACE FUNCTION switchtender_owe_run_end() RETURNS trigger LANGUAGE plpgsql AS $owe$
BEGIN
	INSERT INTO run_ends_owed (run_id, owed_ms) VALUES (NEW.id, ` + nowMillis + `)
	ON CONFLICT (run_id) DO NOTHING;
	RETURN NULL;
END
$owe$;
DROP TRIGGER IF EXISTS runs_owe_end ON runs;
CREATE TRIGGER runs_owe_end AFTER UPDATE OF status ON runs FOR EACH ROW
	WHEN (NEW.parent_id IS NULL AND NEW.` + terminalRun + ` AND NOT OLD.` + terminalRun + `)
	EXECUTE FUNCTION switchtender_owe_run_end();
`

// OwedEnds returns the runs whose end has been owed for at least grace, oldest first.
func (s *store) OwedEnds(ctx context.Context, grace time.Duration, limit int) ([]string, error) {
	q := `SELECT run_id FROM run_ends_owed WHERE owed_ms <= ` + nowMillis + ` - $1
ORDER BY owed_ms, run_id`
	args := []any{grace.Milliseconds()}
	if limit > 0 {
		q += " LIMIT $2"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
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
	if _, err := s.db.ExecContext(ctx, "DELETE FROM run_ends_owed WHERE run_id=$1", id); err != nil {
		return fmt.Errorf("settle run end: %w", err)
	}
	return nil
}
