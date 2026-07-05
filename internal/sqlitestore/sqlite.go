// Package sqlitestore implements run.Store on top of SQLite using the pure Go modernc driver, so
// Yardmaster keeps its single binary promise with no cgo. It is the default backend. A Postgres
// backend can satisfy the same run.Store interface later for multi instance deployments.
package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/run"
)

// schema is the table layout created on open. It is idempotent so open doubles as migration.
const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id            TEXT PRIMARY KEY,
	playbook      TEXT NOT NULL,
	inventory     TEXT NOT NULL,
	status        TEXT NOT NULL,
	exit_code     INTEGER,
	error         TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL,
	started_at    TEXT,
	ended_at      TEXT,
	parent_id     TEXT,
	shard_index   INTEGER,
	shard_count   INTEGER,
	limit_pattern TEXT NOT NULL DEFAULT '',
	kind          TEXT NOT NULL DEFAULT '',
	step_name     TEXT NOT NULL DEFAULT '',
	step_index    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_runs_created_at ON runs(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_runs_parent ON runs(parent_id, shard_index);
CREATE TABLE IF NOT EXISTS run_logs (
	seq    INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL,
	chunk  BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_logs_run ON run_logs(run_id, seq);
CREATE TABLE IF NOT EXISTS run_events (
	seq    INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL,
	data   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_events_run ON run_events(run_id, seq);
CREATE TABLE IF NOT EXISTS run_host_summary (
	run_id      TEXT NOT NULL,
	host        TEXT NOT NULL,
	ok          INTEGER NOT NULL,
	changed     INTEGER NOT NULL,
	failures    INTEGER NOT NULL,
	unreachable INTEGER NOT NULL,
	skipped     INTEGER NOT NULL,
	worst       TEXT NOT NULL,
	ran_at      TEXT NOT NULL,
	PRIMARY KEY (run_id, host)
);
CREATE INDEX IF NOT EXISTS idx_host_summary_host ON run_host_summary(host, ran_at DESC);
`

// store is a run.Store backed by a SQLite database.
type store struct {
	// db is the open database handle.
	db *sql.DB
}

// scanner is the read side shared by sql.Row and sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// New opens the SQLite database at path, applies the schema, and returns a run.Store. The returned
// value also implements io.Closer so callers can close the database on shutdown.
func New(path string) (run.Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply %q: %w", p, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &store{db: db}, nil
}

// Close closes the underlying database.
func (s *store) Close() error {
	return s.db.Close()
}

// runColumns is the shared select list so every read scans the same columns in the same order.
const runColumns = `id, playbook, inventory, status, exit_code, error, created_at, started_at,
	ended_at, parent_id, shard_index, shard_count, limit_pattern, kind, step_name, step_index`

// Save inserts or replaces the run identified by r.ID.
func (s *store) Save(ctx context.Context, r *run.Run) error {
	const q = `
INSERT INTO runs
	(id, playbook, inventory, status, exit_code, error, created_at, started_at, ended_at,
	 parent_id, shard_index, shard_count, limit_pattern, kind, step_name, step_index)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	playbook=excluded.playbook, inventory=excluded.inventory, status=excluded.status,
	exit_code=excluded.exit_code, error=excluded.error, created_at=excluded.created_at,
	started_at=excluded.started_at, ended_at=excluded.ended_at,
	parent_id=excluded.parent_id, shard_index=excluded.shard_index,
	shard_count=excluded.shard_count, limit_pattern=excluded.limit_pattern,
	kind=excluded.kind, step_name=excluded.step_name, step_index=excluded.step_index`
	_, err := s.db.ExecContext(ctx, q,
		r.ID, r.Playbook, r.Inventory, string(r.Status), nullInt(r.ExitCode), r.Error,
		formatTime(r.CreatedAt), nullTime(r.StartedAt), nullTime(r.EndedAt),
		nullString(r.ParentID), nullInt(r.ShardIndex), nullInt(r.ShardCount), r.Limit,
		r.Kind, r.StepName, nullInt(r.StepIndex),
	)
	if err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

// Get returns the run with the given id, or run.ErrNotFound.
func (s *store) Get(ctx context.Context, id string) (*run.Run, error) {
	const q = "SELECT " + runColumns + " FROM runs WHERE id=?"
	r, err := scanRun(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, run.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// List returns top-level runs, excluding shard runs, ordered by creation time, newest first.
func (s *store) List(ctx context.Context) ([]*run.Run, error) {
	const q = "SELECT " + runColumns +
		" FROM runs WHERE parent_id IS NULL ORDER BY created_at DESC, id DESC"
	return s.queryRuns(ctx, "list runs", q)
}

// Shards returns the shard runs of a parent ordered by shard index.
func (s *store) Shards(ctx context.Context, parentID string) ([]*run.Run, error) {
	const q = "SELECT " + runColumns + " FROM runs WHERE parent_id=? ORDER BY shard_index"
	return s.queryRuns(ctx, "list shards", q, parentID)
}

// Steps returns the pipeline step runs of a parent ordered by step index.
func (s *store) Steps(ctx context.Context, parentID string) ([]*run.Run, error) {
	const q = "SELECT " + runColumns + " FROM runs WHERE parent_id=? ORDER BY step_index"
	return s.queryRuns(ctx, "list steps", q, parentID)
}

// NonTerminal returns all runs, including shards, that are not in a terminal state.
func (s *store) NonTerminal(ctx context.Context) ([]*run.Run, error) {
	const q = "SELECT " + runColumns + " FROM runs WHERE status IN ('pending', 'running')"
	return s.queryRuns(ctx, "list non-terminal runs", q)
}

// SaveHostSummary replaces the stored per host summaries for a run.
func (s *store) SaveHostSummary(ctx context.Context, runID string, summaries []run.HostSummary) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM run_host_summary WHERE run_id=?", runID); err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO run_host_summary (run_id, host, ok, changed, failures, unreachable, skipped, worst, ran_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, hs := range summaries {
		if _, err := stmt.ExecContext(ctx, runID, hs.Host, hs.OK, hs.Changed, hs.Failures,
			hs.Unreachable, hs.Skipped, hs.Worst, formatTime(hs.RanAt)); err != nil {
			return fmt.Errorf("save host summary: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	return nil
}

// FleetHealth ranks hosts by failures over their most recent window runs, worst first.
func (s *store) FleetHealth(ctx context.Context, window int) ([]run.HostHealth, error) {
	if window < 1 {
		window = 1
	}
	const q = `
WITH ranked AS (
	SELECT host, worst, ran_at,
		ROW_NUMBER() OVER (PARTITION BY host ORDER BY ran_at DESC) AS rn
	FROM run_host_summary
)
SELECT host,
	SUM(CASE WHEN worst IN ('failed', 'unreachable') THEN 1 ELSE 0 END) AS failures,
	COUNT(*) AS total,
	MAX(CASE WHEN rn = 1 THEN worst END) AS last_outcome,
	MAX(ran_at) AS last_run
FROM ranked
WHERE rn <= ?
GROUP BY host
ORDER BY failures DESC, host`

	rows, err := s.db.QueryContext(ctx, q, window)
	if err != nil {
		return nil, fmt.Errorf("fleet health: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []run.HostHealth
	for rows.Next() {
		var (
			h       run.HostHealth
			lastOut string
			lastRun string
		)
		if err := rows.Scan(&h.Host, &h.Failures, &h.Total, &lastOut, &lastRun); err != nil {
			return nil, fmt.Errorf("fleet health: %w", err)
		}
		h.LastOutcome = lastOut
		if h.LastRun, err = parseTime(lastRun); err != nil {
			return nil, fmt.Errorf("fleet health: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet health: %w", err)
	}
	return out, nil
}

// queryRuns runs a select that returns run rows and scans them all.
func (s *store) queryRuns(ctx context.Context, label, query string, args ...any) ([]*run.Run, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	defer func() { _ = rows.Close() }()

	var out []*run.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return out, nil
}

// AppendLog appends raw output bytes to the run's log. Returns run.ErrNotFound if absent.
func (s *store) AppendLog(ctx context.Context, id string, p []byte) error {
	ok, err := s.exists(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return run.ErrNotFound
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO run_logs (run_id, chunk) VALUES (?, ?)", id, p); err != nil {
		return fmt.Errorf("append log: %w", err)
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
	rows, err := s.db.QueryContext(ctx, "SELECT chunk FROM run_logs WHERE run_id=? ORDER BY seq", id)
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

// AppendEvents appends structured events to the run. Returns run.ErrNotFound if absent.
func (s *store) AppendEvents(ctx context.Context, id string, events []event.Event) error {
	ok, err := s.exists(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return run.ErrNotFound
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append events: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, "INSERT INTO run_events (run_id, data) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("append events: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("append events: %w", err)
		}
		if _, err := stmt.ExecContext(ctx, id, string(data)); err != nil {
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
	rows, err := s.db.QueryContext(ctx, "SELECT data FROM run_events WHERE run_id=? ORDER BY seq", id)
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

// exists reports whether a run with id is present.
func (s *store) exists(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM runs WHERE id=?", id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check run: %w", err)
	}
	return true, nil
}

// scanRun reads one run row from a scanner.
func scanRun(s scanner) (*run.Run, error) {
	var (
		r        run.Run
		status   string
		exit     sql.NullInt64
		created  string
		started  sql.NullString
		ended    sql.NullString
		parent   sql.NullString
		shardIdx sql.NullInt64
		shardCnt sql.NullInt64
		stepIdx  sql.NullInt64
	)
	if err := s.Scan(&r.ID, &r.Playbook, &r.Inventory, &status, &exit, &r.Error,
		&created, &started, &ended, &parent, &shardIdx, &shardCnt, &r.Limit,
		&r.Kind, &r.StepName, &stepIdx); err != nil {
		return nil, err
	}
	r.Status = run.Status(status)
	if exit.Valid {
		v := int(exit.Int64)
		r.ExitCode = &v
	}
	t, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	r.CreatedAt = t
	if r.StartedAt, err = parseNullTime(started); err != nil {
		return nil, err
	}
	if r.EndedAt, err = parseNullTime(ended); err != nil {
		return nil, err
	}
	if parent.Valid {
		p := parent.String
		r.ParentID = &p
	}
	if shardIdx.Valid {
		i := int(shardIdx.Int64)
		r.ShardIndex = &i
	}
	if shardCnt.Valid {
		c := int(shardCnt.Int64)
		r.ShardCount = &c
	}
	if stepIdx.Valid {
		i := int(stepIdx.Int64)
		r.StepIndex = &i
	}
	return &r, nil
}

// formatTime renders a time as a sortable UTC string.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime parses a stored time string.
func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
}

// nullInt maps an optional int to a database value.
func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullString maps an optional string to a database value.
func nullString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullTime maps an optional time to a database value.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// parseNullTime parses an optional stored time.
func parseNullTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
