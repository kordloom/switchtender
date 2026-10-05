package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// ClaimFire claims an occurrence for a fire and marks it in flight in the same statement, unless
// the row marks another occurrence in flight. The mark is stamped with the database clock, the one
// TakeInFlight ages it by, so the servers sharing the database agree on its age.
func (s *scheduleStore) ClaimFire(ctx context.Context, id string, oldNext time.Time,
	next *time.Time) (bool, error) {
	at := sqlutil.FormatTime(oldNext)
	res, err := s.db.ExecContext(ctx, `UPDATE schedules SET next_run_at=$1, inflight_at=$2,
	inflight_ms=`+nowMillis+`
WHERE id=$3 AND next_run_at=$2 AND (inflight_at IS NULL OR inflight_at=$2)`,
		sqlutil.NullTime(next), at, id)
	if err != nil {
		return false, fmt.Errorf("claim schedule fire: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim schedule fire: %w", err)
	}
	return n > 0, nil
}

// TakeInFlight re-marks and returns every occurrence marked in flight for at least grace, in one
// statement. A second sweep running at once waits on the rows this one locks and then finds them
// freshly marked, so it takes none of them.
func (s *scheduleStore) TakeInFlight(ctx context.Context, grace time.Duration) ([]schedule.InFlight,
	error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE schedules SET inflight_ms=`+nowMillis+`
WHERE inflight_at IS NOT NULL AND inflight_ms <= `+nowMillis+` - $1
RETURNING id, inflight_at`, grace.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("take in-flight schedule fires: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []schedule.InFlight{}
	for rows.Next() {
		var o schedule.InFlight
		var at string
		if err := rows.Scan(&o.ScheduleID, &at); err != nil {
			return nil, fmt.Errorf("take in-flight schedule fires: %w", err)
		}
		if o.Occurrence, err = sqlutil.ParseTime(at); err != nil {
			return nil, fmt.Errorf("take in-flight schedule fires: %s: %w", o.ScheduleID, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("take in-flight schedule fires: %w", err)
	}
	schedule.SortInFlight(out)
	return out, nil
}

// SettleInFlight clears the in-flight mark when the row still marks occurrence.
func (s *scheduleStore) SettleInFlight(ctx context.Context, id string, occurrence time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE schedules SET inflight_at=NULL, inflight_ms=0 WHERE id=$1 AND inflight_at=$2",
		id, sqlutil.FormatTime(occurrence)); err != nil {
		return fmt.Errorf("settle in-flight schedule fire: %w", err)
	}
	return nil
}
