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

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlutil"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
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
	steps         TEXT NOT NULL DEFAULT '',
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
	dry_run       INTEGER NOT NULL DEFAULT 0,
	proposed_from TEXT NOT NULL DEFAULT '',
	intent        TEXT NOT NULL DEFAULT '',
	image         TEXT NOT NULL DEFAULT '',
	pull_credential_id TEXT NOT NULL DEFAULT '',
	idempotency_key TEXT NOT NULL DEFAULT '',
	timeout       INTEGER NOT NULL DEFAULT 0,
	notifications TEXT NOT NULL DEFAULT '',
	source        TEXT NOT NULL DEFAULT '',
	source_id     TEXT NOT NULL DEFAULT '',
	actor         TEXT NOT NULL DEFAULT '',
	rerun_of      TEXT NOT NULL DEFAULT '',
	labels        TEXT NOT NULL DEFAULT '',
	warning       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_created_at ON runs(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_runs_parent ON runs(parent_id, shard_index);
-- CREATE TABLE IF NOT EXISTS is a no-op on a database created before these columns, so they are
-- also added on the fly and only then indexed, keeping run submission dedup and timeouts working
-- after an upgrade. Every statement is idempotent, so a fresh database and an existing one converge.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS idempotency_key TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS timeout INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS notifications TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS steps TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS source_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS actor TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS rerun_of TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS labels TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS warning TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_idempotency_key ON runs(idempotency_key) WHERE idempotency_key <> '';
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
CREATE TABLE IF NOT EXISTS host_facts (
	host        TEXT PRIMARY KEY,
	run_id      TEXT NOT NULL,
	facts       TEXT NOT NULL,
	gathered_at TEXT NOT NULL
);
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
-- The profile columns are added rather than declared above, so a database created before them is
-- migrated by the same statement that creates a fresh one. Empty is the default everywhere, so an
-- account made before this carries no profile and stays valid.
ALTER TABLE users ADD COLUMN IF NOT EXISTS full_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS email TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS phone TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS title TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS links TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS notes TEXT NOT NULL DEFAULT '';
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
	org_id        TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL
);
ALTER TABLE projects ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
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
	dry_run        INTEGER NOT NULL DEFAULT 0,
	image          TEXT NOT NULL DEFAULT '',
	pull_credential_id TEXT NOT NULL DEFAULT '',
	org_id         TEXT NOT NULL DEFAULT '',
	notifications  TEXT NOT NULL DEFAULT '',
	selectable_credential_ids TEXT NOT NULL DEFAULT '',
	timeout        INTEGER NOT NULL DEFAULT 0
);
ALTER TABLE templates ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS notifications TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS selectable_credential_ids TEXT NOT NULL DEFAULT '';
-- Zero leaves a launch on the server default, so a template made before this column is unchanged.
ALTER TABLE templates ADD COLUMN IF NOT EXISTS timeout INTEGER NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS inventory_sources (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL DEFAULT '',
	source        TEXT NOT NULL,
	credential_id TEXT NOT NULL DEFAULT '',
	project_id    TEXT NOT NULL DEFAULT '',
	inventory_id  TEXT NOT NULL DEFAULT '',
	synced_at     TEXT,
	last_error    TEXT NOT NULL DEFAULT '',
	update_on_launch INTEGER NOT NULL DEFAULT 0,
	sync_interval_seconds INTEGER NOT NULL DEFAULT 0,
	created_at    TEXT NOT NULL
);
ALTER TABLE inventory_sources ADD COLUMN IF NOT EXISTS update_on_launch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE inventory_sources ADD COLUMN IF NOT EXISTS sync_interval_seconds INTEGER NOT NULL DEFAULT 0;
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
	id        TEXT PRIMARY KEY,
	at        TEXT NOT NULL,
	actor     TEXT NOT NULL DEFAULT '',
	method    TEXT NOT NULL,
	path      TEXT NOT NULL,
	seq       BIGINT NOT NULL DEFAULT 0,
	prev_hash TEXT NOT NULL DEFAULT '',
	hash      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_entries(at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_seq ON audit_entries(seq);
CREATE TABLE IF NOT EXISTS policies (
	id               TEXT PRIMARY KEY,
	name             TEXT NOT NULL DEFAULT '',
	tool             TEXT NOT NULL DEFAULT '',
	command_contains TEXT NOT NULL DEFAULT '',
	inventory_id     TEXT NOT NULL DEFAULT '',
	exclude_dry_run  INTEGER NOT NULL DEFAULT 0,
	max_destroy      INTEGER NOT NULL DEFAULT -1,
	created_at       TEXT NOT NULL
);
ALTER TABLE policies ADD COLUMN IF NOT EXISTS max_destroy INTEGER NOT NULL DEFAULT -1;
CREATE TABLE IF NOT EXISTS inventories (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL DEFAULT '',
	content        TEXT NOT NULL,
	credential_ids TEXT NOT NULL DEFAULT '',
	content_source TEXT NOT NULL DEFAULT '',
	content_config TEXT NOT NULL DEFAULT '',
	queue          TEXT NOT NULL DEFAULT '',
	org_id         TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL
);
ALTER TABLE inventories ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS credentials (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	kind       TEXT NOT NULL,
	secret     TEXT NOT NULL,
	created_at TEXT NOT NULL,
	source     TEXT NOT NULL DEFAULT '',
	org_id     TEXT NOT NULL DEFAULT ''
);
ALTER TABLE credentials ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
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
CREATE TABLE IF NOT EXISTS orgs (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS org_members (
	org_id  TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL,
	role    TEXT NOT NULL DEFAULT 'member',
	PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_org_members_user ON org_members(user_id);
CREATE TABLE IF NOT EXISTS grants (
	id         TEXT PRIMARY KEY,
	subject    TEXT NOT NULL,
	object     TEXT NOT NULL,
	access     TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_grants_object ON grants(object);
CREATE INDEX IF NOT EXISTS idx_runs_pending_claim ON runs(queue, created_at, id)
	WHERE status='pending' AND claimed_by='' AND kind='';
CREATE INDEX IF NOT EXISTS idx_runs_status_parent ON runs(status, parent_id);
CREATE INDEX IF NOT EXISTS idx_runs_leased ON runs(claimed_at) WHERE claimed_by<>'';
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
	// orgs is the organization store.
	orgs *orgStore
	// grants is the per-object access grant store.
	grants *grantStore
	// policies is the approval policy store.
	policies *policyStore
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
	// Cap the pool so a burst of API reads and SSE streams cannot exhaust the server's
	// max_connections, and recycle connections so a load balancer or pooler can rebalance.
	db.SetMaxOpenConns(24)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
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
		orgs:        &orgStore{db: db},
		grants:      &grantStore{db: db},
		policies:    &policyStore{db: db}}, nil
}

// pgUniqueViolation is the PostgreSQL SQLSTATE code for a unique constraint or index violation.
const pgUniqueViolation = "23505"

// isKeyConflict reports whether a keyed insert failed because another run already holds the
// idempotency key. A runs insert carrying a key can only trip the idempotency-key unique index, its
// primary-key conflict being absorbed by ON CONFLICT(id), so a unique violation on one is that race
// and maps to run.ErrDuplicateKey.
func isKeyConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
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

// Policies returns the approval policy store.
func (d *DB) Policies() policy.Store {
	return d.policies
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

// Orgs returns the organization store.
func (d *DB) Orgs() org.Store {
	return d.orgs
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
	retry_of, attempt, steps, extra_vars, outputs, claimed_by, claimed_at, cancel_requested,
	credential_ids, project_id, commit_sha, inventory_id, queue, tool, command, dry_run,
	proposed_from, intent, image, pull_credential_id, idempotency_key, timeout, notifications,
	source, source_id, actor, rerun_of, labels, warning`

// Save inserts or replaces the run identified by r.ID. The cancel flag merges with GREATEST so a
// replace from a stale snapshot cannot erase a cancel another process just requested.
func (s *store) Save(ctx context.Context, r *run.Run) error {
	const q = `
INSERT INTO runs
	(id, playbook, inventory, status, exit_code, error, created_at, started_at, ended_at,
	 parent_id, shard_index, shard_count, limit_pattern, kind, step_name, step_index, retry_of,
	 attempt, steps, extra_vars, outputs, claimed_by, claimed_at, cancel_requested, credential_ids,
	 project_id, commit_sha, inventory_id, queue, tool, command, dry_run, proposed_from, intent,
	 image, pull_credential_id, idempotency_key, timeout, notifications,
	 source, source_id, actor, rerun_of, labels, warning)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
	$21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38,
	$39, $40, $41, $42, $43, $44, $45)
ON CONFLICT(id) DO UPDATE SET
	playbook=excluded.playbook, inventory=excluded.inventory, status=excluded.status,
	exit_code=excluded.exit_code, error=excluded.error, created_at=excluded.created_at,
	started_at=excluded.started_at, ended_at=excluded.ended_at,
	parent_id=excluded.parent_id, shard_index=excluded.shard_index,
	shard_count=excluded.shard_count, limit_pattern=excluded.limit_pattern,
	kind=excluded.kind, step_name=excluded.step_name, step_index=excluded.step_index,
	retry_of=excluded.retry_of, attempt=excluded.attempt, steps=excluded.steps,
	extra_vars=excluded.extra_vars,
	outputs=excluded.outputs, claimed_by=excluded.claimed_by, claimed_at=excluded.claimed_at,
	cancel_requested=GREATEST(runs.cancel_requested, excluded.cancel_requested),
	credential_ids=excluded.credential_ids,
	project_id=excluded.project_id, commit_sha=excluded.commit_sha,
	inventory_id=excluded.inventory_id, queue=excluded.queue, tool=excluded.tool,
	command=excluded.command, dry_run=excluded.dry_run, proposed_from=excluded.proposed_from,
	intent=excluded.intent, image=excluded.image, pull_credential_id=excluded.pull_credential_id,
	idempotency_key=excluded.idempotency_key, timeout=excluded.timeout,
	notifications=excluded.notifications, source=excluded.source, source_id=excluded.source_id,
	actor=excluded.actor, rerun_of=excluded.rerun_of, labels=excluded.labels,
	warning=excluded.warning`
	_, err := s.db.ExecContext(ctx, q,
		r.ID, r.Playbook, r.Inventory, string(r.Status), sqlutil.NullInt(r.ExitCode), r.Error,
		sqlutil.FormatTime(r.CreatedAt), sqlutil.NullTime(r.StartedAt), sqlutil.NullTime(r.EndedAt),
		sqlutil.NullString(r.ParentID), sqlutil.NullInt(r.ShardIndex), sqlutil.NullInt(r.ShardCount), r.Limit,
		r.Kind, r.StepName, sqlutil.NullInt(r.StepIndex), sqlutil.NullString(r.RetryOf), r.Attempt,
		marshalSteps(r.Steps), sqlutil.JSONMap(r.ExtraVars), sqlutil.JSONMap(r.Outputs), r.ClaimedBy, sqlutil.NullTime(r.ClaimedAt),
		sqlutil.BoolToInt(r.CancelRequested), sqlutil.JoinIDs(r.CredentialIDs), r.ProjectID, r.CommitSHA,
		r.InventoryID, r.Queue, r.Tool, r.Command, sqlutil.BoolToInt(r.DryRun), r.ProposedFrom, r.Intent,
		r.Image, r.PullCredentialID, r.IdempotencyKey, r.Timeout, marshalNotifications(r.Notifications),
		r.Source, r.SourceID, r.Actor, r.RerunOf, marshalLabels(r.Labels), r.Warning,
	)
	if err != nil {
		if r.IdempotencyKey != "" && isKeyConflict(err) {
			return run.ErrDuplicateKey
		}
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

// ByIdempotencyKey returns the run that holds key, or run.ErrNotFound. An empty key is never found.
func (s *store) ByIdempotencyKey(ctx context.Context, key string) (*run.Run, error) {
	if key == "" {
		return nil, run.ErrNotFound
	}
	const q = "SELECT " + runColumns + " FROM runs WHERE idempotency_key=$1 LIMIT 1"
	r, err := scanRun(s.db.QueryRowContext(ctx, q, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, run.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("by idempotency key: %w", err)
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
func (s *store) ListPage(ctx context.Context, filter run.ListFilter, limit, offset int) ([]*run.Run, error) {
	q := "SELECT " + runColumns + " FROM runs WHERE parent_id IS NULL"
	// Placeholders are numbered by position in args, so each optional clause binds the next number.
	clause, args := runSearchClause(filter.Query)
	if clause != "" {
		q += " AND " + clause
	}
	if filter.Status != "" {
		args = append(args, filter.Status)
		q += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if filter.Tool != "" {
		// An Ansible run may be stored with an empty tool, its historical form, so normalize in the
		// comparison rather than trusting the column.
		args = append(args, filter.Tool)
		q += fmt.Sprintf(" AND COALESCE(NULLIF(tool, ''), 'ansible') = $%d", len(args))
	}
	// Stored times are RFC 3339 UTC strings, so lexicographic comparison is chronological.
	if !filter.After.IsZero() {
		args = append(args, sqlutil.FormatTime(filter.After))
		q += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if !filter.Before.IsZero() {
		args = append(args, sqlutil.FormatTime(filter.Before))
		q += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	if filter.Source != "" {
		args = append(args, filter.Source)
		q += fmt.Sprintf(" AND source = $%d", len(args))
	}
	if filter.Actor != "" {
		args = append(args, filter.Actor)
		q += fmt.Sprintf(" AND actor = $%d", len(args))
	}
	if filter.SourceID != "" {
		args = append(args, filter.SourceID)
		q += fmt.Sprintf(" AND source_id = $%d", len(args))
	}
	if filter.LabelKey != "" {
		args = append(args, filter.LabelKey, filter.LabelValue)
		q += fmt.Sprintf(" AND NULLIF(labels, '')::jsonb ->> $%d = $%d", len(args)-1, len(args))
	}
	if filter.Host != "" {
		args = append(args, filter.Host)
		q += fmt.Sprintf(" AND EXISTS (SELECT 1 FROM run_host_summary hs WHERE hs.run_id = runs.id AND hs.host = $%d)", len(args))
	}
	order := "DESC"
	if filter.OldestFirst {
		order = "ASC"
	}
	q += " ORDER BY created_at " + order + ", id " + order
	if limit > 0 {
		args = append(args, limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
		args = append(args, offset)
		q += fmt.Sprintf(" OFFSET $%d", len(args))
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
	const q = "SELECT " + runColumns +
		" FROM runs WHERE status NOT IN ('succeeded', 'failed', 'canceled', 'interrupted', 'rejected')"
	return s.queryRuns(ctx, "list non-terminal runs", q)
}

// SaveHostSummary replaces the stored per host summaries for a run.
// summaryFenced reports whether a run's summaries must not be written because the run has reached a
// terminal state, in which case a reclaimed-but-alive worker must not overwrite the final summary a
// healthy finalize already stored. A run with no row is not fenced, since cross-run summary views are
// keyed by run id rather than a stored run.
func summaryFenced(ctx context.Context, q rowQuerier, runID string) (bool, error) {
	var status string
	err := q.QueryRowContext(ctx, "SELECT status FROM runs WHERE id=$1", runID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return run.Status(status).Terminal(), nil
}

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
			hs.Unreachable, hs.Skipped, hs.Worst, hs.DurationSeconds, sqlutil.FormatTime(hs.RanAt)); err != nil {
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
		ROW_NUMBER() OVER (PARTITION BY host ORDER BY ran_at DESC) AS rn
	FROM run_host_summary
), recent AS (
	SELECT host, worst, run_id, ran_at, rn,
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
	STRING_AGG(worst, ',' ORDER BY rn) AS recent,
	STRING_AGG(run_id, ',' ORDER BY rn) AS recent_runs
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

// DriftStatus reports each host's most recent drift check, the latest dry run to touch it, worst
// drift first. It joins host summaries to their run so only dry runs count, where a changed result
// means a task would change, so the host has diverged from the desired state.
func (s *store) DriftStatus(ctx context.Context) ([]run.HostDrift, error) {
	const q = `
WITH checks AS (
	SELECT hs.host, hs.changed, hs.run_id, hs.ran_at,
		ROW_NUMBER() OVER (PARTITION BY hs.host ORDER BY hs.ran_at DESC) AS rn
	FROM run_host_summary hs
	JOIN runs r ON r.id = hs.run_id
	WHERE r.dry_run = 1
)
SELECT host, changed, run_id, ran_at FROM checks WHERE rn = 1 ORDER BY changed DESC, host`

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
		if hs.RanAt, err = sqlutil.ParseTime(ranAt); err != nil {
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
		ROW_NUMBER() OVER (PARTITION BY task ORDER BY ran_at DESC) AS rn
	FROM run_task_summary
)
SELECT task, seconds, ran_at FROM ranked WHERE rn <= $1 ORDER BY task, ran_at`

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
ORDER BY last_seen DESC, claimed_by`

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

// AppendLog appends raw output bytes to the run's log. Returns run.ErrNotFound if absent. The
// insert-select folds the missing-run check into the write so the per-chunk output path costs one
// statement instead of two.
// pgNowText renders the database server's current time in the same UTC RFC 3339 text the Go side
// writes, so a lease stamped in SQL is indistinguishable from one stamped by a store method. Leases
// have to come from the database clock rather than a worker's: with several nodes writing, a worker
// whose clock runs behind would otherwise stamp leases the janitor reads as already expired. The
// fractional second is padded to nine digits so it is the same width as a stamp written by the Go
// side, which keeps text ordering in step with chronological ordering across both writers. The
// database clock is microsecond precision, so the three padded digits are always zero and the value
// is not claiming precision it does not have.
const pgNowText = `(to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US') || '000Z')`

// terminalRun is the SQL predicate for a run that has finished and may be purged. It mirrors
// run.Status.Terminal(). It is stated as the set of terminal statuses rather than as "not pending or
// running", which silently treated pending_approval as finished and deleted runs that were waiting
// for an approver.
const terminalRun = "status IN ('succeeded', 'failed', 'canceled', 'interrupted', 'rejected')"

// nonTerminalRun is the SQL predicate for a run that still accepts auxiliary writes. It mirrors
// run.Status.Terminal, and fences a terminal run so a reclaimed-but-alive worker cannot append logs or
// events to a run that has already ended.
const nonTerminalRun = "status NOT IN ('succeeded', 'failed', 'canceled', 'interrupted', 'rejected')"

func (s *store) AppendLog(ctx context.Context, id string, p []byte) error {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO run_logs (run_id, chunk) SELECT $1, $2 WHERE EXISTS (SELECT 1 FROM runs WHERE id=$1 AND "+nonTerminalRun+")",
		id, p)
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
		notifs   string
		labels   string
		steps    string
	)
	if err := s.Scan(&r.ID, &r.Playbook, &r.Inventory, &status, &exit, &r.Error,
		&created, &started, &ended, &parent, &shardIdx, &shardCnt, &r.Limit,
		&r.Kind, &r.StepName, &stepIdx, &retryOf, &r.Attempt, &steps, &extra, &outputs,
		&r.ClaimedBy, &claimed, &cancelI, &credIDs, &r.ProjectID, &r.CommitSHA,
		&r.InventoryID, &r.Queue, &r.Tool, &r.Command, &dryRun, &r.ProposedFrom, &r.Intent,
		&r.Image, &r.PullCredentialID, &r.IdempotencyKey, &r.Timeout, &notifs,
		&r.Source, &r.SourceID, &r.Actor, &r.RerunOf, &labels, &r.Warning); err != nil {
		return nil, err
	}
	r.CancelRequested = cancelI != 0
	r.DryRun = dryRun != 0
	r.CredentialIDs = sqlutil.SplitIDs(credIDs)
	r.Status = run.Status(status)
	if exit.Valid {
		v := int(exit.Int64)
		r.ExitCode = &v
	}
	t, err := sqlutil.ParseTime(created)
	if err != nil {
		return nil, err
	}
	r.CreatedAt = t
	if r.StartedAt, err = sqlutil.ParseNullTime(started); err != nil {
		return nil, err
	}
	if r.EndedAt, err = sqlutil.ParseNullTime(ended); err != nil {
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
	if r.ExtraVars, err = sqlutil.ParseMap(extra); err != nil {
		return nil, err
	}
	if r.Outputs, err = sqlutil.ParseMap(outputs); err != nil {
		return nil, err
	}
	if r.Labels, err = parseLabels(labels); err != nil {
		return nil, err
	}
	if r.Steps, err = parseSteps(steps); err != nil {
		return nil, err
	}
	if r.ClaimedAt, err = sqlutil.ParseNullTime(claimed); err != nil {
		return nil, err
	}
	r.Notifications = parseNotifications(notifs)
	return &r, nil
}

// marshalLabels renders run labels as JSON for storage, empty for none.
func marshalLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	b, err := json.Marshal(labels)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseLabels decodes stored run labels, tolerating the empty legacy form.
func parseLabels(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("parse labels: %w", err)
	}
	return out, nil
}

// marshalSteps encodes a pipeline's step graph for storage, returning empty for no steps so an
// ordinary run stores nothing rather than a JSON null.
func marshalSteps(steps []run.PipelineStep) string {
	if len(steps) == 0 {
		return ""
	}
	b, err := json.Marshal(steps)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseSteps decodes a stored step graph. An empty column means the run is not a pipeline parent.
func parseSteps(s string) ([]run.PipelineStep, error) {
	if s == "" {
		return nil, nil
	}
	var steps []run.PipelineStep
	if err := json.Unmarshal([]byte(s), &steps); err != nil {
		return nil, fmt.Errorf("parse steps: %w", err)
	}
	return steps, nil
}

// marshalNotifications encodes per-run notification targets for storage, empty for none.
func marshalNotifications(targets []run.NotifyTarget) string {
	if len(targets) == 0 {
		return ""
	}
	b, err := json.Marshal(targets)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseNotifications decodes stored notification targets, nil for an empty or invalid value.
func parseNotifications(s string) []run.NotifyTarget {
	if s == "" {
		return nil
	}
	var targets []run.NotifyTarget
	if err := json.Unmarshal([]byte(s), &targets); err != nil {
		return nil
	}
	return targets
}

// Claim leases the oldest unclaimed pending top-level plain run to owner and returns it. The row
// is locked with SKIP LOCKED so concurrent workers never claim the same run. A run whose cancel
// was requested while it waited is skipped; the cancel handler terminalizes it.
func (s *store) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	placeholders, args := sqlutil.QueuePlaceholders(queues, "$", 2)
	// The lease is stamped from the database clock, not this worker's, so the janitor on another node
	// ages it against the clock that wrote it.
	q := `
UPDATE runs SET claimed_by=$1, claimed_at=` + pgNowText + `
WHERE id = (
	SELECT id FROM runs
	WHERE status='pending' AND claimed_by='' AND kind='' AND cancel_requested=0
		AND queue IN (` + placeholders + `)
	ORDER BY created_at, id LIMIT 1
	FOR UPDATE SKIP LOCKED
)
RETURNING ` + runColumns
	full := append([]any{owner}, args...)
	r, err := scanRun(s.db.QueryRowContext(ctx, q, full...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, run.ErrNonePending
	}
	if err != nil {
		return nil, fmt.Errorf("claim run: %w", err)
	}
	return r, nil
}

// Heartbeat renews owner's lease on a run.
func (s *store) Heartbeat(ctx context.Context, id, owner string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE runs SET claimed_at=$1 WHERE id=$2 AND claimed_by=$3",
		sqlutil.FormatTime(time.Now()), id, owner)
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
//
// Every clock here is the database server's. A lease is stamped from the database clock by Claim and
// Heartbeat, so the sweep has to age it against that same clock: deriving the cutoff from the calling
// node's clock instead would interrupt perfectly healthy runs whenever a worker's clock ran behind
// the control node's by more than the lease age.
//
// The comparison casts the stored text to a timestamp rather than comparing it as text. Timestamps
// are written in RFC 3339, which trims trailing zeros from the fractional second, so their text
// widths vary and lexicographic order does not always match chronological order. Comparing as text
// would let the sweep interrupt a run whose lease is in fact fresh.
func (s *store) ReclaimStale(ctx context.Context, ttl time.Duration) (int, error) {
	age := ttl.Seconds()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
UPDATE runs SET claimed_by='', claimed_at=NULL
WHERE status='pending' AND claimed_by!=''
  AND claimed_at::timestamptz < now() - make_interval(secs => $1)`, age)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	requeued, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	res, err = tx.ExecContext(ctx, `
UPDATE runs SET status='interrupted', claimed_by='', claimed_at=NULL,
ended_at=`+pgNowText+`, error='interrupted: executor lease expired'
WHERE status='running' AND claimed_by!=''
  AND claimed_at::timestamptz < now() - make_interval(secs => $1)`, age)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	interrupted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	if err := tx.Commit(); err != nil {
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

// CancelPending atomically cancels a run still waiting unclaimed in pending or pending_approval.
func (s *store) CancelPending(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE runs SET status='canceled', ended_at=$1
WHERE id=$2 AND claimed_by='' AND status IN ('pending', 'pending_approval')`,
		sqlutil.FormatTime(time.Now()), id)
	if err != nil {
		return false, fmt.Errorf("cancel pending: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cancel pending: %w", err)
	}
	return n > 0, nil
}

// TransitionStatus atomically moves the run from one status to another, reporting whether it changed.
func (s *store) TransitionStatus(ctx context.Context, id string, from, to run.Status) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE runs SET status=$1 WHERE id=$2 AND status=$3", string(to), id, string(from))
	if err != nil {
		return false, fmt.Errorf("transition status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("transition status: %w", err)
	}
	return n > 0, nil
}
