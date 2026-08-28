package pgstore

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

// Save inserts or replaces the run identified by r.ID. The cancel flag merges with GREATEST so a
// replace from a stale snapshot cannot erase a cancel another process just requested.
func (s *store) Save(ctx context.Context, r *run.Run) error {
	// Cleaned here rather than left to fail: PostgreSQL refuses a NUL byte and invalid UTF-8 with
	// SQLSTATE 22021, and the write that carried them is the one recording what the run did.
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
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
	$21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38,
	$39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53, $54, $55, $56, $57,
	$58, $59, $60)
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
		// ESCAPE '' turns off PostgreSQL's default backslash escape, which SQLite's LIKE does not
		// have. Without it the same search term matched different rows on the two backends: a term
		// containing a backslash was read as an escape here and as a literal there.
		parts[i] = "lower(" + col + ") LIKE $1 ESCAPE ''"
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
	const q = "SELECT " + runColumns + " FROM runs WHERE parent_id=$1 ORDER BY shard_index NULLS LAST"
	return s.queryRuns(ctx, "list shards", q, parentID)
}

// Steps returns the pipeline step runs of a parent ordered by step index then attempt.
func (s *store) Steps(ctx context.Context, parentID string) ([]*run.Run, error) {
	// NULLS LAST for the same reason the shard listing above states it: SQLite sorts NULLs first and
	// PostgreSQL last, and step_index is nullable.
	const q = "SELECT " + runColumns +
		" FROM runs WHERE parent_id=$1 ORDER BY step_index NULLS LAST, attempt"
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
	err := q.QueryRowContext(ctx, "SELECT status FROM runs WHERE id=$1", runID).Scan(&status)
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
	err := q.QueryRowContext(ctx, "SELECT dry_run FROM runs WHERE id=$1", runID).Scan(&dryRun)
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

// pgNowText renders the database server's current time in the same UTC RFC 3339 text the Go side
// writes, so a lease stamped in SQL is indistinguishable from one stamped by a store method. Leases
// have to come from the database clock rather than a worker's: with several nodes writing, a worker
// whose clock runs behind would otherwise stamp leases the janitor reads as already expired. The
// fractional second carries the database clock's microsecond precision. The lease sweep casts this
// text to a real timestamp before comparing it, so the width of the fractional part does not have to
// match what the Go side writes.
const pgNowText = `to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`

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

// Claim leases the oldest unclaimed pending top-level plain run to owner and returns it. The row
// is locked with SKIP LOCKED so concurrent workers never claim the same run. A run whose cancel
// was requested while it waited is skipped; the cancel handler terminalizes it.
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
	placeholders, args := sqlutil.QueuePlaceholders(queues, "$", 3)
	// The lease is stamped from the database clock, not this worker's, so the janitor on another node
	// ages it against the clock that wrote it. The capability in $2 is minted here, when the claim is
	// won, and returned to the worker in the run row.
	q := `
UPDATE runs SET claimed_by=$1, claimed_at=` + pgNowText + `, claim_secret=$2
WHERE id = (
	SELECT id FROM runs
	WHERE status='pending' AND claimed_by='' AND kind='' AND cancel_requested=0
		AND queue IN (` + placeholders + `)
		AND (COALESCE(parent_id,'')='' OR parent_id IN (
			SELECT id FROM runs WHERE status='running' AND cancel_requested=0))
	ORDER BY created_at, id LIMIT 1
	FOR UPDATE SKIP LOCKED
)
RETURNING ` + runColumns
	full := append([]any{owner, run.NewClaimSecret()}, args...)
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
	// Stamped from the database clock, the same clock Claim stamps with and ReclaimStale ages
	// against. Using the calling process's clock put two different clocks in one column: a worker
	// running a minute behind renewed its lease into the past, and the next sweep interrupted a
	// perfectly healthy run. Heartbeating made it worse than not heartbeating, because a run that
	// never renewed kept the database-clock stamp Claim gave it and survived.
	res, err := s.db.ExecContext(ctx,
		"UPDATE runs SET claimed_at="+pgNowText+" WHERE id=$1 AND claimed_by=$2",
		id, owner)
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

func (s *store) ReclaimStale(ctx context.Context, ttl time.Duration) (int, error) {
	n, _, err := s.reclaimStale(ctx, ttl)
	return n, err
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

// reclaimStale is the sweep, returning both how many rows it changed and which top-level runs its
// own statements drove terminal.
func (s *store) reclaimStale(ctx context.Context, ttl time.Duration) (int, []string, error) {
	age := ttl.Seconds()
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
UPDATE runs SET status='canceled', claimed_by='', claimed_at=NULL, claim_secret='',
ended_at=`+pgNowText+`
WHERE status='pending' AND claimed_by!='' AND cancel_requested=1
  AND claimed_at::timestamptz < now() - make_interval(secs => $1)`, age)
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}
	canceled, err := res.RowsAffected()
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}

	res, err = tx.ExecContext(ctx, `
UPDATE runs SET claimed_by='', claimed_at=NULL, claim_secret=''
WHERE status='pending' AND claimed_by!='' AND cancel_requested=0
  AND claimed_at::timestamptz < now() - make_interval(secs => $1)`, age)
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
ended_at=`+pgNowText+`, error='interrupted: executor lease expired'
WHERE status='running' AND claimed_by!=''
  AND claimed_at::timestamptz < now() - make_interval(secs => $1)
RETURNING id, parent_id`, age)
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}

	// A parent left pending with no lease has no coordinator and never will: nothing claims a run
	// with a kind, and a live coordinator saves its parent running as its first act. Interrupting it
	// here, before orphans are resolved below, settles its children in this same sweep instead of
	// leaving them claimable under a parent that is never going to finish. Held parents are excluded
	// by the status test, since one awaiting approval is resting rather than abandoned. See
	// run.AbandonedParent for the rule this expresses.
	//
	// created_at is written by the submitting node, not by the database, so this one comparison does
	// cross clocks where the lease sweeps above do not. A node running behind the database would see
	// its own freshly submitted parents as old. The margin is the sweep ttl, which is minutes, and
	// the alternative is a stored server-side timestamp on every run for the sake of one predicate.
	// Keep the nodes disciplined, and note that a parent is only ever swept when it is also pending
	// and unclaimed, which a live coordinator makes false within milliseconds of the save.
	abandoned, settledAbandoned, err := updateReturningTopLevel(ctx, tx, `
UPDATE runs SET status='interrupted', ended_at=`+pgNowText+`,
error=CASE WHEN error='' THEN '`+run.AbandonedParentError()+`' ELSE error END
WHERE status IN ('pending','running') AND claimed_by='' AND kind IN ('split','pipeline')
  AND created_at::timestamptz < now() - make_interval(secs => $1)
RETURNING id, parent_id`, age)
	if err != nil {
		return 0, nil, fmt.Errorf("reclaim stale: %w", err)
	}

	// Interrupting a split or pipeline parent kills the coordinator that would have rolled its
	// children up. A child no executor has started is canceled outright, since leaving it pending
	// means it stays claimable and would run long after its parent gave up.
	res, err = tx.ExecContext(ctx, `
UPDATE runs SET status='canceled', claimed_by='', claimed_at=NULL, claim_secret='', ended_at=`+pgNowText+`,
error=CASE WHEN error='' THEN '`+run.OrphanError()+`' ELSE error END
WHERE status IN ('pending','pending_approval') AND parent_id IS NOT NULL
  AND parent_id IN (SELECT id FROM runs WHERE status='interrupted' AND kind IN ('split','pipeline'))`)
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

// TransitionStatusAndClaim moves the run between statuses and stamps owner's lease in the same
// statement, so the run is never visible in the new status without an owner.
// A requested cancel blocks the claim in the same statement that makes it. Cancel is recorded as a
// flag rather than a status, so a fence that compares only the status cannot see one: a pipeline
// canceled after it was approved and before its coordinator picked it up still read as running, won
// the compare-and-swap, and executed on real hosts. Checking the flag first and swapping second
// leaves the same gap one scheduling delay wide, so it belongs in the predicate.
func (s *store) TransitionStatusAndClaim(ctx context.Context, id string, from, to run.Status,
	owner string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status=$1, claimed_by=$2, claimed_at=`+pgNowText+`,
started_at=COALESCE(NULLIF(started_at,''), `+pgNowText+`)
WHERE id=$3 AND status=$4 AND cancel_requested=0`,
		string(to), owner, id, string(from))
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

// StampApprovedSpec records the spec digest an approver decided on, in a narrow write that cannot
// clobber a concurrent claim or cancel.
func (s *store) StampApprovedSpec(ctx context.Context, id, digest string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE runs SET approved_spec_digest=$1 WHERE id=$2", digest, id)
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
		`UPDATE runs SET status=$1, exit_code=$2, error=$3, image=$4, commit_sha=$5,
pull_credential_id=$6, outputs=$7, warning=$8, ended_at=$9
WHERE id=$10 AND status=$11 AND ($12='' OR claimed_by=$12)`,
		string(fin.Status), sqlutil.NullInt(fin.ExitCode), fin.Error, fin.Image,
		fin.CommitSHA, fin.PullCredentialID, sqlutil.JSONMap(fin.Outputs), fin.Warning,
		sqlutil.FormatTime(fin.EndedAt), id, string(run.StatusRunning), fin.Owner)
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
		`UPDATE runs SET started_at=COALESCE(NULLIF(started_at,''), $1),
warning=CASE WHEN $2='' THEN warning ELSE $2 END,
outputs=CASE WHEN $3='' THEN outputs ELSE $3 END
WHERE id=$4 AND status=$5 AND claimed_by=$6`,
		sqlutil.NullTime(p.StartedAt), p.Warning, sqlutil.JSONMap(p.Outputs),
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
ORDER BY created_at DESC, id DESC LIMIT $1`
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
