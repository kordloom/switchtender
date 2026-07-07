package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dcadolph/yardmaster/internal/schedule"
)

// scheduleColumns is the shared select list for schedule reads.
const scheduleColumns = `id, name, cron, playbook, inventory, shards, steps, enabled,
	created_at, next_run_at, last_run_at, last_run_id, template_id`

// scheduleStore is a schedule.Store backed by the shared SQLite database.
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
	 next_run_at, last_run_at, last_run_id, template_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, cron=excluded.cron, playbook=excluded.playbook,
	inventory=excluded.inventory, shards=excluded.shards, steps=excluded.steps,
	enabled=excluded.enabled, created_at=excluded.created_at, next_run_at=excluded.next_run_at,
	last_run_at=excluded.last_run_at, last_run_id=excluded.last_run_id,
	template_id=excluded.template_id`
	_, err = s.db.ExecContext(ctx, q,
		sc.ID, sc.Name, sc.Cron, sc.Playbook, sc.Inventory, sc.Shards, string(steps),
		boolInt(sc.Enabled), formatTime(sc.CreatedAt), nullTime(sc.NextRunAt), nullTime(sc.LastRunAt),
		sc.LastRunID, sc.TemplateID,
	)
	if err != nil {
		return fmt.Errorf("save schedule: %w", err)
	}
	return nil
}

// Get returns the schedule with the given id, or schedule.ErrNotFound.
func (s *scheduleStore) Get(ctx context.Context, id string) (*schedule.Schedule, error) {
	const q = "SELECT " + scheduleColumns + " FROM schedules WHERE id=?"
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

	var out []*schedule.Schedule
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
	res, err := s.db.ExecContext(ctx, "DELETE FROM schedules WHERE id=?", id)
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
		&out.TemplateID); err != nil {
		return nil, err
	}
	out.Enabled = enabled != 0
	if steps != "" {
		if err := json.Unmarshal([]byte(steps), &out.Steps); err != nil {
			return nil, err
		}
	}
	t, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	out.CreatedAt = t
	if out.NextRunAt, err = parseNullTime(nextRun); err != nil {
		return nil, err
	}
	if out.LastRunAt, err = parseNullTime(lastRun); err != nil {
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

// ClaimDue atomically advances a schedule's next fire time and reports whether this caller won.
func (s *scheduleStore) ClaimDue(ctx context.Context, id string, oldNext, newNext time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE schedules SET next_run_at=? WHERE id=? AND next_run_at=?",
		formatTime(newNext), id, formatTime(oldNext))
	if err != nil {
		return false, fmt.Errorf("claim due schedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim due schedule: %w", err)
	}
	return n > 0, nil
}
