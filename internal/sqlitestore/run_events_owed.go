package sqlitestore

import (
	"context"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// heldStored is the SQL list of the stored statuses a held run can have: waiting for a decision,
// a workflow parked at an approval step, and claimed by a decision.
const heldStored = "('pending_approval', '" + run.StoredParked + "', '" + run.StoredDeciding + "')"

// runEventsSchema is the ledger of the starts and holds of top-level runs still owed to their named
// notification targets, part of schema, kept the way runEndsSchema keeps their ends. One trigger
// owes a start in the statement that moves a run into running from any other status. Two owe a
// hold: one in the statement that moves a run into pending_approval or parked from a status that
// was not already held, so a decision that claims a hold and lets it go owes nothing new, and one
// in the statement that inserts a run held, since a held run is usually created held. An upsert
// that updates a row it found fires the update triggers and not the insert one. A run inserted
// running and a child of a split or pipeline owe nothing. The process that records the event
// deletes the row, and the sweep of owed events records whatever is left.
const runEventsSchema = `
-- The starts and holds of top-level runs owed to their named notification targets, by when they
-- became owed.
CREATE TABLE IF NOT EXISTS run_events_owed (
	run_id  TEXT NOT NULL,
	event   TEXT NOT NULL,
	owed_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, event)
);
CREATE INDEX IF NOT EXISTS idx_run_events_owed_at ON run_events_owed(owed_ms, run_id, event);
CREATE TRIGGER IF NOT EXISTS runs_owe_start AFTER UPDATE OF status ON runs FOR EACH ROW
WHEN NEW.parent_id IS NULL AND NEW.status = 'running' AND OLD.status <> 'running'
BEGIN
	INSERT OR IGNORE INTO run_events_owed (run_id, event, owed_ms)
	VALUES (NEW.id, '` + run.OwedStart + `', ` + nowMillis + `);
END;
CREATE TRIGGER IF NOT EXISTS runs_owe_hold AFTER UPDATE OF status ON runs FOR EACH ROW
WHEN NEW.parent_id IS NULL AND NEW.status IN ('pending_approval', '` + run.StoredParked + `')
	AND OLD.status NOT IN ` + heldStored + `
BEGIN
	INSERT OR IGNORE INTO run_events_owed (run_id, event, owed_ms)
	VALUES (NEW.id, '` + run.OwedHold + `', ` + nowMillis + `);
END;
CREATE TRIGGER IF NOT EXISTS runs_owe_hold_new AFTER INSERT ON runs FOR EACH ROW
WHEN NEW.parent_id IS NULL AND NEW.status IN ('pending_approval', '` + run.StoredParked + `')
BEGIN
	INSERT OR IGNORE INTO run_events_owed (run_id, event, owed_ms)
	VALUES (NEW.id, '` + run.OwedHold + `', ` + nowMillis + `);
END;
`

// OwedEvents returns the starts and holds owed for at least grace, oldest first.
func (s *store) OwedEvents(ctx context.Context, grace time.Duration, limit int) ([]run.OwedEvent, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, event FROM run_events_owed
WHERE owed_ms <= `+nowMillis+` - ? ORDER BY owed_ms, run_id, event LIMIT ?`,
		grace.Milliseconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("list owed run events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []run.OwedEvent{}
	for rows.Next() {
		var ev run.OwedEvent
		if err := rows.Scan(&ev.RunID, &ev.Event); err != nil {
			return nil, fmt.Errorf("list owed run events: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list owed run events: %w", err)
	}
	return out, nil
}

// SettleEvent marks a run's start or hold announced.
func (s *store) SettleEvent(ctx context.Context, id, event string) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM run_events_owed WHERE run_id=? AND event=?", id, event); err != nil {
		return fmt.Errorf("settle run event: %w", err)
	}
	return nil
}
