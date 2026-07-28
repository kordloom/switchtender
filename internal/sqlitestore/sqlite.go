// Package sqlitestore implements run.Store on top of SQLite using the pure Go modernc driver, so
// SwitchTender keeps its single binary promise with no cgo. It is the default backend. A Postgres
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

	sqlite "modernc.org/sqlite"

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

// schema is the full table layout created on open. It uses IF NOT EXISTS, so opening an existing
// database is safe.
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
	dry_run       INTEGER NOT NULL DEFAULT 0,
	proposed_from TEXT NOT NULL DEFAULT '',
	intent        TEXT NOT NULL DEFAULT '',
	image         TEXT NOT NULL DEFAULT '',
	pull_credential_id TEXT NOT NULL DEFAULT '',
	idempotency_key TEXT NOT NULL DEFAULT '',
	timeout INTEGER NOT NULL DEFAULT 0,
	notifications TEXT NOT NULL DEFAULT ''
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
	duration_seconds REAL NOT NULL DEFAULT 0,
	ran_at      TEXT NOT NULL,
	PRIMARY KEY (run_id, host)
);
CREATE INDEX IF NOT EXISTS idx_host_summary_host ON run_host_summary(host, ran_at DESC);
CREATE TABLE IF NOT EXISTS run_task_summary (
	run_id  TEXT NOT NULL,
	task    TEXT NOT NULL,
	seconds REAL NOT NULL,
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
	org_id        TEXT NOT NULL DEFAULT '',
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
	dry_run        INTEGER NOT NULL DEFAULT 0,
	image          TEXT NOT NULL DEFAULT '',
	pull_credential_id TEXT NOT NULL DEFAULT '',
	org_id         TEXT NOT NULL DEFAULT '',
	notifications  TEXT NOT NULL DEFAULT '',
	selectable_credential_ids TEXT NOT NULL DEFAULT ''
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
	update_on_launch INTEGER NOT NULL DEFAULT 0,
	sync_interval_seconds INTEGER NOT NULL DEFAULT 0,
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
	id        TEXT PRIMARY KEY,
	at        TEXT NOT NULL,
	actor     TEXT NOT NULL DEFAULT '',
	method    TEXT NOT NULL,
	path      TEXT NOT NULL,
	seq       INTEGER NOT NULL DEFAULT 0,
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
CREATE TABLE IF NOT EXISTS credentials (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	kind       TEXT NOT NULL,
	secret     TEXT NOT NULL,
	created_at TEXT NOT NULL,
	source     TEXT NOT NULL DEFAULT '',
	org_id     TEXT NOT NULL DEFAULT ''
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
`

// splitDB routes statements over two pools: every write goes to the single serialized write
// connection, while reads run on a read-only pool so a long read never blocks a write, which WAL
// supports. The read pool is opened with query_only set, so a statement misrouted to it fails loudly
// instead of racing the writer into SQLite's read-to-write upgrade deadlock.
type splitDB struct {
	// w is the single write connection.
	w *sql.DB
	// r is the read-only pool. It equals w when the path does not support a second handle, such as an
	// in-memory database.
	r *sql.DB
}

// ExecContext runs a write statement on the write connection.
func (d *splitDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.w.ExecContext(ctx, query, args...)
}

// BeginTx starts a transaction on the write connection.
func (d *splitDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return d.w.BeginTx(ctx, opts)
}

// QueryContext runs a read query on the read pool.
func (d *splitDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.r.QueryContext(ctx, query, args...)
}

// QueryRowContext runs a single-row read query on the read pool.
func (d *splitDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.r.QueryRowContext(ctx, query, args...)
}

// writeQueryRowContext runs a single-row statement on the write connection, for a write that returns
// its row, such as a claim's UPDATE RETURNING.
func (d *splitDB) writeQueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.w.QueryRowContext(ctx, query, args...)
}

// Close closes both pools.
func (d *splitDB) Close() error {
	err := d.w.Close()
	if d.r != d.w {
		err = errors.Join(err, d.r.Close())
	}
	return err
}

// store is a run.Store backed by a SQLite database.
type store struct {
	// db is the open database handle.
	db *splitDB
}

// scanner is the read side shared by sql.Row and sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// DB holds the SQLite backed run and schedule stores sharing one database.
type DB struct {
	// db is the open database handle.
	db *splitDB
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

// Open opens the SQLite database at path, applies the schema, and returns the bundled stores.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection serializes every reader and writer. Concurrent deferred transactions on
	// separate connections deadlock on SQLite's read to write upgrade with an immediate
	// SQLITE_BUSY that busy_timeout never retries, which silently drops writes under load.
	db.SetMaxOpenConns(1)
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		// modernc defaults foreign keys off. Turn them on so declared references, such as a team
		// member pointing at its team, are enforced. The single connection keeps this in effect.
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply %q: %w", p, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateRuns(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateSources(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migratePolicies(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateProjects(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateTemplates(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateInventories(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateCredentials(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := ensureRunIndexes(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	reader, err := openReadPool(path)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if reader == nil {
		reader = db
	}
	split := &splitDB{w: db, r: reader}
	return &DB{db: split, runs: &store{db: split}, schedules: &scheduleStore{db: split}, tokens: &tokenStore{db: split},
		credentials: &credentialStore{db: split},
		projects:    &projectStore{db: split},
		templates:   &templateStore{db: split},
		users:       &userStore{db: split},
		inventories: &inventoryStore{db: split},
		audits:      &auditStore{db: split},
		invSources:  &invSourceStore{db: split},
		triggers:    &triggerStore{db: split},
		teams:       &teamStore{db: split},
		orgs:        &orgStore{db: split},
		grants:      &grantStore{db: split},
		policies:    &policyStore{db: split}}, nil
}

// readPoolConns bounds the read-only pool. WAL readers are cheap, and a handful is enough to keep the
// UI, API listings, and log streams off the write path without holding many file handles.
const readPoolConns = 4

// openReadPool opens the read-only connection pool for path, which WAL supports alongside the single
// writer. Each pooled connection sets query_only through the DSN, so a statement misrouted to the pool
// fails instead of racing the writer. It returns nil for a path that cannot carry a second handle,
// such as an in-memory database or a DSN with its own options, and the caller then routes reads to the
// write connection, preserving the single-connection behavior.
func openReadPool(path string) (*sql.DB, error) {
	if strings.Contains(path, ":memory:") || strings.Contains(path, "?") ||
		strings.HasPrefix(path, "file:") {
		return nil, nil
	}
	dsn := "file:" + path + "?_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite read pool: %w", err)
	}
	r.SetMaxOpenConns(readPoolConns)
	if err := r.Ping(); err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("open sqlite read pool: %w", err)
	}
	return r, nil
}

// migrateRuns brings an existing runs table up to the current shape. CREATE TABLE IF NOT EXISTS is a
// no-op on a database that predates a column, so the idempotency key is added here and only then
// indexed, keeping databases created before this column usable. Adding a column that already exists
// is the ordinary case for a current database and is treated as success.
func migrateRuns(db *sql.DB) error {
	if _, err := db.Exec(
		"ALTER TABLE runs ADD COLUMN idempotency_key TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add idempotency_key column: %w", err)
	}
	if _, err := db.Exec(
		"ALTER TABLE runs ADD COLUMN timeout INTEGER NOT NULL DEFAULT 0"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add timeout column: %w", err)
	}
	if _, err := db.Exec(
		"ALTER TABLE runs ADD COLUMN notifications TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add notifications column: %w", err)
	}
	if _, err := db.Exec(
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_idempotency_key " +
			"ON runs(idempotency_key) WHERE idempotency_key <> ''"); err != nil {
		return fmt.Errorf("index idempotency_key: %w", err)
	}
	return nil
}

// ensureRunIndexes creates the hot-path run indexes after the column migrations so the columns
// they cover exist even on a database created before those columns were added. The claim index
// keeps the executor poll from walking the whole table oldest-first, the status index keeps the
// summary counts off the fat run rows, and the lease index bounds the janitor sweep and the
// worker listing as history grows.
func ensureRunIndexes(db *sql.DB) error {
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_runs_pending_claim ON runs(queue, created_at, id)
			WHERE status='pending' AND claimed_by='' AND kind=''`,
		"CREATE INDEX IF NOT EXISTS idx_runs_status_parent ON runs(status, parent_id)",
		"CREATE INDEX IF NOT EXISTS idx_runs_leased ON runs(claimed_at) WHERE claimed_by<>''",
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("create run index: %w", err)
		}
	}
	return nil
}

