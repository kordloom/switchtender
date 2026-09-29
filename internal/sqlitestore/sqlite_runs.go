package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// runColumns is the shared select list so every read scans the same columns in the same order.
const runColumns = `id, playbook, inventory, status, exit_code, error, created_at, started_at,
	ended_at, parent_id, shard_index, shard_count, limit_pattern, kind, step_name, step_index,
	retry_of, attempt, steps, extra_vars, outputs, claimed_by, claimed_at, cancel_requested,
	credential_ids, project_id, commit_sha, inventory_id, org_id, queue, tool, command, dry_run,
	proposed_from, intent, image, pull_credential_id, idempotency_key, timeout, notifications,
	source, source_id, actor, rerun_of, labels, warning, audit_receipt, held_by_policy,
	tags, skip_tags, verbosity, forks, diff_mode, claim_secret, actor_type, approved_spec_digest,
	distinct_approver, pinned_commit, policy_set, actor_user_id`

// Save inserts or replaces the run identified by r.ID. The cancel flag merges with MAX so a
// replace from a stale snapshot cannot erase a cancel another process just requested.
func (s *store) Save(ctx context.Context, r *run.Run) error {
	// Cleaned here so a stray byte from a tool's output cannot make this write behave differently
	// from the same write on PostgreSQL.
	r.Sanitize()
	const q = `
INSERT INTO runs
	(id, playbook, inventory, status, exit_code, error, created_at, started_at, ended_at,
	 parent_id, shard_index, shard_count, limit_pattern, kind, step_name, step_index, retry_of,
	 attempt, steps, extra_vars, outputs, claimed_by, claimed_at, cancel_requested, credential_ids,
	 project_id, commit_sha, inventory_id, org_id, queue, tool, command, dry_run, proposed_from, intent,
	 image, pull_credential_id, idempotency_key, timeout, notifications,
	 source, source_id, actor, rerun_of, labels, warning, audit_receipt, held_by_policy,
	 tags, skip_tags, verbosity, forks, diff_mode, claim_secret, actor_type, approved_spec_digest,
	 distinct_approver, pinned_commit, policy_set, actor_user_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
	cancel_requested=MAX(runs.cancel_requested, excluded.cancel_requested),
	credential_ids=excluded.credential_ids,
	project_id=excluded.project_id, commit_sha=excluded.commit_sha,
	inventory_id=excluded.inventory_id, org_id=excluded.org_id, queue=excluded.queue, tool=excluded.tool,
	command=excluded.command, dry_run=excluded.dry_run, proposed_from=excluded.proposed_from,
	intent=excluded.intent, image=excluded.image, pull_credential_id=excluded.pull_credential_id,
	idempotency_key=excluded.idempotency_key, timeout=excluded.timeout,
	notifications=excluded.notifications, source=excluded.source, source_id=excluded.source_id,
	actor=excluded.actor, rerun_of=excluded.rerun_of, labels=excluded.labels,
	warning=excluded.warning, audit_receipt=excluded.audit_receipt,
	held_by_policy=excluded.held_by_policy, tags=excluded.tags, skip_tags=excluded.skip_tags,
	verbosity=excluded.verbosity, forks=excluded.forks, diff_mode=excluded.diff_mode,
	claim_secret=excluded.claim_secret, actor_type=excluded.actor_type,
	approved_spec_digest=excluded.approved_spec_digest,
	distinct_approver=excluded.distinct_approver, pinned_commit=excluded.pinned_commit,
	policy_set=excluded.policy_set, actor_user_id=excluded.actor_user_id`
	_, err := s.db.ExecContext(ctx, q,
		r.ID, r.Playbook, r.Inventory, string(r.Status), sqlutil.NullInt(r.ExitCode), r.Error,
		sqlutil.FormatTime(r.CreatedAt), sqlutil.NullTime(r.StartedAt), sqlutil.NullTime(r.EndedAt),
		sqlutil.NullString(r.ParentID), sqlutil.NullInt(r.ShardIndex), sqlutil.NullInt(r.ShardCount), r.Limit,
		r.Kind, r.StepName, sqlutil.NullInt(r.StepIndex), sqlutil.NullString(r.RetryOf), r.Attempt,
		marshalSteps(r.Steps), sqlutil.JSONMap(r.ExtraVars), sqlutil.JSONMap(r.Outputs), r.ClaimedBy, sqlutil.NullTime(r.ClaimedAt),
		sqlutil.BoolToInt(r.CancelRequested), sqlutil.JoinIDs(r.CredentialIDs), r.ProjectID, r.CommitSHA,
		r.InventoryID, r.OrgID, r.Queue, r.Tool, r.Command, sqlutil.BoolToInt(r.DryRun), r.ProposedFrom, r.Intent,
		r.Image, r.PullCredentialID, r.IdempotencyKey, r.Timeout, marshalNotifications(r.Notifications),
		r.Source, r.SourceID, r.Actor, r.RerunOf, marshalLabels(r.Labels), r.Warning, r.AuditReceipt,
		r.HeldByPolicy, sqlutil.JoinIDs(r.Tags), sqlutil.JoinIDs(r.SkipTags), r.Verbosity, r.Forks,
		sqlutil.BoolToInt(r.DiffMode), r.ClaimSecret, r.ActorType, r.ApprovedSpecDigest,
		sqlutil.BoolToInt(r.RequireDistinctApprover), r.PinnedCommit, marshalPolicySet(r.PolicySet), r.ActorUserID,
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
	// Stored times are RFC 3339 UTC strings, so lexicographic comparison is chronological.
	if !filter.After.IsZero() {
		q += " AND created_at >= ?"
		args = append(args, sqlutil.FormatTime(filter.After))
	}
	if !filter.Before.IsZero() {
		q += " AND created_at < ?"
		args = append(args, sqlutil.FormatTime(filter.Before))
	}
	if filter.Source != "" {
		q += " AND source = ?"
		args = append(args, filter.Source)
	}
	if filter.Actor != "" {
		q += " AND actor = ?"
		args = append(args, filter.Actor)
	}
	if filter.SourceID != "" {
		q += " AND source_id = ?"
		args = append(args, filter.SourceID)
	}
	if filter.LabelKey != "" {
		// The key is looked up literally rather than compiled into a JSON path. A path treats a dot
		// as a step, so an ordinary key like app.tier or k8s.io/name matched nothing here and
		// matched correctly on Postgres, and the list came back empty with no error.
		q += " AND EXISTS (SELECT 1 FROM json_each(COALESCE(NULLIF(labels, ''), '{}'))" +
			" WHERE json_each.key = ? AND json_each.value = ?)"
		args = append(args, filter.LabelKey, filter.LabelValue)
	}
	if filter.Host != "" {
		q += " AND EXISTS (SELECT 1 FROM run_host_summary hs WHERE hs.run_id = runs.id AND hs.host = ?)"
		args = append(args, filter.Host)
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
//
// It is deliberately not memoized. A process-local cache is only as correct as the claim that this
// store is the only writer, and that claim is false in the documented split deployment: a worker
// opened on the same database file transitions runs through its own store, which no cache here can
// see. The status chips would then disagree with the run list beside them until some unrelated
// server-side write happened to invalidate, which on a read-mostly install is hours.
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
	// Stated rather than left to the default: SQLite orders nulls first and Postgres orders them
	// last, so "ORDER BY shard_index" alone made the two backends return different orders.
	const q = "SELECT " + runColumns +
		" FROM runs WHERE parent_id=? ORDER BY shard_index IS NULL, shard_index"
	return s.queryRuns(ctx, "list shards", q, parentID)
}

// Steps returns the pipeline step runs of a parent ordered by step index.
func (s *store) Steps(ctx context.Context, parentID string) ([]*run.Run, error) {
	// The same NULL-ordering hazard the shard listing above was fixed for, left here. SQLite sorts
	// NULLs first and PostgreSQL sorts them last, and step_index is nullable, so the two backends
	// returned different orders for the same rows. GET /runs/{id}/steps is not gated on kind, so
	// calling it on a split parent lists shard children, every one of which has a null step index.
	const q = "SELECT " + runColumns +
		" FROM runs WHERE parent_id=? ORDER BY step_index IS NULL, step_index, attempt"
	return s.queryRuns(ctx, "list steps", q, parentID)
}

// NonTerminal returns all runs, including shards, that are not in a terminal state.
func (s *store) NonTerminal(ctx context.Context) ([]*run.Run, error) {
	const q = "SELECT " + runColumns +
		" FROM runs WHERE status NOT IN ('succeeded', 'failed', 'canceled', 'interrupted', 'rejected')"
	return s.queryRuns(ctx, "list non-terminal runs", q)
}

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

// runIsDryRun reports whether a run was a check. A run with no row counts as an apply, since
// nothing proves it was a check and drift must not be invented from a missing record.
func runIsDryRun(ctx context.Context, q rowQuerier, runID string) (bool, error) {
	var dryRun int
	err := q.QueryRowContext(ctx, "SELECT dry_run FROM runs WHERE id=?", runID).Scan(&dryRun)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return dryRun != 0, nil
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

// terminalRun is the SQL predicate for a run that has finished and may be purged. It mirrors
// run.Status.Terminal(). It is stated as the set of terminal statuses rather than as "not pending or
// running", which silently treated pending_approval as finished and deleted runs that were waiting
// for an approver.
const terminalRun = "status IN ('succeeded', 'failed', 'canceled', 'interrupted', 'rejected')"

// nonTerminalRun is the SQL predicate for a run that still accepts auxiliary writes. It mirrors
// run.Status.Terminal, and fences a terminal run so a reclaimed-but-alive worker cannot append logs or
// events to a run that has already ended.
const nonTerminalRun = "status NOT IN ('succeeded', 'failed', 'canceled', 'interrupted', 'rejected')"

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
		labels   string
		steps    string
		tags     string
		skipTags string
		diffMode int
		// distinctApprover is the separation-of-duties flag, stored as an integer like every other
		// boolean on a run.
		distinctApprover int
		// policySet is the recorded rule set, stored as JSON like the run's other structured fields.
		policySet string
	)
	if err := s.Scan(&r.ID, &r.Playbook, &r.Inventory, &status, &exit, &r.Error,
		&created, &started, &ended, &parent, &shardIdx, &shardCnt, &r.Limit,
		&r.Kind, &r.StepName, &stepIdx, &retryOf, &r.Attempt, &steps, &extra, &outputs,
		&r.ClaimedBy, &claimed, &cancelI, &credIDs, &r.ProjectID, &r.CommitSHA,
		&r.InventoryID, &r.OrgID, &r.Queue, &r.Tool, &r.Command, &dryRun, &r.ProposedFrom, &r.Intent,
		&r.Image, &r.PullCredentialID, &r.IdempotencyKey, &r.Timeout, &notifs,
		&r.Source, &r.SourceID, &r.Actor, &r.RerunOf, &labels, &r.Warning, &r.AuditReceipt,
		&r.HeldByPolicy, &tags, &skipTags, &r.Verbosity, &r.Forks, &diffMode,
		&r.ClaimSecret, &r.ActorType, &r.ApprovedSpecDigest, &distinctApprover, &r.PinnedCommit, &policySet, &r.ActorUserID); err != nil {
		return nil, err
	}
	r.RequireDistinctApprover = distinctApprover != 0
	r.PolicySet = unmarshalPolicySet(policySet)
	r.CancelRequested = cancelI != 0
	r.DryRun = dryRun != 0
	r.DiffMode = diffMode != 0
	r.Tags = sqlutil.SplitIDs(tags)
	r.SkipTags = sqlutil.SplitIDs(skipTags)
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
	if r.Labels, err = parseLabels(labels); err != nil {
		return nil, err
	}
	if r.Steps, err = parseSteps(steps); err != nil {
		return nil, err
	}
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

// marshalPolicySet encodes the rule set recorded on a run, empty when there is none. The set is stored
// as JSON rather than as columns because it is evidence read whole: a digest, a count, and the rules as
// they read, which is what lets a receipt be checked without asking this server what a digest meant.
func marshalPolicySet(set *run.PolicySet) string {
	if set == nil {
		return ""
	}
	b, err := json.Marshal(set)
	if err != nil {
		return ""
	}
	return string(b)
}

// unmarshalPolicySet decodes a stored rule set. An empty column is a run from before the set was
// recorded, which is nil rather than an empty set: "no rules" and "not recorded" are different facts.
func unmarshalPolicySet(s string) *run.PolicySet {
	if s == "" {
		return nil
	}
	var set run.PolicySet
	if err := json.Unmarshal([]byte(s), &set); err != nil {
		return nil
	}
	return &set
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

// Claim leases the oldest unclaimed pending top-level plain run to owner and returns it. A run
// whose cancel was requested while it waited is skipped; the cancel handler terminalizes it.
// A child is claimable only while its parent is running. Shards are stored before the coordinator
// fences the parent, so for as long as that parent is merely pending its shards are already sitting
// claimable: a split canceled in that window had the fence correctly refuse to start the parent
// while a claim loop had already taken shards and executed them on real hosts. Allowing a pending
// parent narrowed that window rather than closing it, and under load the loop still won.
//
// Running is the state that says a coordinator took the parent and means to run it, and every path
// that creates a claimable child reaches it: a split and a shard retry both transition the parent
// through the start fence, and pipeline steps are created only after it. A parent whose coordinator
// dies before the fence leaves its children unclaimable, which the abandoned-parent sweep settles.
func (s *store) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	placeholders, args := sqlutil.QueuePlaceholders(queues, "?", 0)
	q := `
UPDATE runs SET claimed_by=?, claimed_at=?, claim_secret=?
WHERE id = (
	SELECT id FROM runs
	WHERE status='pending' AND claimed_by='' AND kind='' AND cancel_requested=0
		AND queue IN (` + placeholders + `)
		AND (COALESCE(parent_id,'')='' OR parent_id IN (
			SELECT id FROM runs WHERE status='running' AND cancel_requested=0))
	ORDER BY created_at, id LIMIT 1
)
RETURNING ` + runColumns
	// The capability is minted here, when the claim is won, and returned to the worker in the run row.
	full := append([]any{owner, sqlutil.FormatTime(time.Now()), run.NewClaimSecret()}, args...)
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

// ReclaimStaleSettled sweeps like ReclaimStale and names the top-level runs the sweep itself drove
// to a terminal state, so the caller can commit their outcomes to the chain. The sweep is a bulk
// update rather than a pass through the dispatcher's finalize, so without this those runs, the ones
// whose worker died mid-change, ended with no evidence at all. A child's outcome rolls up into its
// parent, so children are left out here exactly as the terminal save leaves them out.
//
// Attribution comes from the sweep's own RETURNING rows, never from a read around it: a candidate
// list confirmed afterward credited the sweep with any run whose real finisher landed in between,
// and the caller then committed a second, contradictory outcome entry for it.
func (s *store) ReclaimStaleSettled(ctx context.Context, ttl time.Duration) (int, []string, error) {
	n, settled, err := s.reclaimStale(ctx, ttl)
	sort.Strings(settled)
	return n, settled, err
}

// updateReturningTopLevel runs an UPDATE ... RETURNING id, parent_id inside the sweep's transaction
// and reports how many rows it changed and which of them are top-level runs.
func updateReturningTopLevel(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, []string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()
	var n int64
	var top []string
	for rows.Next() {
		var id string
		var parent sql.NullString
		if err := rows.Scan(&id, &parent); err != nil {
			return n, top, err
		}
		n++
		if !parent.Valid || parent.String == "" {
			top = append(top, id)
		}
	}
	return n, top, rows.Err()
}

// ReclaimStale requeues stale claimed pending runs and interrupts stale running runs.
func (s *store) ReclaimStale(ctx context.Context, ttl time.Duration) (int, error) {
	n, _, err := s.reclaimStale(ctx, ttl)
	return n, err
}

// reclaimStale is the sweep, returning both how many rows it changed and which top-level runs its
// own statements drove terminal.
func (s *store) reclaimStale(ctx context.Context, ttl time.Duration) (int, []string, error) {
	// A SQLite deployment is one node: the process that stamps a lease is the process that sweeps it,
	// so the local clock is the authoritative one and there is no skew to reconcile.
	//
	// The comparison is on text, and that is not exactly chronological. Stored times keep RFC 3339's
	// trimming rather than being padded to a fixed width, so within one second a value with no
	// fractional part sorts after one that has one: "12:00:00Z" is lexicographically greater than
	// "12:00:00.5Z" because 'Z' is greater than '.', although it happened first. The disagreement is
	// bounded by one second and cannot compound, since it only ever involves two values in the same
	// second, and a lease is measured in tens of seconds.
	//
	// It is left as text deliberately. SQLite's julianday resolves to about 86 microseconds, which
	// is coarser than the times stored here, so routing the comparison through it collapses values
	// that differ and makes the sweep miss leases entirely. Padding the stored format would fix the
	// ordering for new rows and break it for every row already written, because a padded value
	// sorts below an unpadded one from the same instant.
	cut := sqlutil.FormatTime(time.Now().Add(-ttl))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A stale claim on a run somebody asked to cancel is settled, not requeued. Canceling a claimed run
	// is cooperative: the flag is set for its holder to read. If that holder died before starting the
	// run, requeuing it cleared the lease and kept the flag, and a claim will not take a cancel-flagged
	// run, so the run sat pending and unclaimable with nothing that sweeps a pending run to end it,
	// reported as canceling forever. The person already asked for this outcome and the run never began,
	// so it ends canceled. This runs before the requeue so the requeue cannot pick the row up first.
	res, err := tx.ExecContext(ctx, `
UPDATE runs SET status='canceled', claimed_by='', claimed_at=NULL, claim_secret='', ended_at=?
WHERE status='pending' AND claimed_by!='' AND claimed_at < ? AND cancel_requested=1`,
		sqlutil.FormatTime(time.Now()), cut)
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}
	canceled, err := res.RowsAffected()
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}

	res, err = tx.ExecContext(ctx, `
UPDATE runs SET claimed_by='', claimed_at=NULL, claim_secret=''
WHERE status='pending' AND claimed_by!='' AND claimed_at < ? AND cancel_requested=0`, cut)
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}
	requeued, err := res.RowsAffected()
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}
	requeued += canceled
	interrupted, settledStale, err := updateReturningTopLevel(ctx, tx, `
UPDATE runs SET status='interrupted', claimed_by='', claimed_at=NULL, claim_secret='',
ended_at=?, error='interrupted: executor lease expired'
WHERE status='running' AND claimed_by!='' AND claimed_at < ?
RETURNING id, parent_id`, sqlutil.FormatTime(time.Now()), cut)
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}

	// A parent left pending with no lease has no coordinator and never will: nothing claims a run
	// with a kind, and a live coordinator saves its parent running as its first act. Interrupting it
	// here, before orphans are resolved below, settles its children in this same sweep instead of
	// leaving them claimable under a parent that is never going to finish. Held parents are excluded
	// by the status test, since one awaiting approval is resting rather than abandoned. See
	// run.AbandonedParent for the rule this expresses.
	abandoned, settledAbandoned, err := updateReturningTopLevel(ctx, tx, `
UPDATE runs SET status='interrupted', ended_at=?,
error=CASE WHEN error='' THEN '`+run.AbandonedParentError()+`' ELSE error END
WHERE status IN ('pending','running') AND claimed_by='' AND kind IN ('split','pipeline') AND created_at < ?
RETURNING id, parent_id`,
		sqlutil.FormatTime(time.Now()), cut)
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}

	// Interrupting a split or pipeline parent kills the coordinator that would have rolled its
	// children up. A child no executor has started is canceled outright, since leaving it pending
	// means it stays claimable and would run long after its parent gave up.
	res, err = tx.ExecContext(ctx, `
UPDATE runs SET status='canceled', claimed_by='', claimed_at=NULL, claim_secret='', ended_at=?,
error=CASE WHEN error='' THEN '`+run.OrphanError()+`' ELSE error END
WHERE status IN ('pending','pending_approval') AND parent_id IS NOT NULL
	AND parent_id IN (SELECT id FROM runs WHERE status='interrupted' AND kind IN ('split','pipeline'))`,
		sqlutil.FormatTime(time.Now()))
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}
	orphaned, err := res.RowsAffected()
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}

	// A child already executing is asked to stop through the flag its executor watches, rather than
	// being finalized out from under the process that is still running it.
	res, err = tx.ExecContext(ctx, `
UPDATE runs SET cancel_requested=1
WHERE status='running' AND cancel_requested=0 AND parent_id IS NOT NULL
	AND parent_id IN (SELECT id FROM runs WHERE status='interrupted' AND kind IN ('split','pipeline'))`)
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}
	stopping, err := res.RowsAffected()
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}
	settled := append(settledStale, settledAbandoned...)
	return int(requeued + interrupted + abandoned + orphaned + stopping), settled, nil
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

