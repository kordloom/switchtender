package dispatch

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"go.uber.org/zap"
	"maps"
)

// parentMayStart claims the parent for this coordinator and reports whether it may begin.
//
// The claim is a compare-and-swap from the status the parent should still be in, which makes the
// check and the start one operation. Reading the stored state and then writing was two, and the
// write is an upsert, so a cancel landing between them was silently undone and the whole fan-out
// executed after the API had already answered "canceled". CancelPending terminalizes an unclaimed
// parent without setting the cancel flag, so nothing downstream would have caught it either.
//
// A swap that changes nothing means the parent is no longer where it was left: canceled, already
// coordinated, or swept. Either way this coordinator does not own it and settles its children
// instead of starting them.
func (d *Dispatcher) parentMayStart(parent *run.Run, childIDs []string) bool {
	ctx := context.Background()
	// Retried like every other store write in this file. It is the one write that decides whether a
	// whole fan-out lives, and under SQLite a single writer with a busy timeout means contention is
	// expected rather than exceptional, so one busy moment must not hard-fail a healthy split.
	var ok bool
	err := withRetries(func() error {
		var terr error
		ok, terr = d.store.TransitionStatusAndClaim(ctx, parent.ID, parent.Status,
			run.StatusRunning, d.owner)
		return terr
	})
	if err != nil {
		// The store is unreachable. Starting on an unknown state risks resurrecting a canceled
		// run, and the fence exists precisely for that uncertainty.
		d.log.Error("dispatch: could not claim parent to start it: "+err.Error(),
			zap.String("run_id", parent.ID))
		d.cancelChildren(childIDs)
		// The parent is settled too, not just its children. Leaving it running while its children
		// are canceled and its stream is closed is a half-state that only the lease sweep would
		// eventually resolve, and only for a parent that happened to hold a lease.
		d.finalize(parent, run.StatusFailed, nil, "could not start coordination: "+err.Error())
		d.publisher.CloseRun(parent.ID)
		return false
	}
	if !ok {
		current, gerr := d.store.Get(ctx, parent.ID)
		status := "unknown"
		if gerr == nil {
			status = string(current.Status)
		}
		d.log.Info("dispatch: parent moved on before its coordination started",
			zap.String("run_id", parent.ID), zap.String("status", status))
		d.cancelChildren(childIDs)
		if gerr == nil && !current.Status.Terminal() {
			d.finalize(parent, run.StatusCanceled, nil, "")
		}
		// A UI tailing this parent has to see the stream end, as it does on every other terminal
		// path.
		d.publisher.CloseRun(parent.ID)
		return false
	}
	return true
}

// idsOf returns the ids of a set of runs.
func idsOf(runs []*run.Run) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

// startParentRow persists a coordinated parent's transition to running without erasing the lease the
// start fence just stamped. These parents never pass through Claim, so their in-memory ClaimedAt is
// nil, and the whole-row save writes every column: saving nil over the fence's stamp left a running,
// claimed row with no lease time, which no sweep clause reaches, so a coordinator that died before
// its first heartbeat stranded the parent as running and unkillable forever. The stamp is read back
// so the store's clock stays the lease's clock; when that read fails the local clock stands in,
// because a slightly skewed lease is sweepable and a missing one is not.
func (d *Dispatcher) startParentRow(parent *run.Run) {
	started := d.now()
	parent.Status = run.StatusRunning
	parent.StartedAt = &started
	parent.ClaimedBy = d.owner
	if current, err := d.store.Get(context.Background(), parent.ID); err == nil && current.ClaimedAt != nil {
		parent.ClaimedAt = current.ClaimedAt
	} else {
		parent.ClaimedAt = &started
	}
	_ = d.save(parent)
}

