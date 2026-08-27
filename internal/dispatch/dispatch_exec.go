package dispatch

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"go.uber.org/zap"
)

// Close stops accepting new work, cancels in-flight runs, and waits for workers to drain. The
// cancellation carries its cause, so a run stopped by the shutdown is recorded as interrupted, which is
// what happened, rather than as the cancel a person asks for.
func (d *Dispatcher) Close() {
	d.cancel(errShuttingDown)
	d.wg.Wait()
	d.notifyWG.Wait()
}

// executeLeased runs a claimed run on the worker slot the claim loop already holds.
func (d *Dispatcher) executeLeased(base context.Context, r *run.Run) run.Status {
	runCtx, cancel := context.WithCancel(base)
	d.register(r.ID, cancel)
	defer d.unregister(r.ID)
	defer cancel()

	// A run timeout stops a hung tool from holding this worker slot forever. The cause lets the
	// outcome tell a timeout apart from a user cancel. A per-run timeout overrides the dispatcher
	// default, and either bound stays off when zero.
	timeout := d.runTimeout
	if r.Timeout > 0 {
		timeout = time.Duration(r.Timeout) * time.Second
	}
	if timeout > 0 {
		var stop context.CancelFunc
		runCtx, stop = context.WithTimeoutCause(runCtx, timeout, errRunTimeout)
		defer stop()
	}

	return d.execute(runCtx, r)
}

// execute runs r and returns its terminal status. A terraform or opentofu apply that a plan-content
// policy scopes is planned first and its apply proposed for approval; every other run executes in a
// single phase, unchanged. The run carries this process's lease while it executes: a watcher renews
// it and honors cancel requests written to the store by any process.
func (d *Dispatcher) execute(ctx context.Context, r *run.Run) run.Status {
	// An approved run executes exactly the spec that was approved. The digest was committed to the
	// chain when the decision was made and stamped on the run; a mismatch here means the row
	// changed underneath the decision, so the run fails with that stated rather than executing
	// something nobody approved. The chain catches the same tampering at verify time; this refuses
	// to perform it in the first place.
	if r.ApprovedSpecDigest != "" {
		got, serr := outcome.SpecDigest(r)
		if serr != nil {
			d.finalize(r, run.StatusFailed, nil, "could not recompute the approved spec: "+serr.Error())
			d.publisher.CloseRun(r.ID)
			return run.StatusFailed
		}
		if got != r.ApprovedSpecDigest {
			d.finalize(r, run.StatusFailed, nil,
				"refused: the spec changed after it was approved, so this is not the change the approver released")
			d.publisher.CloseRun(r.ID)
			return run.StatusFailed
		}
	}
	policies, perr := d.planGatePolicies(ctx, r)
	if perr != nil {
		// The plan-content gate could not be evaluated, so applying now would apply past a gate
		// nobody checked. The run fails with the reason instead.
		d.finalize(r, run.StatusFailed, nil, perr.Error())
		// Every other terminal path closes the stream, so a UI tailing this run sees it end.
		d.publisher.CloseRun(r.ID)
		return run.StatusFailed
	}
	if policies != nil {
		return d.executePlanGate(ctx, r, policies)
	}
	return d.executeRun(ctx, r)
}

// executeRun runs r's spec once, streaming output to the store, and finalizes it from the runner
// outcome. It is the single-phase path taken by every run a plan-content policy does not gate.
func (d *Dispatcher) executeRun(ctx context.Context, r *run.Run) run.Status {
	return d.streamSpec(ctx, r, r.DryRun, nil,
		func(res roundhouse.Result, runErr error, mask *masker, fold *run.SummaryFold) run.Status {
			// Write the summaries and any drift while the run is still non-terminal. The store fences
			// auxiliary writes to a terminal run, so finalizing first would reject the run's own final
			// summaries; ordering the writes before finalize lets them land and drops only a
			// reclaimed-but-alive worker's late writes.
			d.summarize(r, fold)
			if res.Drift {
				d.recordPlanDrift(r)
			}
			return d.outcome(ctx, r, res, runErr, mask)
		})
}

