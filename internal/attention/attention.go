// Package attention answers the question an operator asks of work that is not moving: what is
// stopping it. It reads the unfinished runs, the workers polling for work, and the schedules, and
// sorts everything that waits into four answers: a person has to approve it, no connected worker
// serves its queue, the worker running it stopped reporting, or it is blocked behind another run
// or a full worker. Each item names its main blocker, its other conditions, how long it has been
// in its current blocker, who can act, and what happens next.
//
// The evaluation is a pure function of what it is handed, so the dashboard, the doctor, and the
// alert monitor give the same answer for the same state.
package attention

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// Blocker names what is stopping work.
type Blocker string

const (
	// WorkerLost is work whose worker stopped renewing its lease.
	WorkerLost Blocker = "worker_lost"
	// NoWorker is work queued where no connected worker serves its queue.
	NoWorker Blocker = "no_worker"
	// ApprovalNeeded is a held run, or a workflow waiting at an approval step.
	ApprovalNeeded Blocker = "approval_needed"
	// Blocked is work waiting behind another run or a full worker for longer than its threshold.
	Blocked Blocker = "blocked"
)

// Order lists the blockers from the one that wins as an item's main blocker to the one that yields.
// Work in flight on a worker that vanished is the most urgent thing on the page, a queue nothing
// serves comes next because nothing on it can ever start, a decision a person owes comes after
// that, and waiting behind other work is the condition most likely to clear by itself.
var Order = []Blocker{WorkerLost, NoWorker, ApprovalNeeded, Blocked}

// rank returns b's position in Order, past the end for a blocker it does not list.
func rank(b Blocker) int {
	for i, o := range Order {
		if o == b {
			return i
		}
	}
	return len(Order)
}

// Label returns b as the dashboard names it.
func (b Blocker) Label() string {
	switch b {
	case WorkerLost:
		return "Worker lost"
	case NoWorker:
		return "No worker available"
	case ApprovalNeeded:
		return "Approval needed"
	case Blocked:
		return "Blocked"
	}
	return string(b)
}

// The scopes an approval condition carries, the badge that tells a held run from a workflow step.
const (
	// ScopeRun is a whole run held for approval.
	ScopeRun = "run"
	// ScopeWorkflowStep is a workflow waiting at one of its approval steps.
	ScopeWorkflowStep = "workflow_step"
)

// The states a workflow waiting at an approval step is in.
const (
	// OtherBranchesRunning is a workflow with other branches still executing beside the step.
	OtherBranchesRunning = "other_branches_running"
	// WorkflowPaused is a workflow with nothing left to do but wait for the step.
	WorkflowPaused = "paused"
)

// agentActorType is how a run records that an agent's token asked for it.
const agentActorType = "agent"

// Approvers says who can decide an approval.
type Approvers struct {
	// Role is the role a decider needs.
	Role string `json:"role"`
	// Excluded names the account that may not approve because it asked for the change, when the
	// rule that held it requires a different approver. That account may still deny it.
	Excluded string `json:"excluded,omitempty"`
	// Agent names the agent that asked on the excluded account's behalf, when an agent did. The
	// account an agent is bound to counts as the requester.
	Agent string `json:"agent,omitempty"`
	// Agents reports whether an agent identity can approve. It is always false.
	Agents bool `json:"agents"`
}

