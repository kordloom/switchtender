package pgstore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/run"
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
	approved_spec_binding TEXT NOT NULL DEFAULT '',
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
	-- The plan file a drift check saved, ciphertext only, kept past the check's end so a reconcile
	-- carries out exactly that plan, and dropped when a newer check of the same target lands.
	drift_plan_sealed TEXT NOT NULL DEFAULT '',
	-- Whether a Terraform or OpenTofu apply's own submission asked for approval, so the apply its
	-- plan proposes is held. Set when the run is created and never cleared.
	approval_requested INTEGER NOT NULL DEFAULT 0,
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
	-- When the run came to owe its outcome to the audit chain, in Unix milliseconds by the
	-- database's clock, and zero while it owes nothing. The runs_outcome_owed trigger sets it, and
	-- the commit that puts the outcome on the chain clears it.
	outcome_owed_ms BIGINT NOT NULL DEFAULT 0
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
ALTER TABLE runs ADD COLUMN IF NOT EXISTS approved_spec_binding TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS rerun_of TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS labels TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS warning TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS audit_receipt TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS held_by_policy TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS distinct_approver INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS pinned_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS policy_set TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS actor_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS plan_destroys INTEGER;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS template_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS inventory_resolution TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS sealed_vars TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS git_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS dry_run_scans TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS hold_note TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS sealed_digests TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS policy_notes TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS inventory_check TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS initiator TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS require_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS inventory_snapshot TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS inventory_sealed TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS resolved_hosts TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS plan_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS plan_sealed TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS drift_plan_sealed TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS approval_requested INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS image_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS decision_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS decision_claim TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS queued_at TEXT;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS tags TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS skip_tags TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS verbosity INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS forks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS diff_mode INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS claim_secret TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS use_fact_cache INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS fact_cache_timeout INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_idempotency_key ON runs(idempotency_key) WHERE idempotency_key <> '';
-- At most one unfinished provisioning callback run per template and host, the guard against two
-- replicas launching one host's callback twice. See run.LiveCallback.
CREATE UNIQUE INDEX IF NOT EXISTS ` + callbackLiveIndex + ` ON runs(source_id, limit_pattern)
	WHERE source = '` + run.SourceCallback + `' AND parent_id IS NULL AND ` + nonTerminalRun + `;
-- A top-level run comes to owe its outcome to the audit chain in the write that makes it terminal,
-- whichever statement that is and whichever replica or release runs it, so an append the chain
-- refused or a process that died before it appended cannot lose the outcome for good: the janitor
-- commits what is still owed. A run stored already finished owes nothing, since no process saw it
-- finish.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS outcome_owed_ms BIGINT NOT NULL DEFAULT 0;
CREATE OR REPLACE FUNCTION runs_outcome_owed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF NEW.parent_id IS NULL AND NEW.` + terminalRun + ` AND OLD.` + nonTerminalRun + ` THEN
		NEW.outcome_owed_ms := (extract(epoch FROM clock_timestamp()) * 1000)::bigint;
	END IF;
	RETURN NEW;
END
$$;
DROP TRIGGER IF EXISTS runs_outcome_owed ON runs;
CREATE TRIGGER runs_outcome_owed BEFORE UPDATE OF status ON runs
	FOR EACH ROW EXECUTE FUNCTION runs_outcome_owed();
