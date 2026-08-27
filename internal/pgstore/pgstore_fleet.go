package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// hostSummaryColumns is the shared run_host_summary column list, in the one order the insert binds
// its placeholders and every read scans, so a column cannot land on one path and be missed on
// another.
const hostSummaryColumns = `run_id, host, ok, changed, failures, unreachable, skipped, worst,
	duration_seconds, ran_at, dry_run`

// scanHostSummary reads one host summary row selected as hostSummaryColumns.
func scanHostSummary(rows *sql.Rows) (run.HostSummary, error) {
	var (
		hs     run.HostSummary
		ranAt  string
		dryRun int
	)
	if err := rows.Scan(&hs.RunID, &hs.Host, &hs.OK, &hs.Changed, &hs.Failures, &hs.Unreachable,
		&hs.Skipped, &hs.Worst, &hs.DurationSeconds, &ranAt, &dryRun); err != nil {
		return hs, err
	}
	hs.DryRun = dryRun != 0
	var err error
	hs.RanAt, err = sqlutil.ParseTime(ranAt)
	return hs, err
}

// SaveHostSummary replaces the stored per host summaries for a run, stamping each row with the
// run's dry-run flag so the drift view reads the summary alone and outlives the run record.
func (s *store) SaveHostSummary(ctx context.Context, runID string, summaries []run.HostSummary) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if fenced, err := summaryFenced(ctx, tx, runID); err != nil {
		return fmt.Errorf("save host summary: %w", err)
	} else if fenced {
		return nil
	}
	dry, err := runIsDryRun(ctx, tx, runID)
	if err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM run_host_summary WHERE run_id=$1", runID); err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO run_host_summary
	(`+hostSummaryColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`)
	if err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, hs := range summaries {
		if _, err := stmt.ExecContext(ctx, runID, hs.Host, hs.OK, hs.Changed, hs.Failures,
			hs.Unreachable, hs.Skipped, hs.Worst, hs.DurationSeconds, sqlutil.FormatTime(hs.RanAt),
			sqlutil.BoolToInt(dry)); err != nil {
			return fmt.Errorf("save host summary: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	return nil
}

// FleetHealth ranks hosts by failures over their most recent window runs, worst first. A flip is
// a switch between failing and passing across consecutive runs; two or more flips mark the host
// flaky.
func (s *store) FleetHealth(ctx context.Context, window int) ([]run.HostHealth, error) {
	if window < 1 {
		window = 1
	}
	const q = `
WITH ranked AS (
	SELECT host, worst, run_id, ran_at,
		ROW_NUMBER() OVER (PARTITION BY host ORDER BY ` + sqlutil.TimeOrder + ` DESC, run_id DESC) AS rn
	FROM run_host_summary
), recent AS (
	SELECT host, worst, run_id, ran_at, rn,
		CASE WHEN worst IN ('failed', 'unreachable') THEN 1 ELSE 0 END AS bad,
		LAG(CASE WHEN worst IN ('failed', 'unreachable') THEN 1 ELSE 0 END)
			OVER (PARTITION BY host ORDER BY ` + sqlutil.TimeOrder + ` DESC, run_id DESC) AS prev_bad
	FROM ranked
	WHERE rn <= $1
)
SELECT host,
	SUM(bad) AS failures,
	COUNT(*) AS total,
	MAX(CASE WHEN rn = 1 THEN worst END) AS last_outcome,
	MAX(CASE WHEN rn = 1 THEN ran_at END) AS last_run,
	SUM(CASE WHEN prev_bad IS NOT NULL AND bad != prev_bad THEN 1 ELSE 0 END) AS flips,
	STRING_AGG(worst, ',' ORDER BY rn) AS recent,
	STRING_AGG(run_id, ',' ORDER BY rn) AS recent_runs
FROM recent
GROUP BY host
ORDER BY failures DESC, host COLLATE "C"` // C collation matches SQLite's byte order; see collationNote.

	rows, err := s.db.QueryContext(ctx, q, window)
	if err != nil {
		return nil, fmt.Errorf("fleet health: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []run.HostHealth
	for rows.Next() {
		var (
			h          run.HostHealth
			lastOut    string
			lastRun    string
			recent     string
			recentRuns string
		)
		if err := rows.Scan(&h.Host, &h.Failures, &h.Total, &lastOut, &lastRun, &h.Flips,
			&recent, &recentRuns); err != nil {
			return nil, fmt.Errorf("fleet health: %w", err)
		}
		h.LastOutcome = lastOut
		if h.LastRun, err = sqlutil.ParseTime(lastRun); err != nil {
			return nil, fmt.Errorf("fleet health: %w", err)
		}
		h.Flaky = h.Flips >= 2
		if recent != "" {
			h.Recent = strings.Split(recent, ",")
			h.RecentRuns = strings.Split(recentRuns, ",")
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet health: %w", err)
	}
	return out, nil
}

// HostCosts returns each host's average recorded duration in seconds over its most recent window
// runs, for balancing splits by past cost.
func (s *store) HostCosts(ctx context.Context, window int) (map[string]float64, error) {
	if window < 1 {
		window = 1
	}
	const q = `
WITH ranked AS (
	SELECT host, duration_seconds,
		ROW_NUMBER() OVER (PARTITION BY host ORDER BY ` + sqlutil.TimeOrder + ` DESC, run_id DESC) AS rn
	FROM run_host_summary
)
SELECT host, AVG(duration_seconds) FROM ranked WHERE rn <= $1 GROUP BY host`

	rows, err := s.db.QueryContext(ctx, q, window)
	if err != nil {
		return nil, fmt.Errorf("host costs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]float64)
	for rows.Next() {
		var (
			host string
			cost float64
		)
		if err := rows.Scan(&host, &cost); err != nil {
			return nil, fmt.Errorf("host costs: %w", err)
		}
		out[host] = cost
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("host costs: %w", err)
	}
	return out, nil
}

// DriftStatus reports each host's most recent drift check, the latest dry run to touch it, worst
// drift first. Only dry runs count, where a changed result means a task would change, so the host
// has diverged from the desired state. The dry-run flag is read off the summary row that
// SaveHostSummary stamped, not off the runs table, because retention deletes runs and keeps the
// summaries. Joining runs dropped purged hosts out of this view while fleet health, which reads the
// same summaries, kept them, so the two views of one fleet stopped reconciling.
func (s *store) DriftStatus(ctx context.Context) ([]run.HostDrift, error) {
	const q = `
WITH checks AS (
	SELECT hs.host, hs.changed, hs.run_id, hs.ran_at,
		ROW_NUMBER() OVER (PARTITION BY hs.host ORDER BY ` + sqlutil.TimeOrder + ` DESC, run_id DESC) AS rn
	FROM run_host_summary hs
	WHERE hs.dry_run = 1
)
SELECT host, changed, run_id, ran_at FROM checks WHERE rn = 1 ORDER BY changed DESC, host COLLATE "C"`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("drift status: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []run.HostDrift
	for rows.Next() {
		var (
			d     run.HostDrift
			ranAt string
		)
		if err := rows.Scan(&d.Host, &d.DriftedTasks, &d.RunID, &ranAt); err != nil {
			return nil, fmt.Errorf("drift status: %w", err)
		}
		if d.CheckedAt, err = sqlutil.ParseTime(ranAt); err != nil {
			return nil, fmt.Errorf("drift status: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("drift status: %w", err)
	}
	return out, nil
}

// HostHistory returns a host's most recent per run summaries, newest first, with run ids.
// SaveHostFacts records each host's gathered facts, replacing what was held before since the
// newest gather is the truth about a host.
func (s *store) SaveHostFacts(ctx context.Context, runID string, facts []run.HostFacts) error {
	if len(facts) == 0 {
		return nil
	}
	const q = `
INSERT INTO host_facts (host, run_id, facts, gathered_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT(host) DO UPDATE SET
	run_id=excluded.run_id, facts=excluded.facts, gathered_at=excluded.gathered_at`
	for _, f := range facts {
		if f.Host == "" || len(f.Facts) == 0 {
			continue
		}
		at := f.GatheredAt
		if at.IsZero() {
			at = time.Now()
		}
		blob, err := json.Marshal(f.Facts)
		if err != nil {
			return fmt.Errorf("save host facts: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, q, f.Host, runID, string(blob),
			sqlutil.FormatTime(at)); err != nil {
			return fmt.Errorf("save host facts: %w", err)
		}
	}
	return nil
}

// HostFactsFor returns a host's stored facts, or run.ErrNotFound when it was never gathered.
func (s *store) HostFactsFor(ctx context.Context, host string) (*run.HostFacts, error) {
	const q = "SELECT host, run_id, facts, gathered_at FROM host_facts WHERE host = $1"
	var (
		out      run.HostFacts
		blob     string
		gathered string
	)
	err := s.db.QueryRowContext(ctx, q, host).Scan(&out.Host, &out.RunID, &blob, &gathered)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, run.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("host facts: %w", err)
	}
	if err := json.Unmarshal([]byte(blob), &out.Facts); err != nil {
		return nil, fmt.Errorf("host facts: %w", err)
	}
	if out.GatheredAt, err = sqlutil.ParseTime(gathered); err != nil {
		return nil, fmt.Errorf("host facts: %w", err)
	}
	return &out, nil
}

// HostHistory returns a host's most recent per run summaries, newest first, with run ids.
func (s *store) HostHistory(ctx context.Context, host string, limit int) ([]run.HostSummary, error) {
	if limit < 1 {
		limit = 1
	}
	const q = `SELECT ` + hostSummaryColumns + `
FROM run_host_summary WHERE host = $1 ORDER BY ` + sqlutil.TimeOrder + ` DESC, run_id DESC LIMIT $2`

	rows, err := s.db.QueryContext(ctx, q, host, limit)
	if err != nil {
		return nil, fmt.Errorf("host history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []run.HostSummary
	for rows.Next() {
		hs, err := scanHostSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("host history: %w", err)
		}
		out = append(out, hs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("host history: %w", err)
	}
	return out, nil
}

// RunHostSummaries returns one run's stored per host summaries, ordered by host.
func (s *store) RunHostSummaries(ctx context.Context, runID string) ([]run.HostSummary, error) {
	const q = `SELECT ` + hostSummaryColumns + `
FROM run_host_summary WHERE run_id = $1 ORDER BY host ASC`
	rows, err := s.db.QueryContext(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("run host summaries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []run.HostSummary
	for rows.Next() {
		hs, err := scanHostSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("run host summaries: %w", err)
		}
		out = append(out, hs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run host summaries: %w", err)
	}
	return out, nil
}

// RunTaskSummaries returns one run's stored per task summaries, ordered by task.
func (s *store) RunTaskSummaries(ctx context.Context, runID string) ([]run.TaskSummary, error) {
	const q = `
SELECT run_id, task, seconds, ran_at FROM run_task_summary WHERE run_id = $1 ORDER BY task ASC`
	rows, err := s.db.QueryContext(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("run task summaries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []run.TaskSummary
	for rows.Next() {
		var (
			ts    run.TaskSummary
			ranAt string
		)
		if err := rows.Scan(&ts.RunID, &ts.Task, &ts.Seconds, &ranAt); err != nil {
			return nil, fmt.Errorf("run task summaries: %w", err)
		}
		if ts.RanAt, err = sqlutil.ParseTime(ranAt); err != nil {
			return nil, fmt.Errorf("run task summaries: %w", err)
		}
		out = append(out, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run task summaries: %w", err)
	}
	return out, nil
}

// SaveTaskSummary replaces the stored per task summaries for a run.
func (s *store) SaveTaskSummary(ctx context.Context, runID string, summaries []run.TaskSummary) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save task summary: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if fenced, err := summaryFenced(ctx, tx, runID); err != nil {
		return fmt.Errorf("save task summary: %w", err)
	} else if fenced {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM run_task_summary WHERE run_id=$1", runID); err != nil {
		return fmt.Errorf("save task summary: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		"INSERT INTO run_task_summary (run_id, task, seconds, ran_at) VALUES ($1, $2, $3, $4)")
	if err != nil {
		return fmt.Errorf("save task summary: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, ts := range summaries {
		if _, err := stmt.ExecContext(ctx, runID, ts.Task, ts.Seconds, sqlutil.FormatTime(ts.RanAt)); err != nil {
			return fmt.Errorf("save task summary: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save task summary: %w", err)
	}
	return nil
}

// AppendHostSummary upserts the given per-host summaries into the run's set, keyed by (run_id, host),
// leaving the run's other rows in place, so a relay report continued across batches writes only its
// batch instead of the whole growing set. It fences a terminal run and ignores an empty batch.
func (s *store) AppendHostSummary(ctx context.Context, runID string, summaries []run.HostSummary) error {
	if len(summaries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append host summary: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if fenced, err := summaryFenced(ctx, tx, runID); err != nil {
		return fmt.Errorf("append host summary: %w", err)
	} else if fenced {
		return nil
	}
	dry, err := runIsDryRun(ctx, tx, runID)
	if err != nil {
		return fmt.Errorf("append host summary: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO run_host_summary
	(`+hostSummaryColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (run_id, host) DO UPDATE SET
	ok=EXCLUDED.ok, changed=EXCLUDED.changed, failures=EXCLUDED.failures,
	unreachable=EXCLUDED.unreachable, skipped=EXCLUDED.skipped, worst=EXCLUDED.worst,
	duration_seconds=EXCLUDED.duration_seconds, ran_at=EXCLUDED.ran_at, dry_run=EXCLUDED.dry_run`)
	if err != nil {
		return fmt.Errorf("append host summary: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, hs := range summaries {
		if _, err := stmt.ExecContext(ctx, runID, hs.Host, hs.OK, hs.Changed, hs.Failures,
			hs.Unreachable, hs.Skipped, hs.Worst, hs.DurationSeconds, sqlutil.FormatTime(hs.RanAt),
			sqlutil.BoolToInt(dry)); err != nil {
			return fmt.Errorf("append host summary: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append host summary: %w", err)
	}
	return nil
}

// AppendTaskSummary upserts the given per-task summaries into the run's set, keyed by (run_id, task),
// with the same fencing and empty-batch behavior as AppendHostSummary.
func (s *store) AppendTaskSummary(ctx context.Context, runID string, summaries []run.TaskSummary) error {
	if len(summaries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append task summary: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if fenced, err := summaryFenced(ctx, tx, runID); err != nil {
		return fmt.Errorf("append task summary: %w", err)
	} else if fenced {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO run_task_summary (run_id, task, seconds, ran_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (run_id, task) DO UPDATE SET seconds=EXCLUDED.seconds, ran_at=EXCLUDED.ran_at`)
	if err != nil {
		return fmt.Errorf("append task summary: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, ts := range summaries {
		if _, err := stmt.ExecContext(ctx, runID, ts.Task, ts.Seconds, sqlutil.FormatTime(ts.RanAt)); err != nil {
			return fmt.Errorf("append task summary: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append task summary: %w", err)
	}
	return nil
}

// TaskTrends aggregates each task's durations over its most recent window runs.
func (s *store) TaskTrends(ctx context.Context, window int) ([]run.TaskTrend, error) {
	if window < 1 {
		window = 1
	}
	// Rows come back per task oldest first and fold in Go, so the trend series needs no
	// dialect-specific aggregate.
	const q = `
WITH ranked AS (
	SELECT task, seconds, ran_at,
		ROW_NUMBER() OVER (PARTITION BY task ORDER BY ` + sqlutil.TimeOrder + ` DESC, run_id DESC) AS rn
	FROM run_task_summary
)
SELECT task, seconds, ran_at FROM ranked WHERE rn <= $1 ORDER BY task, rn DESC`

	rows, err := s.db.QueryContext(ctx, q, window)
	if err != nil {
		return nil, fmt.Errorf("task trends: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []run.TaskTrend
	for rows.Next() {
		var (
			task    string
			seconds float64
			ranAt   string
		)
		if err := rows.Scan(&task, &seconds, &ranAt); err != nil {
			return nil, fmt.Errorf("task trends: %w", err)
		}
		at, err := sqlutil.ParseTime(ranAt)
		if err != nil {
			return nil, fmt.Errorf("task trends: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].Task != task {
			out = append(out, run.TaskTrend{Task: task})
		}
		t := &out[len(out)-1]
		t.Runs++
		t.Recent = append(t.Recent, seconds)
		t.AvgSeconds += seconds
		t.LastSeconds = seconds
		t.LastRun = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task trends: %w", err)
	}
	for i := range out {
		out[i].AvgSeconds /= float64(out[i].Runs)
	}
	return out, nil
}

// Workers lists executors by the leases they hold within run.WorkerWindow, most recently seen
// first, so the listing stays bounded as history grows.
func (s *store) Workers(ctx context.Context) ([]run.WorkerInfo, error) {
	const q = `
SELECT claimed_by,
	SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END) AS active,
	SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END) AS completed,
	SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END) AS failed,
	MAX(claimed_at) AS last_seen
FROM runs
WHERE claimed_by != '' AND claimed_at IS NOT NULL AND claimed_at >= $1
GROUP BY claimed_by
ORDER BY last_seen DESC, claimed_by COLLATE "C"`

	rows, err := s.db.QueryContext(ctx, q, sqlutil.FormatTime(time.Now().Add(-run.WorkerWindow)))
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []run.WorkerInfo
	for rows.Next() {
		var (
			w    run.WorkerInfo
			seen string
		)
		if err := rows.Scan(&w.Owner, &w.Active, &w.Completed, &w.Failed, &seen); err != nil {
			return nil, fmt.Errorf("list workers: %w", err)
		}
		if w.LastSeen, err = sqlutil.ParseTime(seen); err != nil {
			return nil, fmt.Errorf("list workers: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	return out, nil
}