// Approval details an approval condition.
type Approval struct {
	// Scope is run for a held run and workflow_step for an approval step.
	Scope string `json:"scope"`
	// WorkflowState says, for a step, whether other branches are running or the workflow is paused.
	WorkflowState string `json:"workflow_state,omitempty"`
	// DecisionID is the id an approve or reject call takes: the held run, or the step's record.
	DecisionID string `json:"decision_id"`
	// Step is the approval step's name.
	Step string `json:"step,omitempty"`
	// Description is what the step's author asked the approver to decide.
	Description string `json:"description,omitempty"`
	// HeldBy names the rule that held a run.
	HeldBy string `json:"held_by,omitempty"`
	// ProposedFrom names the plan a held apply was proposed from, empty for any other run.
	ProposedFrom string `json:"proposed_from,omitempty"`
	// Approvers says who can decide it.
	Approvers Approvers `json:"approvers"`
	// OnApprove says what an approval runs next.
	OnApprove string `json:"on_approve"`
	// OnDeny says what a denial runs next.
	OnDeny string `json:"on_deny"`
	// ExpiresAt is when a step times out and takes its deny path, absent when it waits forever.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Condition is one thing stopping an item, on the item's own run or on one of its steps or shards.
type Condition struct {
	// Blocker is what kind of condition it is.
	Blocker Blocker `json:"blocker"`
	// RunID is the run the condition is on.
	RunID string `json:"run_id,omitempty"`
	// Step names the step or shard the condition is on, empty for the item's own run.
	Step string `json:"step,omitempty"`
	// Since is when the condition began, the start of the time it has been in it.
	Since time.Time `json:"since"`
	// Reason says what is stopping the work, in a sentence.
	Reason string `json:"reason"`
	// WhoCanAct says who can do something about it.
	WhoCanAct string `json:"who_can_act"`
	// Next says what happens next.
	Next string `json:"next"`
	// Queue is the queue a waiting run is on, for a missing worker or a full one.
	Queue *string `json:"queue,omitempty"`
	// EligibleWorkers is how many connected workers serve that queue.
	EligibleWorkers *int `json:"eligible_workers,omitempty"`
	// Worker is the lease holder that stopped reporting.
	Worker string `json:"worker,omitempty"`
	// LastSeen is when that worker last renewed its lease.
	LastSeen *time.Time `json:"last_seen,omitempty"`
	// ReclaimAt is when the lease runs out and the sweep reclaims the run.
	ReclaimAt *time.Time `json:"reclaim_at,omitempty"`
	// HolderRunID names the run a blocked item waits behind.
	HolderRunID string `json:"holder_run_id,omitempty"`
	// Approval details an approval condition.
	Approval *Approval `json:"approval,omitempty"`
	// queue is the queue the condition's run is on, kept for the thresholds lookup.
	queue string
}

// Item is one thing that needs attention: a run, a workflow, a split, or a schedule, with
// everything stopping it.
type Item struct {
	// Key identifies the item: its run id, or schedule: and the schedule's id.
	Key string `json:"key"`
	// Kind is run, workflow, split, or schedule.
	Kind string `json:"kind"`
	// RunID is the item's run, empty for a schedule.
	RunID string `json:"run_id,omitempty"`
	// ScheduleID is the schedule a schedule item is about.
	ScheduleID string `json:"schedule_id,omitempty"`
	// Name labels the item: a playbook, a workflow's name, or a schedule's name.
	Name string `json:"name"`
	// Blocker is the main blocker, the answer to what is stopping this.
	Blocker Blocker `json:"blocker"`
	// Main is the condition behind the main blocker.
	Main Condition `json:"main"`
	// Badges are the item's other conditions.
	Badges []Condition `json:"badges,omitempty"`
	// Interaction is one line on how the conditions interact, empty with one condition.
	Interaction string `json:"interaction,omitempty"`
	// Since is when the item entered its main blocker.
	Since time.Time `json:"since"`
	// WaitingSeconds is how long it has been in its main blocker.
	WaitingSeconds int64 `json:"waiting_seconds"`
	// AlertAfterSeconds is how long in its main blocker before it alerts, zero when it never does.
	AlertAfterSeconds int64 `json:"alert_after_seconds"`
	// Alerting reports that it has been in its main blocker past its alert threshold.
	Alerting bool `json:"alerting"`
	// AlertKey identifies the alert, the same for as long as the main condition lasts.
	AlertKey string `json:"-"`
	// Run is the item's run, or the run a schedule waits behind, for authorization and routing.
	Run *run.Run `json:"-"`
	// Schedule is the schedule a schedule item is about.
	Schedule *schedule.Schedule `json:"-"`
}

// Counts are how many items each main blocker stops.
type Counts struct {
	// ApprovalNeeded counts held runs and workflows waiting at approval steps.
	ApprovalNeeded int `json:"approval_needed"`
	// NoWorker counts work no connected worker serves.
	NoWorker int `json:"no_worker"`
	// WorkerLost counts work whose worker stopped reporting.
	WorkerLost int `json:"worker_lost"`
	// Blocked counts work blocked past its threshold.
	Blocked int `json:"blocked"`
}

// CountItems counts items by their main blocker, so the four counts add up to the items listed.
func CountItems(items []Item) Counts {
	var c Counts
	for _, it := range items {
		switch it.Blocker {
		case ApprovalNeeded:
			c.ApprovalNeeded++
		case NoWorker:
			c.NoWorker++
		case WorkerLost:
			c.WorkerLost++
		case Blocked:
			c.Blocked++
		}
	}
	return c
}

// Snapshot is everything that needs attention at one moment.
type Snapshot struct {
	// Counts are how many items each main blocker stops.
	Counts Counts `json:"counts"`
	// Items are the items, most urgent blocker first and oldest first within one.
	Items []Item `json:"items"`
	// GeneratedAt is the store time the snapshot was taken at.
	GeneratedAt time.Time `json:"generated_at"`
}

// Timing is the lease timing the dispatcher runs with, which decides when a worker counts as lost
// and when its run is reclaimed.
type Timing struct {
	// LeaseTTL is how stale a lease may grow before the sweep reclaims the run.
	LeaseTTL time.Duration
	// SweepInterval is how often the sweep runs.
	SweepInterval time.Duration
	// LostAfter is how stale a lease grows before its worker counts as lost.
	LostAfter time.Duration
	// Fresh is how recently a worker must have reported to count as connected.
	Fresh time.Duration
}

// Input is everything an evaluation reads.
type Input struct {
	// Now is the store's clock.
	Now time.Time
	// Runs are every unfinished run, top level and children alike.
	Runs []*run.Run
	// Queued maps a pending run to when it last re-entered the queue.
	Queued map[string]time.Time
	// Workers are the executors that reported within WorkerRetention.
	Workers []Worker
	// Schedules are the install's schedules.
	Schedules []*schedule.Schedule
	// Accounts resolves an account id to its username, nil when there are no accounts.
	Accounts func(id string) string
	// NextSteps returns the steps an approval step's approval and denial run next, and false when
	// they cannot be worked out.
	NextSteps func(parent, step *run.Run) (onApprove, onDeny []string, ok bool)
	// Config holds the thresholds, nil for the built-in ones.
	Config *Config
	// Timing is the dispatcher's lease timing.
	Timing Timing
}

// eval is one evaluation's working state.
type eval struct {
	// in is what the evaluation reads.
	in Input
	// byID maps each run to itself by id.
	byID map[string]*run.Run
	// children maps a parent run to its unfinished children, in creation order.
	children map[string][]*run.Run
	// fresh are the workers that reported within the freshness window.
	fresh []Worker
	// active counts the runs each owner holds a lease on that use a slot.
	active map[string]int
}

// Evaluate sorts every unfinished run and every schedule into the items that need attention.
func Evaluate(in Input) Snapshot {
	e := &eval{in: in, byID: map[string]*run.Run{}, children: map[string][]*run.Run{},
		active: map[string]int{}}
	for _, r := range in.Runs {
		e.byID[r.ID] = r
		if r.ParentID != nil {
			e.children[*r.ParentID] = append(e.children[*r.ParentID], r)
		}
		if r.ClaimedBy != "" && r.Kind == "" &&
			(r.Status == run.StatusPending || r.Status == run.StatusRunning) {
			e.active[r.ClaimedBy]++
		}
	}
	for _, kids := range e.children {
		sort.Slice(kids, func(i, j int) bool {
			if !kids[i].CreatedAt.Equal(kids[j].CreatedAt) {
				return kids[i].CreatedAt.Before(kids[j].CreatedAt)
			}
			return kids[i].ID < kids[j].ID
		})
	}
	for _, w := range in.Workers {
		if in.Now.Sub(w.LastSeen) <= in.Timing.Fresh {
			e.fresh = append(e.fresh, w)
		}
	}
	items := []Item{}
	for _, r := range in.Runs {
		if r.ParentID != nil {
			continue
		}
		if it, ok := e.runItem(r); ok {
			items = append(items, it)
		}
	}
	items = append(items, e.scheduleItems()...)
	sort.SliceStable(items, func(i, j int) bool {
		if ri, rj := rank(items[i].Blocker), rank(items[j].Blocker); ri != rj {
			return ri < rj
		}
		if !items[i].Since.Equal(items[j].Since) {
			return items[i].Since.Before(items[j].Since)
		}
		return items[i].Key < items[j].Key
	})
	return Snapshot{Counts: CountItems(items), Items: items, GeneratedAt: in.Now}
}

// runItem builds the item for a top-level run from everything stopping it and its children, and
// reports false when nothing is.
func (e *eval) runItem(top *run.Run) (Item, bool) {
	var conds []Condition
	if top.Status == run.StatusPendingApproval && !e.parkedAtStep(top) {
		// A held run executes nothing until somebody decides it, so the decision is the only thing
		// stopping it. Its shards wait held beside it and are not conditions of their own.
		conds = append(conds, e.heldRun(top))
	} else {
		conds = append(conds, e.runConditions(top, top)...)
		for _, c := range e.children[top.ID] {
			conds = append(conds, e.runConditions(top, c)...)
		}
	}
	if len(conds) == 0 {
		return Item{}, false
	}
	kind := "run"
	switch top.Kind {
	case run.KindPipeline:
		kind = "workflow"
	case run.KindSplit:
		kind = "split"
	}
	it := Item{Key: top.ID, Kind: kind, RunID: top.ID, Name: runName(top), Run: top}
	e.finish(&it, conds, top.OrgID, top.TemplateID)
	return it, true
}

// finish picks an item's main blocker from its conditions, makes the rest badges, and measures it
// against its thresholds.
func (e *eval) finish(it *Item, conds []Condition, orgID, templateID string) {
	sort.SliceStable(conds, func(i, j int) bool {
		if ri, rj := rank(conds[i].Blocker), rank(conds[j].Blocker); ri != rj {
			return ri < rj
		}
		return conds[i].Since.Before(conds[j].Since)
	})
	it.Main = conds[0]
	it.Blocker = it.Main.Blocker
	if len(conds) > 1 {
		it.Badges = conds[1:]
	}
	it.Interaction = interaction(it.Main, it.Badges)
	it.Since = it.Main.Since
	it.WaitingSeconds = int64(max(e.in.Now.Sub(it.Since), 0) / time.Second)
	limits := e.in.Config.Limits(orgID, it.Main.queue, templateID, e.in.Timing.LeaseTTL)
	var after time.Duration
	switch it.Blocker {
	case WorkerLost:
		after = limits.AlertWorkerLost
	case NoWorker:
		after = limits.AlertNoWorker
	case ApprovalNeeded:
		after = limits.AlertApproval
	case Blocked:
		after = limits.AlertBlocked
	}
	it.AlertAfterSeconds = int64(after / time.Second)
	it.Alerting = after > 0 && e.in.Now.Sub(it.Since) >= after
	subject := it.Main.RunID
	if subject == "" {
		subject = it.Key
	}
	it.AlertKey = fmt.Sprintf("%s|%s|%s|%d", it.Blocker, it.Key, subject, it.Since.UnixMilli())
}

// parkedAtStep reports whether a held workflow is parked at an approval step rather than held as a
// whole run: it has started, and a step of it is waiting for a decision.
func (e *eval) parkedAtStep(r *run.Run) bool {
	if r.Kind != run.KindPipeline {
		return false
	}
	for _, c := range e.children[r.ID] {
		if c.Kind == run.KindApproval && c.Status == run.StatusPendingApproval {
			return true
		}
	}
	return false
}

// runConditions returns what is stopping one run of an item: an approval step waiting, a lease its
// worker stopped renewing, or a queued run nothing can take.
func (e *eval) runConditions(top, r *run.Run) []Condition {
	switch {
	case r.Kind == run.KindApproval:
		if r.Status == run.StatusPendingApproval && r.ParentID != nil {
			return []Condition{e.stepApproval(top, r)}
		}
	case r.ClaimedBy != "" && r.ClaimedAt != nil &&
		(r.Status == run.StatusPending || r.Status == run.StatusRunning):
		if e.in.Now.Sub(*r.ClaimedAt) >= e.in.Timing.LostAfter {
			return []Condition{e.workerLost(top, r)}
		}
	case r.Status == run.StatusPending && r.ClaimedBy == "" && r.Kind == "" && !r.CancelRequested:
		if r.ParentID != nil {
			// A child is claimable only while its parent runs, so one under a parent that does not
			// is waiting on the parent rather than on a worker.
			if p, ok := e.byID[*r.ParentID]; !ok || p.Status != run.StatusRunning {
				return nil
			}
		}
		if c, ok := e.queued(top, r); ok {
			return []Condition{c}
		}
	}
	return nil
}

// queuedSince returns when a pending run joined the queue it waits in.
func (e *eval) queuedSince(r *run.Run) time.Time {
	if at, ok := e.in.Queued[r.ID]; ok && at.After(r.CreatedAt) {
		return at
	}
	return r.CreatedAt
}

// queued returns the condition of a run waiting in its queue: no worker serves the queue, or every
// worker that does is full and has been for longer than the blocked threshold. A run a free worker
// will take on its next poll is not stopped by anything, and reports false.
func (e *eval) queued(top, r *run.Run) (Condition, bool) {
	since := e.queuedSince(r)
	var eligible []Worker
	for _, w := range e.fresh {
		if w.Serves(r.Queue) {
			eligible = append(eligible, w)
		}
	}
	queue := r.Queue
	if len(eligible) == 0 {
		// The wait for a worker began when the run joined the queue or when the last worker that
		// served the queue stopped reporting, whichever is later.
		for _, w := range e.in.Workers {
			if w.Serves(r.Queue) && w.LastSeen.After(since) {
				since = w.LastSeen
			}
		}
		zero := 0
		return Condition{
			Blocker: NoWorker, RunID: r.ID, Step: childLabel(r), Since: since, Queue: &queue,
			EligibleWorkers: &zero, queue: r.Queue,
			Reason: e.withRetry(r, "No connected worker serves "+queueName(r.Queue)+
				", so nothing can take "+subjectOf(top, r)+"."),
			WhoCanAct: "An admin: connect a worker that serves " + queueName(r.Queue) +
				", or send the work to a queue a worker serves.",
			Next: "It starts automatically when a worker that serves " + queueName(r.Queue) +
				" appears.",
		}, true
	}
	for _, w := range eligible {
		if w.Slots <= 0 || e.active[w.Owner] < w.Slots {
			return Condition{}, false
		}
	}
	// Every worker serving the queue is full. The wait for a slot began when the run joined the
	// queue or when the first of those workers appeared, whichever is later.
	first := eligible[0].FirstSeen
	for _, w := range eligible[1:] {
		if w.FirstSeen.Before(first) {
			first = w.FirstSeen
		}
	}
	if first.After(since) {
		since = first
	}
	limits := e.in.Config.Limits(top.OrgID, r.Queue, top.TemplateID, e.in.Timing.LeaseTTL)
	if e.in.Now.Sub(since) < limits.BlockedAfter {
		return Condition{}, false
	}
	holder := e.oldestHeldBy(eligible)
	n := len(eligible)
	cond := Condition{
		Blocker: Blocked, RunID: r.ID, Step: childLabel(r), Since: since, Queue: &queue,
		EligibleWorkers: &n, queue: r.Queue,
		Reason: e.withRetry(r, fmt.Sprintf("Every worker that serves %s is running as many runs "+
			"as it takes, so %s waits for a free slot.", queueName(r.Queue), subjectOf(top, r))),
		WhoCanAct: "An admin: add worker capacity for " + queueName(r.Queue) +
			", or end the run holding a slot if it is stuck.",
		Next: "It starts automatically when a slot frees up on a worker that serves " +
			queueName(r.Queue) + ".",
	}
	if holder != nil {
		cond.HolderRunID = holder.ID
		cond.Reason += " The longest running of them is run " + holder.ID + "."
	}
	return cond, true
}

// oldestHeldBy returns the run that has held a slot longest on any of the workers, nil when none
// holds one.
func (e *eval) oldestHeldBy(workers []Worker) *run.Run {
	owners := map[string]bool{}
	for _, w := range workers {
		owners[w.Owner] = true
	}
	var oldest *run.Run
	for _, r := range e.in.Runs {
		if r.Kind != "" || !owners[r.ClaimedBy] || r.Status != run.StatusRunning {
			continue
		}
		started := r.CreatedAt
		if r.StartedAt != nil {
			started = *r.StartedAt
		}
		if oldest == nil {
			oldest = r
			continue
		}
		prev := oldest.CreatedAt
		if oldest.StartedAt != nil {
			prev = *oldest.StartedAt
		}
		if started.Before(prev) || (started.Equal(prev) && r.ID < oldest.ID) {
			oldest = r
		}
	}
	return oldest
}

// workerLost returns the condition of a run whose worker stopped renewing its lease.
func (e *eval) workerLost(top, r *run.Run) Condition {
	last := *r.ClaimedAt
	reclaim := last.Add(e.in.Timing.LeaseTTL)
	ago := durationWords(e.in.Now.Sub(last))
	cond := Condition{
		Blocker: WorkerLost, RunID: r.ID, Step: childLabel(r), Since: last, Worker: r.ClaimedBy,
		LastSeen: &last, ReclaimAt: &reclaim, queue: r.Queue,
		Reason: fmt.Sprintf("Worker %s stopped reporting on %s %s ago.", r.ClaimedBy,
			subjectOf(top, r), ago),
	}
	outcome := "a run that had started ends interrupted and can be retried"
	if r.Status == run.StatusPending {
		outcome = "it goes back to the queue, since it had not started"
	}
	if r.Kind != "" {
		outcome = "the coordinator's run ends interrupted, and so do the steps it was coordinating"
	}
	deadline := reclaim.Add(e.in.Timing.SweepInterval)
	switch {
	case e.in.Now.Before(reclaim):
		cond.WhoCanAct = "Nobody yet. The lease sweep reclaims it automatically."
		cond.Next = fmt.Sprintf("Reclaimed automatically in %s: %s.",
			durationWords(reclaim.Sub(e.in.Now)), outcome)
	case e.in.Now.Before(deadline):
		cond.WhoCanAct = "Nobody yet. The lease sweep reclaims it automatically."
		cond.Next = "Reclaimed at the next lease sweep, within " +
			durationWords(e.in.Timing.SweepInterval) + ": " + outcome + "."
	default:
		cond.WhoCanAct = "An admin. Every server runs the lease sweep, so a reclaim this late " +
			"means no server is sweeping: check that one is running and can reach the database."
		cond.Next = fmt.Sprintf("The reclaim is overdue by %s. Once a server sweeps, %s.",
			durationWords(e.in.Now.Sub(reclaim)), outcome)
	}
	return cond
}

// heldRun returns the approval condition of a whole run held for a person.
func (e *eval) heldRun(r *run.Run) Condition {
	approvers := e.approvers(r)
	ap := &Approval{
		Scope: ScopeRun, DecisionID: r.ID, HeldBy: r.HeldByPolicy, ProposedFrom: r.ProposedFrom,
		Approvers: approvers,
	}
	switch r.Kind {
	case run.KindPipeline:
		ap.OnApprove = "The workflow starts"
		if first := firstSteps(r.Steps); len(first) > 0 {
			ap.OnApprove += " with " + strings.Join(first, ", ")
		}
		ap.OnApprove += "."
		ap.OnDeny = "The workflow ends rejected and none of its steps run."
	case run.KindSplit:
		shards := len(e.children[r.ID])
		if r.ShardCount != nil {
			shards = *r.ShardCount
		}
		ap.OnApprove = fmt.Sprintf("Its %d shards join %s.", shards, queueName(r.Queue))
		ap.OnDeny = "It ends rejected and none of its shards run."
	default:
		ap.OnApprove = "It joins " + queueName(r.Queue) + " and starts on a free worker."
		ap.OnDeny = "It ends rejected and never runs."
	}
	if r.Kind != run.KindPipeline && !e.anyFresh(r.Queue) {
		ap.OnApprove += " No connected worker serves " + queueName(r.Queue) +
			" right now, so it would wait for one."
	}
	reason := "It is held for approval" + heldBy(r.HeldByPolicy) + "."
	if r.ProposedFrom != "" {
		reason = "The change proposed from run " + r.ProposedFrom + " is held for approval" +
			heldBy(r.HeldByPolicy) + "."
	}
	reason += agentHoldWhy(r)
	return Condition{
		Blocker: ApprovalNeeded, RunID: r.ID, Since: r.CreatedAt, queue: r.Queue, Approval: ap,
		Reason: reason, WhoCanAct: approverSentence(approvers),
		Next: "Approve: " + ap.OnApprove + " Deny: " + ap.OnDeny,
	}
}

// heldBy says what held a run, as the end of a sentence that begins "held for approval": the rule
// that held it, quoted by name, or for the built-in hold on a run an agent asked for, the reason
// itself, since that is not a rule anybody wrote or can find in the policy list. It is empty when
// nothing is named.
func heldBy(rule string) string {
	switch rule {
	case "":
		return ""
	case policy.AgentDefaultName:
		return ": " + rule
	}
	return " by rule " + quoted(rule)
}

// agentHoldWhy returns the sentence an alert adds about a run the built-in agent hold keeps waiting
// when what the agent asked for runs code with this server's credentials, such as a dry run or an
// apply nothing has planned, led by a space. It is empty for every other run.
func agentHoldWhy(r *run.Run) string {
	if r.HeldByPolicy != policy.AgentDefaultName {
		return ""
	}
	if why := policy.AgentHoldReason(r); why != "" {
		return " " + why
	}
	return ""
}

// stepApproval returns the approval condition of a workflow waiting at one of its approval steps.
func (e *eval) stepApproval(parent, node *run.Run) Condition {
	approvers := e.approvers(node)
	state := WorkflowPaused
	if parent.Status == run.StatusRunning {
		state = OtherBranchesRunning
	}
	ap := &Approval{
		Scope: ScopeWorkflowStep, WorkflowState: state, DecisionID: node.ID, Step: node.StepName,
		Approvers: approvers,
	}
	if node.StepIndex != nil && *node.StepIndex >= 0 && *node.StepIndex < len(parent.Steps) {
		ap.Description = parent.Steps[*node.StepIndex].Description
	}
	if node.Timeout > 0 {
		at := node.CreatedAt.Add(time.Duration(node.Timeout) * time.Second)
		ap.ExpiresAt = &at
	}
	ap.OnApprove, ap.OnDeny = "What runs next could not be read.", "What runs next could not be read."
	if e.in.NextSteps != nil {
		if onApprove, onDeny, ok := e.in.NextSteps(parent, node); ok {
			ap.OnApprove = "Nothing further runs."
			if len(onApprove) > 0 {
				ap.OnApprove = "Runs " + strings.Join(onApprove, ", ") + "."
			}
			ap.OnDeny = "No step handles a denial, so the workflow fails."
			if len(onDeny) > 0 {
				ap.OnDeny = "Runs " + strings.Join(onDeny, ", ") + "."
			}
		}
	}
	reason := "The workflow is waiting at approval step " + quoted(node.StepName) + "."
	if state == OtherBranchesRunning {
		reason += " Its other branches are still running."
	} else {
		reason += " Nothing else in it is running."
	}
	next := "Approve: " + ap.OnApprove + " Deny: " + ap.OnDeny
	if ap.ExpiresAt != nil {
		next += " If nobody decides by " + ap.ExpiresAt.UTC().Format(time.RFC3339) +
			", it takes the deny path."
	}
	return Condition{
		Blocker: ApprovalNeeded, RunID: node.ID, Step: node.StepName, Since: node.CreatedAt,
		queue: parent.Queue, Approval: ap, Reason: reason, WhoCanAct: approverSentence(approvers),
		Next: next,
	}
}

// approvers says who can decide an approval on r. An admin decides, never an agent, and a rule that
// requires a different approver excludes the account that asked, which for an agent is the account
// the agent is bound to.
func (e *eval) approvers(r *run.Run) Approvers {
	a := Approvers{Role: "admin"}
	if !r.RequireDistinctApprover {
		return a
	}
	a.Excluded = r.Actor
	if r.ActorUserID != "" && e.in.Accounts != nil {
		if name := e.in.Accounts(r.ActorUserID); name != "" {
			a.Excluded = name
		}
	}
	if r.ActorType == agentActorType {
		a.Agent = r.Actor
	}
	return a
}

// approverSentence says who can decide, in a sentence.
func approverSentence(a Approvers) string {
	if a.Excluded == "" {
		return "An admin can approve or deny it. No agent can approve it."
	}
	asker := a.Excluded
	if a.Agent != "" && a.Agent != a.Excluded {
		asker = a.Excluded + ", the account agent " + a.Agent + " acts for,"
	}
	return "An admin other than " + asker + " can approve it, because the rule that held it " +
		"requires a different person from the one who asked. " + a.Excluded +
		" can still deny it. No agent can approve it."
}

// anyFresh reports whether a connected worker serves queue.
func (e *eval) anyFresh(queue string) bool {
	for _, w := range e.fresh {
		if w.Serves(queue) {
			return true
		}
	}
	return false
}

// scheduleItems returns a blocked item for each schedule skipping its fires because a run it
// started earlier is still going, once that has lasted past the blocked threshold.
func (e *eval) scheduleItems() []Item {
	var items []Item
	for _, sc := range e.in.Schedules {
		if !sc.Enabled || sc.LastRunAt == nil {
			continue
		}
		holder := e.scheduleHolder(sc)
		if holder == nil {
			continue
		}
		// The first fire after the one that started the holder is the first one skipped.
		missed, err := sc.NextFire(*sc.LastRunAt)
		if err != nil || missed.After(e.in.Now) {
			continue
		}
		limits := e.in.Config.Limits(sc.OrgID, holder.Queue, sc.TemplateID, e.in.Timing.LeaseTTL)
		if e.in.Now.Sub(missed) < limits.BlockedAfter {
			continue
		}
		cond := Condition{
			Blocker: Blocked, RunID: holder.ID, Since: missed, HolderRunID: holder.ID,
			queue: holder.Queue,
			Reason: "It has skipped every fire since " + missed.UTC().Format(time.RFC3339) +
				" because run " + holder.ID + ", which it started earlier, is still " +
				statusWords(holder) + ".",
			WhoCanAct: holderSentence(holder),
			Next:      "It fires again at its first time due after run " + holder.ID + " finishes.",
		}
		name := sc.Name
		if name == "" {
			name = sc.ID
		}
		it := Item{Key: "schedule:" + sc.ID, Kind: "schedule", ScheduleID: sc.ID, Name: name,
			Run: holder, Schedule: sc}
		e.finish(&it, []Condition{cond}, sc.OrgID, sc.TemplateID)
		items = append(items, it)
	}
	return items
}

// scheduleHolder returns the unfinished top-level run a schedule started that its next fire waits
// behind, the oldest when there are several, the way the scheduler's own overlap check finds it.
func (e *eval) scheduleHolder(sc *schedule.Schedule) *run.Run {
	var holder *run.Run
	for _, r := range e.in.Runs {
		if r.ParentID != nil || r.Status.Terminal() {
			continue
		}
		if r.ID != sc.LastRunID && (r.Source != "schedule" || r.SourceID != sc.ID) {
			continue
		}
		if holder == nil || r.CreatedAt.Before(holder.CreatedAt) {
			holder = r
		}
	}
	return holder
}

// statusWords describes where an unfinished run stands, for a sentence.
func statusWords(r *run.Run) string {
	switch r.Status {
	case run.StatusPendingApproval:
		return "waiting for approval"
	case run.StatusPending:
		return "queued"
	}
	return "running"
}

// holderSentence says who can release a schedule held back by r.
func holderSentence(r *run.Run) string {
	switch r.Status {
	case run.StatusPendingApproval:
		return "An admin deciding run " + r.ID + " releases the schedule, and so does canceling " +
			"the run."
	case run.StatusPending:
		return "The schedule resumes once run " + r.ID + " runs and finishes. It is still queued, " +
			"so look at why nothing has taken it."
	}
	return "The schedule resumes when run " + r.ID + " finishes. Canceling the run releases it " +
		"sooner."
}

// interaction says in one line how an item's main blocker and its other conditions bear on each
// other, so a reader knows whether dealing with one clears the rest. It is empty with no other
// condition.
func interaction(main Condition, badges []Condition) string {
	if len(badges) == 0 {
		return ""
	}
	other := badges[0].Blocker
	for _, b := range badges[1:] {
		if rank(b.Blocker) < rank(other) {
			other = b.Blocker
		}
	}
	if other == main.Blocker {
		switch main.Blocker {
		case ApprovalNeeded:
			return "Each approval step is decided on its own id, and deciding one does not decide " +
				"the others."
		case WorkerLost:
			return "The lease sweep reclaims each lost worker's run on its own."
		case NoWorker:
			return "Each queued run starts as soon as a worker that serves its queue connects."
		default:
			return "Each blocked run starts on its own when what holds it frees up."
		}
	}
	switch main.Blocker {
	case WorkerLost:
		switch other {
		case NoWorker:
			return "The lease sweep settles the lost worker's run on its own, and the queued runs " +
				"still need a worker that serves their queue."
		case ApprovalNeeded:
			return "The approval can be decided now. Deciding it does not wait for the lost " +
				"worker's run, which the lease sweep settles on its own."
		default:
			return "The lease sweep settles the lost worker's run on its own, and the blocked run " +
				"still waits for what holds it."
		}
	case NoWorker:
		if other == ApprovalNeeded {
			return "The approval can be decided now, and the queued runs start once a worker that " +
				"serves their queue connects. Neither waits on the other."
		}
		return "A worker connecting for the queue nobody serves starts the queued runs, and the " +
			"blocked run still waits for what holds it."
	}
	return "Deciding the approval does not start the blocked run, which still waits for what " +
		"holds it."
}

// firstSteps names the steps a workflow starts with: those that wait on nothing.
func firstSteps(steps []run.PipelineStep) []string {
	var out []string
	for _, s := range run.GraphSteps(steps) {
		if len(s.DependsOn) == 0 && len(s.IfDenied) == 0 {
			out = append(out, s.Name)
		}
	}
	return out
}

// runName labels a run for a person: its playbook or workflow name, else its tool and id. It never
// returns a command, which can carry a script with secrets in it.
func runName(r *run.Run) string {
	if r.Playbook != "" {
		return r.Playbook
	}
	if tool := run.NormalizeTool(r.Tool); tool != run.ToolAnsible {
		return tool + " " + r.ID
	}
	return r.ID
}

// childLabel names a step or shard for a condition on it, empty for a top-level run.
func childLabel(r *run.Run) string {
	switch {
	case r.ParentID == nil:
		return ""
	case r.StepName != "":
		return r.StepName
	case r.ShardIndex != nil:
		return fmt.Sprintf("shard %d", *r.ShardIndex+1)
	}
	return r.ID
}

// subjectOf names what a condition is on, for a sentence: the item's run, or one of its steps.
func subjectOf(top, r *run.Run) string {
	if r.ID == top.ID {
		return "this run"
	}
	if label := childLabel(r); label != r.ID {
		if r.StepName != "" {
			return "step " + quoted(label)
		}
		return label
	}
	return "run " + r.ID
}

// withRetry adds, for a retry of a workflow step, which attempt it is, so a retry reads as a
// reason on the item rather than as a condition of its own.
func (e *eval) withRetry(r *run.Run, sentence string) string {
	if r.Attempt <= 0 || r.ParentID == nil {
		return sentence
	}
	parent, ok := e.byID[*r.ParentID]
	if !ok || r.StepIndex == nil || *r.StepIndex < 0 || *r.StepIndex >= len(parent.Steps) {
		return sentence + fmt.Sprintf(" This is retry %d.", r.Attempt)
	}
	return sentence + fmt.Sprintf(" This is retry %d of %d.", r.Attempt,
		parent.Steps[*r.StepIndex].Retries)
}

// queueName names a queue in a sentence.
func queueName(q string) string {
	if q == "" {
		return "the default queue"
	}
	return "queue " + quoted(q)
}

// quoted wraps a name in double quotes for a sentence.
func quoted(s string) string {
	return "\"" + s + "\""
}

// durationWords renders a duration the way the dashboard reads it: seconds under two minutes,
// minutes under two hours, hours under two days, and days past that.
func durationWords(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < 2*time.Minute:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	case d < 2*time.Hour:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int64(d/(24*time.Hour)))
}

