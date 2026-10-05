package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/attention"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// The run store records queue times and the attention store is a full attention.Store.
var (
	_ run.QueueTimes  = (*store)(nil)
	_ attention.Store = (*attentionStore)(nil)
)

// attentionStore is an attention.Store backed by the shared SQLite database. A SQLite deployment is
// one process, so the process clock is the store clock it stamps reports with.
type attentionStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
}

// Attention returns the store that records which workers are polling for work and which attention
// alerts were raised.
func (d *DB) Attention() attention.Store {
	return &attentionStore{db: d.db}
}

// encodeQueues writes a worker's queues as a JSON array, so the default queue's empty name
// survives.
func encodeQueues(queues []string) string {
	if queues == nil {
		queues = []string{}
	}
	raw, err := json.Marshal(queues)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

// decodeQueues reads what encodeQueues wrote, reading anything else as no queues.
func decodeQueues(raw string) []string {
	var out []string
	if raw == "" || json.Unmarshal([]byte(raw), &out) != nil {
		return []string{}
	}
	return out
}

// NoteWorker records a report and forgets workers silent longer than the retention, in one write.
func (s *attentionStore) NoteWorker(ctx context.Context, owner string, queues []string,
	slots int) error {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("note worker: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM worker_presence WHERE last_seen < ?",
		sqlutil.FormatTime(now.Add(-attention.WorkerRetention))); err != nil {
		return fmt.Errorf("note worker: %w", err)
	}
	stamp := sqlutil.FormatTime(now)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO worker_presence (owner, queues, slots, first_seen, last_seen) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(owner) DO UPDATE SET
	queues=excluded.queues, slots=excluded.slots, last_seen=excluded.last_seen`,
		owner, encodeQueues(queues), slots, stamp, stamp); err != nil {
		return fmt.Errorf("note worker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("note worker: %w", err)
	}
	return nil
}

// TouchWorker refreshes a worker already noted and leaves an unknown one alone.
func (s *attentionStore) TouchWorker(ctx context.Context, owner string) error {
	if _, err := s.db.ExecContext(ctx, "UPDATE worker_presence SET last_seen=? WHERE owner=?",
		sqlutil.FormatTime(time.Now()), owner); err != nil {
		return fmt.Errorf("touch worker: %w", err)
	}
	return nil
}

// Workers returns the workers seen at or after since, ordered by owner.
func (s *attentionStore) Workers(ctx context.Context, since time.Time) ([]attention.Worker, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT owner, queues, slots, first_seen, last_seen FROM worker_presence
WHERE last_seen >= ? ORDER BY owner`, sqlutil.FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []attention.Worker{}
	for rows.Next() {
		var w attention.Worker
		var queues, first, last string
		if err := rows.Scan(&w.Owner, &queues, &w.Slots, &first, &last); err != nil {
			return nil, fmt.Errorf("list workers: %w", err)
		}
		w.Queues = decodeQueues(queues)
		w.FirstSeen = sqlutil.ParseTimeOrAbsent(first)
		w.LastSeen = sqlutil.ParseTimeOrAbsent(last)
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	return out, nil
}

// ClaimAlert records key once, forgetting keys older than the retention in the same write.
func (s *attentionStore) ClaimAlert(ctx context.Context, key string) (bool, error) {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("claim alert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM attention_alerts WHERE raised_at < ?",
		sqlutil.FormatTime(now.Add(-attention.AlertRetention))); err != nil {
		return false, fmt.Errorf("claim alert: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO attention_alerts (alert_key, raised_at) VALUES (?, ?)
ON CONFLICT(alert_key) DO NOTHING`, key, sqlutil.FormatTime(now))
	if err != nil {
		return false, fmt.Errorf("claim alert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim alert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("claim alert: %w", err)
	}
	return n == 1, nil
}

// Now returns the clock reports are stamped with.
func (s *attentionStore) Now(context.Context) (time.Time, error) { return time.Now(), nil }

// QueuedTimes returns when each pending run last re-entered the queue, for the runs that did.
func (s *store) QueuedTimes(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, queued_at FROM runs WHERE status='pending' AND queued_at IS NOT NULL")
	if err != nil {
		return nil, fmt.Errorf("queued times: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at sql.NullString
		if err := rows.Scan(&id, &at); err != nil {
			return nil, fmt.Errorf("queued times: %w", err)
		}
		if t := sqlutil.ParseTimeOrAbsent(at.String); !t.IsZero() {
			out[id] = t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queued times: %w", err)
	}
	return out, nil
}
