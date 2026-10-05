package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// agentActorType is how the audit chain names a caller that authenticated with an agent's token.
const agentActorType = "agent"

// timeoutDecider is who the chain names for an approval step that timed out. Nobody decided it, so
// the entry says the system ended the wait, on behalf of whoever launched the workflow.
const timeoutDecider = "system:approval-timeout"

// requestDecider is who the chain names for an approval step asking for its decision: the workflow
// reached the step, on behalf of whoever launched it.
const requestDecider = "system:workflow"

// StepDecision is one decision on a workflow approval step.
type StepDecision struct {
	// Approve is true to approve the step and false to deny it.
	Approve bool
	// Reason is the decider's stated reason as submitted, empty for none. It is masked before anything
	// records it and kept in the decision record with a hiding commitment in the decision entry.
	Reason string
	// ConfirmedMask is the masked reason the decider was shown and confirmed, for a reason the masker
	// changed. Until it matches, the decision is refused with a ReasonMaskedError.
	ConfirmedMask string
	// Shown is the state digest the approver was shown. When set, the decision is refused unless the
	// workflow still reduces to it, so an approval binds to exactly what was looked at. Empty binds
	// to the state as it is when the decision is made, as a run's approval binds to its spec.
	Shown string
	// By names the decider in the audit chain's vocabulary.
	By outcome.Decider
}

// sameRequester reports whether the decider is the requester of r for separation of duties.
// Accounts are compared when both sides name one: an agent's run records the account it is bound to
// as its requester's account, so that account cannot release the agent's work when a rule requires
// an independent approver, and a person's token and browser session count as one person. Names are
// compared only when an account is missing on either side.
func sameRequester(by outcome.Decider, r *run.Run) bool {
	if by.AccountID != "" && r.ActorUserID != "" {
		return by.AccountID == r.ActorUserID
	}
	return by.Name != "" && by.Name == r.Actor
}

// approvalStepRun builds the record an approval step asks through. It is built from the step the
// way an executable step is, so it carries the workflow's scope, its requester, and its
// organization, which is what lets the same authorization and separation-of-duties checks a held
// run gets apply to it. It executes nothing: its kind keeps every claim off it and it is born
// waiting.
func approvalStepRun(parent *run.Run, step run.PipelineStep, idx int, at time.Time) *run.Run {
	node := stepRun(parent, step, idx, 0, nil)
	node.Kind = run.KindApproval
	node.Status = run.StatusPendingApproval
	node.CreatedAt = at
	node.Timeout = step.ApprovalTimeout
	node.DryRun = false
	node.HeldByPolicy = fmt.Sprintf("approval step %q", step.Name)
	// The rule that applied to the workflow applies to each step that releases part of it. It was
	// computed when the workflow was submitted, from the policies then in force, so a rule edited
	// while the workflow runs cannot loosen a decision it is about to ask for.
	node.RequireDistinctApprover = parent.RequireDistinctApprover
	node.RequireReason = parent.RequireReason
	return node
}

// openApprovalStep records that the workflow reached an approval step and is waiting for a person:
// the step's record, then the request on the audit chain, then the notification. The request is
// appended fail-closed. A step whose request the chain does not hold is settled failed and returned
// with the error, so the walk takes its deny path rather than waiting on a request with no record.
func (d *Dispatcher) openApprovalStep(ctx context.Context, parent *run.Run, step run.PipelineStep,
	idx int) (*run.Run, error) {
	// The store's clock, because the timeout sweep ages the request with it.
	at, err := d.store.Now(ctx)
	if err != nil {
		at = time.Now()
	}
	node := approvalStepRun(parent, step, idx, at)
	save := func() error { return d.store.Save(context.Background(), node) }
	if err := withRetries(save); err != nil {
		return nil, fmt.Errorf("save approval step: %w", err)
	}
	if d.audits != nil {
		// The step is saved, and offered for a decision, from here on, so its request is recorded
		// whatever happens to the walk. Recorded on the walk's own context, a shutdown landing now
		// failed the step as a request nobody could be asked after it had already been offered.
		rec := context.WithoutCancel(ctx)
		state, serr := outcome.StepStateOf(rec, d.store, parent, node)
		if serr == nil {
			_, serr = outcome.CommitStepDecision(rec, d.audits, state, outcome.StepRequested,
				outcome.Decider{Name: requestDecider, Type: "system", OnBehalfOf: parent.Actor}, d.now)
		}
		if serr != nil {
			_, _ = d.store.SettleHeld(context.Background(), node.ID, run.Finalization{
				Status: run.StatusFailed, EndedAt: d.now(),
				Error: "the approval request could not be recorded, so nobody could be asked: " +
					serr.Error(),
			})
			return node, fmt.Errorf("record the approval request: %w", serr)
		}
	}
	d.notifyStepHeld(parent, node)
	return node, nil
}

