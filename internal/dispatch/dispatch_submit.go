package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// validateRun checks a run's credential and project references before it is accepted. The run's own
// credentials are held to the tool-compatibility gate; credentials inherited from the target
// inventory are checked for existence and decryptability but not tool fit, since materializeCredentials
// resolves the inherited set too and an undecryptable one there would otherwise fail only at execution.
func (d *Dispatcher) validateRun(ctx context.Context, r *run.Run) error {
	if err := d.validateCredentials(ctx, r.Tool, r.CredentialIDs, true); err != nil {
		return err
	}
	if err := d.validateInventory(ctx, r.InventoryID); err != nil {
		return err
	}
	if err := d.validateCredentials(ctx, r.Tool, d.inventoryCredentialIDs(ctx, r), false); err != nil {
		return err
	}
	return d.validateProject(ctx, r.ProjectID)
}

// resolveQueue fills a run's queue from its stored inventory when neither the request nor its
// template pinned one, so an inventory can pin all of its work to a worker group. The precedence
// is run, then template (already applied as the run's queue by launch), then inventory. A lookup
// failure leaves the queue empty rather than failing the submit, since validateRun has already
// confirmed the inventory exists.
func (d *Dispatcher) resolveQueue(ctx context.Context, r *run.Run) {
	if r.Queue != "" || r.InventoryID == "" || d.inventories == nil {
		return
	}
	inv, err := d.inventories.Get(ctx, r.InventoryID)
	if err != nil {
		return
	}
	r.Queue = inv.Queue
}

// requireToolInput checks that a run carries the input its tool needs: a playbook for Ansible, a
// command for bash, terraform, and python. It also rejects a run naming an unsupported tool.
func requireToolInput(r *run.Run) error {
	if !run.ValidTool(r.Tool) {
		return ErrUnknownTool
	}
	if run.NormalizeTool(r.Tool) == run.ToolAnsible {
		if r.Playbook == "" {
			return ErrNoPlaybook
		}
		return nil
	}
	if r.Command == "" {
		return ErrNoCommand
	}
	return nil
}