// streamSpec runs one execution of r's spec, streaming combined output and structured events to the
// store, then calls finish with the runner outcome to finalize r while the run's temp files and lease
// watcher are still live. dryRun forces the tool's no-change mode regardless of r.DryRun, which the
// plan gate uses to plan before applying, and tee, when non-nil, also receives the combined output so
// the gate can inspect the plan. A setup failure finalizes r as failed, redacting the detail, and
// returns without calling finish. It always closes the run's output stream before returning, and
// returns finish's status on success or StatusFailed on a setup failure.
//
// finish also receives the summary fold the tailer filled from the run's events. The tailer has
// stopped by the time finish is called, so the fold is complete and safe to read.
func (d *Dispatcher) streamSpec(ctx context.Context, r *run.Run, dryRun bool, tee io.Writer,
	finish func(res roundhouse.Result, runErr error, mask *masker, fold *run.SummaryFold) run.Status,
) run.Status {
	started := d.now()
	r.Status = run.StatusRunning
	r.StartedAt = &started
	r.ClaimedBy = d.owner
	// The lease time is not written from here. The store stamps it when it grants the claim and again on
	// every renewal, and Postgres ages leases against that same clock, so writing this process's time
	// over it recorded a lease already expired on any worker whose clock trails the database and the next
	// sweep interrupted a run that had just started.
	_ = d.save(r)

	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go d.watch(watchCtx, r.ID)

	eventsPath, cleanup, eventsErr := d.eventsFile(r.ID)
	defer cleanup()
	if eventsErr != nil {
		// Capture is off, so this run will finish with an empty matrix and no events however well
		// it goes. Record why on the run, rather than leaving a green run that shows nothing.
		r.Warning = "event capture unavailable, so this run records no events: " + eventsErr.Error()
		_ = d.save(r)
	}

	parent := ""
	if r.ParentID != nil {
		parent = *r.ParentID
	}
	stop := make(chan struct{})
	tailed := make(chan struct{})
	mask := &masker{}
	// The fold accumulates the run's summaries as its events go by, so finishing needs no second
	// read of them. Only the tail goroutine writes to it, and it has exited before finish reads it.
	fold := run.NewSummaryFold(r.CreatedAt)
	go func() {
		defer close(tailed)
		d.tailEvents(r.ID, parent, eventsPath, stop, mask, fold)
	}()

	// fail finalizes r as failed and closes its output stream when a setup step cannot complete, so a
	// run that never reached the runner still records why and stops the tailer.
	fail := func(err error) run.Status {
		close(stop)
		<-tailed
		d.finalize(r, run.StatusFailed, nil, mask.redactString(err.Error()))
		d.publisher.CloseRun(r.ID)
		return run.StatusFailed
	}

	// A cancel requested between the claim and the running save is honored here, before any tool
	// starts. The store keeps the cancel flag sticky across saves, so this read observes it.
	//
	// A read that fails is not a run with no cancel on it. Treating an error as "carry on" started
	// the tool on the strength of a question nobody answered, which is the same fail-open shape
	// every other gate in this file was written to avoid.
	//
	// Refusing is not the same as failing it, though, and failing it was too blunt. This read sits
	// on the start path of every run on every executor, and a relay worker asking a control node
	// that is restarting gets a refused connection back in microseconds, so the whole retry budget
	// burns in well under a second. A rolling upgrade then terminally failed every run that
	// happened to start during it, and each one had to be found and replayed by hand.
	//
	// So the run is left alone instead: no tool starts, and the lease is simply not renewed. The
	// sweep that already exists for an executor that stopped without finishing takes it back and
	// marks it interrupted, which is retryable, and that is exactly what happened here.
	cur, cerr := d.storeGetWithRetries(ctx, r.ID)
	switch {
	case cerr != nil:
		d.log.Error("dispatch: could not check for a cancel before starting: "+cerr.Error(),
			zap.String("run_id", r.ID))
		// The run is left non-terminal for the sweep to reclaim and mark interrupted, then retry.
		// So the tailer is stopped but the run is not closed to viewers: CloseRun signals that a run
		// finished producing output, which would stop a co-located live viewer from watching a run
		// that is about to run again. Closing the socket without that end signal, the way the stream
		// handler's draining early-return does, lets the viewer reconnect and resume.
		close(stop)
		<-tailed
		return run.StatusRunning
	case cur.CancelRequested:
		close(stop)
		<-tailed
		d.finalize(r, run.StatusCanceled, nil, "")
		d.publisher.CloseRun(r.ID)
		return run.StatusCanceled
	}

	logs := &logSink{store: d.store, id: r.ID, log: d.log, publisher: d.publisher, mask: mask}
	var sink io.Writer = logs
	if tee != nil {
		sink = io.MultiWriter(sink, tee)
	}
	spec := roundhouse.Spec{
		Playbook: r.Playbook, Inventory: r.Inventory, Tool: r.Tool, Command: r.Command,
		DryRun: dryRun, EventsPath: eventsPath, Limit: r.Limit, ExtraVars: r.ExtraVars,
		Tags: r.Tags, SkipTags: r.SkipTags, Verbosity: r.Verbosity, Forks: r.Forks,
		DiffMode: r.DiffMode, Image: r.Image,
	}
	if r.Image != "" {
		if err := d.resolvePullCredential(r.PullCredentialID, &spec); err != nil {
			return fail(err)
		}
	}
	d.refreshOnLaunch(ctx, r)
	invCleanup, invSecrets, err := d.materializeInventory(ctx, r, &spec)
	if err != nil {
		return fail(err)
	}
	defer invCleanup()
	mask.set(invSecrets)

	projectCleanup, err := d.resolveProject(r, &spec)
	defer projectCleanup()
	if err != nil {
		return fail(err)
	}
	d.applyDefaultImage(&spec)
	// Record the image the run actually executed in. spec.Image was seeded from r.Image and is only
	// ever filled when it was empty, so a run that pinned its own image sees no change, a run that
	// took a project or server default now has that image on its record, and a host run stays empty.
	// The evidence a run leaves must show which environment ran it, not only the one it asked for.
	r.Image = spec.Image

	credCleanup, secrets, err := d.materializeCredentials(ctx, r, &spec)
	if err != nil {
		credCleanup()
		return fail(err)
	}
	defer credCleanup()
	// The registry pull login reaches the spec through resolvePullCredential, not
	// materializeCredentials, so its values are not in secrets. Add them here, or a container
	// runner that echoes a failed registry login leaks the password into the run's output the way
	// every other credential is masked against. Both the run's and the project's pull credential
	// have resolved onto the spec by now, so masking the effective login covers both.
	allSecrets := append(secrets, registrySecrets(&spec)...)
	mask.set(append(allSecrets, invSecrets...))

	res, runErr := d.runner.Run(ctx, spec, sink)
	// The masker holds back the end of each chunk so a secret split across two of them is caught
	// before either half is emitted. The process can write no more, so the withheld tail is released
	// now, while the run is still live: an append to a finalized run is fenced and would be dropped.
	logs.flush()

	close(stop)
	<-tailed

	status := finish(res, runErr, mask, fold)
	d.publisher.CloseRun(r.ID)
	return status
}

