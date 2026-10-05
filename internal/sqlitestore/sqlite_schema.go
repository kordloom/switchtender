package sqlitestore

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

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
	approved_spec_binding TEXT NOT NULL DEFAULT '',
	distinct_approver INTEGER NOT NULL DEFAULT 0,
	pinned_commit TEXT NOT NULL DEFAULT '',
	policy_set TEXT NOT NULL DEFAULT '',
	actor_user_id TEXT NOT NULL DEFAULT '',
	plan_destroys INTEGER,
	template_id TEXT NOT NULL DEFAULT '',
	inventory_resolution TEXT NOT NULL DEFAULT '',
	-- The sealed answers to a run's secret survey fields, a JSON object of ciphertext. It is never
	-- plain text and leaves the database only through the executor that opens it.
	sealed_vars TEXT NOT NULL DEFAULT '',
	use_fact_cache INTEGER NOT NULL DEFAULT 0,
	fact_cache_timeout INTEGER NOT NULL DEFAULT 0,
	git_ref TEXT NOT NULL DEFAULT '',
	-- What the gate's scan of a dry run read, as JSON: the scanner, the files examined, what was
	-- found and what could not be read, and the classification that followed.
	dry_run_scans TEXT NOT NULL DEFAULT '',
	hold_note TEXT NOT NULL DEFAULT '',
	-- The digest of each sealed answer's ciphertext, a JSON list fixed when the run is created. It
	-- binds which sealed answer the run carries, so an approval covers it, and never the answer.
	sealed_digests TEXT NOT NULL DEFAULT '',
	policy_notes TEXT NOT NULL DEFAULT '',
	-- The cross-check an Ansible run against a natively resolved inventory made before it ran, as
	-- JSON: the ansible-core that read the inventory, and the digests or the differences.
	inventory_check TEXT NOT NULL DEFAULT '',
	-- The agent identity of an agent-initiated run, a JSON object, empty for any other run.
	initiator TEXT NOT NULL DEFAULT '',
	require_reason TEXT NOT NULL DEFAULT '',
	-- The stored inventory the run executes against, materialized when it was submitted: a JSON
	-- record of the sealed content's digest, a digest with its secrets masked, and the hosts it
	-- names. The content itself is in inventory_sealed, ciphertext only, and is wiped when the run
	-- ends.
	inventory_snapshot TEXT NOT NULL DEFAULT '',
	inventory_sealed TEXT NOT NULL DEFAULT '',
	-- The hosts a dynamic inventory source resolved to when the run executed, as a JSON list.
	resolved_hosts TEXT NOT NULL DEFAULT '',
	-- The digest of the sealed plan file a gated apply carries out, and the plan file itself,
	-- ciphertext only, wiped when the run ends.
	plan_sha256 TEXT NOT NULL DEFAULT '',
	plan_sealed TEXT NOT NULL DEFAULT '',
	-- The digest of the image the container runtime pulled and ran.
	image_digest TEXT NOT NULL DEFAULT '',
	-- The decision that won a held run or an approval step: the id of its record and of the chain
	-- entry recording it. Set when the decision claims the run and never changed afterward.
	decision_id TEXT NOT NULL DEFAULT '',
	-- The decision that claimed the run and has not settled it, as JSON: its chain entry and how it
	-- settles the run. Empty once it settles. While it is set the row's status reads deciding.
	decision_claim TEXT NOT NULL DEFAULT '',
	-- When the run last re-entered the queue: released by an approval or put back by the lease
	-- sweep. Empty for a run that has waited since it was created.
	queued_at TEXT,
	-- When the run came to owe its outcome to the audit chain, in Unix milliseconds by the store's
	-- clock, and zero while it owes nothing. The runs_outcome_owed trigger sets it, and the commit
	-- that puts the outcome on the chain clears it.
	outcome_owed_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_runs_created_at ON runs(created_at DESC, id DESC);
