package sqlitestore

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// schema is the full table layout created on open. It uses IF NOT EXISTS, so opening an existing
// database is safe. The summary indexes are dropped by their old names first because they were
// created over the raw ran_at column, which does not sort in time order; the replacements carry new
// names so IF NOT EXISTS cannot skip them on an upgrade.
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
	timeout INTEGER NOT NULL DEFAULT 0,
	notifications TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	source_id TEXT NOT NULL DEFAULT '',
	actor TEXT NOT NULL DEFAULT '',
	rerun_of TEXT NOT NULL DEFAULT '',
	labels TEXT NOT NULL DEFAULT '',
	warning TEXT NOT NULL DEFAULT '',
	audit_receipt TEXT NOT NULL DEFAULT '',
	held_by_policy TEXT NOT NULL DEFAULT '',
	tags TEXT NOT NULL DEFAULT '',
	skip_tags TEXT NOT NULL DEFAULT '',
	verbosity INTEGER NOT NULL DEFAULT 0,
	forks INTEGER NOT NULL DEFAULT 0,
	diff_mode INTEGER NOT NULL DEFAULT 0,
	actor_type TEXT NOT NULL DEFAULT '',
	approved_spec_digest TEXT NOT NULL DEFAULT '',
	distinct_approver INTEGER NOT NULL DEFAULT 0,
	pinned_commit TEXT NOT NULL DEFAULT '',
	policy_set TEXT NOT NULL DEFAULT '',
	actor_user_id TEXT NOT NULL DEFAULT ''
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
	dry_run     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, host)
);
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
	seconds REAL NOT NULL,
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
	created_at        TEXT NOT NULL,
	created_by        TEXT NOT NULL DEFAULT ''
);
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
	seq       INTEGER NOT NULL DEFAULT 0,
	prev_hash TEXT NOT NULL DEFAULT '',
	hash      TEXT NOT NULL DEFAULT '',
	nonce     TEXT NOT NULL DEFAULT '',
	install_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS audit_anchors (
	id    TEXT PRIMARY KEY,
	type  TEXT NOT NULL,
	shape TEXT NOT NULL,
	seq   INTEGER NOT NULL,
	link  TEXT NOT NULL,
	at    TEXT NOT NULL,
	ref   TEXT NOT NULL DEFAULT '',
	proof TEXT NOT NULL DEFAULT '',
	install_id TEXT NOT NULL DEFAULT ''
);
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
	exclude_dry_run  INTEGER NOT NULL DEFAULT 0,
	max_destroy      INTEGER NOT NULL DEFAULT -1,
	actor_kind       TEXT NOT NULL DEFAULT '',
	actor            TEXT NOT NULL DEFAULT '',
	min_risk         TEXT NOT NULL DEFAULT '',
	effect           TEXT NOT NULL DEFAULT '',
	distinct_approver INTEGER NOT NULL DEFAULT 0,
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
	org_id     TEXT NOT NULL DEFAULT '',
	type_id    TEXT NOT NULL DEFAULT '',
	vault_id   TEXT NOT NULL DEFAULT '',
	settings   TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS credential_types (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	fields     TEXT NOT NULL DEFAULT '[]',
	env        TEXT NOT NULL DEFAULT '{}',
	extra_vars TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL DEFAULT 0
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
	// Heal existing tables before anything else touches them. The schema's CREATE TABLE IF NOT
	// EXISTS is a no-op on a table that already exists, however old its shape, and both the schema's
	// index statements and ensureRunIndexes fail outright on a column an old table is missing.
	// Healing derives what to add from the schema itself, so a new column needs no migration entry:
	// the hand-kept ALTER lists below drifted once, runs.org_id reached the CREATE and the shared
	// select list and never the lists, and every database from before it failed every read of the
	// runs table after an upgrade.
	if err := healColumns(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateRuns(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateHostSummary(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateTeamMembers(db); err != nil {
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
	if err := migrateSchedules(db); err != nil {
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
	if err := migrateAuditEntries(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateUsers(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateTokens(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := normalizeScheduleTimes(db); err != nil {
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
		credTypes:   &credTypeStore{db: split},
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

// healColumns adds every column the schema declares that an existing table lacks, with the type and
// default the schema declares for it. Tables the database does not have yet are left to the schema's
// own CREATE. Columns ALTER cannot add, primary key members and NOT NULL without a default, are
// original-era columns a created table always has, so skipping them skips nothing real.
func healColumns(db *sql.DB) error {
	if err := refusePreChainAudit(db); err != nil {
		return err
	}
	for table, cols := range sqlutil.ParseSchemaColumns(schema) {
		var exists int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&exists); err != nil {
			return fmt.Errorf("heal %s: %w", table, err)
		}
		if exists == 0 {
			continue
		}
		have := map[string]bool{}
		rows, err := db.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			return fmt.Errorf("heal %s: %w", table, err)
		}
		for rows.Next() {
			var cid int
			var name, typ string
			var notnull int
			var dflt sql.NullString
			var pk int
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
				_ = rows.Close()
				return fmt.Errorf("heal %s: %w", table, err)
			}
			have[name] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("heal %s: %w", table, err)
		}
		_ = rows.Close()
		for _, col := range cols {
			if have[col.Name] || !col.Addable() {
				continue
			}
			if _, err := db.Exec(
				"ALTER TABLE " + table + " ADD COLUMN " + col.Name + " " + col.Clause); err != nil {
				return fmt.Errorf("heal %s: add %s: %w", table, col.Name, err)
			}
		}
	}
	return nil
}

// refusePreChainAudit refuses a database whose audit trail predates the hash chain, with a message an
// operator can act on.
//
// A trail from before the chain has no seq. Healing would add one with every row at zero, and the
// schema's unique index over seq then fails on the duplicates, so the open died with "UNIQUE
// constraint failed: audit_entries.seq", which reads as corruption and says nothing about the cause
// or the way out. No backfill can help: the chain's hashes commit to seq, so rows written before it
// cannot be minted into a valid chain after the fact, and pretending otherwise would manufacture
// evidence. Only databases from the few days between the audit trail existing and the chain existing
// are in this state.
func refusePreChainAudit(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='audit_entries'").Scan(&exists); err != nil {
		return fmt.Errorf("audit trail check: %w", err)
	}
	if exists == 0 {
		return nil
	}
	var hasSeq int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM pragma_table_info('audit_entries') WHERE name='seq'").Scan(&hasSeq); err != nil {
		return fmt.Errorf("audit trail check: %w", err)
	}
	if hasSeq > 0 {
		return nil
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit_entries").Scan(&rows); err != nil {
		return fmt.Errorf("audit trail check: %w", err)
	}
	if rows == 0 {
		return nil
	}
	return fmt.Errorf("this database's audit trail predates the tamper-evident chain and its %d "+
		"entries cannot be joined to one: the chain's hashes commit to a sequence those entries never "+
		"had. Archive the database if the old trail matters, then either start fresh or delete the "+
		"audit_entries rows to begin the chain from here", rows)
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
	for _, column := range []string{"source", "source_id", "actor", "rerun_of", "labels", "steps", "warning", "audit_receipt", "held_by_policy", "tags", "skip_tags", "claim_secret", "actor_type", "approved_spec_digest", "pinned_commit", "policy_set", "actor_user_id"} {
		if _, err := db.Exec(
			"ALTER TABLE runs ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("add %s column: %w", column, err)
		}
	}
	for _, column := range []string{"verbosity", "forks", "diff_mode", "distinct_approver"} {
		if _, err := db.Exec(
			"ALTER TABLE runs ADD COLUMN " + column + " INTEGER NOT NULL DEFAULT 0"); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("add %s column: %w", column, err)
		}
	}
	if _, err := db.Exec(
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_idempotency_key " +
			"ON runs(idempotency_key) WHERE idempotency_key <> ''"); err != nil {
		return fmt.Errorf("index idempotency_key: %w", err)
	}
	// An anchor records which coordinate space its link lives in, a linear entry hash or a tree
	// root, and a database created before the column existed gains it here.
	if _, err := db.Exec(
		"ALTER TABLE audit_anchors ADD COLUMN shape TEXT NOT NULL DEFAULT 'linear'"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add anchor shape column: %w", err)
	}
	// A policy can demand that the approver be someone other than the requester, which a database
	// created before the column gains here. Without the column the rule loaded back with the
	// requirement off, so the requester could approve their own run.
	if _, err := db.Exec(
		"ALTER TABLE policies ADD COLUMN distinct_approver INTEGER NOT NULL DEFAULT 0"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add policy distinct_approver column: %w", err)
	}
	// An anchor also records which install computed the value it fixes, so a chain read under a
	// different identity, which is what a restore without its key file produces, is diagnosed rather
	// than reported as a rewrite. An anchor from before the column has none, and is checked the old way.
	if _, err := db.Exec(
		"ALTER TABLE audit_anchors ADD COLUMN install_id TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add anchor install_id column: %w", err)
	}
	// An entry records the install that wrote it, which is folded into its chain link so a receipt
	// cannot be presented as another install's history. The column has to exist wherever the link
	// is recomputed: hashing a value the read path cannot return breaks every chain in the install.
	// An entry from before the column has none, and hashes exactly as it did when it was written.
	if _, err := db.Exec(
		"ALTER TABLE audit_entries ADD COLUMN install_id TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add entry install_id column: %w", err)
	}
	return nil
}

// migrateTeamMembers rebuilds team_members with the foreign key its CREATE promises. The constraint
// was added to the CREATE one day after the table first shipped, with no migration, and SQLite has no
// ALTER that adds one: a database from that first day enforced nothing while every fresh database
// cascaded a deleted team's memberships. The rebuild is the standard SQLite shape, copy and swap, and
// runs only when the constraint is actually absent.
func migrateTeamMembers(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='team_members'").Scan(&exists); err != nil {
		return fmt.Errorf("team_members migration: %w", err)
	}
	if exists == 0 {
		return nil
	}
	var fks int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM pragma_foreign_key_list('team_members')").Scan(&fks); err != nil {
		return fmt.Errorf("team_members migration: %w", err)
	}
	if fks > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("team_members migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE team_members_new (
	team_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL,
	PRIMARY KEY (team_id, user_id)
)`,
		"INSERT INTO team_members_new SELECT team_id, user_id FROM team_members",
		"DROP TABLE team_members",
		"ALTER TABLE team_members_new RENAME TO team_members",
		// Dropping the old table dropped its index, and the schema exec that would recreate it has
		// already run this open, so the rebuild recreates it itself.
		"CREATE INDEX IF NOT EXISTS idx_team_members_user ON team_members(user_id)",
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("team_members migration: %w", err)
		}
	}
	return tx.Commit()
}

// migrateHostSummary adds the dry-run flag to an existing host summary table. The flag is copied
// onto the summary at write time so the drift view never joins the runs table, which retention
// purges out from under it. Rows written before this column keep the zero value, counting as
// applies, which is the safe reading: an old row cannot be proven to have been a check.
func migrateHostSummary(db *sql.DB) error {
	if _, err := db.Exec(
		"ALTER TABLE run_host_summary ADD COLUMN dry_run INTEGER NOT NULL DEFAULT 0"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add dry_run column: %w", err)
	}
	return nil
}

// ensureRunIndexes creates the hot-path run indexes after the column migrations so the columns
// they cover exist even on a database created before those columns were added. The claim index
// keeps the executor poll from walking the whole table oldest-first, the status index keeps the
// summary counts off the fat run rows, and the lease index bounds the janitor sweep and the
// worker listing as history grows. The actor and source indexes back the runs view's fielded
// search, whose equality filters otherwise walked the whole table; each carries the page ordering
// behind its filtered columns so the search pages without a sort.
func ensureRunIndexes(db *sql.DB) error {
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_runs_pending_claim ON runs(queue, created_at, id)
			WHERE status='pending' AND claimed_by='' AND kind=''`,
		"CREATE INDEX IF NOT EXISTS idx_runs_status_parent ON runs(status, parent_id)",
		"CREATE INDEX IF NOT EXISTS idx_runs_leased ON runs(claimed_at) WHERE claimed_by<>''",
		"CREATE INDEX IF NOT EXISTS idx_runs_actor ON runs(actor, created_at DESC, id DESC)",
		"CREATE INDEX IF NOT EXISTS idx_runs_source ON runs(source, source_id, created_at DESC, id DESC)",
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
	for _, column := range []string{"actor_kind", "actor", "min_risk", "effect"} {
		if _, err := db.Exec(
			"ALTER TABLE policies ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate policies: add %s: %w", column, err)
		}
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

// migrateTemplates adds the columns a templates table created before them lacks: the owning
// organization, notification targets, the selectable credential set, and the run timeout. Every one
// defaults to unset, so a template made before a migration keeps its previous behavior, global and
// on the server default timeout. Adding a column that already exists is the ordinary case for a
// current database and is treated as success.
func migrateTemplates(db *sql.DB) error {
	for _, stmt := range []string{
		"ALTER TABLE templates ADD COLUMN org_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE templates ADD COLUMN notifications TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE templates ADD COLUMN selectable_credential_ids TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE templates ADD COLUMN timeout INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE templates ADD COLUMN confirm_on_launch INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE templates ADD COLUMN tags TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE templates ADD COLUMN skip_tags TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE templates ADD COLUMN verbosity INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE templates ADD COLUMN forks INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE templates ADD COLUMN diff_mode INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE templates ADD COLUMN steps TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE templates ADD COLUMN limit_pattern TEXT NOT NULL DEFAULT ''",
	} {
		if _, err := db.Exec(stmt); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate templates: %w", err)
		}
	}
	return nil
}

