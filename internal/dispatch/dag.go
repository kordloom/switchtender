package dispatch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// stepState tracks where a pipeline step is in the graph walk.
type stepState int

const (
	// stepWaiting means the step has not started and its dependencies are not settled.
	stepWaiting stepState = iota
	// stepRunning means the step's child run is executing.
	stepRunning
	// stepDone means the step's child run reached a terminal status, or an approval step was decided.
	stepDone
	// stepSkipped means the step never ran because a dependency failed, was skipped, or the
	// pipeline was canceled first.
	stepSkipped
	// stepAwaiting means an approval step has asked and is waiting for a person to decide it.
	stepAwaiting
)

// approvalPollInterval is how often a coordinator with an approval step waiting reads the step's
// stored record, since the decision is written by whichever process the approver reached.
const approvalPollInterval = childPollInterval

// stepResult carries one step's terminal status and published outputs back to the graph walk.
type stepResult struct {
	// idx is the step's declaration index.
	idx int
	// status is the child run's terminal status.
	status run.Status
	// outputs holds the values the step published for its dependents.
	outputs map[string]any
	// reason says why the step was refused, before anything of it was stored or once it finished,
	// empty when it was not.
	reason string
}

// depClosures returns, per step, which steps it transitively depends on, over both the steps it
// waits for and the approval steps whose denial it handles.
func depClosures(steps []run.PipelineStep, byName map[string]int) [][]bool {
	closures := make([][]bool, len(steps))
	var build func(i int) []bool
	build = func(i int) []bool {
		if closures[i] != nil {
			return closures[i]
		}
		closure := make([]bool, len(steps))
		closures[i] = closure
		for _, dep := range append(append([]string(nil), steps[i].DependsOn...), steps[i].IfDenied...) {
			j := byName[dep]
			closure[j] = true
			for k, in := range build(j) {
				if in {
					closure[k] = true
				}
			}
		}
		return closure
	}
	for i := range steps {
		build(i)
	}
	return closures
}

// hasDependencies reports whether any step declares a dependency, which switches the pipeline
// from ordered execution to a graph walk.
func hasDependencies(steps []run.PipelineStep) bool {
	for _, s := range steps {
		if len(s.DependsOn) > 0 || len(s.IfDenied) > 0 {
			return true
		}
	}
	return false
}

// dagWalk is one coordinator's walk of a pipeline graph. A walk can begin from nothing, or from the
// step records a paused workflow left behind, which is how a workflow that parked at an approval
// step continues on whichever process resumes it.
type dagWalk struct {
	// d is the dispatcher the walk executes steps through.
	d *Dispatcher
	// parent is the pipeline run.
	parent *run.Run
	// steps is the graph being walked.
	steps []run.PipelineStep
	// byName indexes the steps by name.
	byName map[string]int
	// closures holds each step's transitive dependencies.
	closures [][]bool
	// states is where each step is in the walk.
	states []stepState
	// results holds each settled step's terminal status.
	results []run.Status
	// outputs holds what each finished step published.
	outputs []map[string]any
	// nodes maps an approval step's index to the record of its request.
	nodes map[int]string
	// handled marks an approval step that some step handles the denial of, so its denial is the
	// workflow taking a path its author wrote rather than the workflow failing.
	handled []bool
	// refused marks an approval step whose approval no longer matched the state it was given for, or
	// whose denial the chain does not record, so neither of its paths runs.
	refused []bool
	// roots holds, per decided approval step, the chain entry that records its verdict, for the
	// steps it releases to name. It is empty where nothing was read: a step not yet decided, one
	// withdrawn, or any step on a dispatcher with no binding key.
	roots []binding.Root
	// workflowRoots holds the chain entry recording the decision that won the workflow, which every
	// step it releases names first, once workflowRead is set. It is empty for a workflow nobody
	// decided on and on a dispatcher with no binding key.
	workflowRoots []binding.Root
	// workflowRead reports that workflowRoots was read.
	workflowRead bool
	// done receives each launched step's result.
	done chan stepResult
	// running counts launched steps that have not reported.
	running int
	// awaiting counts approval steps waiting for a decision.
	awaiting int
	// res is how the walk ended.
	res stepsResult
}

