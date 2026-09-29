package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// AppendLog appends raw output bytes to the run's log. Returns run.ErrNotFound if absent. The
// insert-select folds the missing-run check into the write so the per-chunk output path costs one
// statement instead of two.
func (s *store) AppendLog(ctx context.Context, id string, p []byte) error {
	// The accumulated size is fenced in the same statement as the terminal check, so two writers
	// racing cannot both read a size under the cap and both append past it.
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO run_logs (run_id, chunk) SELECT $1, $2 WHERE EXISTS (SELECT 1 FROM runs WHERE id=$1 AND "+
			nonTerminalRun+") AND COALESCE((SELECT SUM(LENGTH(chunk)) FROM run_logs WHERE run_id=$1), 0) < $3",
		id, p, run.MaxLogBytes)
	if err != nil {
		return fmt.Errorf("append log: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("append log: %w", err)
	}
	if n == 0 {
		// The insert was fenced. A missing run is still an error; a terminal run is a silent no-op, so a
		// reclaimed-but-alive worker cannot write into a run that has ended.
		ok, err := s.exists(ctx, id)
		if err != nil {
			return fmt.Errorf("append log: %w", err)
		}
		if !ok {
			return run.ErrNotFound
		}
		// The run is still live, so the cap is what refused the write. Say so on the run itself,
		// once, or a reader sees a log that simply stops and reads it as a run that went quiet.
		if _, err := s.db.ExecContext(ctx,
			"UPDATE runs SET warning=$1 WHERE id=$2 AND (warning IS NULL OR warning='')",
			run.LogTruncatedWarning, id); err != nil {
			return fmt.Errorf("append log: %w", err)
		}
	}
	return nil
}

// Log returns a copy of the run's captured output, or run.ErrNotFound.
func (s *store) Log(ctx context.Context, id string) ([]byte, error) {
	ok, err := s.exists(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, run.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT chunk FROM run_logs WHERE run_id=$1 ORDER BY seq", id)
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var buf []byte
	for rows.Next() {
		var chunk []byte
		if err := rows.Scan(&chunk); err != nil {
			return nil, fmt.Errorf("read log: %w", err)
		}
		buf = append(buf, chunk...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	return buf, nil
}

// LogAfter returns the run's log chunks past afterSeq in order, capped at limit chunks.
func (s *store) LogAfter(ctx context.Context, id string, afterSeq int64, limit int) ([]run.LogChunk, error) {
	ok, err := s.exists(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, run.ErrNotFound
	}
	query := "SELECT seq, chunk FROM run_logs WHERE run_id=$1 AND seq > $2 ORDER BY seq"
	args := []any{id, afterSeq}
	if limit > 0 {
		query += " LIMIT $3"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []run.LogChunk
	for rows.Next() {
		var c run.LogChunk
		if err := rows.Scan(&c.Seq, &c.Data); err != nil {
			return nil, fmt.Errorf("read log: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	return out, nil
}

// LastLogSeq returns the seq of the run's most recent log chunk, or zero when it has none.
func (s *store) LastLogSeq(ctx context.Context, id string) (int64, error) {
	ok, err := s.exists(ctx, id)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, run.ErrNotFound
	}
	var seq int64
	err = s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0) FROM run_logs WHERE run_id=$1", id).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("read log: %w", err)
	}
	return seq, nil
}

// AppendEvents appends structured events to the run. Returns run.ErrNotFound if absent. Events
// are marshaled before the transaction opens so the transaction stays short.
func (s *store) AppendEvents(ctx context.Context, id string, events []event.Event) error {
	rows := make([]string, len(events))
	for i, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("append events: %w", err)
		}
		rows[i] = string(data)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append events: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	err = tx.QueryRowContext(ctx, "SELECT status FROM runs WHERE id=$1", id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return run.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("append events: %w", err)
	}
	if run.Status(status).Terminal() {
		// Fence a terminal run so a reclaimed-but-alive worker cannot stream events into it.
		return nil
	}

	stmt, err := tx.PrepareContext(ctx, "INSERT INTO run_events (run_id, data) VALUES ($1, $2)")
	if err != nil {
		return fmt.Errorf("append events: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, data := range rows {
		if _, err := stmt.ExecContext(ctx, id, data); err != nil {
			return fmt.Errorf("append events: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append events: %w", err)
	}
	return nil
}

// Events returns a copy of the run's structured events, or run.ErrNotFound.
func (s *store) Events(ctx context.Context, id string) ([]event.Event, error) {
	ok, err := s.exists(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, run.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT data FROM run_events WHERE run_id=$1 ORDER BY seq", id)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []event.Event
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		var e event.Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return out, nil
}

// EventsAfter returns the run's events with seq greater than afterSeq, ordered, capped at
// limit. A limit of zero or less returns every matching event. Each event carries its Seq.
func (s *store) EventsAfter(ctx context.Context, id string, afterSeq int64, limit int) ([]event.Event, error) {
	ok, err := s.exists(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, run.ErrNotFound
	}
	query := "SELECT seq, data FROM run_events WHERE run_id=$1 AND seq > $2 ORDER BY seq"
	args := []any{id, afterSeq}
	if limit > 0 {
		query += " LIMIT $3"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []event.Event
	for rows.Next() {
		var seq int64
		var data string
		if err := rows.Scan(&seq, &data); err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		var e event.Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		e.Seq = seq
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return out, nil
}

// LastEventSeq returns the seq of the run's most recent event, or zero when it has none.
func (s *store) LastEventSeq(ctx context.Context, id string) (int64, error) {
	ok, err := s.exists(ctx, id)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, run.ErrNotFound
	}
	var seq int64
	err = s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0) FROM run_events WHERE run_id=$1", id).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("read events: %w", err)
	}
	return seq, nil
}