// coordinate waits for the parent's shards, which execute wherever a claim loop picks them up,
// and finalizes the parent from their stored results. The parent carries this process's lease so
// a dead coordinator is swept, and canceling the parent propagates through the store to every
// shard no matter which process holds it.
func (d *Dispatcher) coordinate(parent *run.Run, children []*run.Run) {
	defer d.wg.Done()

	parentCtx, cancelParent := context.WithCancel(d.ctx)
	d.register(parent.ID, cancelParent)
	defer d.unregister(parent.ID)
	defer cancelParent()

	if !d.parentMayStart(parent, idsOf(children)) {
		return
	}
	d.startParentRow(parent)

	watchCtx, stopWatch := context.WithCancel(parentCtx)
	defer stopWatch()
	go d.watch(watchCtx, parent.ID)

	ids := make([]string, len(children))
	for i, c := range children {
		ids[i] = c.ID
	}
	statuses := d.waitChildren(parentCtx, ids)

	allSucceeded := true
	anyCanceled := false
	anyInterrupted := false
	for _, status := range statuses {
		if status != run.StatusSucceeded {
			allSucceeded = false
		}
		switch status {
		case run.StatusCanceled:
			anyCanceled = true
		case run.StatusInterrupted:
			anyInterrupted = true
		}
	}
	switch {
	case allSucceeded:
		code := 0
		d.finalize(parent, run.StatusSucceeded, &code, "")
	// A shard the server stopped explains the whole split, and it takes precedence over a failure:
	// nothing was learned about the shards that never finished, so calling the split failed would
	// state an outcome the run never reached. It is also the state a partial retry accepts.
	case anyInterrupted:
		d.finalize(parent, run.StatusInterrupted, nil, errShuttingDown.Error())
	case anyCanceled:
		d.finalize(parent, run.StatusCanceled, nil, "")
	default:
		code := 1
		d.finalize(parent, run.StatusFailed, &code, "")
	}
	d.publisher.CloseRun(parent.ID)
}

// childPollInterval is how often a coordinator checks its children's stored states.
const childPollInterval = 500 * time.Millisecond

// waitChildren polls the store until every child reaches a terminal state and returns their
// statuses in order. Reads use ctx, so a shutdown or a canceled parent stops the poll promptly
// rather than spinning against a closing store. On cancellation the children are cancel-requested
// through the store, any that no executor has claimed yet are finalized canceled directly, and any
// child not yet terminal is reported canceled, the honest summary for an interrupted parent.
func (d *Dispatcher) waitChildren(ctx context.Context, ids []string) []run.Status {
	statuses := make([]run.Status, len(ids))
	canceled := false
	parent := ""
	for {
		byID := d.childStatuses(ctx, ids, &parent)
		done := 0
		for i, id := range ids {
			if statuses[i].Terminal() {
				done++
				continue
			}
			status, ok := byID[id]
			if !ok {
				continue
			}
			if ctx.Err() != nil && !canceled {
				continue
			}
			if status.Terminal() {
				statuses[i] = status
				done++
			}
		}
		if done == len(ids) {
			return statuses
		}
		if ctx.Err() != nil && !canceled {
			canceled = true
			d.cancelChildren(ids)
		}
		select {
		case <-time.After(childPollInterval):
		case <-ctx.Done():
			// Shutting down or the parent was canceled: request cancellation once, then stop waiting
			// instead of polling a store that may be closing. Children still running are reported
			// stopped, so the parent finalizes the same way they did.
			if !canceled {
				d.cancelChildren(ids)
			}
			stopped := d.stoppedStatus()
			for i := range statuses {
				if !statuses[i].Terminal() {
					statuses[i] = stopped
				}
			}
			return statuses
		}
	}
}

// childStatuses reads the current status of the tracked children. A single child is a point read;
// a wider set resolves the shared parent once, then reads all children in one parent-scoped query
// per tick, so a 512-shard split does not issue hundreds of point reads every poll interval. When
// the parent-scoped read fails it falls back to point reads for that tick.
func (d *Dispatcher) childStatuses(ctx context.Context, ids []string, parent *string) map[string]run.Status {
	out := make(map[string]run.Status, len(ids))
	pointReads := func() {
		for _, id := range ids {
			if r, err := d.store.Get(ctx, id); err == nil {
				out[id] = r.Status
			}
		}
	}
	if len(ids) == 1 {
		pointReads()
		return out
	}
	if *parent == "" {
		r, err := d.store.Get(ctx, ids[0])
		if err != nil || r.ParentID == nil {
			pointReads()
			return out
		}
		*parent = *r.ParentID
	}
	children, err := d.store.Shards(ctx, *parent)
	if err != nil {
		pointReads()
		return out
	}
	for _, c := range children {
		out[c.ID] = c.Status
	}
	return out
}

// stoppedStatus reports the terminal status to record for a run this dispatcher is stopping: canceled
// when somebody asked for it, interrupted when the server itself is going down. The two read the same
// from inside a stop, and telling them apart is what keeps a restart from writing the record a person
// clicking cancel leaves, and what lets a partial retry recover afterward.
func (d *Dispatcher) stoppedStatus() run.Status {
	if errors.Is(context.Cause(d.ctx), errShuttingDown) {
		return run.StatusInterrupted
	}
	return run.StatusCanceled
}