CREATE INDEX IF NOT EXISTS idx_runs_outcome_owed ON runs(outcome_owed_ms) WHERE outcome_owed_ms > 0;
CREATE TABLE IF NOT EXISTS run_logs (
	seq    BIGSERIAL PRIMARY KEY,
	run_id TEXT NOT NULL,
	chunk  BYTEA NOT NULL
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
-- Fixed-window allowances every replica spends from, such as a provisioning callback's per-address
-- request and wrong-key budgets. window_end is in Unix nanoseconds, and a row whose window has
-- closed is deleted when the next window anywhere opens.
CREATE TABLE IF NOT EXISTS budgets (
	key        TEXT PRIMARY KEY,
	window_end BIGINT NOT NULL DEFAULT 0,
	spent      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_budgets_window_end ON budgets(window_end);

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
	created_by  TEXT NOT NULL DEFAULT '',
	last_error  TEXT NOT NULL DEFAULT '',
	rrule       TEXT NOT NULL DEFAULT '',
	spring_forward TEXT NOT NULL DEFAULT '',
	-- Why the most recent fire was skipped, and how many fires in a row ending with it were. A
	-- fire whose inventory matched no hosts is skipped rather than failed.
	last_skip     TEXT NOT NULL DEFAULT '',
	skipped_fires INTEGER NOT NULL DEFAULT 0,
	-- The occurrence a claim took for a fire whose run is not yet known to exist, and when it was
	-- marked, by the database clock in Unix milliseconds. A sweep fires it again once it has been
	-- marked too long, so a server that stops mid-fire leaves the occurrence late rather than lost.
	inflight_at   TEXT,
	inflight_ms   BIGINT NOT NULL DEFAULT 0
);
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS rrule TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS spring_forward TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS last_skip TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS skipped_fires INTEGER NOT NULL DEFAULT 0;
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS inflight_at TEXT;
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS inflight_ms BIGINT NOT NULL DEFAULT 0;
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
	kind         TEXT NOT NULL DEFAULT '',
	created_by      TEXT NOT NULL DEFAULT '',
	created_by_type TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tokens_hash ON tokens(hash);
ALTER TABLE tokens ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN IF NOT EXISTS created_by_type TEXT NOT NULL DEFAULT '';
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
	limit_pattern  TEXT NOT NULL DEFAULT '',
	use_fact_cache INTEGER NOT NULL DEFAULT 0,
	fact_cache_timeout INTEGER NOT NULL DEFAULT 0,
	allow_callbacks INTEGER NOT NULL DEFAULT 0,
	host_config_key TEXT NOT NULL DEFAULT '',
	callback_limit TEXT NOT NULL DEFAULT '',
	awx_callback   INTEGER NOT NULL DEFAULT 0
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
ALTER TABLE templates ADD COLUMN IF NOT EXISTS use_fact_cache INTEGER NOT NULL DEFAULT 0;
-- Zero serves cached facts however old they are, which is what AWX does by default.
ALTER TABLE templates ADD COLUMN IF NOT EXISTS fact_cache_timeout INTEGER NOT NULL DEFAULT 0;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS allow_callbacks INTEGER NOT NULL DEFAULT 0;
-- The provisioning callback key, sealed with the server key. It is never stored in the clear.
ALTER TABLE templates ADD COLUMN IF NOT EXISTS host_config_key TEXT NOT NULL DEFAULT '';
-- Empty keeps a callback inside the template's limit, which is the default for every template.
ALTER TABLE templates ADD COLUMN IF NOT EXISTS callback_limit TEXT NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS awx_callback INTEGER NOT NULL DEFAULT 0;
-- awx_callback_bindings ties an AWX job template id to the template an import created from that AWX
-- job template, so a host's boot script that still posts to AWX's callback address reaches it. Only
-- an import of the same AWX object, by organization and name, points a binding at another template.
-- A binding keeps the id of a deleted template, which is never reused, so the address answers gone.
CREATE TABLE IF NOT EXISTS awx_callback_bindings (
	awx_id         BIGINT PRIMARY KEY,
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
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS review_provider TEXT NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS review_api_url TEXT NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS review_repository TEXT NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS review_credential_id TEXT NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS review_allow_forks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS last_error_at TEXT;
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
	seq        BIGINT NOT NULL,
	event      TEXT NOT NULL,
	branch     TEXT NOT NULL DEFAULT '',
	dedupe_key TEXT NOT NULL,
	snapshot   TEXT NOT NULL,
	created_ms BIGINT NOT NULL,
	PRIMARY KEY (run_id, seq)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_notification_events_dedupe
	ON notification_events(run_id, dedupe_key);
-- One row per event and target, keyed by target, run, and sequence, so no retry and no second
-- worker can queue or claim the same delivery twice.
CREATE TABLE IF NOT EXISTS notification_deliveries (
	notification_id TEXT NOT NULL,
	run_id          TEXT NOT NULL,
	seq             BIGINT NOT NULL,
	event           TEXT NOT NULL,
	branch          TEXT NOT NULL DEFAULT '',
	follows         TEXT NOT NULL DEFAULT '',
	target_name     TEXT NOT NULL DEFAULT '',
	target_kind     TEXT NOT NULL DEFAULT '',
	status          TEXT NOT NULL,
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt_ms BIGINT NOT NULL DEFAULT 0,
	claimed_by      TEXT NOT NULL DEFAULT '',
	claim_until_ms  BIGINT NOT NULL DEFAULT 0,
	last_error      TEXT NOT NULL DEFAULT '',
	note            TEXT NOT NULL DEFAULT '',
	created_ms      BIGINT NOT NULL,
	finished_ms     BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (notification_id, run_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_due
	ON notification_deliveries(status, next_attempt_ms);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_run
	ON notification_deliveries(run_id, seq);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_target
	ON notification_deliveries(notification_id, created_ms);
` + runEndsSchema + `
` + runEventsSchema + `
` + secretLeasesSchema + `
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
-- The grade a rule holds on. Absent, a policy saved with it loaded back without it and held
-- nothing, while the API answered 200 and the page listed the rule as though it were in force.
ALTER TABLE policies ADD COLUMN IF NOT EXISTS reversibility TEXT NOT NULL DEFAULT '';
ALTER TABLE policies ADD COLUMN IF NOT EXISTS effect TEXT NOT NULL DEFAULT '';
ALTER TABLE policies ADD COLUMN IF NOT EXISTS queue TEXT NOT NULL DEFAULT '';
ALTER TABLE policies ADD COLUMN IF NOT EXISTS require_reason TEXT NOT NULL DEFAULT '';
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
ALTER TABLE inventories ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';
-- A smart or constructed inventory: empty in every column is the static kind, so an inventory
-- made before composition existed reads back unchanged.
ALTER TABLE inventories ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';
ALTER TABLE inventories ADD COLUMN IF NOT EXISTS host_filter TEXT NOT NULL DEFAULT '';
ALTER TABLE inventories ADD COLUMN IF NOT EXISTS input_ids TEXT NOT NULL DEFAULT '';
ALTER TABLE inventories ADD COLUMN IF NOT EXISTS source_vars TEXT NOT NULL DEFAULT '';
ALTER TABLE inventories ADD COLUMN IF NOT EXISTS limit_pattern TEXT NOT NULL DEFAULT '';
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
	files      TEXT NOT NULL DEFAULT '{}',
	created_at BIGINT NOT NULL DEFAULT 0,
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
	version         BIGINT NOT NULL DEFAULT 0,
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

// ErrForeignSchema is returned when the database holds tables of another application's under the
// names this schema uses.
var ErrForeignSchema = errors.New("this database belongs to another application")

// Open connects to the PostgreSQL database at dsn, applies the schema, and returns the bundled
// stores.
func Open(dsn string) (*DB, error) {
	db, pin, err := openPinnable(dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	// Cap the pool so a burst of API reads and SSE streams cannot exhaust the server's
	// max_connections, and recycle connections so a load balancer or pooler can rebalance.
	// Initializing a brand-new schema is a Team feature; opening a database that already holds
	// one is never gated, in any license state, because a lapsed license must take nothing. The
	// probe runs before the migration lock so a Community refusal leaves the database untouched.
	var initialized bool
	if err := db.QueryRow("SELECT to_regclass('runs') IS NOT NULL").Scan(&initialized); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("probe postgres schema: %w", err)
	}
	// A table named runs is not proof the schema is this product's. Another application's runs
	// table passed the probe, which skipped the gate above and then failed the migration with an
	// error about a column nobody had heard of. The columns every version of this schema has had
	// are what identify it.
	if initialized {
		var ours int
		if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'runs'
AND column_name IN ('playbook', 'inventory', 'status')`).Scan(&ours); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("probe postgres schema: %w", err)
		}
		if ours != 3 {
			_ = db.Close()
			return nil, fmt.Errorf("%w: it holds a table named runs that SwitchTender did not create. "+
				"Point --db at a database of SwitchTender's own", ErrForeignSchema)
		}
	}
	if !initialized {
		if aerr := license.Allow(license.FeaturePostgresInit); aerr != nil {
			_ = db.Close()
			return nil, aerr
		}
	}
	db.SetMaxOpenConns(poolMaxOpen)
	db.SetMaxIdleConns(poolMaxIdle)
	db.SetConnMaxLifetime(poolMaxLifetime)
	db.SetConnMaxIdleTime(poolMaxIdleTime)
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
	//
	// A start with nothing to migrate skips all of that. The question is asked of the catalog, which
	// locks nothing the cluster is using, and a question that did not answer is not an answer that
	// the schema is current: the error means the migration runs, since skipping on an unanswered
	// question would leave a database unmigrated while running one that was not needed costs a
	// single transaction on a start.
	current, cerr := schemaIsCurrent(db)
	if cerr != nil || !current {
		if err := migrate(db); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := normalizeScheduleTimes(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := pinScheduleZones(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{db: db, runs: &store{db: db}, schedules: &scheduleStore{db: db}, tokens: &tokenStore{db: db},
		pin:           pin,
		credentials:   &credentialStore{db: db},
		credTypes:     &credTypeStore{db: db},
		fedKeys:       &fedKeyStore{db: db},
		projects:      &projectStore{db: db},
		templates:     &templateStore{db: db},
		users:         &userStore{db: db},
		inventories:   &inventoryStore{db: db},
		audits:        &auditStore{db: db},
		invSources:    &invSourceStore{db: db},
		triggers:      &triggerStore{db: db},
		notifications: &notificationStore{db: db},
		teams:         &teamStore{db: db},
		orgs:          &orgStore{db: db},
		grants:        &grantStore{db: db},
		policies:      &policyStore{db: db},
		factCache:     &factCacheStore{db: db},
		reviews:       &reviewStore{db: db}}, nil
}

// pgUniqueViolation is the PostgreSQL SQLSTATE code for a unique constraint or index violation.
const pgUniqueViolation = "23505"

// isKeyConflict reports whether a keyed insert failed because another run already holds the
// idempotency key. A runs insert carrying a key can only trip the idempotency-key unique index, its
// primary-key conflict being absorbed by ON CONFLICT(id), so a unique violation on one is that race
// schemaIsCurrent reports whether applying the schema would change nothing, so the caller can skip
// the migration entirely.
//
// The migration issues ADD COLUMN IF NOT EXISTS for every declared column and then executes the
// schema, and an ADD COLUMN that changes nothing still takes AccessExclusiveLock. Every process
// calls Open, workers included, so on an install whose schema is already current that lock was taken
// on every table by every process that started. PostgreSQL breaks the resulting cycle by killing one
// of the two transactions, and about half the time the victim was an ordinary runtime write coming
// back as SQLSTATE 40P01 to a caller with no retry. Six nodes starting beside six writers cost 28 of
// 180 host summary writes. In production that is a rolling restart making healthy nodes lose
// history, which is the opposite of the direction the migration's own timeout comment intends to
// fail in.
//
// This asks the catalog instead, which takes no lock on anything the rest of the cluster is using,
// and a start that has nothing to migrate now takes no exclusive lock at all. A real upgrade still
// migrates and still contends, which is the one time contending is the right answer.
//
// What it checks is exactly what the schema can change: every table it declares exists, every column
// an ALTER could add exists, every index it declares exists, and every index it drops is gone. A
// column no ALTER could add is deliberately not checked, because the migration cannot create one
// either, and demanding it would say "not current" on every start forever.
func schemaIsCurrent(db *sql.DB) (bool, error) {
	columns, err := liveColumns(db)
	if err != nil {
		return false, err
	}
	for table, cols := range sqlutil.ParseSchemaColumns(schema) {
		live, ok := columns[strings.ToLower(table)]
		if !ok {
			return false, nil
		}
		for _, col := range cols {
			if !col.Addable() {
				continue
			}
			if !live[strings.ToLower(col.Name)] {
				return false, nil
			}
		}
	}
	indexes, err := liveIndexes(db)
	if err != nil {
		return false, err
	}
	declared := sqlutil.ParseSchemaIndexes(schema)
	for _, name := range declared.Created {
		if !indexes[name] {
			return false, nil
		}
	}
	for _, name := range declared.Dropped {
		if indexes[name] {
			return false, nil
		}
	}
	// The triggers that keep the owed ledgers are part of what the schema creates, and nothing above
	// would notice one missing: runs_outcome_owed marks a finished run's outcome owed to the audit
	// chain, runs_owe_end marks its end owed to its named notification targets, and runs_owe_start,
	// runs_owe_hold, and runs_owe_hold_new mark its start and its hold owed to them.
	var triggers int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT tgname) FROM pg_trigger
WHERE tgname IN ('runs_outcome_owed', 'runs_owe_end', 'runs_owe_start', 'runs_owe_hold',
	'runs_owe_hold_new') AND tgrelid = to_regclass('runs')`).
		Scan(&triggers); err != nil {
		return false, fmt.Errorf("read the live triggers: %w", err)
	}
	return triggers == 5, nil
}

// liveColumns returns the columns this database actually has, by table.
func liveColumns(db *sql.DB) (map[string]map[string]bool, error) {
	const q = `SELECT table_name, column_name FROM information_schema.columns
	WHERE table_schema = current_schema()`
	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("read the live columns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, fmt.Errorf("read the live columns: %w", err)
		}
		table, column = strings.ToLower(table), strings.ToLower(column)
		if out[table] == nil {
			out[table] = map[string]bool{}
		}
		out[table][column] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the live columns: %w", err)
	}
	return out, nil
}

// liveIndexes returns the index names this database actually has.
func liveIndexes(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query("SELECT indexname FROM pg_indexes WHERE schemaname = current_schema()")
	if err != nil {
		return nil, fmt.Errorf("read the live indexes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read the live indexes: %w", err)
		}
		out[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the live indexes: %w", err)
	}
	return out, nil
}

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
	if err := healFedKeyLifecycle(tx); err != nil {
		return err
	}
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