// TransitionStatusAndClaim moves the run between statuses and stamps owner's lease in the same
// statement, so the run is never visible in the new status without an owner.
// A requested cancel blocks the claim in the same statement that makes it. Cancel is recorded as a
// flag rather than a status, so a fence that compares only the status cannot see one: a pipeline
// canceled after it was approved and before its coordinator picked it up still read as running, won
// the compare-and-swap, and executed on real hosts. Checking the flag first and swapping second
// leaves the same gap one scheduling delay wide, so it belongs in the predicate.
func (s *store) TransitionStatusAndClaim(ctx context.Context, id string, from, to run.Status,
	owner string) (bool, error) {
	now := sqlutil.FormatTime(time.Now())
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status=?, claimed_by=?, claimed_at=?,
started_at=COALESCE(NULLIF(started_at,''), ?)
WHERE id=? AND status=? AND cancel_requested=0`,
		string(to), owner, now, now, id, string(from))
	if err != nil {
		return false, fmt.Errorf("transition status and claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("transition status and claim: %w", err)
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

// StampApprovedSpec records the spec digest an approver decided on, in a narrow write that cannot
// clobber a concurrent claim or cancel.
func (s *store) StampApprovedSpec(ctx context.Context, id, digest string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE runs SET approved_spec_digest=? WHERE id=?", digest, id)
	if err != nil {
		return fmt.Errorf("stamp approved spec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("stamp approved spec: %w", err)
	}
	if n == 0 {
		return run.ErrNotFound
	}
	return nil
}

// FinalizeRunning moves a running run to its terminal status and records the exit code, failure
// detail, resolved image, and end time in the same statement, so a run is never terminal with the
// facts that explain it missing.
func (s *store) FinalizeRunning(ctx context.Context, id string, fin run.Finalization) (bool, error) {
	fin.SanitizeText()
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status=?, exit_code=?, error=?, image=?, commit_sha=?,
pull_credential_id=?, outputs=?, warning=?, ended_at=?
WHERE id=? AND status=? AND (?='' OR claimed_by=?)`,
		string(fin.Status), sqlutil.NullInt(fin.ExitCode), fin.Error, fin.Image,
		fin.CommitSHA, fin.PullCredentialID, sqlutil.JSONMap(fin.Outputs), fin.Warning,
		sqlutil.FormatTime(fin.EndedAt), id, string(run.StatusRunning), fin.Owner, fin.Owner)
	if err != nil {
		return false, fmt.Errorf("finalize running run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("finalize running run: %w", err)
	}
	return n > 0, nil
}