// checkStepBinding reports whether an approved step still matches the state it was approved for.
// The binding was stamped when the approval was recorded; a workflow whose finished steps or
// published values changed since then is not the workflow the approver released.
func (d *Dispatcher) checkStepBinding(ctx context.Context, parent, node *run.Run) error {
	if node.ApprovedSpecBinding == "" {
		// Stamped before the record is settled, so an approved step without one was not approved
		// through DecideStep.
		return fmt.Errorf("refused: approval step %q carries no record of what was approved",
			node.StepName)
	}
	state, err := outcome.StepStateOf(ctx, d.store, parent, node)
	if err != nil {
		return fmt.Errorf("refused: could not rebuild what approval step %q released: %w",
			node.StepName, err)
	}
	binding, err := state.Binding()
	if err != nil {
		return fmt.Errorf("refused: could not bind approval step %q: %w", node.StepName, err)
	}
	if binding != node.ApprovedSpecBinding {
		return fmt.Errorf("refused: the workflow changed after approval step %q was approved, so "+
			"this is not what the approver released", node.StepName)
	}
	return nil
}

// DecideStep approves or denies a workflow approval step. It applies the rules a held run's
// decision does: an agent never approves, the person who launched the workflow cannot approve it
// when the rule in force requires a different approver, and the decision is committed to the chain
// before it takes effect, binding the decider to the digest of the state they decided on. The
// decision first claims the step in one compare-and-set, so of two deciders racing, or a decider
// racing the timeout or a cancel, exactly one wins and only the winner is recorded. A workflow
// parked at the step is then resumed by this process.
func (d *Dispatcher) DecideStep(ctx context.Context, id string, dec StepDecision) (*run.Run, error) {
	node, err := d.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if node.Kind != run.KindApproval || node.ParentID == nil {
		return nil, ErrNotApprovalStep
	}
	if node.Status != run.StatusPendingApproval {
		return nil, fmt.Errorf("%w: this approval step is %s", ErrNotPendingApproval, node.Status)
	}
	parent, err := d.store.Get(ctx, *node.ParentID)
	if err != nil {
		return nil, err
	}
	if parent.CancelRequested || parent.Status.Terminal() {
		return nil, fmt.Errorf("%w: the workflow is %s", ErrNotPendingApproval, parent.Status)
	}
	if dec.Approve {
		// An agent's token cannot reach an approval at the door, and this holds the line again where
		// the decision is made, so no other path to the dispatcher can release a step an agent
		// approves.
		if dec.By.Type == agentActorType {
			return nil, fmt.Errorf("%w a workflow approval step", ErrAgentApproval)
		}
		if node.RequireDistinctApprover && sameRequester(dec.By, node) {
			return nil, fmt.Errorf("%w: %q launched this workflow, and the rule in force requires a "+
				"different person to approve its steps", ErrSelfApproval, dec.By.Name)
		}
	}
	state, err := outcome.StepStateOf(ctx, d.store, parent, node)
	if err != nil {
		return nil, fmt.Errorf("rebuild the approval step state: %w", err)
	}
	digest, err := state.Digest()
	if err != nil {
		return nil, fmt.Errorf("digest the approval step state: %w", err)
	}
	if dec.Shown != "" && dec.Shown != digest {
		return nil, fmt.Errorf("%w: you were shown %s and it is now %s", ErrStateMoved, dec.Shown, digest)
	}
	binding, err := state.Binding()
	if err != nil {
		return nil, fmt.Errorf("bind the approval step state: %w", err)
	}
	verdict, status := outcome.StepApproved, run.StatusSucceeded
	reason := ""
	if !dec.Approve {
		verdict, status = outcome.StepRejected, run.StatusRejected
		// The step's error says it was denied and nothing more, for the reason a rejected run's
		// does: the reason itself is kept where a redaction can remove it.
		reason = "denied by an approver"
	}
	text, masked, err := d.prepareReason(ctx, parent, dec.Reason, dec.ConfirmedMask)
	if err != nil {
		return nil, err
	}
	if err := requireReason(node, text, dec.Approve); err != nil {
		return nil, err
	}
	rec, err := d.newDecisionRecord(parent, node, verdict, text, masked, dec.By)
	if err != nil {
		return nil, fmt.Errorf("record the approval step decision: %w", err)
	}
	// The step is stamped with what it was decided on whichever way it went, so a resumed walk can
	// hold an approval to the state it was given for.
	c := &decisionClaim{ID: rec.ID, Verdict: verdict, Step: true, Status: status, Error: reason,
		Digest: digest, Binding: binding}
	if d.audits != nil {
		entry, _, err := outcome.StepDecisionEntry(state, verdict, dec.By, d.now,
			outcome.ExtrasOf(rec))
		if err != nil {
			return nil, fmt.Errorf("record the approval step decision: %w", err)
		}
		c.Entry = claimEntryOf(entry)
	}
	if _, err := d.decide(ctx, node.ID, rec, c); err != nil {
		return nil, decisionError("approval step decision", err)
	}
	d.resumeParked(d.ctx, parent.ID)
	return d.storeGetWithRetries(ctx, node.ID)
}