// migrateSources adds the dynamic inventory source config columns to a database created before them.
// Adding a column that already exists is the ordinary case for a current database and is treated as
// success.
func migrateSources(db *sql.DB) error {
	for _, stmt := range []string{
		"ALTER TABLE inventory_sources ADD COLUMN update_on_launch INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE inventory_sources ADD COLUMN sync_interval_seconds INTEGER NOT NULL DEFAULT 0",
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate inventory_sources: %w", err)
		}
	}
	return nil
}

// migratePolicies adds the plan-content threshold column to a policies table created before it. The
// default of -1 disables the plan-content check, so existing policies keep their blanket behavior
// rather than starting to hold on any destroy. Adding a column that already exists is the ordinary
// case for a current database and is treated as success.
func migratePolicies(db *sql.DB) error {
	if _, err := db.Exec(
		"ALTER TABLE policies ADD COLUMN max_destroy INTEGER NOT NULL DEFAULT -1"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrate policies: %w", err)
	}
	return nil
}

// migrateProjects adds the owning-organization column to a projects table created before object
// tenancy. Empty is the unowned default, so a project made before this column stays global. Adding a
// column that already exists is the ordinary case for a current database and is treated as success.
func migrateProjects(db *sql.DB) error {
	if _, err := db.Exec(
		"ALTER TABLE projects ADD COLUMN org_id TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrate projects: %w", err)
	}
	return nil
}