-- Every run listing selects top-level runs and pages them by creation time, and that pair has to sit
-- in one index or the planner cannot serve both halves at once. Given only separate indexes it drove
-- the query off idx_runs_parent to satisfy parent_id IS NULL, then pushed every matching row through
-- a sorter to recover the ordering, so asking for a page of fifty read and sorted every top-level run
-- on the install and the LIMIT saved nothing. These carry the page ordering underneath the top-level
-- filter, so a page is the first rows of the index and shard rows are not in it at all. Both are
-- scanned backwards for the oldest-first ordering, so neither needs a second ascending copy.
CREATE INDEX IF NOT EXISTS idx_runs_toplevel_created
	ON runs(created_at DESC, id DESC) WHERE parent_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_runs_toplevel_status
	ON runs(status, created_at DESC, id DESC) WHERE parent_id IS NULL;
-- Dropped by its old name and recreated under a new one, because IF NOT EXISTS would keep the old
-- definition on every existing database and the change is the WHERE, not the columns. The index
-- exists to find a parent's children, and every query that reads it names a parent, so restricting it
-- to rows that have one costs nothing and shrinks it to the shard and step rows. It also takes the
-- index away from parent_id IS NULL, which matters: SQLite reads that as an equality test and,
-- without table statistics, estimates it selects a handful of rows when it selects nearly all of
-- them, which is what sent every run listing through a sorter.
DROP INDEX IF EXISTS idx_runs_parent;
CREATE INDEX IF NOT EXISTS idx_runs_child_parent
	ON runs(parent_id, shard_index) WHERE parent_id IS NOT NULL;
-- The dispatcher sweeps every unfinished run on a timer, and that read used to be a full scan of the
-- runs table, so its cost was the size of the whole history rather than the size of the work in
-- flight: an install with a year of runs behind it paid for all of them on every tick to find the
-- handful still moving. The partial index holds only the unfinished rows, so the sweep stays the
-- size of the queue. Its condition is the nonTerminalRun predicate itself, not a copy of it, because
-- SQLite only uses a partial index when the query's WHERE matches the index's, so a predicate that
-- drifted from the index would silently return the scan rather than fail.
CREATE INDEX IF NOT EXISTS idx_runs_live ON runs(created_at) WHERE ` + nonTerminalRun + `;
-- A top-level run comes to owe its outcome to the audit chain in the write that makes it terminal,
-- whichever statement that is, so an append the chain refused or a process that died before it
-- appended cannot lose the outcome for good: the janitor commits what is still owed. A run stored
-- already finished owes nothing, since no process saw it finish. The trigger is created once and
-- never replaced on open, because replacing it would leave a moment in which another process's
-- terminal write marked nothing, so a change to it takes a new name.
CREATE TRIGGER IF NOT EXISTS runs_outcome_owed AFTER UPDATE OF status ON runs
FOR EACH ROW WHEN NEW.parent_id IS NULL AND NEW.` + terminalRun + ` AND OLD.` + nonTerminalRun + `
BEGIN
	UPDATE runs SET outcome_owed_ms = CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)
	WHERE id = NEW.id;