// watch renews the executing run's lease and cancels it when another process requests a stop or
// the lease is convincingly lost. It exits when the run's context ends.
//
// An unreachable store is not a lost lease. The lease in the store lives leaseTTL from its last
// renewal, and no sweep can reclaim a lease that has not expired, so while it is still valid nothing
// else can touch this run and stopping early only kills a tool partway through its changes for
// nothing. Counting a fixed three failures instead gave up after about nine seconds of a thirty second
// lease, and a refused connection comes back in microseconds, so every control node restart and every
// brief store outage killed the runs on every healthy worker mid-change and recorded each as canceled
// with no error, indistinguishable from a cancel somebody asked for. The executor now works on while
// the lease could still be its own and stops when it has actually expired, which is the first moment
// another process could claim the run.
//
// A store that reports the run is not this owner's is different: somebody else holds it, and carrying
// on would mean two executors changing the same hosts. That stops at once.
func (d *Dispatcher) watch(ctx context.Context, id string) {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	// The lease was stamped by the claim that led here, so it is live as of now.
	lastRenewed := time.Now()
	// Renewed once before the first tick, which is what makes the store's clock the one that owns this
	// lease.
	//
	// The save that set the run running rewrites the whole row, lease time included, from this process's
	// clock. Postgres stamps a lease from the database clock and ages leases against that same clock, so
	// on a worker whose clock trails the database by more than the lease lifetime that save recorded a
	// lease already expired, and the next sweep interrupted a run that had just started. Renewing here
	// puts the database's own time back on the row immediately rather than up to a tick later, so the
	// window in which a sweep could read a fresh lease as an old one closes at once.
	_ = d.store.Heartbeat(context.Background(), id, d.owner)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		switch err := d.store.Heartbeat(context.Background(), id, d.owner); {
		case err == nil:
			lastRenewed = time.Now()
		case errors.Is(err, run.ErrNotFound):
			d.log.Warn("dispatch: lease is no longer ours: "+err.Error(), zap.String("run_id", id))
			d.Cancel(id)
			return
		default:
			if expired := time.Since(lastRenewed); expired < leaseTTL {
				d.log.Warn("dispatch: heartbeat failed, lease still valid: "+err.Error(),
					zap.String("run_id", id), zap.Duration("unrenewed_for", expired))
				continue
			}
			d.log.Warn("dispatch: lease expired unrenewed: "+err.Error(), zap.String("run_id", id))
			d.Cancel(id)
			return
		}
		r, err := d.store.Get(context.Background(), id)
		if err != nil {
			continue
		}
		if r.CancelRequested {
			d.Cancel(id)
			return
		}
	}
}