// migrateTemplates adds the owning-organization column to a templates table created before object
// tenancy. Empty is the unowned default, so a template made before this column stays global. Adding a
// column that already exists is the ordinary case for a current database and is treated as success.
func migrateTemplates(db *sql.DB) error {
	for _, stmt := range []string{
		"ALTER TABLE templates ADD COLUMN org_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE templates ADD COLUMN notifications TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE templates ADD COLUMN selectable_credential_ids TEXT NOT NULL DEFAULT ''",
	} {
		if _, err := db.Exec(stmt); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate templates: %w", err)
		}
	}
	return nil
}

// migrateInventories adds the owning-organization column to an inventories table created before object
// tenancy. Empty is the unowned default, so an inventory made before this column stays global. Adding
// a column that already exists is the ordinary case for a current database and is treated as success.
func migrateInventories(db *sql.DB) error {
	if _, err := db.Exec(
		"ALTER TABLE inventories ADD COLUMN org_id TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrate inventories: %w", err)
	}
	return nil
}

// migrateCredentials adds the owning-organization column to a credentials table created before object
// tenancy. Empty is the unowned default, so a credential made before this column stays global. Adding
// a column that already exists is the ordinary case for a current database and is treated as success.
func migrateCredentials(db *sql.DB) error {
	if _, err := db.Exec(
		"ALTER TABLE credentials ADD COLUMN org_id TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrate credentials: %w", err)
	}
	return nil
}

// sqliteConstraint is the primary SQLite result code shared by every constraint violation. The
// extended unique code carries it in its low byte, so masking to it matches a unique violation
// regardless of whether the driver reports the primary or extended code.
const sqliteConstraint = 19