// newDAGWalk prepares a walk over steps for parent.
func (d *Dispatcher) newDAGWalk(parent *run.Run, steps []run.PipelineStep) *dagWalk {
	w := &dagWalk{
		d: d, parent: parent, steps: steps, byName: make(map[string]int, len(steps)),
		states: make([]stepState, len(steps)), results: make([]run.Status, len(steps)),
		outputs: make([]map[string]any, len(steps)), nodes: map[int]string{},
		handled: make([]bool, len(steps)), refused: make([]bool, len(steps)),
		roots: make([]binding.Root, len(steps)), done: make(chan stepResult, len(steps)),
	}
	for i, s := range steps {
		w.byName[s.Name] = i
	}
	for _, s := range steps {
		for _, dep := range s.IfDenied {
			w.handled[w.byName[dep]] = true
		}
	}
	w.closures = depClosures(steps, w.byName)
	return w
}

// restore loads the step records a paused workflow left behind, so the walk continues from where it
// stopped rather than from the top. A step's latest attempt is its result. A step still executing
// is waited on rather than started again, which only a crash between two writes can leave behind.
func (w *dagWalk) restore(ctx context.Context, children []*run.Run) {
	w.load(ctx, children, false)
}

// restoreDry loads the step records the way restore does without checking a binding or waiting on
// anything, for a sweep that only asks whether a parked workflow has work.
func (w *dagWalk) restoreDry(children []*run.Run) {
	w.load(context.Background(), children, true)
}

// load restores the walk from step records. A succeeded step is held to the result seal its terminal
// write carried before its status or its outputs are settled, and one that fails the check is
// refused, which fails the workflow. With dry set it verifies neither an approval's binding nor a
// step's result, and starts no wait on a step still executing.
func (w *dagWalk) load(ctx context.Context, children []*run.Run, dry bool) {
	latest := map[int]*run.Run{}
	for _, c := range children {
		if c.StepIndex == nil || *c.StepIndex < 0 || *c.StepIndex >= len(w.steps) {
			continue
		}
		prev, ok := latest[*c.StepIndex]
		if !ok || c.Attempt > prev.Attempt ||
			(c.Attempt == prev.Attempt && c.CreatedAt.After(prev.CreatedAt)) {
			latest[*c.StepIndex] = c
		}
	}
	for i := range w.steps {
		c, ok := latest[i]
		if !ok {
			continue
		}
		switch {
		case c.Kind == run.KindApproval && c.Status == run.StatusPendingApproval:
			w.states[i] = stepAwaiting
			w.nodes[i] = c.ID
			w.awaiting++
		case c.Kind == run.KindApproval && dry:
			w.nodes[i] = c.ID
			w.settle(i, c.Status, nil)
		case c.Kind == run.KindApproval:
			w.nodes[i] = c.ID
			w.settle(i, w.decided(ctx, i, c), nil)
		case c.Status.Terminal():
			if !dry {
				if reason := w.d.resultRefusal(ctx, c); reason != "" {
					w.refuse(i, reason)
					continue
				}
			}
			var out map[string]any
			if c.Status == run.StatusSucceeded {
				out = c.Outputs
			}
			w.settle(i, c.Status, out)
		case dry:
			w.states[i] = stepRunning
			w.running++
		default:
			w.states[i] = stepRunning
			w.running++
			idx, id := i, c.ID
			go func() {
				status := w.d.waitChildren(ctx, []string{id})[0]
				var out map[string]any
				var reason string
				if status == run.StatusSucceeded {
					out, reason = w.d.stepOutputs(c)
				}
				w.done <- stepResult{idx: idx, status: status, outputs: out, reason: reason}
			}()
		}
	}
}