// summarize records the run's per-host, per-task, and facts summaries and the values it published,
// from the fold the tailer filled as the run's events streamed past. It runs while the run is still
// non-terminal, so the writes are not fenced, and it must run before the run's terminal save, which
// is what commits its outcome.
//
// The fold is filled while the events go by rather than read back afterwards. Reading them back
// asked the store for a run's own events, which the control node can serve and a relay worker's
// store cannot: every run executed across the relay recorded no host at all, emptying fleet health,
// drift, host history, task trends, host costs, run comparison, and the failed-host relaunch for it,
// while the outcome committed on the control node said the run had no hosts. Folding as the events
// stream also keeps the state proportional to hosts and tasks rather than to a page of events, and
// spends no second pass over a run that can carry hundreds of thousands of them.
//
// The caller must have stopped the tailer before calling this, which is what makes reading the
// fold safe: the tail goroutine is the only writer, and its completion happens before this read.
func (d *Dispatcher) summarize(r *run.Run, fold *run.SummaryFold) {
	summaries := fold.HostSummaries()
	if len(summaries) > 0 {
		if err := withRetries(func() error {
			return d.store.SaveHostSummary(context.Background(), r.ID, summaries)
		}); err != nil {
			d.log.Error("dispatch: save host summary: "+err.Error(), zap.String("run_id", r.ID))
		}
	} else if run.NormalizeTool(r.Tool) == run.ToolAnsible {
		// A playbook run with no recap leaves nothing behind for fleet health, drift, host history,
		// or a failed-host relaunch. Recording zero hosts silently is what made that invisible, so
		// the run says so on its own record.
		addWarning(r, "this run recorded no per-host result, so it is absent from fleet health, "+
			"drift, and host history")
	}
	if facts := fold.HostFacts(); len(facts) > 0 {
		if err := withRetries(func() error {
			return d.store.SaveHostFacts(context.Background(), r.ID, facts)
		}); err != nil {
			d.log.Error("dispatch: save host facts: "+err.Error(), zap.String("run_id", r.ID))
		}
	}
	if tasks := fold.TaskSummaries(); len(tasks) > 0 {
		if err := withRetries(func() error {
			return d.store.SaveTaskSummary(context.Background(), r.ID, tasks)
		}); err != nil {
			d.log.Error("dispatch: save task summary: "+err.Error(), zap.String("run_id", r.ID))
		}
	}
	// The values a playbook published belong on the run, so a pipeline step's dependents read them
	// from the step's record instead of re-reading its events from a store that may not hold them.
	// The terminal save that follows carries them, across the relay as well as in process.
	if outputs := fold.Outputs(); len(outputs) > 0 {
		r.Outputs = outputs
	}
}