// ApplyRunningProgress records a worker's progress in one write fenced on the run still being
// running and still held by owner, so a report in flight cannot resurrect a run the sweep settled.
//
// started_at uses COALESCE so a repeated report never moves a start time backward, and warning and
// outputs keep their stored value when the report carries none, which is what lets one statement
// stand in for the read-modify-write this replaced.
func (s *store) ApplyRunningProgress(ctx context.Context, id, owner string,
	p run.Progress) (bool, error) {
	p.SanitizeText()
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET started_at=COALESCE(NULLIF(started_at,''), ?),
warning=CASE WHEN ?='' THEN warning ELSE ? END,
outputs=CASE WHEN ?='' THEN outputs ELSE ? END
WHERE id=? AND status=? AND claimed_by=?`,
		sqlutil.NullTime(p.StartedAt),
		p.Warning, p.Warning,
		sqlutil.JSONMap(p.Outputs), sqlutil.JSONMap(p.Outputs),
		id, string(run.StatusRunning), owner)
	if err != nil {
		return false, fmt.Errorf("apply running progress: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("apply running progress: %w", err)
	}
	return n > 0, nil
}

// RunTimings returns the timing fields of the most recent top-level runs, newest first.
//
// It selects seven columns rather than the whole row on purpose. The metrics endpoint reads this on
// every scrape, and a run row carries its extra vars, steps, labels, and notification targets, so
// decoding full rows for ten thousand runs cost more than everything else the endpoint does put
// together.
func (s *store) RunTimings(ctx context.Context, limit int) ([]run.RunTiming, error) {
	const q = `
SELECT id, status, kind, queue, claimed_by, created_at, started_at, ended_at
FROM runs WHERE parent_id IS NULL
ORDER BY created_at DESC, id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("run timings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanTimings(rows)
}

// scanTimings reads the narrow timing rows.
func scanTimings(rows *sql.Rows) ([]run.RunTiming, error) {
	var out []run.RunTiming
	for rows.Next() {
		var (
			t              run.RunTiming
			status         string
			created        string
			started, ended sql.NullString
		)
		if err := rows.Scan(&t.ID, &status, &t.Kind, &t.Queue, &t.ClaimedBy, &created, &started,
			&ended); err != nil {
			return nil, fmt.Errorf("run timings: %w", err)
		}
		t.Status = run.Status(status)
		at, err := sqlutil.ParseTime(created)
		if err != nil {
			return nil, fmt.Errorf("run timings: %w", err)
		}
		t.CreatedAt = at
		if t.StartedAt, err = sqlutil.ParseNullTime(started); err != nil {
			return nil, fmt.Errorf("run timings: %w", err)
		}
		if t.EndedAt, err = sqlutil.ParseNullTime(ended); err != nil {
			return nil, fmt.Errorf("run timings: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