// stoppedReason is the error text to store beside stoppedStatus, empty for a cancel because a cancel
// speaks for itself.
func (d *Dispatcher) stoppedReason() string {
	if errors.Is(context.Cause(d.ctx), errShuttingDown) {
		return errShuttingDown.Error()
	}
	return ""
}

// cancelChildren asks every non-terminal child to stop: claimed children through their executor's
// cancel watch, unclaimed ones finalized directly since no executor will ever run them.
func (d *Dispatcher) cancelChildren(ids []string) {
	for _, id := range ids {
		r, err := d.store.Get(context.Background(), id)
		if err != nil || r.Status.Terminal() {
			continue
		}
		if err := d.store.RequestCancel(context.Background(), id); err != nil {
			d.log.Warn("dispatch: request child cancel: "+err.Error(), zap.String("run_id", id))
		}
		d.Cancel(id)
		// A held shard is settled here too. Finalizing only an unclaimed pending child left a shard
		// in pending_approval carrying a cancel flag that nothing acts on: no executor holds it, so
		// nothing reads the flag, and orphan resolution covers only an interrupted parent. It sat
		// in the approval queue forever, and approving it ran it under a parent that is gone.
		if r.ClaimedBy == "" &&
			(r.Status == run.StatusPending || r.Status == run.StatusPendingApproval) {
			d.finalize(r, d.stoppedStatus(), nil, d.stoppedReason())
		}
	}
}

// SubmitPipeline runs playbook steps as one pipeline and returns the parent run in pending state.
// Each step is a child run, so it gets the full matrix, events, and cross run treatment. Steps run
// in order, or as a dependency graph when any step declares depends_on. A step that fails stops
// what follows or depends on it unless the step is marked continue on failure.
func (d *Dispatcher) SubmitPipeline(ctx context.Context, name, inventory string, steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error) {
	if err := run.ValidatePipeline(steps); err != nil {
		return nil, err
	}

	parent := &run.Run{
		ID: run.NewID(), Playbook: name, Inventory: inventory, Kind: run.KindPipeline,
		Status: run.StatusPending, CreatedAt: d.now(),
	}
	run.ApplyOptions(parent, opts)
	stampReceipt(ctx, parent)
	stampOrg(ctx, parent)
	// The graph is stored on the parent so a pipeline held for approval can still be executed after
	// a restart, and so a finished pipeline can show the shape it ran.
	parent.Steps = steps
	// A retried pipeline returns the original parent instead of running its steps a second time.
	if existing, err := d.idempotentLookup(ctx, parent.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	if err := d.validateRun(ctx, parent); err != nil {
		return nil, err
	}
	d.resolveQueue(ctx, parent)
	if err := d.pipelineDenied(ctx, parent, steps); err != nil {
		return nil, err
	}
	// Consulted even when the parent arrives held, so the rule governing it binds its approval.
	held, perr := d.pipelineRequiresApproval(ctx, parent, steps)
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
		// A concurrent submission won the key; return its parent and start no steps here.
		return created, nil
	}
	if parent.Status == run.StatusPendingApproval {
		// Held for an approver. Approve starts it, since no claim loop picks up a pipeline parent.
		return parent, nil
	}

	d.wg.Add(1)
	go d.runPipeline(parent.Clone(), steps)

	return parent, nil
}

// runPipeline executes pipeline steps, in order or as a dependency graph, and finalizes the
// parent. The parent registers its own cancel so stopping the parent stops the running steps and
// halts everything that has not started.
func (d *Dispatcher) runPipeline(parent *run.Run, steps []run.PipelineStep) {
	defer d.wg.Done()

	pipeCtx, cancelPipe := context.WithCancel(d.ctx)
	d.register(parent.ID, cancelPipe)
	defer d.unregister(parent.ID)
	defer cancelPipe()

	// A pipeline's steps do not exist yet, so there are no children to settle here.
	if !d.parentMayStart(parent, nil) {
		return
	}
	d.startParentRow(parent)

	watchCtx, stopWatch := context.WithCancel(pipeCtx)
	defer stopWatch()
	go d.watch(watchCtx, parent.ID)

	var failed, canceled bool
	if hasDependencies(steps) {
		failed, canceled = d.runStepsDAG(pipeCtx, parent.Clone(), steps)
	} else {
		failed, canceled = d.runStepsLinear(pipeCtx, parent.Clone(), steps)
	}

	switch {
	case canceled:
		d.finalize(parent, d.stoppedStatus(), nil, d.stoppedReason())
	case failed:
		code := 1
		d.finalize(parent, run.StatusFailed, &code, "")
	default:
		code := 0
		d.finalize(parent, run.StatusSucceeded, &code, "")
	}
	d.publisher.CloseRun(parent.ID)
}