// addWarning records a degradation on r, keeping any warning already there. A run can be degraded
// more than once, and the later note must not erase the earlier one.
func addWarning(r *run.Run, warning string) {
	if r.Warning == "" {
		r.Warning = warning
		return
	}
	r.Warning += "; " + warning
}

// outcome finalizes r from the run result and returns the terminal status. Failure text passes
// through the run's masker so a runner error cannot leak a resolved secret into the stored run.
func (d *Dispatcher) outcome(
	ctx context.Context, r *run.Run, res roundhouse.Result, err error, mask *masker,
) run.Status {
	switch {
	case err != nil && errors.Is(context.Cause(ctx), errRunTimeout):
		d.finalize(r, run.StatusFailed, nil, "run canceled: exceeded its timeout")
		return run.StatusFailed
	case err != nil && errors.Is(context.Cause(ctx), errShuttingDown):
		// The server stopped mid-run. That is interrupted, the status whose meaning is exactly this and
		// which a partial retry accepts, not the cancel a person asks for.
		d.finalize(r, run.StatusInterrupted, nil, errShuttingDown.Error())
		return run.StatusInterrupted
	case err != nil && ctx.Err() != nil:
		d.finalize(r, run.StatusCanceled, nil, "")
		return run.StatusCanceled
	case err != nil:
		d.finalize(r, run.StatusFailed, nil, mask.redactString(err.Error()))
		return run.StatusFailed
	case res.ExitCode == 0:
		d.finalize(r, run.StatusSucceeded, &res.ExitCode, "")
		return run.StatusSucceeded
	default:
		d.finalize(r, run.StatusFailed, &res.ExitCode, "")
		return run.StatusFailed
	}
}

// register records a cancel func for a run so it can be stopped by id.
func (d *Dispatcher) register(id string, cancel context.CancelFunc) {
	d.cmu.Lock()
	d.cancels[id] = cancel
	d.cmu.Unlock()
}

// unregister drops a run's cancel func once it is no longer cancelable.
func (d *Dispatcher) unregister(id string) {
	d.cmu.Lock()
	delete(d.cancels, id)
	d.cmu.Unlock()
}