// timeOutStep ends an approval step nobody decided within its timeout. It takes the deny path, as
// AWX takes an approval node's failure path when its timeout passes, and the chain records that the
// system ended the wait rather than naming an approver who never acted.
//
// The timeout is a decision like any other: it claims the step before anything is recorded, so a
// sweep racing a person, or the sweep on another replica, writes nothing when it loses. A step a
// decision already claimed is left to that decision, which the sweep finishes instead.
func (d *Dispatcher) timeOutStep(ctx context.Context, node *run.Run) bool {
	node, err := d.store.Get(ctx, node.ID)
	if err != nil || node.Status != run.StatusPendingApproval || node.ParentID == nil ||
		node.InFlight() {
		return false
	}
	parent, err := d.store.Get(ctx, *node.ParentID)
	if err != nil {
		return false
	}
	c := &decisionClaim{ID: audit.NewID(), Verdict: outcome.StepTimedOut, Step: true,
		Status: run.StatusFailed, Error: fmt.Sprintf("timed out: nobody decided within %ds, so "+
			"the deny path was taken", node.Timeout)}
	if d.audits != nil {
		state, serr := outcome.StepStateOf(ctx, d.store, parent, node)
		var entry *audit.Entry
		if serr == nil {
			entry, _, serr = outcome.StepDecisionEntry(state, outcome.StepTimedOut,
				outcome.Decider{Name: timeoutDecider, Type: "system", OnBehalfOf: parent.Actor},
				d.now, outcome.DecisionExtras{})
		}
		if serr != nil {
			d.log.Error("dispatch: record approval step timeout: "+serr.Error(),
				zap.String("run_id", node.ID))
			return false
		}
		c.ID, c.Entry = entry.ID, claimEntryOf(entry)
	}
	if _, err := d.decide(ctx, node.ID, nil, c); err != nil {
		if !errors.Is(err, ErrNotPendingApproval) {
			d.log.Error("dispatch: time out an approval step: "+err.Error(),
				zap.String("run_id", node.ID))
		}
		return false
	}
	d.resumeParked(d.ctx, parent.ID)
	return true
}

// resumeParked continues a workflow parked at an approval step, on this process. The parent moves
// from held to running with this process's lease in one compare-and-swap, so of every replica that
// sees the decision exactly one resumes it. A parent held before it ever started is never resumed
// here: that is a whole run awaiting its own approval, which only Approve releases.
func (d *Dispatcher) resumeParked(ctx context.Context, parentID string) bool {
	started, err := run.Started(ctx, d.store, parentID)
	if err != nil || !started {
		return false
	}
	ok, err := d.store.TransitionStatusAndClaim(ctx, parentID, run.StatusPendingApproval,
		run.StatusRunning, d.owner, time.Time{})
	if err != nil {
		// The resume may have committed with its answer lost. Read as a resume that did not
		// happen, it left the workflow running under this process's lease with nothing walking
		// it, until the lease sweep interrupted a workflow whose approval had been accepted. The
		// store says what happened: a workflow this process now holds, which no coordinator here
		// is walking, is walked.
		cur, gerr := d.storeGetWithRetries(context.WithoutCancel(ctx), parentID)
		ok = gerr == nil && cur.Status == run.StatusRunning && cur.ClaimedBy == d.owner &&
			!d.coordinating(parentID)
	}
	if !ok {
		return false
	}
	parent, err := d.storeGetWithRetries(ctx, parentID)
	if err != nil {
		// The lease is ours and nothing will walk it, so the lease sweep settles it once the lease
		// ages out, which is the recovery path for a coordinator that died.
		d.log.Error("dispatch: read a resumed workflow: "+err.Error(), zap.String("run_id", parentID))
		return false
	}
	d.log.Info("dispatch: resuming a workflow after an approval step was decided",
		zap.String("run_id", parentID))
	d.wg.Add(1)
	go d.continuePipeline(parent)
	return true
}