// decided returns the status an approval step's record settles the walk with, holding the record to
// what decided it. An approval is held to the state it was given for: a workflow whose finished
// steps no longer reduce to it is not the workflow that was approved. A denial or a timeout is held
// to the verdict the chain records for the step, because a row can read as denied without anybody
// having denied it. A step that fails either check is refused, so neither of its paths runs and the
// workflow fails with the reason. A withdrawn step is the workflow stopping and needs no verdict.
func (w *dagWalk) decided(ctx context.Context, i int, node *run.Run) run.Status {
	var err error
	switch node.Status {
	case run.StatusSucceeded:
		err = w.approved(ctx, i, node)
	case run.StatusRejected, run.StatusFailed:
		err = w.denied(ctx, i, node)
	default:
		return node.Status
	}
	if err != nil {
		w.d.log.Error("dispatch: "+err.Error(), zap.String("run_id", w.parent.ID),
			zap.String("step", w.steps[i].Name))
		w.refused[i] = true
		w.res.reason = RefusalText(err)
		return run.StatusFailed
	}
	return node.Status
}

// approved holds an approved step to the state it was given for. On a dispatcher with a binding key
// the step's DECISION entry is read first and kept on the walk once the seal over it holds.
func (w *dagWalk) approved(ctx context.Context, i int, node *run.Run) error {
	if w.d.binder == nil {
		return w.d.checkStepBinding(ctx, w.parent, node)
	}
	root, err := w.d.verdictRoot(ctx, w.parent, node, outcome.StepApproved)
	if err != nil {
		return err
	}
	if err := w.d.checkStepBinding(ctx, w.parent, node); err != nil {
		return err
	}
	w.roots[i] = root
	return nil
}

// denied holds a denied or timed-out step to the verdict the chain records for it. A dispatcher
// that keeps no chain records no verdict for any step, so its deny path runs on the row's word, as
// it always did.
func (w *dagWalk) denied(ctx context.Context, i int, node *run.Run) error {
	if w.d.audits == nil {
		return nil
	}
	root, err := w.d.verdictRoot(ctx, w.parent, node, outcome.StepRejected, outcome.StepTimedOut)
	if err != nil {
		return err
	}
	if w.d.binder != nil {
		w.roots[i] = root
	}
	return nil
}

// verdictRoot reads the chain entry an approval step's decision names and returns the root naming
// it. The entry must be a DECISION on this step of this workflow carrying one of verdicts. A step
// that names no decision, or whose entry is gone or records something else, is refused with
// binding.ErrDrift. A read that keeps failing is binding.ErrUnverified, which says nothing about
// the step either way. The reads are not ended by the walk's cancellation, so a shutdown is never
// recorded as a verdict that could not be read.
func (d *Dispatcher) verdictRoot(ctx context.Context, parent, node *run.Run, verdicts ...string) (binding.Root, error) {
	subject := fmt.Sprintf("approval step %q (run %s)", node.StepName, node.ID)
	if node.DecisionID == "" {
		return binding.Root{}, fmt.Errorf("%w: %s: it reads as %s and names no decision on the chain",
			binding.ErrDrift, subject, node.Status)
	}
	read := context.WithoutCancel(ctx)
	var e *audit.Entry
	err := withRetries(func() error {
		var rerr error
		e, rerr = d.chainEntry(read, node.DecisionID)
		return rerr
	})
	switch {
	case err != nil:
		return binding.Root{}, fmt.Errorf("%w: %s: read chain entry %s: %w", binding.ErrUnverified,
			subject, node.DecisionID, err)
	case e == nil:
		return binding.Root{}, fmt.Errorf("%w: %s: chain entry %s it names is not on the chain",
			binding.ErrDrift, subject, node.DecisionID)
	}
	for _, verdict := range verdicts {
		if e.Method == audit.MethodDecision &&
			e.Path == outcome.StepDecisionPath(parent.ID, node.ID, verdict) {
			return binding.RootOf(e), nil
		}
	}
	return binding.Root{}, fmt.Errorf("%w: %s: chain entry %s does not record a verdict of %s on it",
		binding.ErrDrift, subject, node.DecisionID, strings.Join(verdicts, " or "))
}