// runStepsLinear executes the steps one after another, stopping at a failure unless the failing
// step continues on failure. It returns whether any step failed and whether execution was
// canceled.
func (d *Dispatcher) runStepsLinear(ctx context.Context, parent *run.Run, steps []run.PipelineStep) (failed, canceled bool) {
	vars := baseStepVars(parent)
	for i, step := range steps {
		if ctx.Err() != nil {
			return failed, true
		}

		status, outputs := d.runStepAttempts(ctx, parent, step, i, cloneVars(vars))
		// A step the server stopped ends the pipeline the same way a canceled one does. The parent
		// records which of the two it was from the dispatcher's own state.
		if status == run.StatusCanceled || status == run.StatusInterrupted {
			return failed, true
		}
		if status != run.StatusSucceeded {
			failed = true
			if !step.ContinueOnFailure {
				return failed, canceled
			}
			continue
		}
		maps.Copy(vars, outputs)
	}
	return failed, canceled
}

// cloneVars copies a variable map, returning nil for an empty one so runs without inputs stay
// clean.
func cloneVars(vars map[string]any) map[string]any {
	if len(vars) == 0 {
		return nil
	}
	return maps.Clone(vars)
}

// baseStepVars is the variable map a step starts from before any dependency outputs are layered on.
//
// It is the pipeline parent's own extra vars, so a workflow's survey answers and extra vars reach its
// steps. A saved workflow template applies its survey answers to the pipeline parent, and without
// this seed those answers reached no step and were silently dropped. A fresh map is returned each
// call so a step mutating its inputs cannot reach the parent or another step. Dependency outputs are
// copied on top of this, so a step's published output still overrides a parent var of the same name
// for anything downstream.
func baseStepVars(parent *run.Run) map[string]any {
	vars := make(map[string]any, len(parent.ExtraVars))
	maps.Copy(vars, parent.ExtraVars)
	return vars
}

// stepRun builds the run a pipeline step executes as. The approval gate and the executor both go
// through this, so a policy is always evaluated against exactly what would run rather than against a
// separately assembled approximation that could drift from it.
func stepRun(parent *run.Run, step run.PipelineStep, idx, attempt int, vars map[string]any) *run.Run {
	inventory := step.Inventory
	if inventory == "" {
		inventory = parent.Inventory
	}
	i := idx
	child := &run.Run{
		ID: run.NewID(), Playbook: step.Playbook, Inventory: inventory,
		Tool: step.Tool, Command: step.Command,
		Status: run.StatusPending, CreatedAt: time.Now(),
		ParentID: &parent.ID, StepIndex: &i, StepName: step.Name, Attempt: attempt,
		ExtraVars: vars,
		// A step is part of the pipeline its parent was authorized by, not a new request, and it
		// may be built after that request returned, so the parent's receipt is the truthful one.
		AuditReceipt: parent.AuditReceipt,
	}
	// A step names its own tool, command, playbook, and inventory, so those are not inherited. How
	// the run is executed still comes from the pipeline: the environment it runs in, the credentials
	// it may use, the project it reads from, the queue it lands on, and how long it may take.
	child.CredentialIDs = parent.CredentialIDs
	child.ProjectID = parent.ProjectID
	child.InventoryID = parent.InventoryID
	child.Queue = parent.Queue
	child.Timeout = parent.Timeout
	child.Image = parent.Image
	child.PullCredentialID = parent.PullCredentialID
	child.Labels = parent.Labels
	child.Actor = parent.Actor
	child.ActorType = parent.ActorType
	// A step is scoped to the pipeline's tenant. A step may name no stored object, so without the
	// parent's org it would be an objectless run readable across every tenant.
	child.OrgID = parent.OrgID
	// A launch-time host limit constrains every step, matching how a workflow limit applies to each
	// job. A step names its own inventory but never its own host limit, so the parent's is the only
	// one, and dropping it would run a limited launch against the whole fleet.
	child.Limit = parent.Limit
	// A dry-run pipeline is dry all the way down. A step cannot opt out of a parent that was
	// submitted to make no changes: check mode is a promise about the whole run, and a step running
	// for real underneath it would break that promise silently.
	child.DryRun = step.DryRun || parent.DryRun
	// Notifications stay on the parent. The pipeline is the thing that finished, and copying its
	// targets onto every step would page once per step instead of once per run.
	return child
}