// Cancel stops the pending or executing run with the given id, including a parent split and its
// shards. It reports whether a cancelable run was found.
func (d *Dispatcher) Cancel(id string) bool {
	d.cmu.Lock()
	cancel, ok := d.cancels[id]
	d.cmu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// finalize records the terminal status, exit code, failure detail, and end time of r, commits the
// run's outcome to the audit chain, and sends webhook notifications for top-level runs. It refuses to
// resurrect a run another actor already moved to a different terminal state, such as the janitor
// interrupting an expired lease, so a slow but still alive worker that is reclaimed cannot overwrite
// the interrupt with a success.
//
// The chain is written only after the store confirms the terminal record landed. An outcome entry
// commits a digest of what the stored evidence says the run did, so committing one the store never
// accepted leaves the chain asserting an outcome the database contradicts and a receipt nobody can
// verify. When the write does not land, r keeps the status the store holds and the run is left for
// the sweep to reclaim and mark interrupted, which is retryable.
func (d *Dispatcher) finalize(r *run.Run, status run.Status, exitCode *int, failure string) {
	fin := run.Finalization{
		Status: status, ExitCode: exitCode, Error: failure, Image: r.Image,
		CommitSHA: r.CommitSHA, PullCredentialID: r.PullCredentialID,
		Outputs: r.Outputs, Warning: r.Warning, EndedAt: d.now(),
	}
	stored, recorded := d.recordTerminal(r, fin)
	if !recorded {
		r.Status = stored
		return
	}
	// Commit the outcome to the chain before notifying anyone. A notification is external and
	// after-the-fact; the tamper-evident record of what the run did comes first.
	d.commitOutcome(r)
	d.notify(r)
}

// recordTerminal writes r's terminal record to the store and reports the run's stored status and
// whether the write landed. It applies fin to r only once the store has accepted it, so an in-memory
// run that says succeeded is a run the database says succeeded too.
//
// The first attempt is one conditional update from running, the state every executing run finalizes
// from: it claims the transition and records the facts that explain it together, so no failure can
// leave a run terminal with no exit code, where neither this dispatcher nor the janitor, which sweeps
// pending and running runs, would ever look at it again.
//
// A run that is not running falls back to reading the stored state and deciding from it. That covers
// a legitimate finalize from a non running state, such as a rejection, which is never fenced because
// its stored status already equals the target, and it covers a relay worker, whose client cannot
// compare and swap and reports through Save. When even a retried read cannot establish the stored
// state, nothing is written: skipping the write risks a janitor interrupt on a healthy run, but
// writing blind risks resurrecting a run another actor already terminalized, which is the failure the
// fence exists to stop.
func (d *Dispatcher) recordTerminal(r *run.Run, fin run.Finalization) (run.Status, bool) {
	ctx := context.Background()
	// This dispatcher is finalizing a run it executed, so the write is fenced on the lease it thinks
	// it holds as well as on the status. If the janitor requeued the run and another worker claimed
	// and started it, the status is running again and the status fence alone would let this write
	// terminalize the second worker's live run. The fallback below then reads the stored state and
	// decides from it, which is exactly the path a lost lease should take.
	fin.Owner = r.ClaimedBy
	if moved, err := d.store.FinalizeRunning(ctx, r.ID, fin); err == nil && moved {
		applyFinalization(r, fin)
		return fin.Status, true
	}
	cur, err := d.storeGetWithRetries(ctx, r.ID)
	if err != nil {
		d.log.Warn("dispatch: cannot verify run state, skipping finalize: "+err.Error(),
			zap.String("run_id", r.ID))
		return r.Status, false
	}
	if cur.Status.Terminal() && cur.Status != fin.Status {
		d.log.Warn("dispatch: run already finalized by another actor, not overwriting",
			zap.String("run_id", r.ID), zap.String("stored", string(cur.Status)),
			zap.String("attempted", string(fin.Status)))
		return cur.Status, false
	}
	// Save writes the whole run, so the terminal fields go on a copy: a save that fails must leave
	// the caller's run reading the way the store still does.
	next := *r
	applyFinalization(&next, fin)
	if err := d.save(&next); err != nil {
		return cur.Status, false
	}
	*r = next
	return fin.Status, true
}

// applyFinalization copies a stored terminal record onto the run in memory.
func applyFinalization(r *run.Run, fin run.Finalization) {
	ended := fin.EndedAt
	r.Status = fin.Status
	r.ExitCode = fin.ExitCode
	r.Error = fin.Error
	r.EndedAt = &ended
}

// save persists r using a background context so terminal state is recorded even during shutdown.
// A failed save retries briefly, since losing a terminal status strands the run as running, and is
// logged here and returned for a caller whose next step depends on the write having landed.
func (d *Dispatcher) save(r *run.Run) error {
	err := withRetries(func() error {
		return d.store.Save(context.Background(), r)
	})
	if err != nil {
		d.log.Error("dispatch: save run: "+err.Error(), zap.String("run_id", r.ID))
	}
	return err
}

// withRetries runs a store write, retrying transient failures with a short backoff. Concurrent
// executors contend on a single writer under SQLite, so one busy moment must not lose state.
// storeGetWithRetries reads a run, retrying the way every other store call on this path does, so a
// single busy moment under a contended writer is not mistaken for an answer.
func (d *Dispatcher) storeGetWithRetries(ctx context.Context, id string) (*run.Run, error) {
	var out *run.Run
	err := withRetries(func() error {
		var gerr error
		out, gerr = d.store.Get(ctx, id)
		return gerr
	})
	return out, err
}

func withRetries(f func() error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if err = f(); err == nil {
			return nil
		}
		// A write that landed in part is not retried as a whole: what already arrived is recorded, and
		// sending it again would record it twice. The store that reports this has retried the part that
		// actually failed.
		if errors.Is(err, run.ErrPartlyDelivered) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 75 * time.Millisecond)
	}
	return err
}