// continuePipeline walks a resumed workflow from the step records it left behind.
func (d *Dispatcher) continuePipeline(parent *run.Run) {
	defer d.wg.Done()
	if d.coordinatePipeline(parent, true) {
		d.afterPark(parent.ID)
	}
}

// afterPark checks a workflow that just parked for decisions that landed while it was parking. A
// decider that settled a step before the park finished found the parent still running and left the
// resume to its coordinator, so the coordinator looks once more after handing the parent to the
// store.
func (d *Dispatcher) afterPark(parentID string) {
	if d.ctx.Err() != nil {
		return
	}
	parent, err := d.store.Get(d.ctx, parentID)
	if err != nil || parent.Status != run.StatusPendingApproval {
		return
	}
	if d.parkedHasWork(d.ctx, parent) {
		d.resumeParked(d.ctx, parentID)
	}
}

// parkedHasWork reports whether a parked workflow has anything to do: a decided approval step whose
// path can now start, or no waiting step at all, which means it is ready to finish. A parked
// workflow with neither is resting correctly and is left alone, so the sweep does not wake every
// paused workflow on every tick.
func (d *Dispatcher) parkedHasWork(ctx context.Context, parent *run.Run) bool {
	children, err := d.store.Steps(ctx, parent.ID)
	if err != nil || len(children) == 0 {
		return false
	}
	w := d.newDAGWalk(parent, run.GraphSteps(parent.Steps))
	w.restoreDry(children)
	if w.awaiting == 0 || w.running > 0 {
		return true
	}
	return w.advance(ctx, true)
}

// sweepApprovalSteps finishes decisions a process claimed and did not settle, times out approval
// steps whose wait has passed, and resumes any parked workflow with work waiting. It runs on every
// replica's janitor: each decision is claimed in a compare-and-set before it is recorded, and each
// resume is one, so replicas sweeping together cannot decide a step twice or resume a workflow
// twice, and a decision made by a process that died before settling or resuming is finished here.
func (d *Dispatcher) sweepApprovalSteps() {
	rows, err := d.store.NonTerminal(d.ctx)
	if err != nil {
		if d.ctx.Err() == nil {
			d.log.Error("dispatch: list waiting approval steps: " + err.Error())
		}
		return
	}
	// A decision claimed and never settled, by a process that died or lost its connection after
	// the claim, is finished first, by every replica's janitor alike: it is the decision that won,
	// and nothing else may decide the run while it stands.
	for _, r := range rows {
		if r.InFlight() {
			d.finishClaimed(d.ctx, r, true)
		}
	}
	now, err := d.store.Now(d.ctx)
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.Kind != run.KindApproval || r.Status != run.StatusPendingApproval || r.ParentID == nil ||
			r.Timeout <= 0 || r.InFlight() {
			continue
		}
		if now.Before(r.CreatedAt.Add(time.Duration(r.Timeout) * time.Second)) {
			continue
		}
		if d.timeOutStep(d.ctx, r) {
			d.log.Info("dispatch: an approval step timed out", zap.String("run_id", r.ID))
		}
	}
	for _, r := range rows {
		if r.Kind != run.KindPipeline || r.Status != run.StatusPendingApproval {
			continue
		}
		if d.parkedHasWork(d.ctx, r) {
			d.resumeParked(d.ctx, r.ID)
		}
	}
}

// notifyStepHeld tells the channels a workflow is waiting for a person at an approval step, through
// the path a held run takes but as its own event. The notification is about the workflow, which is
// what a person recognizes and can open, and it names the step holding it, what the step asks, and
// what each answer runs next.
func (d *Dispatcher) notifyStepHeld(parent, node *run.Run) {
	held := parent.Clone()
	held.Status = run.StatusPendingApproval
	held.HeldByPolicy = node.HeldByPolicy
	held.AwaitingStep = d.awaitingStep(parent, node)
	// The step is a branch of the workflow: a named target hears it after the workflow's start,
	// after the steps it comes after, and before the workflow's end, and in no invented order
	// against a step that runs beside it.
	d.notifyHeldOn(held, stepBranch(parent.Steps, node.StepName))
}