// settle records a step's terminal status and folds it into how the walk ends.
func (w *dagWalk) settle(i int, status run.Status, out map[string]any) {
	w.states[i] = stepDone
	w.results[i] = status
	w.outputs[i] = out
	switch status {
	// A step the coordinator stopped is a cancel; a step whose executor died is an interrupt,
	// the state a rerun resumes from. Folding both into canceled recorded a crashed pipeline
	// as withdrawn and lost the recovery signal, so they are kept apart.
	case run.StatusInterrupted:
		w.res.interrupted = true
	case run.StatusCanceled:
		w.res.canceled = true
	case run.StatusSucceeded:
	default:
		// A denied approval step whose denial the graph handles is the workflow taking the deny path
		// its author wrote, which is not a failure of the workflow, the way AWX reads a node with a
		// failure path. A refused approval is a failure whatever the graph says.
		if w.steps[i].IsApproval() && w.handled[i] && !w.refused[i] {
			return
		}
		w.res.failed = true
	}
}

// inputsFor merges the outputs of the step's transitive dependencies in declaration order, so the
// result does not depend on which branch finished first.
//
// The parent's own vars are the base every step starts from, so a workflow's survey answers reach
// each step; a transitive dependency's outputs are layered on top in declaration order, so a
// published output overrides a parent var of the same name and the result does not depend on which
// branch finished first.
func (w *dagWalk) inputsFor(i int) map[string]any {
	vars := baseStepVars(w.parent)
	for j := range w.steps {
		if !w.closures[i][j] || len(w.outputs[j]) == 0 {
			continue
		}
		maps.Copy(vars, w.outputs[j])
	}
	return vars
}

// depSettled reports whether the step at j is finished for good.
func (w *dagWalk) depSettled(j int) bool {
	return w.states[j] == stepDone || w.states[j] == stepSkipped
}

// depAllows reports whether the step at j finished in a way that lets a step waiting for it run.
func (w *dagWalk) depAllows(j int) bool {
	if w.states[j] != stepDone || w.refused[j] {
		return false
	}
	if w.steps[j].IsApproval() {
		return w.results[j] == run.StatusSucceeded
	}
	return w.results[j] == run.StatusSucceeded || w.steps[j].ContinueOnFailure
}

// depDenied reports whether the approval step at j was denied or timed out, which is what lets a
// step on its deny path run.
func (w *dagWalk) depDenied(j int) bool {
	if w.states[j] != stepDone || w.refused[j] {
		return false
	}
	return w.results[j] == run.StatusRejected || w.results[j] == run.StatusFailed
}

// behindRefusal reports whether a step the step at i waits behind, directly or through other steps,
// was refused. A live walk never reaches a step behind a refusal, since nothing waiting on a refused
// step is let through and what waits on that is skipped in turn. A restored walk can hold a refused
// step whose dependent had already settled, such as an approval step decided before the refused
// step's record was rewritten, and nothing behind that dependent may run either.
func (w *dagWalk) behindRefusal(i int) bool {
	for j, in := range w.closures[i] {
		if in && w.refused[j] {
			return true
		}
	}
	return false
}

// readiness reports whether the waiting step at i can start, and whether it never can.
func (w *dagWalk) readiness(i int) (ready, blocked bool) {
	if w.behindRefusal(i) {
		return false, true
	}
	ready = true
	for _, dep := range w.steps[i].DependsOn {
		j := w.byName[dep]
		if !w.depSettled(j) {
			ready = false
			continue
		}
		if w.states[j] == stepSkipped || !w.depAllows(j) {
			blocked = true
		}
	}
	for _, dep := range w.steps[i].IfDenied {
		j := w.byName[dep]
		if !w.depSettled(j) {
			ready = false
			continue
		}
		if w.states[j] == stepSkipped || !w.depDenied(j) {
			blocked = true
		}
	}
	return ready, blocked
}