END;
CREATE INDEX IF NOT EXISTS idx_runs_outcome_owed ON runs(outcome_owed_ms) WHERE outcome_owed_ms > 0;
CREATE TABLE IF NOT EXISTS run_logs (
	seq    INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL,
	chunk  BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_logs_run ON run_logs(run_id, seq);
-- Stream tickets live here so a ticket minted on one replica redeems on any other. Only the
-- ticket's hash is stored: a leaked table holds no live credentials.
CREATE TABLE IF NOT EXISTS stream_tickets (
	secret_hash TEXT PRIMARY KEY,
	run_id      TEXT NOT NULL,
	actor_key   TEXT NOT NULL,
	actor       TEXT NOT NULL,
	expires_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS stream_tickets_actor ON stream_tickets (actor_key, expires_at);
-- Fixed-window allowances every process sharing the database spends from, such as a provisioning
-- callback's per-address request and wrong-key budgets. window_end is in Unix nanoseconds, and a row
-- whose window has closed is deleted when the next window anywhere opens.
CREATE TABLE IF NOT EXISTS budgets (
	key        TEXT PRIMARY KEY,
	window_end INTEGER NOT NULL DEFAULT 0,
	spent      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_budgets_window_end ON budgets(window_end);

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
-- host_facts above holds only the newest reading per host, because it answers "what is this host
-- now". This table is the same readings kept over time, which is what answers "what did the estate
-- look like on the audit date". Keyed by bucket rather than by run so a second gather inside the
-- same period replaces the first: see run.FactsBucket for how a bucket is chosen and why changing
-- run_auth is what decides who may read a run, kept after the run itself is deleted.
--
-- Derived rows outlive their runs on purpose: summaries, drift, and host state history answer
-- questions about a fleet over time. Readability is decided by resolving the governing run, so a
-- purged run made every derived row it governs unreadable to a grant-restricted caller, silently.
-- Retaining the decision rather than the run closes that without creating a second authorization
-- rule: see run.RunAuth.
--
-- It is small by construction, a handful of ids, so it can be kept for far longer than a run.
CREATE TABLE IF NOT EXISTS run_auth (
	run_id             TEXT PRIMARY KEY,
	org_id             TEXT NOT NULL DEFAULT '',
	project_id         TEXT NOT NULL DEFAULT '',
	inventory_id       TEXT NOT NULL DEFAULT '',
	pull_credential_id TEXT NOT NULL DEFAULT '',
	credential_ids     TEXT NOT NULL DEFAULT ''
);
-- the spacing later is safe.
CREATE TABLE IF NOT EXISTS host_facts_history (
	host        TEXT NOT NULL,
	bucket      TEXT NOT NULL,
	run_id      TEXT NOT NULL,
	facts       TEXT NOT NULL,
	gathered_at TEXT NOT NULL,
	PRIMARY KEY (host, bucket)
);
CREATE INDEX IF NOT EXISTS idx_host_facts_history_host_time
	ON host_facts_history (host, gathered_at DESC);
-- host_fact_cache is the fact cache a template turns on with use_fact_cache: the whole fact
-- document Ansible gathered for one host of one stored inventory, written back into Ansible's
-- jsonfile cache for the next run of a template that uses it. host_facts above keeps a handful of
-- identifying facts for the estate views, and this keeps everything a play can read, which is why
-- it is kept apart. It is runtime state like run history: it never enters the audit chain, a
-- receipt, or a backup, because a fact document routinely carries the remote user's environment.
CREATE TABLE IF NOT EXISTS host_fact_cache (
	inventory_id TEXT NOT NULL,
	host         TEXT NOT NULL,
	facts        TEXT NOT NULL,
	run_id       TEXT NOT NULL DEFAULT '',
	modified_at  TEXT NOT NULL,
	PRIMARY KEY (inventory_id, host)
);
-- worker_presence holds each executor's latest report that it is polling for work: the queues it
-- serves, how many runs it takes at once, and when it was first and last seen. It is how the
-- dashboard tells a queue nothing serves from one whose workers are busy. It is runtime state, kept
-- only while a worker reports and a day after, and never enters a backup or the audit chain.
CREATE TABLE IF NOT EXISTS worker_presence (
	owner      TEXT PRIMARY KEY,
	queues     TEXT NOT NULL DEFAULT '',
	slots      INTEGER NOT NULL DEFAULT 0,
	first_seen TEXT NOT NULL DEFAULT '',
	last_seen  TEXT NOT NULL DEFAULT ''
);
-- attention_alerts records each attention alert the first time any server raises it, so replicas
-- sharing the database alert once between them. A row outlives its condition by a week at most.
CREATE TABLE IF NOT EXISTS attention_alerts (
	alert_key TEXT PRIMARY KEY,
	raised_at TEXT NOT NULL DEFAULT ''
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
	created_by  TEXT NOT NULL DEFAULT '',
	last_error  TEXT NOT NULL DEFAULT '',
	rrule       TEXT NOT NULL DEFAULT '',
	spring_forward TEXT NOT NULL DEFAULT '',
	-- Why the most recent fire was skipped, and how many fires in a row ending with it were. A
	-- fire whose inventory matched no hosts is skipped rather than failed.
	last_skip     TEXT NOT NULL DEFAULT '',
	skipped_fires INTEGER NOT NULL DEFAULT 0
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
	kind         TEXT NOT NULL DEFAULT '',
	created_by      TEXT NOT NULL DEFAULT '',
	created_by_type TEXT NOT NULL DEFAULT ''
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
	limit_pattern  TEXT NOT NULL DEFAULT '',
	use_fact_cache INTEGER NOT NULL DEFAULT 0,
	fact_cache_timeout INTEGER NOT NULL DEFAULT 0,
	allow_callbacks INTEGER NOT NULL DEFAULT 0,
	host_config_key TEXT NOT NULL DEFAULT '',
	callback_limit TEXT NOT NULL DEFAULT '',
	awx_callback   INTEGER NOT NULL DEFAULT 0
);
-- awx_callback_bindings ties an AWX job template id to the template an import created from that AWX
-- job template, so a host's boot script that still posts to AWX's callback address reaches it. Only
-- an import of the same AWX object, by organization and name, points a binding at another template.
-- A binding keeps the id of a deleted template, which is never reused, so the address answers gone.
CREATE TABLE IF NOT EXISTS awx_callback_bindings (
	awx_id         INTEGER PRIMARY KEY,
	template_id    TEXT NOT NULL DEFAULT '',
	awx_org        TEXT NOT NULL DEFAULT '',
	awx_name       TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	last_called_at TEXT
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
	created_by        TEXT NOT NULL DEFAULT '',
	review_provider   TEXT NOT NULL DEFAULT '',
	review_api_url    TEXT NOT NULL DEFAULT '',
	review_repository TEXT NOT NULL DEFAULT '',
	review_credential_id TEXT NOT NULL DEFAULT '',
	review_allow_forks INTEGER NOT NULL DEFAULT 0,
	-- Why the most recent delivery started no run, and when it arrived. Cleared by the next fire.
	last_error TEXT NOT NULL DEFAULT '',
	last_error_at TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_triggers_hash ON triggers(token_hash);
CREATE TABLE IF NOT EXISTS notification_targets (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL DEFAULT '',
	description  TEXT NOT NULL DEFAULT '',
	org_id       TEXT NOT NULL DEFAULT '',
	kind         TEXT NOT NULL,
	recipient    TEXT NOT NULL DEFAULT '',
	url_hint     TEXT NOT NULL DEFAULT '',
	key_set      INTEGER NOT NULL DEFAULT 0,
	needs_secret INTEGER NOT NULL DEFAULT 0,
	sealed_url   TEXT NOT NULL DEFAULT '',
	sealed_key   TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	created_by   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_notification_targets_created ON notification_targets(created_at, id);
CREATE TABLE IF NOT EXISTS notification_attachments (
	id              TEXT PRIMARY KEY,
	notification_id TEXT NOT NULL,
	object_kind     TEXT NOT NULL,
	object_id       TEXT NOT NULL,
	event           TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	created_by      TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_notification_attachments_unique
	ON notification_attachments(notification_id, object_kind, object_id, event);
CREATE INDEX IF NOT EXISTS idx_notification_attachments_object
	ON notification_attachments(object_kind, object_id, created_at, id);
-- A run's notification events in the order the run reached them: seq is the run's own sequence,
-- which is what delivery is ordered by. The snapshot is the run as it stood at the event, redacted
-- the way everything sent off the host is. Times in these two tables are Unix milliseconds, so the
-- claim's comparisons are exact without depending on how a text timestamp sorts.
CREATE TABLE IF NOT EXISTS notification_events (
	run_id     TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	event      TEXT NOT NULL,
	branch     TEXT NOT NULL DEFAULT '',
	dedupe_key TEXT NOT NULL,
	snapshot   TEXT NOT NULL,
	created_ms INTEGER NOT NULL,
	PRIMARY KEY (run_id, seq)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_notification_events_dedupe
	ON notification_events(run_id, dedupe_key);
-- One row per event and target, keyed by target, run, and sequence, so no retry and no second
-- worker can queue or claim the same delivery twice.
CREATE TABLE IF NOT EXISTS notification_deliveries (
	notification_id TEXT NOT NULL,
	run_id          TEXT NOT NULL,
	seq             INTEGER NOT NULL,
	event           TEXT NOT NULL,
	branch          TEXT NOT NULL DEFAULT '',
	follows         TEXT NOT NULL DEFAULT '',
	target_name     TEXT NOT NULL DEFAULT '',
	target_kind     TEXT NOT NULL DEFAULT '',
	status          TEXT NOT NULL,
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt_ms INTEGER NOT NULL DEFAULT 0,
	claimed_by      TEXT NOT NULL DEFAULT '',
	claim_until_ms  INTEGER NOT NULL DEFAULT 0,
	last_error      TEXT NOT NULL DEFAULT '',
	note            TEXT NOT NULL DEFAULT '',
	created_ms      INTEGER NOT NULL,
	finished_ms     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (notification_id, run_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_due
	ON notification_deliveries(status, next_attempt_ms);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_run
	ON notification_deliveries(run_id, seq);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_target
	ON notification_deliveries(notification_id, created_ms);
` + runEndsSchema + `
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
-- A run has one outcome entry, and every append of one first asks whether the chain already holds
-- it. The index holds outcome entries only, so that question costs a lookup instead of a scan of
-- the chain.
CREATE INDEX IF NOT EXISTS idx_audit_outcome ON audit_entries(path) WHERE method = 'RUN';
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
	reversibility    TEXT NOT NULL DEFAULT '',
	effect           TEXT NOT NULL DEFAULT '',
	distinct_approver INTEGER NOT NULL DEFAULT 0,
	created_at       TEXT NOT NULL,
	require_reason   TEXT NOT NULL DEFAULT ''
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
	created_at     TEXT NOT NULL,
	kind           TEXT NOT NULL DEFAULT '',
	host_filter    TEXT NOT NULL DEFAULT '',
	input_ids      TEXT NOT NULL DEFAULT '',
	source_vars    TEXT NOT NULL DEFAULT '',
	limit_pattern  TEXT NOT NULL DEFAULT ''
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
	files      TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL DEFAULT '0001-01-01T00:00:00Z',
	-- Where the type was defined: empty for one created here, awx for one an AWX import brought
	-- across, which may keep a file no injector references.
	origin     TEXT NOT NULL DEFAULT ''
);
-- The workload identity federation signing keys. The private half arrives sealed under the
-- encryption key, so the column holds ciphertext, and every process on the database signs with and
-- publishes the same keys. Each key records when it was created, when it starts signing, when it
-- stops, and when it leaves the published set, empty for a time that has not been set; a removed
-- key keeps its row, with sealed emptied, as the record of when it was trusted.
CREATE TABLE IF NOT EXISTS federation_keys (
	id           TEXT PRIMARY KEY,
	algorithm    TEXT NOT NULL DEFAULT 'RS256',
	public_key   TEXT NOT NULL DEFAULT '',
	sealed       TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL DEFAULT '',
	retired_at   TEXT NOT NULL DEFAULT '',
	activated_at TEXT NOT NULL DEFAULT '',
	removed_at   TEXT NOT NULL DEFAULT ''
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
-- What each pull request review report last told its pull request, and the claim and retry state
-- that sends each report once across every process sharing the database. A plan's record is keyed
-- by its run id and goes when retention deletes the run.
CREATE TABLE IF NOT EXISTS review_reports (
	id              TEXT PRIMARY KEY,
	kind            TEXT NOT NULL DEFAULT 'plan',
	run_id          TEXT NOT NULL DEFAULT '',
	trigger_id      TEXT NOT NULL DEFAULT '',
	pull_request    INTEGER NOT NULL DEFAULT 0,
	commit_sha      TEXT NOT NULL DEFAULT '',
	reason          TEXT NOT NULL DEFAULT '',
	receipt         TEXT NOT NULL DEFAULT '',
	phase           TEXT NOT NULL DEFAULT '',
	held_by         TEXT NOT NULL DEFAULT '',
	status_state    TEXT NOT NULL DEFAULT '',
	comment_sha256  TEXT NOT NULL DEFAULT '',
	recorded_sha256 TEXT NOT NULL DEFAULT '',
	reported_at     TEXT NOT NULL DEFAULT '',
	done            INTEGER NOT NULL DEFAULT 0,
	version         INTEGER NOT NULL DEFAULT 0,
	claimed_by      TEXT NOT NULL DEFAULT '',
	claimed_until   TEXT NOT NULL DEFAULT '',
	attempts        INTEGER NOT NULL DEFAULT 0,
	failing_since   TEXT NOT NULL DEFAULT '',
	retry_at        TEXT NOT NULL DEFAULT '',
	last_error      TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL DEFAULT '',
	-- Where the report goes, fixed when the record is made, so a trigger pointed elsewhere later
	-- cannot carry it to a pull request it never planned. Empty on a record an earlier release made.
	provider        TEXT NOT NULL DEFAULT '',
	api_url         TEXT NOT NULL DEFAULT '',
	repository      TEXT NOT NULL DEFAULT '',
	-- The commit status context the record's first status was set under, which it keeps.
	status_context  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_review_reports_pending ON review_reports(done, created_at, id);
CREATE INDEX IF NOT EXISTS idx_review_reports_lane ON review_reports(trigger_id, pull_request);

-- One row per approval or denial a person made, and per correction appended to one. The reason is
-- the masked text and the random value its chain commitment hides it under, stored together and
-- removed together by a redaction, which keeps the commitment and records why in the redaction
-- column.
CREATE TABLE IF NOT EXISTS run_decisions (
	id                TEXT PRIMARY KEY,
	kind              TEXT NOT NULL DEFAULT '',
	decision_id       TEXT NOT NULL DEFAULT '',
	run_id            TEXT NOT NULL DEFAULT '',
	step_run_id       TEXT NOT NULL DEFAULT '',
	verdict           TEXT NOT NULL DEFAULT '',
	recorded_at       TEXT NOT NULL DEFAULT '',
	actor             TEXT NOT NULL DEFAULT '',
	actor_type        TEXT NOT NULL DEFAULT '',
	on_behalf_of      TEXT NOT NULL DEFAULT '',
	reason_text       TEXT NOT NULL DEFAULT '',
	reason_random     TEXT NOT NULL DEFAULT '',
	reason_commitment TEXT NOT NULL DEFAULT '',
	reason_masked     INTEGER NOT NULL DEFAULT 0,
	redaction         TEXT NOT NULL DEFAULT '',
	sod               TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_run_decisions_run ON run_decisions(run_id);
`

// createPrivate creates the database file readable by its owner alone when it does not exist yet.
//
// SQLite created it with the process umask, which on a stock system is world-readable, while the
// file holds hashed tokens, sealed credentials, stored inventory content, and the audit chain. SQLite
// gives the WAL and shared-memory files the database file's permissions, so creating the file first
// is enough to cover all three. An existing file is left as its owner set it, and an in-memory
// database has no file to create.
func createPrivate(path string) error {
	if path == "" || strings.HasPrefix(path, ":memory:") || strings.Contains(path, "mode=memory") {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create database file: %w", err)
	}
	return f.Close()
}

// Open opens the SQLite database at path, applies the schema, and returns the bundled stores.
func Open(path string) (*DB, error) {
	// Every transaction takes the write lock when it begins rather than upgrading into it.
	//
	// SetMaxOpenConns(1) below serializes this process, but a SQLite install is one FILE, not one
	// process: the quickstart has the operator run a CLI command against the database the server is
	// serving, and `switchtender examples`, `token new`, `audit anchor` and `import` all write it.
	// A deferred transaction reads first and upgrades to a write, and when another process advanced
	// the WAL in between, SQLite answers SQLITE_BUSY_SNAPSHOT (261), which busy_timeout is
	// documented NOT to retry because retrying could not produce a consistent read. The server then
	// answered 503 on every mutation and the CLI exited non-zero with "database is locked", which is
	// a database-corruption message for what is ordinary two-process use.
	//
	// An immediate transaction takes the lock up front, so there is no upgrade to lose, and
	// busy_timeout does apply to acquiring it: the second writer waits its turn instead of failing.
	if err := createPrivate(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection serializes every reader and writer inside this process.
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
	if err := healFedKeyLifecycle(db); err != nil {
		_ = db.Close()
		return nil, err
	}
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
	if err := upgradeIdempotencyKeys(db); err != nil {
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
	if err := migrateOrgMembers(db); err != nil {
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
	if err := migrateCredentialTypes(db); err != nil {
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
	if err := pinScheduleZones(db); err != nil {
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
		credentials:   &credentialStore{db: split},
		credTypes:     &credTypeStore{db: split},
		fedKeys:       &fedKeyStore{db: split},
		projects:      &projectStore{db: split},
		templates:     &templateStore{db: split},
		users:         &userStore{db: split},
		inventories:   &inventoryStore{db: split},
		audits:        &auditStore{db: split},
		invSources:    &invSourceStore{db: split},
		triggers:      &triggerStore{db: split},
		notifications: &notificationStore{db: split},
		teams:         &teamStore{db: split},
		orgs:          &orgStore{db: split},
		grants:        &grantStore{db: split},
		policies:      &policyStore{db: split},
		factCache:     &factCacheStore{db: split},
		reviews:       &reviewStore{db: split}}, nil
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
	for _, column := range []string{"source", "source_id", "actor", "rerun_of", "labels", "steps",
		"warning", "audit_receipt", "held_by_policy", "tags", "skip_tags", "claim_secret",
		"actor_type", "approved_spec_digest", "approved_spec_binding", "pinned_commit", "policy_set",
		"actor_user_id", "template_id", "inventory_resolution", "sealed_vars", "git_ref",
		"dry_run_scans", "hold_note", "sealed_digests", "policy_notes", "inventory_check",
		"initiator", "require_reason", "inventory_snapshot", "inventory_sealed", "resolved_hosts",
		"plan_sha256", "plan_sealed", "image_digest", "decision_id", "decision_claim"} {
		if _, err := db.Exec(
			"ALTER TABLE runs ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("add %s column: %w", column, err)
		}
	}
	// When a run last re-entered the queue has no value until it does, so it is the one run column
	// that stays NULL rather than empty.
	if _, err := db.Exec("ALTER TABLE runs ADD COLUMN queued_at TEXT"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add queued_at column: %w", err)
	}
	for _, column := range []string{"verbosity", "forks", "diff_mode", "distinct_approver",
		"use_fact_cache", "fact_cache_timeout"} {
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
//
// The copy skips memberships whose team does not exist. Those rows are exactly what the
// pre-constraint table permitted, since AddMember on a table with no foreign key accepted any team
// id at all, and they are what the constraint was added to stop. Copying one into a table that
// declares the reference aborts the insert, and the abort reaches Open, so a single membership
// nobody can see would stop the whole install from starting: no server, no audit chain, no runs.
// Dropping the orphan is the safe direction, since a membership pointing at no team grants access
// to nothing and a fresh install would have refused to record it.
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
		"INSERT INTO team_members_new SELECT team_id, user_id FROM team_members " +
			"WHERE team_id IN (SELECT id FROM teams)",
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

// migrateOrgMembers rebuilds org_members with the foreign key its CREATE promises, for the same
// reason migrateTeamMembers rebuilds the table beside it. org_members declares the identical
// ON DELETE CASCADE reference, so without this rebuild a database whose copy of the table predates
// the reference enforces nothing forever: a membership can name an organization that does not
// exist, and deleting an organization leaves its memberships behind, while every fresh install
// refuses the first and cascades the second. Organization membership carries a role, so the gap
// let an upgraded install hold an admin grant on nothing at all.
//
// The copy drops memberships whose organization is missing, which the pre-constraint table
// permitted and the constraint refuses, so one unreferenced row cannot abort Open.
func migrateOrgMembers(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='org_members'").Scan(&exists); err != nil {
		return fmt.Errorf("org_members migration: %w", err)
	}
	if exists == 0 {
		return nil
	}
	var fks int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM pragma_foreign_key_list('org_members')").Scan(&fks); err != nil {
		return fmt.Errorf("org_members migration: %w", err)
	}
	if fks > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("org_members migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE org_members_new (
	org_id  TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL,
	role    TEXT NOT NULL DEFAULT 'member',
	PRIMARY KEY (org_id, user_id)
)`,
		"INSERT INTO org_members_new SELECT org_id, user_id, role FROM org_members " +
			"WHERE org_id IN (SELECT id FROM orgs)",
		"DROP TABLE org_members",
		"ALTER TABLE org_members_new RENAME TO org_members",
		// Dropping the old table dropped its index, and the schema exec that would recreate it has
		// already run this open, so the rebuild recreates it itself.
		"CREATE INDEX IF NOT EXISTS idx_org_members_user ON org_members(user_id)",
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("org_members migration: %w", err)
		}
	}
	return tx.Commit()
}

// migrateCredentialTypes moves credential_types.created_at from the UnixNano integer it first held
// to the text form every other stored timestamp uses. The read path now parses text, so an
// untouched integer column would fail every read of the table after an upgrade. SQLite cannot
// change a column's declared type, so the table is rebuilt in the standard copy and swap shape,
// and the rebuild runs only while the column is still declared INTEGER.
//
// Each stored integer is converted through the same time.Unix it was read through before, so an
// existing type keeps the creation time it has been reporting all along. A type stamped with the
// overflowed zero time keeps the eighteenth-century value it already shows, and unlike before it
// can now be corrected by saving the type again.
func migrateCredentialTypes(db *sql.DB) error {
	var typ string
	err := db.QueryRow(
		"SELECT type FROM pragma_table_info('credential_types') WHERE name='created_at'").Scan(&typ)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("credential_types migration: %w", err)
	}
	if !strings.EqualFold(typ, "INTEGER") {
		return nil
	}
	stamps, err := readCredTypeNanos(db)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("credential_types migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE credential_types_new (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	fields     TEXT NOT NULL DEFAULT '[]',
	env        TEXT NOT NULL DEFAULT '{}',
	extra_vars TEXT NOT NULL DEFAULT '{}',
	files      TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL DEFAULT '0001-01-01T00:00:00Z',
	origin     TEXT NOT NULL DEFAULT ''
)`,
		"INSERT INTO credential_types_new (id, name, fields, env, extra_vars, files, origin) " +
			"SELECT id, name, fields, env, extra_vars, files, origin FROM credential_types",
		"DROP TABLE credential_types",
		"ALTER TABLE credential_types_new RENAME TO credential_types",
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("credential_types migration: %w", err)
		}
	}
	for id, nanos := range stamps {
		if _, err := tx.Exec("UPDATE credential_types SET created_at=? WHERE id=?",
			sqlutil.FormatTime(time.Unix(0, nanos)), id); err != nil {
			return fmt.Errorf("credential_types migration: %w", err)
		}
	}
	return tx.Commit()
}

// readCredTypeNanos reads the stored UnixNano creation stamp of every credential type, keyed by id,
// before the rebuild replaces the column that holds them.
func readCredTypeNanos(db *sql.DB) (map[string]int64, error) {
	rows, err := db.Query("SELECT id, created_at FROM credential_types")
	if err != nil {
		return nil, fmt.Errorf("credential_types migration: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var nanos int64
		if err := rows.Scan(&id, &nanos); err != nil {
			return nil, fmt.Errorf("credential_types migration: %w", err)
		}
		out[id] = nanos
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credential_types migration: %w", err)
	}
	return out, nil
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
		callbackLiveIndex,
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
	for _, column := range []string{"actor_kind", "actor", "min_risk", "effect", "queue",
		"reversibility"} {
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
		"ALTER TABLE templates ADD COLUMN use_fact_cache INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE templates ADD COLUMN fact_cache_timeout INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE templates ADD COLUMN allow_callbacks INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE templates ADD COLUMN host_config_key TEXT NOT NULL DEFAULT ''",
	} {
		if _, err := db.Exec(stmt); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate templates: %w", err)
		}
	}
	return nil
}

// migrateSchedules adds the columns a schedules table created before them lacks: the timezone,
// whose empty default is server-local so an older schedule fires exactly as it did, the reason the
// last fire started no run, whose empty default says nothing failed, the recurrence rule, whose
// empty default leaves every existing schedule on its cron expression, the spring-forward setting,
// whose empty default takes each kind of schedule's own default, and the skip reason and count,
// whose empty defaults say no fire was skipped.
func migrateSchedules(db *sql.DB) error {
	for _, column := range []string{
		"timezone TEXT NOT NULL DEFAULT ''",
		"last_error TEXT NOT NULL DEFAULT ''",
		"rrule TEXT NOT NULL DEFAULT ''",
		"spring_forward TEXT NOT NULL DEFAULT ''",
		"last_skip TEXT NOT NULL DEFAULT ''",
		"skipped_fires INTEGER NOT NULL DEFAULT 0",
	} {
		if _, err := db.Exec("ALTER TABLE schedules ADD COLUMN " + column); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate schedules: %w", err)
		}
	}
	return nil
}

// migrateInventories adds the owning-organization column to an inventories table created before object
// tenancy. Empty is the unowned default, so an inventory made before this column stays global. Adding
// a column that already exists is the ordinary case for a current database and is treated as success.
//
// The composition columns follow the same rule: every one defaults to empty, which is the static
// kind, so an inventory made before smart and constructed inventories existed reads back unchanged.
func migrateInventories(db *sql.DB) error {
	for _, column := range []string{"org_id", "kind", "host_filter", "input_ids", "source_vars",
		"limit_pattern"} {
		if _, err := db.Exec(
			"ALTER TABLE inventories ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate inventories: %w", err)
		}
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
