package pgstore

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// schema is the table layout created on open. It is idempotent so open doubles as migration. The
// summary indexes are dropped by their old names first because they were created over the raw
// ran_at column, which does not sort in time order; the replacements carry new names so IF NOT
// EXISTS cannot skip them on an upgrade.
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
	claim_secret  TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0,
	credential_ids TEXT NOT NULL DEFAULT '',
	project_id    TEXT NOT NULL DEFAULT '',
	commit_sha    TEXT NOT NULL DEFAULT '',
	inventory_id  TEXT NOT NULL DEFAULT '',
	org_id        TEXT NOT NULL DEFAULT '',
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
	actor_type    TEXT NOT NULL DEFAULT '',
	approved_spec_digest TEXT NOT NULL DEFAULT '',
	rerun_of      TEXT NOT NULL DEFAULT '',
	labels        TEXT NOT NULL DEFAULT '',
	warning       TEXT NOT NULL DEFAULT '',
	audit_receipt TEXT NOT NULL DEFAULT '',
	held_by_policy TEXT NOT NULL DEFAULT '',
	tags          TEXT NOT NULL DEFAULT '',
	skip_tags     TEXT NOT NULL DEFAULT '',
	verbosity     INTEGER NOT NULL DEFAULT 0,
	forks         INTEGER NOT NULL DEFAULT 0,
	diff_mode     INTEGER NOT NULL DEFAULT 0,
	distinct_approver INTEGER NOT NULL DEFAULT 0,
	pinned_commit TEXT NOT NULL DEFAULT '',
	policy_set TEXT NOT NULL DEFAULT '',
	actor_user_id TEXT NOT NULL DEFAULT ''
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
ALTER TABLE runs ADD COLUMN IF NOT EXISTS actor_type TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS approved_spec_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS rerun_of TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS labels TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS warning TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS audit_receipt TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS held_by_policy TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS distinct_approver INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS pinned_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS policy_set TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS actor_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS tags TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS skip_tags TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS verbosity INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS forks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS diff_mode INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS claim_secret TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
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
	dry_run          INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, host)
);
ALTER TABLE run_host_summary ADD COLUMN IF NOT EXISTS dry_run INTEGER NOT NULL DEFAULT 0;
DROP INDEX IF EXISTS idx_host_summary_host;
CREATE INDEX IF NOT EXISTS idx_host_summary_order
	ON run_host_summary(host, ` + sqlutil.TimeOrder + ` DESC, run_id DESC);
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
DROP INDEX IF EXISTS idx_task_summary_task;
CREATE INDEX IF NOT EXISTS idx_task_summary_order
	ON run_task_summary(task, ` + sqlutil.TimeOrder + ` DESC, run_id DESC);
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
	template_id TEXT NOT NULL DEFAULT '',
	timezone    TEXT NOT NULL DEFAULT '',
	org_id      TEXT NOT NULL DEFAULT '',
	created_by  TEXT NOT NULL DEFAULT ''
);
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_schedules_created ON schedules(created_at, id);
CREATE TABLE IF NOT EXISTS users (
	id            TEXT PRIMARY KEY,
	username      TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	role          TEXT NOT NULL,
	created_at    TEXT NOT NULL,
	full_name     TEXT NOT NULL DEFAULT '',
	email         TEXT NOT NULL DEFAULT '',
	phone         TEXT NOT NULL DEFAULT '',
	title         TEXT NOT NULL DEFAULT '',
	links         TEXT NOT NULL DEFAULT '',
	notes         TEXT NOT NULL DEFAULT '',
	source        TEXT NOT NULL DEFAULT ''
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
ALTER TABLE users ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username ON users(username);
CREATE TABLE IF NOT EXISTS tokens (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL DEFAULT '',
	hash         TEXT NOT NULL,
	user_id      TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	last_used_at TEXT,
	expires_at   TEXT,
	kind         TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tokens_hash ON tokens(hash);
ALTER TABLE tokens ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';
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
	timeout        INTEGER NOT NULL DEFAULT 0,
	confirm_on_launch INTEGER NOT NULL DEFAULT 0,
	tags           TEXT NOT NULL DEFAULT '',
	skip_tags      TEXT NOT NULL DEFAULT '',
	verbosity      INTEGER NOT NULL DEFAULT 0,
	forks          INTEGER NOT NULL DEFAULT 0,
	diff_mode      INTEGER NOT NULL DEFAULT 0,
	steps          TEXT NOT NULL DEFAULT '',
	limit_pattern  TEXT NOT NULL DEFAULT ''
);
ALTER TABLE templates ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS notifications TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS selectable_credential_ids TEXT NOT NULL DEFAULT '';
-- Zero leaves a launch on the server default, so a template made before this column is unchanged.
ALTER TABLE templates ADD COLUMN IF NOT EXISTS timeout INTEGER NOT NULL DEFAULT 0;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS confirm_on_launch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS tags TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS skip_tags TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS verbosity INTEGER NOT NULL DEFAULT 0;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS forks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS diff_mode INTEGER NOT NULL DEFAULT 0;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS steps TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS limit_pattern TEXT NOT NULL DEFAULT '';
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
	created_at        TEXT NOT NULL,
	created_by        TEXT NOT NULL DEFAULT ''
);
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_triggers_hash ON triggers(token_hash);
CREATE TABLE IF NOT EXISTS audit_entries (
	id        TEXT PRIMARY KEY,
	at        TEXT NOT NULL,
	actor     TEXT NOT NULL DEFAULT '',
	actor_type TEXT NOT NULL DEFAULT '',
	on_behalf_of TEXT NOT NULL DEFAULT '',
	method    TEXT NOT NULL,
	path      TEXT NOT NULL,
	content_digest TEXT NOT NULL DEFAULT '',
	seq       BIGINT NOT NULL DEFAULT 0,
	prev_hash TEXT NOT NULL DEFAULT '',
	hash      TEXT NOT NULL DEFAULT '',
	nonce     TEXT NOT NULL DEFAULT '',
	install_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS audit_anchors (
	id    TEXT PRIMARY KEY,
	type  TEXT NOT NULL,
	shape TEXT NOT NULL,
	seq   BIGINT NOT NULL,
	link  TEXT NOT NULL,
	at    TEXT NOT NULL,
	ref   TEXT NOT NULL DEFAULT '',
	proof TEXT NOT NULL DEFAULT '',
	install_id TEXT NOT NULL DEFAULT ''
);
-- An anchor records which coordinate space its link lives in, a linear entry hash or a tree root.
-- A database created before the column existed gains it here, the same way every other added column
-- in this schema does.
ALTER TABLE audit_anchors ADD COLUMN IF NOT EXISTS shape TEXT NOT NULL DEFAULT 'linear';
-- And which install computed the value it fixes, so a chain read under a different identity, which is
-- what every replica minting its own key produces, is diagnosed rather than called a rewrite.
ALTER TABLE audit_anchors ADD COLUMN IF NOT EXISTS install_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_audit_anchor_seq ON audit_anchors(seq);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_entries(at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_seq ON audit_entries(seq);
-- Span beats are a narrow slice of the chain selected by actor and method and ordered by seq.
-- Without this the unauthenticated beat feed and every beat append walked the whole entries table.
CREATE INDEX IF NOT EXISTS idx_audit_span ON audit_entries(actor, method, seq);
CREATE TABLE IF NOT EXISTS policies (
	id               TEXT PRIMARY KEY,
	name             TEXT NOT NULL DEFAULT '',
	tool             TEXT NOT NULL DEFAULT '',
	command_contains TEXT NOT NULL DEFAULT '',
	inventory_id     TEXT NOT NULL DEFAULT '',
	queue            TEXT NOT NULL DEFAULT '',
	exclude_dry_run  INTEGER NOT NULL DEFAULT 0,
	max_destroy      INTEGER NOT NULL DEFAULT -1,
	actor_kind       TEXT NOT NULL DEFAULT '',
	actor            TEXT NOT NULL DEFAULT '',
	min_risk         TEXT NOT NULL DEFAULT '',
	effect           TEXT NOT NULL DEFAULT '',
	distinct_approver INTEGER NOT NULL DEFAULT 0,
	created_at       TEXT NOT NULL
);
-- A policy can demand that the approver be someone other than the requester. The column rides an
-- ALTER for databases from before it; without it the rule loaded back with the requirement off, so
-- the requester could approve their own run. It sits after the CREATE it amends, because this blob
-- executes top to bottom and an ALTER naming a table that does not exist yet fails the whole
-- migration on a fresh database, which is exactly what it did.
ALTER TABLE policies ADD COLUMN IF NOT EXISTS distinct_approver INTEGER NOT NULL DEFAULT 0;
ALTER TABLE policies ADD COLUMN IF NOT EXISTS max_destroy INTEGER NOT NULL DEFAULT -1;
ALTER TABLE policies ADD COLUMN IF NOT EXISTS actor_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE policies ADD COLUMN IF NOT EXISTS actor TEXT NOT NULL DEFAULT '';
ALTER TABLE policies ADD COLUMN IF NOT EXISTS min_risk TEXT NOT NULL DEFAULT '';
ALTER TABLE policies ADD COLUMN IF NOT EXISTS effect TEXT NOT NULL DEFAULT '';
ALTER TABLE policies ADD COLUMN IF NOT EXISTS queue TEXT NOT NULL DEFAULT '';
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
	org_id     TEXT NOT NULL DEFAULT '',
	type_id    TEXT NOT NULL DEFAULT '',
	vault_id   TEXT NOT NULL DEFAULT '',
	settings   TEXT NOT NULL DEFAULT ''
);
ALTER TABLE credentials ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
ALTER TABLE credentials ADD COLUMN IF NOT EXISTS type_id TEXT NOT NULL DEFAULT '';
ALTER TABLE credentials ADD COLUMN IF NOT EXISTS vault_id TEXT NOT NULL DEFAULT '';
ALTER TABLE credentials ADD COLUMN IF NOT EXISTS settings TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_entries ADD COLUMN IF NOT EXISTS actor_type TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_entries ADD COLUMN IF NOT EXISTS on_behalf_of TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_entries ADD COLUMN IF NOT EXISTS content_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_entries ADD COLUMN IF NOT EXISTS nonce TEXT NOT NULL DEFAULT '';
-- The install that wrote an entry is folded into its chain link, so the column has to exist
-- wherever the link is recomputed. Without it the read path returns nothing for a value the
-- write path hashed, and every chain in the install reports broken at its first entry.
ALTER TABLE audit_entries ADD COLUMN IF NOT EXISTS install_id TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS credential_types (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	fields     TEXT NOT NULL DEFAULT '[]',
	env        TEXT NOT NULL DEFAULT '{}',
	extra_vars TEXT NOT NULL DEFAULT '{}',
	created_at BIGINT NOT NULL DEFAULT 0
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
CREATE INDEX IF NOT EXISTS idx_runs_pending_claim ON runs(queue, created_at, id)
	WHERE status='pending' AND claimed_by='' AND kind='';
CREATE INDEX IF NOT EXISTS idx_runs_status_parent ON runs(status, parent_id);
CREATE INDEX IF NOT EXISTS idx_runs_leased ON runs(claimed_at) WHERE claimed_by<>'';
-- The runs view search takes actor:, source:, and from: as equality filters and pages the result
-- newest first, so each index carries the page ordering behind the filtered column. Without them
-- every fielded search walked the whole runs table. They come after the column migrations above,
-- since a database created before those columns cannot be indexed on them.
CREATE INDEX IF NOT EXISTS idx_runs_actor ON runs(actor, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_runs_source ON runs(source, source_id, created_at DESC, id DESC);
`

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
	// concurrent ALTER TABLE statements deadlock, so migration is serialized by an advisory lock.
	//
	// The lock, the migration, and the release are one transaction. Taken as three calls on the pool
	// they were three connections as far as Postgres is concerned: a session lock belongs to the
	// backend that took it, so the schema could run on a connection holding nothing and the release
	// could run on a third, where pg_advisory_unlock returns false rather than an error and the lock
	// leaks for the life of that connection. Nothing else touches the pool in between today, so one
	// connection is reused and it works, which is an accident of timing rather than a property. A
	// transaction-scoped lock releases when the transaction ends, including when it fails.
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := normalizeScheduleTimes(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{db: db, runs: &store{db: db}, schedules: &scheduleStore{db: db}, tokens: &tokenStore{db: db},
		credentials: &credentialStore{db: db},
		credTypes:   &credTypeStore{db: db},
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
// migrateLockKey serializes schema migration across every process opening this database.
const migrateLockKey = 7973821001

// migrate applies the schema under a transaction-scoped advisory lock.
// collationNote records why the text tiebreakers in this file say COLLATE "C".
//
// SQLite always orders text by bytes. PostgreSQL orders it by the database collation, which on the
// default postgres:16 image and on most managed instances is a glibc locale that sorts
// linguistically, so punctuation and case are weighted differently. The two backends therefore
// returned the same rows in different orders for the same data: an identical four-host set came back
// as Web2, web-10, web1, web_3 on one and web1, web-10, Web2, web_3 on the other. On a listing that
// is cosmetic. On the summary trim it is not, because that query picks which rows to delete, so a
// tie decided differently meant the two backends kept and destroyed different history. The alpine
// image CI runs its PostgreSQL service on hides this, since musl has no collation tables and falls
// back to byte order.
func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Bound how long this transaction will wait for a lock, and how long any one statement may run.
	//
	// An ADD COLUMN IF NOT EXISTS that changes nothing still takes AccessExclusiveLock, and this
	// transaction issues one for every declared column plus the schema's own ALTERs, so it ends up
	// holding an exclusive lock on every table until it commits. Every process calls Open, workers
	// included, so this runs on ordinary starts and not only on upgrades. With no timeout a new node
	// coming up behind one long read queued for the lock, and because the lock queue is first in
	// first out every later reader queued behind the migration: one slow retention purge or chain
	// scan could stall the whole cluster, API, claim loop and heartbeats alike, for as long as it
	// ran. Failing fast instead means the starting process retries rather than freezing everyone
	// else, which is the direction this should fail in.
	if _, err := tx.Exec("SET LOCAL lock_timeout = '5s'"); err != nil {
		return fmt.Errorf("migration lock timeout: %w", err)
	}
	if _, err := tx.Exec("SET LOCAL statement_timeout = '60s'"); err != nil {
		return fmt.Errorf("migration statement timeout: %w", err)
	}
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	// Healing runs BEFORE the schema blob, on every table that already exists. The blob's own CREATE
	// INDEX statements reference columns only the heal would add: idx_runs_pending_claim covers queue,
	// and a database from before the queue column, which the deleted hand ALTERs prove exists, failed
	// the blob with "column does not exist" and aborted the transaction before the heal it needed ever
	// ran. The SQLite store orders these the same way for the same reason. A table the database does
	// not have yet is skipped here and created whole by the blob.
	//
	// The statements are derived from the schema itself rather than from the hand-kept ALTER list in
	// the blob. That list drifted once on the SQLite side, where runs.org_id reached the CREATE and
	// the shared select list and never the migrations, and every database from before it failed every
	// read of the runs table after an upgrade. The hand list stays because it is idempotent and
	// documents when each column arrived.
	for table, cols := range sqlutil.ParseSchemaColumns(schema) {
		var exists bool
		if err := tx.QueryRow("SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
			return fmt.Errorf("heal %s: %w", table, err)
		}
		if !exists {
			continue
		}
		for _, col := range cols {
			if !col.Addable() {
				continue
			}
			if _, err := tx.Exec("ALTER TABLE " + table + " ADD COLUMN IF NOT EXISTS " +
				col.Name + " " + col.Clause); err != nil {
				return fmt.Errorf("heal %s.%s: %w", table, col.Name, err)
			}
		}
	}
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	return nil
}