// isKeyConflict reports whether a keyed insert failed because another run already holds the
// idempotency key. A runs insert carrying a key can only trip the idempotency-key unique index, its
// primary-key conflict being absorbed by ON CONFLICT(id), so any constraint violation on one is that
// race and maps to run.ErrDuplicateKey.
func isKeyConflict(err error) bool {
	var serr *sqlite.Error
	return errors.As(err, &serr) && serr.Code()&0xFF == sqliteConstraint
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
	retry_of, attempt, extra_vars, outputs, claimed_by, claimed_at, cancel_requested,
	credential_ids, project_id, commit_sha, inventory_id, queue, tool, command, dry_run,
	proposed_from, intent, image, pull_credential_id, idempotency_key, timeout, notifications`

// Save inserts or replaces the run identified by r.ID. The cancel flag merges with MAX so a
// replace from a stale snapshot cannot erase a cancel another process just requested.
func (s *store) Save(ctx context.Context, r *run.Run) error {
	const q = `
INSERT INTO runs
	(id, playbook, inventory, status, exit_code, error, created_at, started_at, ended_at,
	 parent_id, shard_index, shard_count, limit_pattern, kind, step_name, step_index, retry_of,
	 attempt, extra_vars, outputs, claimed_by, claimed_at, cancel_requested, credential_ids,
	 project_id, commit_sha, inventory_id, queue, tool, command, dry_run, proposed_from, intent,
	 image, pull_credential_id, idempotency_key, timeout, notifications)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	playbook=excluded.playbook, inventory=excluded.inventory, status=excluded.status,
	exit_code=excluded.exit_code, error=excluded.error, created_at=excluded.created_at,
	started_at=excluded.started_at, ended_at=excluded.ended_at,
	parent_id=excluded.parent_id, shard_index=excluded.shard_index,
	shard_count=excluded.shard_count, limit_pattern=excluded.limit_pattern,
	kind=excluded.kind, step_name=excluded.step_name, step_index=excluded.step_index,
	retry_of=excluded.retry_of, attempt=excluded.attempt, extra_vars=excluded.extra_vars,
	outputs=excluded.outputs, claimed_by=excluded.claimed_by, claimed_at=excluded.claimed_at,
	cancel_requested=MAX(runs.cancel_requested, excluded.cancel_requested),
	credential_ids=excluded.credential_ids,
	project_id=excluded.project_id, commit_sha=excluded.commit_sha,
	inventory_id=excluded.inventory_id, queue=excluded.queue, tool=excluded.tool,
	command=excluded.command, dry_run=excluded.dry_run, proposed_from=excluded.proposed_from,
	intent=excluded.intent, image=excluded.image, pull_credential_id=excluded.pull_credential_id,
	idempotency_key=excluded.idempotency_key, timeout=excluded.timeout,
	notifications=excluded.notifications`
	_, err := s.db.ExecContext(ctx, q,
		r.ID, r.Playbook, r.Inventory, string(r.Status), sqlutil.NullInt(r.ExitCode), r.Error,
		sqlutil.FormatTime(r.CreatedAt), sqlutil.NullTime(r.StartedAt), sqlutil.NullTime(r.EndedAt),
		sqlutil.NullString(r.ParentID), sqlutil.NullInt(r.ShardIndex), sqlutil.NullInt(r.ShardCount), r.Limit,
		r.Kind, r.StepName, sqlutil.NullInt(r.StepIndex), sqlutil.NullString(r.RetryOf), r.Attempt,
		sqlutil.JSONMap(r.ExtraVars), sqlutil.JSONMap(r.Outputs), r.ClaimedBy, sqlutil.NullTime(r.ClaimedAt),
		sqlutil.BoolToInt(r.CancelRequested), sqlutil.JoinIDs(r.CredentialIDs), r.ProjectID, r.CommitSHA,
		r.InventoryID, r.Queue, r.Tool, r.Command, sqlutil.BoolToInt(r.DryRun), r.ProposedFrom, r.Intent,
		r.Image, r.PullCredentialID, r.IdempotencyKey, r.Timeout, marshalNotifications(r.Notifications),
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

// ByIdempotencyKey returns the run that holds key, or run.ErrNotFound. An empty key is never found.
func (s *store) ByIdempotencyKey(ctx context.Context, key string) (*run.Run, error) {
	if key == "" {
		return nil, run.ErrNotFound
	}
	const q = "SELECT " + runColumns + " FROM runs WHERE idempotency_key=? LIMIT 1"
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
	clause, args := runSearchClause(filter.Query)
	if clause != "" {
		q += " AND " + clause
	}
	if filter.Status != "" {
		q += " AND status = ?"
		args = append(args, filter.Status)
	}
	if filter.Tool != "" {
		// An Ansible run may be stored with an empty tool, its historical form, so normalize in the
		// comparison rather than trusting the column.
		q += " AND COALESCE(NULLIF(tool, ''), 'ansible') = ?"
		args = append(args, filter.Tool)
	}
	order := "DESC"
	if filter.OldestFirst {
		order = "ASC"
	}
	q += " ORDER BY created_at " + order + ", id " + order
	if limit > 0 {
		q += " LIMIT ? OFFSET ?"
		args = append(args, limit, offset)
	}
	return s.queryRuns(ctx, "list runs", q, args...)
}

// runSearchColumns are the run columns the runs-view search matches. They mirror the fields the run
// package's matchesQuery searches in the in-memory store.
var runSearchColumns = []string{"id", "playbook", "command", "tool", "status", "step_name", "inventory"}

// runSearchClause builds a case-insensitive LIKE clause over runSearchColumns and the args to bind,
// or empty when the search term is blank.
func runSearchClause(query string) (string, []any) {
	term := strings.ToLower(strings.TrimSpace(query))
	if term == "" {
		return "", nil
	}
	like := "%" + term + "%"
	parts := make([]string, len(runSearchColumns))
	args := make([]any, len(runSearchColumns))
	for i, col := range runSearchColumns {
		parts[i] = "lower(" + col + ") LIKE ?"
		args[i] = like
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
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
	const q = "SELECT " + runColumns + " FROM runs WHERE parent_id=? ORDER BY shard_index"
	return s.queryRuns(ctx, "list shards", q, parentID)
}

// Steps returns the pipeline step runs of a parent ordered by step index.
func (s *store) Steps(ctx context.Context, parentID string) ([]*run.Run, error) {
	const q = "SELECT " + runColumns + " FROM runs WHERE parent_id=? ORDER BY step_index, attempt"
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
	err := q.QueryRowContext(ctx, "SELECT status FROM runs WHERE id=?", runID).Scan(&status)
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
	if _, err := tx.ExecContext(ctx, "DELETE FROM run_host_summary WHERE run_id=?", runID); err != nil {
		return fmt.Errorf("save host summary: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO run_host_summary
	(run_id, host, ok, changed, failures, unreachable, skipped, worst, duration_seconds, ran_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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
	WHERE rn <= ?
)
SELECT host,
	SUM(bad) AS failures,
	COUNT(*) AS total,
	MAX(CASE WHEN rn = 1 THEN worst END) AS last_outcome,
	MAX(ran_at) AS last_run,
	SUM(CASE WHEN prev_bad IS NOT NULL AND bad != prev_bad THEN 1 ELSE 0 END) AS flips,
	GROUP_CONCAT(worst, ',' ORDER BY rn) AS recent,
	GROUP_CONCAT(run_id, ',' ORDER BY rn) AS recent_runs
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
func (s *store) HostHistory(ctx context.Context, host string, limit int) ([]run.HostSummary, error) {
	if limit < 1 {
		limit = 1
	}
	const q = `
SELECT run_id, host, ok, changed, failures, unreachable, skipped, worst, duration_seconds, ran_at
FROM run_host_summary WHERE host = ? ORDER BY ran_at DESC LIMIT ?`

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
	if _, err := tx.ExecContext(ctx, "DELETE FROM run_task_summary WHERE run_id=?", runID); err != nil {
		return fmt.Errorf("save task summary: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		"INSERT INTO run_task_summary (run_id, task, seconds, ran_at) VALUES (?, ?, ?, ?)")
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
SELECT task, seconds, ran_at FROM ranked WHERE rn <= ? ORDER BY task, ran_at`

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
SELECT host, AVG(duration_seconds) FROM ranked WHERE rn <= ? GROUP BY host`

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
WHERE claimed_by != '' AND claimed_at IS NOT NULL AND claimed_at >= ?
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
// nonTerminalRun is the SQL predicate for a run that still accepts auxiliary writes. It mirrors
// run.Status.Terminal, and fences a terminal run so a reclaimed-but-alive worker cannot append logs or
// events to a run that has already ended.
const nonTerminalRun = "status NOT IN ('succeeded', 'failed', 'canceled', 'interrupted', 'rejected')"

func (s *store) AppendLog(ctx context.Context, id string, p []byte) error {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO run_logs (run_id, chunk) SELECT ?, ? WHERE EXISTS (SELECT 1 FROM runs WHERE id=? AND "+nonTerminalRun+")",
		id, p, id)
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

// LogAfter returns the run's log chunks past afterSeq in order, capped at limit chunks.
func (s *store) LogAfter(ctx context.Context, id string, afterSeq int64, limit int) ([]run.LogChunk, error) {
	ok, err := s.exists(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, run.ErrNotFound
	}
	query := "SELECT seq, chunk FROM run_logs WHERE run_id=? AND seq > ? ORDER BY seq"
	args := []any{id, afterSeq}
	if limit > 0 {
		query += " LIMIT ?"
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
		"SELECT COALESCE(MAX(seq), 0) FROM run_logs WHERE run_id=?", id).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("read log: %w", err)
	}
	return seq, nil
}

// AppendEvents appends structured events to the run. Returns run.ErrNotFound if absent. Events
// are marshaled before the transaction opens so the write lock is held only for the inserts.
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
	err = tx.QueryRowContext(ctx, "SELECT status FROM runs WHERE id=?", id).Scan(&status)
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

	stmt, err := tx.PrepareContext(ctx, "INSERT INTO run_events (run_id, data) VALUES (?, ?)")
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
	query := "SELECT seq, data FROM run_events WHERE run_id=? AND seq > ? ORDER BY seq"
	args := []any{id, afterSeq}
	if limit > 0 {
		query += " LIMIT ?"
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
		"SELECT COALESCE(MAX(seq), 0) FROM run_events WHERE run_id=?", id).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("read events: %w", err)
	}
	return seq, nil
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
		retryOf  sql.NullString
		extra    string
		outputs  string
		claimed  sql.NullString
		cancelI  int
		credIDs  string
		dryRun   int
		notifs   string
	)
	if err := s.Scan(&r.ID, &r.Playbook, &r.Inventory, &status, &exit, &r.Error,
		&created, &started, &ended, &parent, &shardIdx, &shardCnt, &r.Limit,
		&r.Kind, &r.StepName, &stepIdx, &retryOf, &r.Attempt, &extra, &outputs,
		&r.ClaimedBy, &claimed, &cancelI, &credIDs, &r.ProjectID, &r.CommitSHA,
		&r.InventoryID, &r.Queue, &r.Tool, &r.Command, &dryRun, &r.ProposedFrom, &r.Intent,
		&r.Image, &r.PullCredentialID, &r.IdempotencyKey, &r.Timeout, &notifs); err != nil {
		return nil, err
	}
	r.CancelRequested = cancelI != 0
	r.DryRun = dryRun != 0
	r.CredentialIDs = sqlutil.SplitIDs(credIDs)
	extraVars, err := sqlutil.ParseMap(extra)
	if err != nil {
		return nil, err
	}
	r.ExtraVars = extraVars
	outs, err := sqlutil.ParseMap(outputs)
	if err != nil {
		return nil, err
	}
	r.Outputs = outs
	if r.ClaimedAt, err = sqlutil.ParseNullTime(claimed); err != nil {
		return nil, err
	}
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
	r.Notifications = parseNotifications(notifs)
	return &r, nil
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

// Claim leases the oldest unclaimed pending top-level plain run to owner and returns it. A run
// whose cancel was requested while it waited is skipped; the cancel handler terminalizes it.
func (s *store) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	placeholders, args := sqlutil.QueuePlaceholders(queues, "?")
	q := `
UPDATE runs SET claimed_by=?, claimed_at=?
WHERE id = (
	SELECT id FROM runs
	WHERE status='pending' AND claimed_by='' AND kind='' AND cancel_requested=0
		AND queue IN (` + placeholders + `)
	ORDER BY created_at, id LIMIT 1
)
RETURNING ` + runColumns
	full := append([]any{owner, sqlutil.FormatTime(time.Now())}, args...)
	// The claim is a write that returns its row, so it must run on the write connection.
	r, err := scanRun(s.db.writeQueryRowContext(ctx, q, full...))
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
		"UPDATE runs SET claimed_at=? WHERE id=? AND claimed_by=?",
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
func (s *store) ReclaimStale(ctx context.Context, cutoff time.Time) (int, error) {
	cut := sqlutil.FormatTime(cutoff)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
UPDATE runs SET claimed_by='', claimed_at=NULL
WHERE status='pending' AND claimed_by!='' AND claimed_at < ?`, cut)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	requeued, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	res, err = tx.ExecContext(ctx, `
UPDATE runs SET status='interrupted', claimed_by='', claimed_at=NULL,
ended_at=?, error='interrupted: executor lease expired'
WHERE status='running' AND claimed_by!='' AND claimed_at < ?`, sqlutil.FormatTime(time.Now()), cut)
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
	res, err := s.db.ExecContext(ctx, "UPDATE runs SET cancel_requested=1 WHERE id=?", id)
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
UPDATE runs SET status='canceled', ended_at=?
WHERE id=? AND claimed_by='' AND status IN ('pending', 'pending_approval')`,
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
		"UPDATE runs SET status=? WHERE id=? AND status=?", string(to), id, string(from))
	if err != nil {
		return false, fmt.Errorf("transition status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("transition status: %w", err)
	}
	return n > 0, nil
}
