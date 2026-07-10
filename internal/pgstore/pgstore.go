// Package pgstore implements run.Store and schedule.Store on PostgreSQL for multi instance
// deployments. It mirrors the SQLite backend behind the same interfaces and the same shared
// contract tests, storing times as RFC3339 text so both backends order and compare identically.
package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/dcadolph/yardmaster/internal/audit"
	"github.com/dcadolph/yardmaster/internal/auth"
	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/grant"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/invsource"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/schedule"
	"github.com/dcadolph/yardmaster/internal/team"
	"github.com/dcadolph/yardmaster/internal/template"
	"github.com/dcadolph/yardmaster/internal/trigger"
	"github.com/dcadolph/yardmaster/internal/user"
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
	step_index    INTEGER,
	retry_of      TEXT,
	attempt       INTEGER NOT NULL DEFAULT 0,
	extra_vars    TEXT NOT NULL DEFAULT '',
	outputs       TEXT NOT NULL DEFAULT '',
	claimed_by    TEXT NOT NULL DEFAULT '',
	claimed_at    TEXT,
	cancel_requested INTEGER NOT NULL DEFAULT 0,
	credential_ids TEXT NOT NULL DEFAULT '',
	project_id    TEXT NOT NULL DEFAULT '',
	commit_sha    TEXT NOT NULL DEFAULT '',
	inventory_id  TEXT NOT NULL DEFAULT '',
	queue         TEXT NOT NULL DEFAULT '',
	tool          TEXT NOT NULL DEFAULT '',
	command       TEXT NOT NULL DEFAULT '',
	dry_run       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_runs_created_at ON runs(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_runs_parent ON runs(parent_id, shard_index);
CREATE TABLE IF NOT EXISTS run_logs (
	seq    BIGSERIAL PRIMARY KEY,
	run_id TEXT NOT NULL,
	chunk  BYTEA NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_logs_run ON run_logs(run_id, seq);
CREATE TABLE IF NOT EXISTS run_events (
	seq    BIGSERIAL PRIMARY KEY,
	run_id TEXT NOT NULL,
	data   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_events_run ON run_events(run_id, seq);
CREATE TABLE IF NOT EXISTS run_host_summary (
	run_id           TEXT NOT NULL,
	host             TEXT NOT NULL,
	ok               INTEGER NOT NULL,
	changed          INTEGER NOT NULL,
	failures         INTEGER NOT NULL,
	unreachable      INTEGER NOT NULL,
	skipped          INTEGER NOT NULL,
	worst            TEXT NOT NULL,
	duration_seconds DOUBLE PRECISION NOT NULL DEFAULT 0,
	ran_at           TEXT NOT NULL,
	PRIMARY KEY (run_id, host)
);
CREATE INDEX IF NOT EXISTS idx_host_summary_host ON run_host_summary(host, ran_at DESC);
CREATE TABLE IF NOT EXISTS run_task_summary (
	run_id  TEXT NOT NULL,
	task    TEXT NOT NULL,
	seconds DOUBLE PRECISION NOT NULL,
	ran_at  TEXT NOT NULL,
	PRIMARY KEY (run_id, task)
);
CREATE INDEX IF NOT EXISTS idx_task_summary_task ON run_task_summary(task, ran_at DESC);
CREATE TABLE IF NOT EXISTS schedules (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL DEFAULT '',
	cron        TEXT NOT NULL,
	playbook    TEXT NOT NULL DEFAULT '',
	inventory   TEXT NOT NULL DEFAULT '',
	shards      INTEGER NOT NULL DEFAULT 0,
	steps       TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL,
	next_run_at TEXT,
	last_run_at TEXT,
	last_run_id TEXT NOT NULL DEFAULT '',
	template_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_schedules_created ON schedules(created_at, id);
CREATE TABLE IF NOT EXISTS users (
	id            TEXT PRIMARY KEY,
	username      TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	role          TEXT NOT NULL,
	created_at    TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username ON users(username);
CREATE TABLE IF NOT EXISTS tokens (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL DEFAULT '',
	hash         TEXT NOT NULL,
	user_id      TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	last_used_at TEXT,
	expires_at   TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tokens_hash ON tokens(hash);
CREATE TABLE IF NOT EXISTS projects (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL DEFAULT '',
	repo_url      TEXT NOT NULL,
	branch        TEXT NOT NULL DEFAULT '',
	credential_id TEXT NOT NULL DEFAULT '',
	install_deps  INTEGER NOT NULL DEFAULT 1,
	image         TEXT NOT NULL DEFAULT '',
	pull_credential_id TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS templates (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL DEFAULT '',
	project_id     TEXT NOT NULL DEFAULT '',
	playbook       TEXT NOT NULL,
	inventory      TEXT NOT NULL DEFAULT '',
	inventory_id   TEXT NOT NULL DEFAULT '',
	shards         INTEGER NOT NULL DEFAULT 0,
	credential_ids TEXT NOT NULL DEFAULT '',
	extra_vars     TEXT NOT NULL DEFAULT '',
	survey         TEXT NOT NULL DEFAULT '',
	queue          TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	tool           TEXT NOT NULL DEFAULT '',
	command        TEXT NOT NULL DEFAULT '',
	dry_run        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS inventory_sources (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL DEFAULT '',
	source        TEXT NOT NULL,
	credential_id TEXT NOT NULL DEFAULT '',
	project_id    TEXT NOT NULL DEFAULT '',
	inventory_id  TEXT NOT NULL DEFAULT '',
	synced_at     TEXT,
	last_error    TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS triggers (
	id                TEXT PRIMARY KEY,
	name              TEXT NOT NULL DEFAULT '',
	template_id       TEXT NOT NULL,
	token_hash        TEXT NOT NULL,
	signing_secret    TEXT NOT NULL DEFAULT '',
	require_signature INTEGER NOT NULL DEFAULT 0,
	last_fired_at     TEXT,
	created_at        TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_triggers_hash ON triggers(token_hash);
CREATE TABLE IF NOT EXISTS audit_entries (
	id     TEXT PRIMARY KEY,
	at     TEXT NOT NULL,
	actor  TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL,
	path   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_entries(at DESC);
CREATE TABLE IF NOT EXISTS inventories (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	content    TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS credentials (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	kind       TEXT NOT NULL,
	secret     TEXT NOT NULL,
	created_at TEXT NOT NULL,
	source     TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS teams (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS team_members (
	team_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL,
	PRIMARY KEY (team_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_team_members_user ON team_members(user_id);
CREATE TABLE IF NOT EXISTS grants (
	id         TEXT PRIMARY KEY,
	subject    TEXT NOT NULL,
	object     TEXT NOT NULL,
	access     TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_grants_object ON grants(object);
ALTER TABLE runs ADD COLUMN IF NOT EXISTS claimed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS claimed_at TEXT;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS cancel_requested INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS credential_ids TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS project_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS commit_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS inventory_id TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS template_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN IF NOT EXISTS expires_at TEXT;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS survey TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS queue TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS queue TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS install_deps INTEGER NOT NULL DEFAULT 1;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS image TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS pull_credential_id TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS inventory_id TEXT NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS signing_secret TEXT NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS require_signature INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS tool TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS command TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS dry_run INTEGER NOT NULL DEFAULT 0;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS tool TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS command TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS dry_run INTEGER NOT NULL DEFAULT 0;
ALTER TABLE credentials ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '';
`

// store is a run.Store backed by a PostgreSQL database.
type store struct {
	// db is the open database handle.
	db *sql.DB
}

// scanner is the read side shared by sql.Row and sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// DB holds the PostgreSQL backed run and schedule stores sharing one database.
type DB struct {
	// db is the open database handle.
	db *sql.DB
	// runs is the run store.
	runs *store
	// schedules is the schedule store.
	schedules *scheduleStore
	// tokens is the API token store.
	tokens *tokenStore
	// credentials is the execution secret store.
	credentials *credentialStore
	// projects is the git project store.
	projects *projectStore
	// templates is the job template store.
	templates *templateStore
	// users is the account store.
	users *userStore
	// inventories is the stored inventory store.
	inventories *inventoryStore
	// audits is the audit trail store.
	audits *auditStore
	// invSources is the dynamic inventory source store.
	invSources *invSourceStore
	// triggers is the webhook trigger store.
	triggers *triggerStore
	// teams is the team store.
	teams *teamStore
	// grants is the per-object access grant store.
	grants *grantStore
}

// Open connects to the PostgreSQL database at dsn, applies the schema, and returns the bundled
// stores.
func Open(dsn string) (*DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	// Several processes, a server and its workers, may open the same database at once, and
	// concurrent ALTER TABLE statements deadlock. A session advisory lock serializes migration.
	if _, err := db.Exec("SELECT pg_advisory_lock(7973821001)"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migration lock: %w", err)
	}
	_, migrateErr := db.Exec(schema)
	if _, err := db.Exec("SELECT pg_advisory_unlock(7973821001)"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migration unlock: %w", err)
	}
	if migrateErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate schema: %w", migrateErr)
	}
	return &DB{db: db, runs: &store{db: db}, schedules: &scheduleStore{db: db}, tokens: &tokenStore{db: db},
		credentials: &credentialStore{db: db},
		projects:    &projectStore{db: db},
		templates:   &templateStore{db: db},
		users:       &userStore{db: db},
		inventories: &inventoryStore{db: db},
		audits:      &auditStore{db: db},
		invSources:  &invSourceStore{db: db},
		triggers:    &triggerStore{db: db},
		teams:       &teamStore{db: db},
		grants:      &grantStore{db: db}}, nil
}

// Runs returns the run store.
func (d *DB) Runs() run.Store {
	return d.runs
}

// Schedules returns the schedule store.
func (d *DB) Schedules() schedule.Store {
	return d.schedules
}

// Tokens returns the API token store.
func (d *DB) Tokens() auth.Store {
	return d.tokens
}

// Credentials returns the execution secret store.
func (d *DB) Credentials() credential.Store {
	return d.credentials
}

// Projects returns the git project store.
func (d *DB) Projects() project.Store {
	return d.projects
}

// Templates returns the job template store.
func (d *DB) Templates() template.Store {
	return d.templates
}

// Users returns the account store.
func (d *DB) Users() user.Store {
	return d.users
}

// Inventories returns the stored inventory store.
func (d *DB) Inventories() inventory.Store {
	return d.inventories
}

// Audits returns the audit trail store.
func (d *DB) Audits() audit.Store {
	return d.audits
}

// InventorySources returns the dynamic inventory source store.
func (d *DB) InventorySources() invsource.Store {
	return d.invSources
}

// Triggers returns the webhook trigger store.
func (d *DB) Triggers() trigger.Store {
	return d.triggers
}

// Teams returns the team store.
func (d *DB) Teams() team.Store {
	return d.teams
}

// Grants returns the per-object access grant store.
func (d *DB) Grants() grant.Store {
	return d.grants
}

// Close closes the underlying database.
func (d *DB) Close() error {
	return d.db.Close()
}

// runColumns is the shared select list so every read scans the same columns in the same order.
const runColumns = `id, playbook, inventory, status, exit_code, error, created_at, started_at,
	ended_at, parent_id, shard_index, shard_count, limit_pattern, kind, step_name, step_index,
	retry_of, attempt, extra_vars, outputs, claimed_by, claimed_at, cancel_requested,
	credential_ids, project_id, commit_sha, inventory_id, queue, tool, command, dry_run`

// Save inserts or replaces the run identified by r.ID.
func (s *store) Save(ctx context.Context, r *run.Run) error {
	const q = `
INSERT INTO runs
	(id, playbook, inventory, status, exit_code, error, created_at, started_at, ended_at,
	 parent_id, shard_index, shard_count, limit_pattern, kind, step_name, step_index, retry_of,
	 attempt, extra_vars, outputs, claimed_by, claimed_at, cancel_requested, credential_ids,
	 project_id, commit_sha, inventory_id, queue, tool, command, dry_run)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
	$21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31)
ON CONFLICT(id) DO UPDATE SET
	playbook=excluded.playbook, inventory=excluded.inventory, status=excluded.status,
	exit_code=excluded.exit_code, error=excluded.error, created_at=excluded.created_at,
	started_at=excluded.started_at, ended_at=excluded.ended_at,
	parent_id=excluded.parent_id, shard_index=excluded.shard_index,
	shard_count=excluded.shard_count, limit_pattern=excluded.limit_pattern,
	kind=excluded.kind, step_name=excluded.step_name, step_index=excluded.step_index,
	retry_of=excluded.retry_of, attempt=excluded.attempt, extra_vars=excluded.extra_vars,
	outputs=excluded.outputs, claimed_by=excluded.claimed_by, claimed_at=excluded.claimed_at,
	cancel_requested=excluded.cancel_requested, credential_ids=excluded.credential_ids,
	project_id=excluded.project_id, commit_sha=excluded.commit_sha,
	inventory_id=excluded.inventory_id, queue=excluded.queue, tool=excluded.tool,
	command=excluded.command, dry_run=excluded.dry_run`
	_, err := s.db.ExecContext(ctx, q,
		r.ID, r.Playbook, r.Inventory, string(r.Status), nullInt(r.ExitCode), r.Error,
		formatTime(r.CreatedAt), nullTime(r.StartedAt), nullTime(r.EndedAt),
		nullString(r.ParentID), nullInt(r.ShardIndex), nullInt(r.ShardCount), r.Limit,
		r.Kind, r.StepName, nullInt(r.StepIndex), nullString(r.RetryOf), r.Attempt,
		jsonMap(r.ExtraVars), jsonMap(r.Outputs), r.ClaimedBy, nullTime(r.ClaimedAt),
		boolToInt(r.CancelRequested), joinIDs(r.CredentialIDs), r.ProjectID, r.CommitSHA,
		r.InventoryID, r.Queue, r.Tool, r.Command, boolToInt(r.DryRun),
	)
	if err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

// Get returns the run with the given id, or run.ErrNotFound.
func (s *store) Get(ctx context.Context, id string) (*run.Run, error) {
	const q = "SELECT " + runColumns + " FROM runs WHERE id=$1"
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

// ListPage returns a page of top-level runs newest first, capped at limit and skipping offset.
func (s *store) ListPage(ctx context.Context, query string, limit, offset int) ([]*run.Run, error) {
	q := "SELECT " + runColumns + " FROM runs WHERE parent_id IS NULL"
	clause, args := runSearchClause(query)
	if clause != "" {
		q += " AND " + clause
	}
	q += " ORDER BY created_at DESC, id DESC"
	if limit > 0 {
		// The search term, when present, takes $1, so the page bounds follow it.
		if len(args) == 0 {
			q += " LIMIT $1 OFFSET $2"
		} else {
			q += " LIMIT $2 OFFSET $3"
		}
		args = append(args, limit, offset)
	}
	return s.queryRuns(ctx, "list runs", q, args...)
}

// runSearchColumns are the run columns the runs-view search matches. They mirror the fields the run
// package's matchesQuery searches in the in-memory store.
var runSearchColumns = []string{"id", "playbook", "command", "tool", "status", "step_name", "inventory"}

// runSearchClause builds a case-insensitive LIKE clause over runSearchColumns, reusing the single
// $1 placeholder for every column, and the one arg to bind. It returns empty when the term is blank.
func runSearchClause(query string) (string, []any) {
	term := strings.ToLower(strings.TrimSpace(query))
	if term == "" {
		return "", nil
	}
	parts := make([]string, len(runSearchColumns))
	for i, col := range runSearchColumns {
		parts[i] = "lower(" + col + ") LIKE $1"
	}
	return "(" + strings.Join(parts, " OR ") + ")", []any{"%" + term + "%"}
}

// RunStatusCounts tallies top-level runs by status with a single grouped query.
func (s *store) RunStatusCounts(ctx context.Context) (map[run.Status]int, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT status, COUNT(*) FROM runs WHERE parent_id IS NULL GROUP BY status")
	if err != nil {
		return nil, fmt.Errorf("run status counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[run.Status]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("run status counts: %w", err)
		}
		out[run.Status(status)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run status counts: %w", err)
	}
	return out, nil
}

// Shards returns the shard runs of a parent ordered by shard index.
func (s *store) Shards(ctx context.Context, parentID string) ([]*run.Run, error) {
	const q = "SELECT " + runColumns + " FROM runs WHERE parent_id=$1 ORDER BY shard_index"
	return s.queryRuns(ctx, "list shards", q, parentID)
}

// Steps returns the pipeline step runs of a parent ordered by step index then attempt.
func (s *store) Steps(ctx context.Context, parentID string) ([]*run.Run, error) {
	const q = "SELECT " + runColumns + " FROM runs WHERE parent_id=$1 ORDER BY step_index, attempt"
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

	if _, err := tx.ExecContext(ctx, "DELETE FROM run_host_summary WHERE run_id=$1", runID); err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO run_host_summary
	(run_id, host, ok, changed, failures, unreachable, skipped, worst, duration_seconds, ran_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`)
	if err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, hs := range summaries {
		if _, err := stmt.ExecContext(ctx, runID, hs.Host, hs.OK, hs.Changed, hs.Failures,
			hs.Unreachable, hs.Skipped, hs.Worst, hs.DurationSeconds, formatTime(hs.RanAt)); err != nil {
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
	SELECT host, worst, ran_at,
		ROW_NUMBER() OVER (PARTITION BY host ORDER BY ran_at DESC) AS rn
	FROM run_host_summary
), recent AS (
	SELECT host, worst, ran_at, rn,
		CASE WHEN worst IN ('failed', 'unreachable') THEN 1 ELSE 0 END AS bad,
		LAG(CASE WHEN worst IN ('failed', 'unreachable') THEN 1 ELSE 0 END)
			OVER (PARTITION BY host ORDER BY ran_at DESC) AS prev_bad
	FROM ranked
	WHERE rn <= $1
)
SELECT host,
	SUM(bad) AS failures,
	COUNT(*) AS total,
	MAX(CASE WHEN rn = 1 THEN worst END) AS last_outcome,
	MAX(ran_at) AS last_run,
	SUM(CASE WHEN prev_bad IS NOT NULL AND bad != prev_bad THEN 1 ELSE 0 END) AS flips,
	STRING_AGG(worst, ',' ORDER BY rn) AS recent
FROM recent
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
			recent  string
		)
		if err := rows.Scan(&h.Host, &h.Failures, &h.Total, &lastOut, &lastRun, &h.Flips,
			&recent); err != nil {
			return nil, fmt.Errorf("fleet health: %w", err)
		}
		h.LastOutcome = lastOut
		if h.LastRun, err = parseTime(lastRun); err != nil {
			return nil, fmt.Errorf("fleet health: %w", err)
		}
		h.Flaky = h.Flips >= 2
		if recent != "" {
			h.Recent = strings.Split(recent, ",")
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
		ROW_NUMBER() OVER (PARTITION BY host ORDER BY ran_at DESC) AS rn
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

// HostHistory returns a host's most recent per run summaries, newest first, with run ids.
func (s *store) HostHistory(ctx context.Context, host string, limit int) ([]run.HostSummary, error) {
	if limit < 1 {
		limit = 1
	}
	const q = `
SELECT run_id, host, ok, changed, failures, unreachable, skipped, worst, duration_seconds, ran_at
FROM run_host_summary WHERE host = $1 ORDER BY ran_at DESC LIMIT $2`

	rows, err := s.db.QueryContext(ctx, q, host, limit)
	if err != nil {
		return nil, fmt.Errorf("host history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []run.HostSummary
	for rows.Next() {
		var (
			hs    run.HostSummary
			ranAt string
		)
		if err := rows.Scan(&hs.RunID, &hs.Host, &hs.OK, &hs.Changed, &hs.Failures,
			&hs.Unreachable, &hs.Skipped, &hs.Worst, &hs.DurationSeconds, &ranAt); err != nil {
			return nil, fmt.Errorf("host history: %w", err)
		}
		if hs.RanAt, err = parseTime(ranAt); err != nil {
			return nil, fmt.Errorf("host history: %w", err)
		}
		out = append(out, hs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("host history: %w", err)
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
		if _, err := stmt.ExecContext(ctx, runID, ts.Task, ts.Seconds, formatTime(ts.RanAt)); err != nil {
			return fmt.Errorf("save task summary: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save task summary: %w", err)
	}
	return nil
}

// TaskTrends aggregates each task's durations over its most recent window runs.
func (s *store) TaskTrends(ctx context.Context, window int) ([]run.TaskTrend, error) {
	if window < 1 {
		window = 1
	}
	const q = `
WITH ranked AS (
	SELECT task, seconds, ran_at,
		ROW_NUMBER() OVER (PARTITION BY task ORDER BY ran_at DESC) AS rn
	FROM run_task_summary
)
SELECT task,
	COUNT(*) AS runs,
	AVG(seconds) AS avg_seconds,
	MAX(CASE WHEN rn = 1 THEN seconds END) AS last_seconds,
	MAX(ran_at) AS last_run
FROM ranked
WHERE rn <= $1
GROUP BY task
ORDER BY task`

	rows, err := s.db.QueryContext(ctx, q, window)
	if err != nil {
		return nil, fmt.Errorf("task trends: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []run.TaskTrend
	for rows.Next() {
		var (
			t       run.TaskTrend
			lastRun string
		)
		if err := rows.Scan(&t.Task, &t.Runs, &t.AvgSeconds, &t.LastSeconds, &lastRun); err != nil {
			return nil, fmt.Errorf("task trends: %w", err)
		}
		if t.LastRun, err = parseTime(lastRun); err != nil {
			return nil, fmt.Errorf("task trends: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task trends: %w", err)
	}
	return out, nil
}

// Workers lists executors by the leases they hold, most recently seen first.
func (s *store) Workers(ctx context.Context) ([]run.WorkerInfo, error) {
	const q = `
SELECT claimed_by,
	SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END) AS active,
	MAX(claimed_at) AS last_seen
FROM runs
WHERE claimed_by != '' AND claimed_at IS NOT NULL
GROUP BY claimed_by
ORDER BY last_seen DESC, claimed_by`

	rows, err := s.db.QueryContext(ctx, q)
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
		if err := rows.Scan(&w.Owner, &w.Active, &seen); err != nil {
			return nil, fmt.Errorf("list workers: %w", err)
		}
		if w.LastSeen, err = parseTime(seen); err != nil {
			return nil, fmt.Errorf("list workers: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
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
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO run_logs (run_id, chunk) VALUES ($1, $2)", id, p); err != nil {
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

	stmt, err := tx.PrepareContext(ctx, "INSERT INTO run_events (run_id, data) VALUES ($1, $2)")
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

// exists reports whether a run with id is present.
func (s *store) exists(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM runs WHERE id=$1", id).Scan(&one)
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
		retryOf  sql.NullString
		extra    string
		outputs  string
		claimed  sql.NullString
		cancelI  int
		credIDs  string
		dryRun   int
	)
	if err := s.Scan(&r.ID, &r.Playbook, &r.Inventory, &status, &exit, &r.Error,
		&created, &started, &ended, &parent, &shardIdx, &shardCnt, &r.Limit,
		&r.Kind, &r.StepName, &stepIdx, &retryOf, &r.Attempt, &extra, &outputs,
		&r.ClaimedBy, &claimed, &cancelI, &credIDs, &r.ProjectID, &r.CommitSHA,
		&r.InventoryID, &r.Queue, &r.Tool, &r.Command, &dryRun); err != nil {
		return nil, err
	}
	r.CancelRequested = cancelI != 0
	r.DryRun = dryRun != 0
	r.CredentialIDs = splitIDs(credIDs)
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
	if retryOf.Valid {
		id := retryOf.String
		r.RetryOf = &id
	}
	if r.ExtraVars, err = parseMap(extra); err != nil {
		return nil, err
	}
	if r.Outputs, err = parseMap(outputs); err != nil {
		return nil, err
	}
	if r.ClaimedAt, err = parseNullTime(claimed); err != nil {
		return nil, err
	}
	return &r, nil
}

// joinIDs renders an id list for storage, empty string for none.
func joinIDs(ids []string) string {
	return strings.Join(ids, ",")
}

// splitIDs parses a stored id list, nil for an empty string.
func splitIDs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// boolToInt maps a bool to a database integer.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// jsonMap renders a map as JSON for storage, empty string for an empty map.
func jsonMap(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(data)
}

// parseMap parses a stored JSON map, nil for an empty string.
func parseMap(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("parse stored map: %w", err)
	}
	return m, nil
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

// Claim leases the oldest unclaimed pending top-level plain run to owner and returns it. The row
// is locked with SKIP LOCKED so concurrent workers never claim the same run.
func (s *store) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	placeholders, args := queuePlaceholders(queues, "$")
	q := `
UPDATE runs SET claimed_by=$1, claimed_at=$2
WHERE id = (
	SELECT id FROM runs
	WHERE status='pending' AND claimed_by='' AND kind='' AND queue IN (` + placeholders + `)
	ORDER BY created_at, id LIMIT 1
	FOR UPDATE SKIP LOCKED
)
RETURNING ` + runColumns
	full := append([]any{owner, formatTime(time.Now())}, args...)
	r, err := scanRun(s.db.QueryRowContext(ctx, q, full...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, run.ErrNonePending
	}
	if err != nil {
		return nil, fmt.Errorf("claim run: %w", err)
	}
	return r, nil
}

// queuePlaceholders builds a comma separated placeholder list and the matching queue args,
// numbered from $3 since owner and claim time take $1 and $2.
func queuePlaceholders(queues []string, style string) (string, []any) {
	if len(queues) == 0 {
		queues = []string{""}
	}
	parts := make([]string, len(queues))
	args := make([]any, len(queues))
	for i, q := range queues {
		if style == "?" {
			parts[i] = "?"
		} else {
			parts[i] = fmt.Sprintf("$%d", i+3)
		}
		args[i] = q
	}
	return strings.Join(parts, ", "), args
}

// Heartbeat renews owner's lease on a run.
func (s *store) Heartbeat(ctx context.Context, id, owner string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE runs SET claimed_at=$1 WHERE id=$2 AND claimed_by=$3",
		formatTime(time.Now()), id, owner)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if n == 0 {
		return run.ErrNotFound
	}
	return nil
}

// ReclaimStale requeues stale claimed pending runs and interrupts stale running runs.
func (s *store) ReclaimStale(ctx context.Context, cutoff time.Time) (int, error) {
	cut := formatTime(cutoff)
	res, err := s.db.ExecContext(ctx, `
UPDATE runs SET claimed_by='', claimed_at=NULL
WHERE status='pending' AND claimed_by!='' AND claimed_at < $1`, cut)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	requeued, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	res, err = s.db.ExecContext(ctx, `
UPDATE runs SET status='interrupted', ended_at=$1, error='interrupted: executor lease expired'
WHERE status='running' AND claimed_by!='' AND claimed_at < $2`, formatTime(time.Now()), cut)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	interrupted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	return int(requeued + interrupted), nil
}

// RequestCancel marks the run so whichever process holds it stops it.
func (s *store) RequestCancel(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE runs SET cancel_requested=1 WHERE id=$1", id)
	if err != nil {
		return fmt.Errorf("request cancel: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("request cancel: %w", err)
	}
	if n == 0 {
		return run.ErrNotFound
	}
	return nil
}