// advance settles everything that can start or can never start, repeating until quiescent since one
// skip can unblock the decision for its dependents. With dry set nothing is started: it only
// reports whether something would be, which is how a sweep asks whether a parked workflow has work
// waiting.
func (w *dagWalk) advance(ctx context.Context, dry bool) bool {
	for progress := true; progress; {
		progress = false
		for i := range w.steps {
			if w.states[i] != stepWaiting {
				continue
			}
			if !dry && ctx.Err() != nil {
				w.states[i] = stepSkipped
				w.res.canceled = true
				progress = true
				continue
			}
			ready, blocked := w.readiness(i)
			switch {
			case blocked:
				w.states[i] = stepSkipped
				progress = true
			case ready && dry:
				return true
			case ready && w.steps[i].IsApproval():
				w.open(ctx, i)
				progress = true
			case ready:
				w.launch(ctx, i)
				progress = true
			}
		}
	}
	return false
}

// launch starts a step that runs a tool. The entries that released it are gathered here, on the
// walk's goroutine, and handed to the launch by value. A step whose release cannot be named is
// refused without starting.
func (w *dagWalk) launch(ctx context.Context, i int) {
	roots, err := w.releaseRoots(ctx, i)
	if err != nil {
		w.d.log.Error("dispatch: "+err.Error(), zap.String("run_id", w.parent.ID),
			zap.String("step", w.steps[i].Name))
		w.refuse(i, RefusalText(err))
		return
	}
	w.states[i] = stepRunning
	w.running++
	idx := i
	step := w.steps[i]
	vars := w.inputsFor(i)
	go func() {
		status, published, reason := w.d.runStepAttempts(ctx, w.parent, step, idx, vars, roots)
		w.done <- stepResult{idx: idx, status: status, outputs: published, reason: reason}
	}()
}

// releaseRoots returns the chain entries the step at i derives from, in the order its seal and its
// release name them: the decision that won the workflow, when one did, then the decision on each
// approval step it waits behind, directly or through other steps, in declaration order. This is the
// order outcome.ReleaseRoots reads them back from the stored rows. A dispatcher with no binding key
// names none.
func (w *dagWalk) releaseRoots(ctx context.Context, i int) ([]binding.Root, error) {
	if w.d.binder == nil {
		return nil, nil
	}
	if !w.workflowRead {
		roots, err := w.d.workflowRoots(ctx, w.parent)
		if err != nil {
			return nil, err
		}
		w.workflowRoots, w.workflowRead = roots, true
	}
	roots := slices.Clone(w.workflowRoots)
	for j := range w.steps {
		if w.closures[i][j] && w.steps[j].IsApproval() && w.roots[j].EntryID != "" {
			roots = append(roots, w.roots[j])
		}
	}
	return roots, nil
}

// refuse settles the step at i failed for reason, so nothing waiting on it runs over either kind of
// edge whatever the step allows on failure, and fails the workflow with the reason.
func (w *dagWalk) refuse(i int, reason string) {
	w.refused[i] = true
	w.res.reason = reason
	w.settle(i, run.StatusFailed, nil)
}

// finish settles a launched step from its result. A step refused, before anything of it was stored
// or once it finished, is settled the way refuse settles one.
func (w *dagWalk) finish(r stepResult) {
	if r.reason != "" {
		w.refuse(r.idx, r.reason)
		return
	}
	w.settle(r.idx, r.status, r.outputs)
}