// migrateSchedules adds the timezone column a schedules table created before per-schedule timezones
// lacks. Empty is the server-local default, so a schedule made before it fires exactly as it did.
func migrateSchedules(db *sql.DB) error {
	if _, err := db.Exec(
		"ALTER TABLE schedules ADD COLUMN timezone TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrate schedules: %w", err)
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

// migrateUsers adds the profile columns to a users table created before them. Every one defaults to
// empty, so an account made before this migration keeps working with no profile at all. Adding a
// column that already exists is the ordinary case for a current database and is treated as success.
// migrateTokens adds the kind column to a tokens table created before it, defaulting to a person.
func migrateTokens(db *sql.DB) error {
	if _, err := db.Exec(
		"ALTER TABLE tokens ADD COLUMN kind TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add tokens kind column: %w", err)
	}
	return nil
}

// migrateUsers adds the profile columns to a users table created before them. Every one defaults to
func migrateUsers(db *sql.DB) error {
	for _, column := range []string{"full_name", "email", "phone", "title", "links", "notes"} {
		if _, err := db.Exec(
			"ALTER TABLE users ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("add %s column: %w", column, err)
		}
	}
	return nil
}

// migrateCredentials adds the owning-organization column to a credentials table created before object
// tenancy. Empty is the unowned default, so a credential made before this column stays global. Adding
// a column that already exists is the ordinary case for a current database and is treated as success.
func migrateCredentials(db *sql.DB) error {
	for _, col := range []string{
		"ALTER TABLE credentials ADD COLUMN org_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE credentials ADD COLUMN type_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE credentials ADD COLUMN vault_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE credentials ADD COLUMN settings TEXT NOT NULL DEFAULT ''",
	} {
		if _, err := db.Exec(col); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate credentials: %w", err)
		}
	}
	return nil
}

// migrateAuditEntries adds the columns the chain link commits to beyond the original six values: the
// actor's authentication type, the account a token acted on behalf of, and the digest of the change
// payload. Each defaults to empty, which is exactly what an entry recorded before the column existed
// carries, and an empty field is omitted from the link, so migrating a database does not disturb a
// single existing hash.
func migrateAuditEntries(db *sql.DB) error {
	for _, col := range []string{
		"ALTER TABLE audit_entries ADD COLUMN actor_type TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE audit_entries ADD COLUMN on_behalf_of TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE audit_entries ADD COLUMN content_digest TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE audit_entries ADD COLUMN nonce TEXT NOT NULL DEFAULT ''",
	} {
		if _, err := db.Exec(col); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate audit entries: %w", err)
		}
	}
	return nil
}
