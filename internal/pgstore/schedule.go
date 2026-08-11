package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// scheduleColumns is the shared select list for schedule reads.
const scheduleColumns = `id, name, cron, playbook, inventory, shards, steps, enabled,
	created_at, next_run_at, last_run_at, last_run_id, template_id, timezone`

// scheduleStore is a schedule.Store backed by the shared PostgreSQL database.
type scheduleStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the schedule.
func (s *scheduleStore) Save(ctx context.Context, sc *schedule.Schedule) error {
	steps, err := json.Marshal(sc.Steps)
	if err != nil {
		return fmt.Errorf("save schedule: %w", err)
	}
	const q = `
INSERT INTO schedules
	(id, name, cron, playbook, inventory, shards, steps, enabled, created_at,
	 next_run_at, last_run_at, last_run_id, template_id, timezone)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, cron=excluded.cron, playbook=excluded.playbook,
	inventory=excluded.inventory, shards=excluded.shards, steps=excluded.steps,
	enabled=excluded.enabled, created_at=excluded.created_at, next_run_at=excluded.next_run_at,
	last_run_at=excluded.last_run_at, last_run_id=excluded.last_run_id,
	template_id=excluded.template_id, timezone=excluded.timezone`
	_, err = s.db.ExecContext(ctx, q,
		sc.ID, sc.Name, sc.Cron, sc.Playbook, sc.Inventory, sc.Shards, string(steps),
		boolInt(sc.Enabled), sqlutil.FormatTime(sc.CreatedAt), sqlutil.NullTime(sc.NextRunAt), sqlutil.NullTime(sc.LastRunAt),
		sc.LastRunID, sc.TemplateID, sc.Timezone,
	)
	if err != nil {
		return fmt.Errorf("save schedule: %w", err)
	}
	return nil
}

// Get returns the schedule with the given id, or schedule.ErrNotFound.
func (s *scheduleStore) Get(ctx context.Context, id string) (*schedule.Schedule, error) {
	const q = "SELECT " + scheduleColumns + " FROM schedules WHERE id=$1"
	sc, err := scanSchedule(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, schedule.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get schedule: %w", err)
	}
	return sc, nil
}

// List returns all schedules ordered by creation time, oldest first.
func (s *scheduleStore) List(ctx context.Context) ([]*schedule.Schedule, error) {
	const q = "SELECT " + scheduleColumns + " FROM schedules ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*schedule.Schedule{}
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("list schedules: %w", err)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	return out, nil
}

// Delete removes the schedule with the given id, or returns schedule.ErrNotFound.
func (s *scheduleStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM schedules WHERE id=$1", id)
	if err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	if n == 0 {
		return schedule.ErrNotFound
	}
	return nil
}

// scanSchedule reads one schedule row from a scanner.
func scanSchedule(sc scanner) (*schedule.Schedule, error) {
	var (
		out     schedule.Schedule
		steps   string
		enabled int
		created string
		nextRun sql.NullString
		lastRun sql.NullString
	)
	if err := sc.Scan(&out.ID, &out.Name, &out.Cron, &out.Playbook, &out.Inventory, &out.Shards,
		&steps, &enabled, &created, &nextRun, &lastRun, &out.LastRunID,
		&out.TemplateID, &out.Timezone); err != nil {
		return nil, err
	}
	out.Enabled = enabled != 0
	if steps != "" {
		if err := json.Unmarshal([]byte(steps), &out.Steps); err != nil {
			return nil, err
		}
	}
	t, err := sqlutil.ParseTime(created)
	if err != nil {
		return nil, err
	}
	out.CreatedAt = t
	if out.NextRunAt, err = sqlutil.ParseNullTime(nextRun); err != nil {
		return nil, err
	}
	if out.LastRunAt, err = sqlutil.ParseNullTime(lastRun); err != nil {
		return nil, err
	}
	return &out, nil
}

// boolInt maps a bool to a database integer.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Update replaces an existing schedule, or returns ErrNotFound when the row is gone. It exists so
// an edit racing a delete cannot re-create what was deleted, which the upsert in Save would.
func (s *scheduleStore) Update(ctx context.Context, sc *schedule.Schedule) error {
	steps, err := json.Marshal(sc.Steps)
	if err != nil {
		return fmt.Errorf("update schedule: %w", err)
	}
	const q = `
UPDATE schedules SET
	name=$1, cron=$2, playbook=$3, inventory=$4, shards=$5, steps=$6, enabled=$7, created_at=$8,
	next_run_at=$9, last_run_at=$10, last_run_id=$11, template_id=$12, timezone=$13
WHERE id=$14`
	res, err := s.db.ExecContext(ctx, q,
		sc.Name, sc.Cron, sc.Playbook, sc.Inventory, sc.Shards, string(steps),
		boolInt(sc.Enabled), sqlutil.FormatTime(sc.CreatedAt), sqlutil.NullTime(sc.NextRunAt),
		sqlutil.NullTime(sc.LastRunAt), sc.LastRunID, sc.TemplateID, sc.Timezone, sc.ID,
	)
	if err != nil {
		return fmt.Errorf("update schedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update schedule: %w", err)
	}
	if n == 0 {
		return schedule.ErrNotFound
	}
	return nil
}

// RecordFire records that a schedule fired, writing only the two columns a fire owns. An empty run
// id keeps the stored one, and a row that is gone is not an error. The text cast is defensive
// rather than required: pg resolves the parameter from the text sibling operand either way.
func (s *scheduleStore) RecordFire(ctx context.Context, id string, at time.Time, runID string) error {
	const q = `
UPDATE schedules SET
	last_run_at=$1, last_run_id=COALESCE(NULLIF($2::text, ''), last_run_id)
WHERE id=$3`
	if _, err := s.db.ExecContext(ctx, q, sqlutil.FormatTime(at), runID, id); err != nil {
		return fmt.Errorf("record schedule fire: %w", err)
	}
	return nil
}

// ClaimDue atomically advances a schedule's next fire time and reports whether this caller won.
func (s *scheduleStore) ClaimDue(ctx context.Context, id string, oldNext, newNext time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE schedules SET next_run_at=$1 WHERE id=$2 AND next_run_at=$3",
		sqlutil.FormatTime(newNext), id, sqlutil.FormatTime(oldNext))
	if err != nil {
		return false, fmt.Errorf("claim due schedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim due schedule: %w", err)
	}
	return n > 0, nil
}