// runStepAttempts executes one pipeline step, re-running it until it succeeds or its retry budget
// is spent. Every attempt is its own child run with an attempt number, so each try keeps a full
// matrix, events, and history. The step receives vars as its extra vars, and on success the
// values it published with set_stats come back for its dependents.
func (d *Dispatcher) runStepAttempts(ctx context.Context, parent *run.Run, step run.PipelineStep,
	idx int, vars map[string]any) (run.Status, map[string]any) {
	status := run.StatusFailed
	for attempt := 0; attempt <= step.Retries; attempt++ {
		if ctx.Err() != nil {
			return run.StatusCanceled, nil
		}
		child := stepRun(parent, step, idx, attempt, vars)
		child.CreatedAt = d.now()
		if err := d.store.Save(context.Background(), child); err != nil {
			d.log.Error("dispatch: save pipeline step: "+err.Error(), zap.String("run_id", parent.ID))
			return run.StatusFailed, nil
		}
		d.wake()
		status = d.waitChildren(ctx, []string{child.ID})[0]
		if status == run.StatusSucceeded {
			return status, d.stepOutputs(child)
		}
		if status == run.StatusCanceled {
			return status, nil
		}
	}
	return status, nil
}

// stepOutputs returns what a finished step published with set_stats, read from the step's own run
// record. It is best effort; a read failure just means no outputs flow downstream.
//
// The values are folded and recorded by whichever process executed the step, before the step went
// terminal, so the coordinator reads the answer rather than recomputing it from events. Recomputing
// it here re-read the step's events through the coordinator's store, and a step executed across the
// relay leaves its events on the control node while the executor's store cannot read them back at
// all, so every relay-executed step silently published nothing to its dependents.
func (d *Dispatcher) stepOutputs(child *run.Run) map[string]any {
	fresh, err := d.storeGetWithRetries(context.Background(), child.ID)
	if err != nil {
		d.log.Error("dispatch: read run for outputs: "+err.Error(), zap.String("run_id", child.ID))
		return nil
	}
	if len(fresh.Outputs) == 0 {
		return nil
	}
	return fresh.Outputs
}

// DefaultMaxShards caps how many groups a split fans out into when an operator sets no override, so
// one submission cannot spawn thousands of child runs and overwhelm the coordinator's per-child
// polling and the single store writer. The --max-shards flag raises or lowers it; a split is always
// bounded below by the host count regardless.
const DefaultMaxShards = 512

// partition splits hosts into at most shards groups balanced by expected cost. Each host weighs
// its average duration from costs; a host without history weighs the average of the known costs,
// or one when nothing is known, which degrades to balancing by host count. Hosts are placed
// heaviest first into the group with the least total weight, breaking ties by fewer hosts and then
// lower group index so the result is deterministic.
func partition(hosts []string, shards int, costs map[string]float64) [][]string {
	n := min(shards, len(hosts))
	weights := hostWeights(hosts, costs)

	order := make([]string, len(hosts))
	copy(order, hosts)
	sort.SliceStable(order, func(i, j int) bool {
		if weights[order[i]] != weights[order[j]] {
			return weights[order[i]] > weights[order[j]]
		}
		return order[i] < order[j]
	})

	groups := make([][]string, n)
	totals := make([]float64, n)
	for _, host := range order {
		lightest := 0
		for i := 1; i < n; i++ {
			switch {
			case totals[i] < totals[lightest]:
				lightest = i
			case totals[i] == totals[lightest] && len(groups[i]) < len(groups[lightest]):
				lightest = i
			}
		}
		groups[lightest] = append(groups[lightest], host)
		totals[lightest] += weights[host]
	}
	return groups
}

// hostWeights maps each host to its expected cost. Hosts missing from costs get the average known
// cost so they neither dominate nor vanish, and a flat one when no host has a usable cost.
func hostWeights(hosts []string, costs map[string]float64) map[string]float64 {
	known := 0.0
	knownCount := 0
	for _, host := range hosts {
		if c, ok := costs[host]; ok && c > 0 {
			known += c
			knownCount++
		}
	}
	fallback := 1.0
	if knownCount > 0 {
		fallback = known / float64(knownCount)
	}

	out := make(map[string]float64, len(hosts))
	for _, host := range hosts {
		if c, ok := costs[host]; ok && c > 0 {
			out[host] = c
			continue
		}
		out[host] = fallback
	}
	return out
}