// eventsFile creates a temp file for the run's structured events and returns its path and a cleanup
// func. On failure it logs and returns an empty path with the error, which disables event capture:
// the run still executes, but it produces no events, so the caller records why on the run.
func (d *Dispatcher) eventsFile(id string) (string, func(), error) {
	f, err := os.CreateTemp("", "switchtender-events-*.ndjson")
	if err != nil {
		d.log.Error("dispatch: create events file: "+err.Error(), zap.String("run_id", id))
		return "", func() {}, err
	}
	path := f.Name()
	_ = f.Close()
	return path, func() { _ = os.Remove(path) }, nil
}

// tailEvents follows the run's event sidecar file, parsing, storing, and publishing complete lines
// as they appear, until stop is closed and a final drain has run. Each poll tick flushes every new
// line as one batch, so a chatty tool costs one store write per tick instead of one per line.
// Events from a child run are also published under its parent so a split or pipeline page streams
// live. The final drain keeps a trailing line missing its newline, since a killed tool can be cut
// off mid-write and what it managed to publish still belongs to the run. Every line it parses also
// goes into fold, which is how the run's summaries are built without reading its events back.
func (d *Dispatcher) tailEvents(id, parent, path string, stop <-chan struct{}, mask *masker,
	fold *run.SummaryFold) {
	if path == "" {
		<-stop
		return
	}
	f, err := os.Open(path)
	if err != nil {
		d.log.Error("dispatch: open events file: "+err.Error(), zap.String("run_id", id))
		<-stop
		return
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(f)
	var partial []byte
	drain := func(final bool) {
		var lines [][]byte
		for {
			chunk, err := reader.ReadBytes('\n')
			if len(chunk) > 0 {
				partial = append(partial, chunk...)
				if partial[len(partial)-1] == '\n' {
					lines = append(lines, append([]byte(nil), partial...))
					partial = partial[:0]
				}
			}
			if err != nil {
				break
			}
		}
		if final && len(partial) > 0 {
			lines = append(lines, append([]byte(nil), partial...))
			partial = partial[:0]
		}
		d.flushEventLines(id, parent, lines, mask, fold)
	}

	ticker := time.NewTicker(tailPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			drain(true)
			return
		case <-ticker.C:
			drain(false)
		}
	}
}

// flushEventLines parses a batch of event lines, redacts known secrets from them, appends them to
// the run's own event log in one write, and publishes them to the run's topic. When the run is a
// child of a split or pipeline, it also publishes them to the parent's topic, where the parent's
// stream forwards them live, since a coordinator keeps no event log of its own to re-read. A single
// damaged line is logged and skipped so the rest of the batch lands.
//
// The batch is folded into the run's summaries here too, after redaction, so what a summary records
// is what the store holds. Folding as the batch passes is what lets a run be summarized without
// reading its events back out of a store, which a relay worker cannot do.
func (d *Dispatcher) flushEventLines(id, parent string, lines [][]byte, mask *masker,
	fold *run.SummaryFold) {
	var events []event.Event
	for _, raw := range lines {
		e, ok, err := event.ParseLine(raw)
		if err != nil {
			d.log.Error("dispatch: parse event line: "+err.Error(), zap.String("run_id", id))
			continue
		}
		if !ok {
			continue
		}
		events = append(events, e)
	}
	if len(events) == 0 {
		return
	}
	for i := range events {
		mask.redactEvent(&events[i])
	}
	fold.Add(events)
	if err := withRetries(func() error {
		return d.store.AppendEvents(context.Background(), id, events)
	}); err != nil {
		d.log.Error("dispatch: append events: "+err.Error(), zap.String("run_id", id))
	}
	d.publisher.PublishEvents(id, events)
	if parent != "" {
		d.publisher.PublishEvents(parent, events)
	}
}