// idempotentLookup returns the run already recorded under key, or nil when the key is empty or
// unused. A retried submission carrying a key a prior submission already used resolves to that
// original run, so the retry never fires a second run.
func (d *Dispatcher) idempotentLookup(ctx context.Context, key string) (*run.Run, error) {
	if key == "" {
		return nil, nil
	}
	existing, err := d.store.ByIdempotencyKey(ctx, key)
	if errors.Is(err, run.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return existing, nil
}

// idempotentSave persists a newly built run and settles the concurrent-retry race the pre-check
// cannot. When another submission claimed the same key between the lookup and this save, the store's
// unique index rejects r with run.ErrDuplicateKey; the winning run is fetched and returned with dup
// true so the caller returns it and skips any follow-on work such as spawning children. Without a
// key it is an ordinary save.
func (d *Dispatcher) idempotentSave(ctx context.Context, r *run.Run) (result *run.Run, dup bool, err error) {
	saveErr := d.store.Save(ctx, r)
	if errors.Is(saveErr, run.ErrDuplicateKey) {
		winner, ferr := d.store.ByIdempotencyKey(ctx, r.IdempotencyKey)
		if ferr != nil {
			return nil, false, ferr
		}
		return winner, true, nil
	}
	if saveErr != nil {
		return nil, false, saveErr
	}
	return r, false, nil
}

// Submit accepts a run for a tool against inventory and returns the created run in pending state.
// Execution proceeds asynchronously; callers observe progress through the store. A submission
// carrying an idempotency key that a prior submit already used returns that original run untouched.
func (d *Dispatcher) Submit(ctx context.Context, playbook, inventory string, opts ...run.SubmitOption) (*run.Run, error) {
	r := &run.Run{
		ID:        run.NewID(),
		Playbook:  playbook,
		Inventory: inventory,
		Status:    run.StatusPending,
		CreatedAt: d.now(),
	}
	run.ApplyOptions(r, opts)
	stampReceipt(ctx, r)
	stampOrg(ctx, r)
	if err := requireToolInput(r); err != nil {
		return nil, err
	}
	if existing, err := d.idempotentLookup(ctx, r.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	if err := d.validateRun(ctx, r); err != nil {
		return nil, err
	}
	d.resolveQueue(ctx, r)
	// Deny is checked before the hold, and even for a run born held: a rule that refuses a
	// submission outright must not be satisfied by parking the run in front of an approver.
	if err := d.denied(ctx, r); err != nil {
		return nil, err
	}
	// The policy pass runs even for a run that arrives held, for the same reason denied does. A run
	// born held skipped it, so the rule that governs it was never consulted and its
	// require_distinct_approver was never copied onto the run. The run was still held, so it looked
	// governed, but whoever asked for it could approve it themselves. Every path that submits with
	// approval already requested reaches this, including the AI proposal and the drift reconcile,
	// which is exactly where a second pair of eyes is the point.
	held, perr := d.requiresApproval(ctx, r)
	if perr != nil {
		return nil, perr
	}
	if held {
		r.Status = run.StatusPendingApproval
	}
	recordHold(r, holdRequested)
	created, _, err := d.idempotentSave(ctx, r)
	if err != nil {
		return nil, err
	}
	// Execution happens through the claim loop, here or in any worker sharing the store, so the
	// local loop is nudged rather than left to finish an idle backoff a user would wait out.
	d.wake()
	return created, nil
}

// SubmitSplit shards a run across the inventory and returns the parent run in pending state. Each
// shard runs the same playbook limited to its slice of hosts, and the parent rolls up their result.
// Hosts are packed into shards by their average duration in recent runs so each shard carries a
// similar amount of work; hosts without history balance by count. When shards is below two or the
// inventory has fewer than two hosts, it falls back to a single run.
func (d *Dispatcher) SubmitSplit(ctx context.Context, playbook, inventory string, shards int, opts ...run.SubmitOption) (*run.Run, error) {
	// Sharding fans a playbook across inventory hosts, which only Ansible does; other tools run once.
	probe := &run.Run{}
	run.ApplyOptions(probe, opts)
	stampReceipt(ctx, probe)
	// A retried split returns the original parent without re-listing hosts or resharding.
	if existing, err := d.idempotentLookup(ctx, probe.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	if run.NormalizeTool(probe.Tool) != run.ToolAnsible {
		return d.Submit(ctx, playbook, inventory, opts...)
	}
	if playbook == "" {
		return nil, ErrNoPlaybook
	}
	if shards < 2 {
		return d.Submit(ctx, playbook, inventory, opts...)
	}
	if d.hostLister == nil {
		return nil, ErrNoHostLister
	}

	// A stored inventory must exist as a file before its hosts can be enumerated for sharding.
	listPath := inventory
	if probe.InventoryID != "" {
		path, cleanup, _, err := d.inventoryFile(ctx, probe.InventoryID)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		listPath = path
	}
	// The caller's host limit narrows the enumeration, not just the runs. Listing the whole
	// inventory and sharding that meant a limited submit fanned out across every host it excluded,
	// and answered 202, so the first sign was the run touching hosts nobody asked for.
	hosts, err := d.hostLister.Hosts(ctx, listPath, probe.Limit)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	if len(hosts) < 2 {
		return d.Submit(ctx, playbook, inventory, opts...)
	}

	costs, err := d.store.HostCosts(ctx, costWindow)
	if err != nil {
		d.log.Warn("dispatch: host costs unavailable: balancing by host count: " + err.Error())
		costs = nil
	}

	groups := partition(hosts, min(shards, d.maxShards), costs)
	count := len(groups)
	parent := &run.Run{
		ID: run.NewID(), Playbook: playbook, Inventory: inventory, Kind: run.KindSplit,
		Status: run.StatusPending, CreatedAt: d.now(), ShardCount: &count,
	}
	run.ApplyOptions(parent, opts)
	stampReceipt(ctx, parent)
	stampOrg(ctx, parent)
	if err := d.validateRun(ctx, parent); err != nil {
		return nil, err
	}
	d.resolveQueue(ctx, parent)
	// A split is submitted through a different path than a single run, so without this the same
	// command an operator gated ran freely by being sharded: the identical playbook that Submit
	// held for an approver executed on every host the moment it was split in two. A shard matches
	// exactly what its parent matches, since it inherits everything but its host group, so the
	// parent is the only thing worth testing.
	if err := d.denied(ctx, parent); err != nil {
		return nil, err
	}
	// Consulted even when the parent arrives held, so the rule governing it binds its approval.
	held, perr := d.requiresApproval(ctx, parent)
	if perr != nil {
		return nil, perr
	}
	if held {
		parent.Status = run.StatusPendingApproval
	}
	recordHold(parent, holdRequested)
	created, dup, err := d.idempotentSave(ctx, parent)
	if err != nil {
		return nil, err
	}
	if dup {
		// A concurrent submission won the key; return its parent and create no children here.
		return created, nil
	}

	// Shards of a held split are stored held too. Nothing can claim them in that state, and they
	// have to exist before the approval so the decision covers the shards an approver was shown and
	// so the split survives a restart while it waits.
	childStatus := run.StatusPending
	if parent.Status == run.StatusPendingApproval {
		childStatus = run.StatusPendingApproval
	}
	parentID := parent.ID
	children := make([]*run.Run, 0, count)
	for i, group := range groups {
		idx, shardCount := i, count
		child := &run.Run{
			ID: run.NewID(), Playbook: playbook, Inventory: inventory,
			Status: childStatus, CreatedAt: d.now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &shardCount,
			Limit: strings.Join(group, ","),
		}
		// A shard is the parent run over a subset of its hosts, so it executes the same way. Copying
		// a chosen few fields meant a split silently dropped the rest: extra vars vanished, shards
		// ran outside the execution image the parent pinned, and the run timeout did not apply.
		inheritExecution(child, parent)
		// A held child was held by whatever held its parent, so it says the same thing rather than
		// reading as a change nothing stopped.
		child.HeldByPolicy = parent.HeldByPolicy
		// A child belongs to its parent's authorization, whether it is built inside the request or
		// after it returned. Inheriting is the one rule; re-deriving from context would make an
		// in-request shard and a later step disagree about which receipt is truthful.
		child.AuditReceipt = parent.AuditReceipt
		// A shard owns the same tenant as its parent. A shard names no stored object of its own, so
		// without the parent's org it would be an objectless run readable across every tenant.
		child.OrgID = parent.OrgID
		if err := d.store.Save(ctx, child); err != nil {
			return nil, err
		}
		children = append(children, child)
	}

	if parent.Status == run.StatusPendingApproval {
		// Held for an approver. Approve starts the coordinator, since no claim loop takes a parent.
		return parent, nil
	}

	d.wake()
	d.wg.Add(1)
	go d.coordinate(parent.Clone(), children)

	return parent, nil
}

// stampReceipt records which chain entry authorized this run's creation, defaulting it to the
// request in flight.
//
// The receipt is not an execution option, so rerun, reconcile, and shard retry do not replay it:
// each is a new request with its own authorization, and carrying the source run's receipt through
// them made a rerun's evidence name whoever launched the original, weeks earlier, which is worse
// than naming nobody. With that inheritance gone, an explicit WithAuditReceiptOf is a deliberate
// statement about which request set this run in motion, so it wins over the ambient context; the
// plan gate uses it to attribute a proposed apply to the request that submitted the plan. A run
// created outside a recorded request, by the scheduler or the seeder, carries none.
func stampReceipt(ctx context.Context, r *run.Run) {
	if r.AuditReceipt == "" {
		r.AuditReceipt = run.AuditReceiptFrom(ctx)
	}
}

// stampOrg records the submitting actor's owning organization on the run, defaulting it to the org
// carried on the request context. It is what scopes an objectless run to a tenant: a run that names
// no stored project, inventory, or credential has nothing for the per-object grant check to filter
// on, so without an owning org it is readable, cancelable, and approvable across every tenant. An
// explicit WithOrgID, such as a child inheriting its parent's org, wins over the ambient context.
func stampOrg(ctx context.Context, r *run.Run) {
	if r.OrgID == "" {
		r.OrgID = run.SubmitterOrgFrom(ctx)
	}
}

// inheritExecution copies onto child every field that decides how a run executes, so a shard of a
// split, or a retry of one, runs exactly the way its parent would have.
//
// The fields come from run.Run.ExecutionOptions rather than a list kept here. They were copied by
// hand in several places and every list fell behind the run model: a split lost its extra vars, ran
// outside the execution image its parent pinned, and ignored the parent's timeout, while a rerun
// lost the timeout and the notifications. Anything added to run.Run that changes how a run executes
// belongs in ExecutionOptions, where every path derived from a run reads it.
func inheritExecution(child, parent *run.Run) {
	for _, opt := range parent.ExecutionOptions() {
		opt(child)
	}
}

// RetryFailedShards creates and starts a new split run that re-runs only the failed shards of a
// finished split parent, keeping each failed shard's host group. Shards that succeeded do not run
// again. The new parent links back to the run it retries through RetryOf. Retrying the same parent
// twice inside the dedupe window returns the first retry, so a double click cannot fire two.
func (d *Dispatcher) RetryFailedShards(ctx context.Context, parentID string) (*run.Run, error) {
	existing, key, err := run.ResolveDedupe(ctx, d.store, dedupeRetryShards, parentID, time.Now())
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	parent, err := d.store.Get(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if parent.Kind != run.KindSplit {
		return nil, ErrNotSplit
	}
	if !parent.Status.Terminal() {
		return nil, ErrNotFinished
	}

	shards, err := d.store.Shards(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("list shards: %w", err)
	}
	var failed []*run.Run
	// A shard is worth running again when it ran and failed, or when it was canceled because its
	// parent's coordinator died.
	//
	// Treating every non-succeeded shard as failed is what turned a rejection into a retryable set,
	// since rejecting a split cancels the shards held with it. Excluding every canceled shard went
	// too far the other way: the orphan sweep also cancels shards when a control node dies, and
	// retrying those is the whole recovery path after a crash. The orphan reason is what separates
	// the two on a healthy parent, and it is already recorded on a shard the sweep canceled before it
	// started.
	//
	// A shard that was still running when the coordinator died is different: the sweep only sets its
	// cancel flag, and its own executor finalizes it canceled with no error, indistinguishable by
	// error text from a user cancel. When the parent itself is interrupted the whole split crashed,
	// so every shard that did not succeed is an orphan of that crash and must be retried, including
	// the mid-flight ones most likely to have partially applied. A user cancel leaves the parent
	// failed or canceled, not interrupted, so this does not sweep up a shard a person stopped on a
	// healthy run.
	parentInterrupted := parent.Status == run.StatusInterrupted
	for _, s := range shards {
		switch {
		case s.Status == run.StatusSucceeded:
			// A shard that already finished green is never rerun.
		case parentInterrupted:
			failed = append(failed, s)
		case s.Status == run.StatusFailed, s.Status == run.StatusInterrupted:
			failed = append(failed, s)
		case s.Status == run.StatusCanceled && s.Error == run.OrphanError():
			failed = append(failed, s)
		}
	}
	if len(failed) == 0 {
		return nil, ErrNoFailedShards
	}

	count := len(failed)
	retry := &run.Run{
		ID: run.NewID(), Playbook: parent.Playbook, Inventory: parent.Inventory,
		Kind: run.KindSplit, Status: run.StatusPending, CreatedAt: d.now(),
		ShardCount: &count, RetryOf: &parent.ID, IdempotencyKey: key,
	}
	inheritExecution(retry, parent)
	retry.OrgID = parent.OrgID
	// A retry is authorized by the retry request, not by whatever authorized the parent weeks ago.
	stampReceipt(ctx, retry)
	// A retry is a fourth way to submit a run, and it inherits the parent's entire execution spec,
	// so it has to face the same gate as the other three. Submit, SubmitSplit, and SubmitPipeline
	// each consult the policy; this path did not, which made retrying a way to run a spec an
	// approver would have held.
	if err := d.denied(ctx, retry); err != nil {
		return nil, err
	}
	held, perr := d.requiresApproval(ctx, retry)
	if perr != nil {
		return nil, perr
	}
	if held {
		retry.Status = run.StatusPendingApproval
	}
	saved, dup, err := d.idempotentSave(ctx, retry)
	if err != nil {
		return nil, err
	}
	if dup {
		// A concurrent click won the key; return its retry and create no second set of shards.
		return saved, nil
	}

	retryChildStatus := run.StatusPending
	if retry.Status == run.StatusPendingApproval {
		retryChildStatus = run.StatusPendingApproval
	}
	retryID := retry.ID
	children := make([]*run.Run, 0, count)
	for i, shard := range failed {
		idx, shardCount := i, count
		child := &run.Run{
			ID: run.NewID(), Playbook: retry.Playbook, Inventory: retry.Inventory,
			Status: retryChildStatus, CreatedAt: d.now(),
			ParentID: &retryID, ShardIndex: &idx, ShardCount: &shardCount,
			// The host group is the one thing a shard owns; everything about how it executes comes
			// from the run it is a shard of.
			Limit: shard.Limit,
		}
		inheritExecution(child, retry)
		child.AuditReceipt = retry.AuditReceipt
		child.OrgID = retry.OrgID
		if err := d.store.Save(ctx, child); err != nil {
			return nil, err
		}
		children = append(children, child)
	}

	if retry.Status == run.StatusPendingApproval {
		// Held for an approver. Approve starts the coordinator, since no claim loop takes a parent.
		return retry, nil
	}
	d.wake()
	d.wg.Add(1)
	go d.coordinate(retry.Clone(), children)

	return retry, nil
}

// RelaunchFailedHosts creates and starts a new run of the same spec restricted to only the hosts
// that failed or were unreachable in a finished run, links it back to that run, and records the
// derivation in the chain. Where AWX and Rundeck re-run the failed nodes, the new run here carries a
// receipt to the run it came from, so an auditor can see exactly which run's failures this one was
// built to fix rather than taking the operator's word.
//
// It targets a run that recorded per-host results, which is an Ansible run; a run with none, such as
// a single bash command, has no notion of a failed host and is refused. Relaunching the same run
// twice inside the dedupe window returns the first relaunch, so a double click cannot fire two.
//
// The actor is whoever asked for the relaunch, which is not necessarily whoever launched the run it
// is built from.
func (d *Dispatcher) RelaunchFailedHosts(ctx context.Context, runID, actor, actorType string) (*run.Run, error) {
	existing, key, err := run.ResolveDedupe(ctx, d.store, dedupeRelaunchHosts, runID, time.Now())
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	src, err := d.store.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !src.Status.Terminal() {
		return nil, ErrNotFinished
	}
	summaries, err := d.store.RunHostSummaries(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read host summaries: %w", err)
	}
	if len(summaries) == 0 {
		return nil, ErrNoHostSummary
	}
	var failed []string
	for _, s := range summaries {
		if s.Failures > 0 || s.Unreachable > 0 {
			failed = append(failed, s.Host)
		}
	}
	if len(failed) == 0 {
		return nil, ErrNoFailedHosts
	}
	opts := append(src.ExecutionOptions(),
		run.WithLimit(strings.Join(failed, ",")),
		run.WithSource("relaunch", runID),
		run.WithRetryOf(runID),
		run.WithIdempotencyKey(key),
		// The relaunch is a new launch by whoever asked for it, not by whoever ran the original.
		// Stamping the source run's actor credited the relaunch to the wrong person, so asking
		// what a given operator started missed the runs they started this way.
		run.WithActor(actor),
		run.WithActorType(actorType),
		// The relaunch belongs to the same tenant as the run it fixes. A relaunch of an objectless
		// run names no stored object, so without the source run's org it would be readable across
		// every tenant.
		run.WithOrgID(src.OrgID),
	)
	return d.Submit(ctx, src.Playbook, src.Inventory, opts...)
}