// open asks for an approval step's decision. A request that cannot be recorded is not a request
// anybody can answer, so the step fails rather than waiting forever. On a dispatcher that keeps a
// chain the step is also refused: nobody denied it, so its deny path does not run either, and the
// workflow fails with the reason. A dispatcher that keeps no chain records no verdict for any step,
// and its failed step takes the deny path as it always did.
func (w *dagWalk) open(ctx context.Context, i int) {
	node, err := w.d.openApprovalStep(ctx, w.parent, w.steps[i], i)
	if err != nil {
		w.d.log.Error("dispatch: open approval step: "+err.Error(), zap.String("run_id", w.parent.ID),
			zap.String("step", w.steps[i].Name))
		if node != nil {
			w.nodes[i] = node.ID
		}
		if w.d.audits != nil {
			w.refused[i] = true
			w.res.reason = fmt.Sprintf("approval step %q could not record its request, so nobody "+
				"could be asked and neither of its paths runs", w.steps[i].Name)
		}
		w.settle(i, run.StatusFailed, nil)
		return
	}
	w.states[i] = stepAwaiting
	w.nodes[i] = node.ID
	w.awaiting++
}

// poll reads every waiting approval step's record and settles the ones somebody decided, reporting
// whether any was.
func (w *dagWalk) poll(ctx context.Context) bool {
	changed := false
	for i, id := range w.nodes {
		if w.states[i] != stepAwaiting {
			continue
		}
		node, err := w.d.store.Get(ctx, id)
		if err != nil || !node.Status.Terminal() {
			continue
		}
		w.awaiting--
		w.settle(i, w.decided(ctx, i, node), nil)
		changed = true
	}
	return changed
}

// withdraw settles every waiting approval step because the workflow is stopping. A waiting step
// left behind would sit in the approval queue for a workflow that is gone, and approving it would
// decide nothing.
func (w *dagWalk) withdraw() {
	for i, id := range w.nodes {
		if w.states[i] != stepAwaiting {
			continue
		}
		if _, err := w.d.store.CancelPending(context.Background(), id); err != nil {
			w.d.log.Error("dispatch: withdraw approval step: "+err.Error(), zap.String("run_id", id))
		}
		w.awaiting--
		w.settle(i, w.d.stoppedStatus(), nil)
	}
	// A step a decision claimed refuses the withdrawal and is finished instead, so its decision is
	// recorded before the outcome of the workflow that is stopping.
	w.d.finishClaimedSteps(context.Background(), w.parent.ID)
}

// run walks the graph until every step is settled, or until nothing is left to do but wait for a
// person, in which case it reports the walk parked and its coordinator hands the workflow to the
// store rather than holding it in memory for as long as somebody takes to decide.
//
// Steps whose dependencies are settled run concurrently through the worker pool. A step runs when
// every dependency succeeded, or failed with continue on failure set, and every approval whose
// denial it handles was denied; otherwise the step is skipped and creates no run. Each step
// receives the merged outputs of its transitive dependencies as extra vars.
func (w *dagWalk) run(ctx context.Context) stepsResult {
	for {
		w.advance(ctx, false)
		if w.running == 0 && w.awaiting == 0 {
			return w.res
		}
		if w.running == 0 && ctx.Err() == nil {
			// Read once more before parking, so a decision that landed while the last step was
			// finishing continues here rather than costing a park and a resume.
			if w.poll(ctx) {
				continue
			}
			w.res.parked = true
			return w.res
		}
		if w.running == 0 && !w.res.interrupted && errors.Is(context.Cause(w.d.ctx), errShuttingDown) {
			// The process is stopping with nothing running and a step open for a person, which is the
			// wait a park hands to the store, and the park writes with its own context. Withdrawing
			// the step instead ended a workflow nobody decided after its step had already been
			// offered for a decision, so the replica that came back found nothing to approve.
			w.res.parked = true
			return w.res
		}
		if w.awaiting == 0 {
			r := <-w.done
			w.running--
			w.finish(r)
			continue
		}
		select {
		case r := <-w.done:
			w.running--
			w.finish(r)
		case <-time.After(approvalPollInterval):
			w.poll(ctx)
		case <-ctx.Done():
			w.withdraw()
		}
	}
}