// Note returns the alert an item raises, as the notification channels carry it.
func (it Item) Note(now time.Time) *run.AttentionNote {
	sum := sha256.Sum256([]byte(it.AlertKey))
	n := &run.AttentionNote{
		ID: "att_" + hex.EncodeToString(sum[:8]), Blocker: string(it.Blocker),
		Reason: it.Main.Reason, WhoCanAct: it.Main.WhoCanAct, Next: it.Main.Next,
		Since: it.Since, WaitingSeconds: int64(max(now.Sub(it.Since), 0) / time.Second),
		ThresholdSeconds: it.AlertAfterSeconds, Path: "/ui/?attention=" + string(it.Blocker),
	}
	waited := durationWords(now.Sub(it.Since))
	subject := "Run " + it.Name
	if it.Kind == "workflow" {
		subject = "Workflow " + it.Name
	}
	switch it.Blocker {
	case WorkerLost:
		n.Summary = fmt.Sprintf("%s has not been reclaimed %s after worker %s stopped reporting.",
			subject, waited, it.Main.Worker)
	case NoWorker:
		queue := ""
		if it.Main.Queue != nil {
			queue = *it.Main.Queue
		}
		n.Summary = fmt.Sprintf("%s has waited %s for a worker: none serves %s.", subject, waited,
			queueName(queue))
	case ApprovalNeeded:
		n.Summary = fmt.Sprintf("%s has waited %s for approval.", subject, waited)
		if it.Main.Approval != nil && it.Main.Approval.Scope == ScopeWorkflowStep {
			n.Summary = fmt.Sprintf("%s has waited %s at approval step %s.", subject, waited,
				quoted(it.Main.Approval.Step))
		}
	case Blocked:
		n.Summary = fmt.Sprintf("%s has been blocked for %s.", subject, waited)
		if it.Schedule != nil {
			n.ScheduleID, n.ScheduleName = it.Schedule.ID, it.Name
			n.Summary = fmt.Sprintf("Schedule %s has skipped its fires for %s behind run %s.",
				it.Name, waited, it.Main.HolderRunID)
		}
	}
	return n
}