// stepBranch places a workflow step within its workflow: the step, and every step it transitively
// comes after in the graph the workflow is walked as. A sequence is walked as a chain, so each of
// its steps comes after all the ones before it.
func stepBranch(steps []run.PipelineStep, name string) named.Branch {
	graph := run.GraphSteps(steps)
	byName := make(map[string]int, len(graph))
	for i, s := range graph {
		byName[s.Name] = i
	}
	i, ok := byName[name]
	if !ok {
		return named.Branch{Step: name}
	}
	var follows []string
	for j, in := range depClosures(graph, byName)[i] {
		if in {
			follows = append(follows, graph[j].Name)
		}
	}
	return named.Branch{Step: name, Follows: follows}
}

// enterCoordinator waits until no coordinator for the workflow runs in this process, then records
// this one, and returns the call that releases it. A coordinator that parks and a decision that
// resumes the workflow on the same process can overlap by the few statements the first takes to
// return, and the second must not register its cancel only to have the first one's cleanup remove
// it: a cancel requested afterward would then find nothing to stop.
func (d *Dispatcher) enterCoordinator(id string) func() {
	for {
		d.cmu.Lock()
		prev, busy := d.coordinators[id]
		if !busy {
			mine := make(chan struct{})
			d.coordinators[id] = mine
			d.cmu.Unlock()
			return func() {
				d.cmu.Lock()
				delete(d.coordinators, id)
				d.cmu.Unlock()
				close(mine)
			}
		}
		d.cmu.Unlock()
		<-prev
	}
}

// park hands a workflow with nothing to do but wait for a person to the store, releasing this
// process's lease so that no process holds it while it waits. It reports whether the workflow
// parked. When it could not, because somebody canceled it or the store refused, the waiting
// approval steps are withdrawn and the caller settles the workflow instead.
func (d *Dispatcher) park(parent *run.Run) bool {
	var ok bool
	err := withRetries(func() error {
		var perr error
		ok, perr = d.store.ParkForApproval(context.Background(), parent.ID, d.owner)
		return perr
	})
	if err == nil && !ok {
		// A park whose answer was lost after it committed is retried, and the retry matches
		// nothing, which reads exactly like a cancel. Withdrawing the steps then ended a workflow
		// nobody canceled. The store says which it was: a workflow held with no lease and no
		// cancel requested is parked.
		ok = d.parkedNow(parent.ID)
	}
	if err == nil && ok {
		d.log.Info("dispatch: workflow paused at an approval step", zap.String("run_id", parent.ID))
		return true
	}
	if err != nil {
		if d.parkedNow(parent.ID) {
			d.log.Info("dispatch: workflow paused at an approval step",
				zap.String("run_id", parent.ID))
			return true
		}
		d.log.Error("dispatch: park a workflow: "+err.Error(), zap.String("run_id", parent.ID))
	}
	pending, perr := run.PendingApprovalSteps(context.Background(), d.store, parent.ID)
	if perr != nil {
		d.log.Error("dispatch: list waiting approval steps: "+perr.Error(),
			zap.String("run_id", parent.ID))
	}
	for _, node := range pending {
		if _, cerr := d.store.CancelPending(context.Background(), node.ID); cerr != nil {
			d.log.Error("dispatch: withdraw approval step: "+cerr.Error(), zap.String("run_id", node.ID))
		}
	}
	// A step a decision claimed is not withdrawn: it is finished, so its decision is on the chain
	// before the caller records how the workflow ended.
	d.finishClaimedSteps(context.Background(), parent.ID)
	return false
}

// parkedNow reports whether the store holds the workflow parked: held, with no lease and no cancel
// requested.
func (d *Dispatcher) parkedNow(id string) bool {
	cur, err := d.storeGetWithRetries(context.Background(), id)
	return err == nil && cur.Status == run.StatusPendingApproval && cur.ClaimedBy == "" &&
		!cur.CancelRequested
}

// coordinating reports whether a coordinator for the workflow is registered on this process.
func (d *Dispatcher) coordinating(id string) bool {
	d.cmu.Lock()
	defer d.cmu.Unlock()
	_, busy := d.coordinators[id]
	return busy
}
