package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// parsePlanDestroys returns the destroy count from a plan's change summary and whether that summary
// was read at all. It reuses the parser the drift check reads, taking only the resources a plan would
// destroy so the plan-content gate weighs destruction rather than total change. An unread summary
// reports zero destroys with read false, and the caller must not confuse that with a plan proven to
// destroy nothing.
func parsePlanDestroys(out string) (destroys int, read bool) {
	counts, ok := parsePlanSummary(out)
	return counts.Destroy, ok
}

// planGatePolicies returns the stored policies when r is a terraform or opentofu apply that a
// plan-content policy scopes, so execute plans it before applying. It returns nil when r is not a
// candidate: policies are off, the tool is not terraform or opentofu, the run is a dry run, the run
// is itself a proposed apply, which must never re-gate and loop, or no plan-content policy matches. A
// policy store failure is reported, not treated as no gate: a run that cannot be checked against the
// plan-content policies must not apply as though it had been.
func (d *Dispatcher) planGatePolicies(ctx context.Context, r *run.Run) ([]*policy.Policy, error) {
	if d.policies == nil {
		return nil, nil
	}
	tool := run.NormalizeTool(r.Tool)
	if tool != run.ToolTerraform && tool != run.ToolOpenTofu {
		return nil, nil
	}
	if r.DryRun || r.ProposedFrom != "" {
		return nil, nil
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return nil, fmt.Errorf("%w: %w", ErrPolicyUnavailable, err)
	}
	if !policy.PlanGated(policies, r) {
		return nil, nil
	}
	return policies, nil
}

// executePlanGate runs r as a plan that saves its plan file, then proposes an apply that carries out
// exactly that plan instead of applying in place. The plan's output becomes r's log so an approver
// reviews exactly what would change. What the plan destroys is measured from the saved plan itself,
// rendered as JSON, never from the text the tool printed, and the plan file is sealed onto the
// proposed apply, whose approval binds its digest. The apply then runs the plan file, so the tool
// refuses it if the infrastructure changed since the plan was made, rather than planning again and
// applying something nobody weighed or approved.
//
// The proposed apply carries r's id in ProposedFrom so it never re-gates. This run finalizes as
// succeeded because it ran a plan; a plan that cannot run, or saved no plan file, finalizes as failed
// or canceled and proposes nothing.
func (d *Dispatcher) executePlanGate(ctx context.Context, r *run.Run, policies []*policy.Policy) run.Status {
	return d.streamSpec(ctx, r, true, nil,
		func(res roundhouse.Result, runErr error, mask *masker, _ *run.SummaryFold) run.Status {
			// The plan file and its rendering hold the plan's values in the clear, so they are dropped
			// here whatever happens: the sealed copy on the proposed apply is the only one kept.
			defer clear(res.PlanFile)
			defer clear(res.PlanJSON)
			switch {
			case runErr != nil && ctx.Err() != nil:
				d.finalize(r, run.StatusCanceled, nil, "")
				return run.StatusCanceled
			case runErr != nil:
				d.finalize(r, run.StatusFailed, nil, mask.redactString(runErr.Error()))
				return run.StatusFailed
			case res.ExitCode != 0:
				// A nonzero plan means init or plan itself failed; the runner maps a plan with pending
				// changes to zero. Do not propose an apply from a broken plan.
				d.finalize(r, run.StatusFailed, &res.ExitCode, "")
				return run.StatusFailed
			case len(res.PlanFile) == 0:
				// An apply carries out a saved plan or nothing, so a plan that saved none proposes
				// nothing an approver could release.
				d.finalize(r, run.StatusFailed, nil, "the plan saved no plan file, so there is no "+
					"plan for an apply to carry out")
				return run.StatusFailed
			}
			// A rendering that would not read, or was too large to hold, was never weighed, so it is
			// not reported as weighed. Declining is the fail-safe: an unmeasured plan holds the apply
			// for a person.
			destroys, read := planDestroysFromJSON(res.PlanJSON)
			return d.proposeApply(ctx, r, policies, destroys, read, res.PlanFile, mask)
		})
}

// planDestroysFromJSON returns how many resources a plan rendered as JSON would destroy, and whether
// the rendering was read at all. A resource is destroyed when its planned actions include delete,
// which counts a replacement once, the way the tool's own summary counts it, and leaves out a
// resource the plan only forgets. A rendering that does not decode reports zero with read false,
// and the caller must not confuse that with a plan proven to destroy nothing.
func planDestroysFromJSON(rendered []byte) (int, bool) {
	if len(rendered) == 0 {
		return 0, false
	}
	var plan struct {
		// FormatVersion is the rendering's format version, present in every plan the tool renders.
		FormatVersion string `json:"format_version"`
		// ResourceChanges are the planned resource changes.
		ResourceChanges []struct {
			// Change is the planned change.
			Change struct {
				// Actions are the planned actions, such as create, update, delete, and no-op.
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(rendered, &plan); err != nil || plan.FormatVersion == "" {
		return 0, false
	}
	destroys := 0
	for _, rc := range plan.ResourceChanges {
		if slices.Contains(rc.Change.Actions, "delete") {
			destroys++
		}
	}
	return destroys, true
}

// proposeApply builds the apply proposed from a completed plan run and finalizes the plan run as
// succeeded. The proposal is a clone of r run for real, held for approval when destroys exceeds a
// scoping policy's threshold and queued to run otherwise, and it carries r's id so it never re-gates.
// The read argument reports whether destroys came from a summary the parser actually found, and a
// plan that could not be read is held rather than queued. A synthesized log line records the
// decision beneath the plan output. A failure to create the proposal fails the plan run so the
// missing apply is visible rather than silently dropped.
func (d *Dispatcher) proposeApply(
	ctx context.Context, r *run.Run, policies []*policy.Policy, destroys int, read bool, plan []byte,
	mask *masker,
) run.Status {
	// A relay-backed store cannot create a run, so the control node is asked to build the proposal
	// from the plan it already holds, handed the plan file to seal with its own key. Everywhere else
	// the plan file is sealed here and the proposal submitted directly.
	var proposal *run.Run
	var err error
	if proposer, ok := d.store.(applyProposer); ok {
		proposal, err = proposer.ProposeApply(ctx, r.ID, destroys, read, plan)
	} else {
		sealed, serr := d.sealBytes(plan)
		if serr != nil {
			err = fmt.Errorf("%w: seal the plan file: %w", ErrPlanFile, serr)
		} else {
			opts := append(applyOptions(r, policies, destroys, read), run.WithPlanFile(sealed))
			proposal, err = d.Submit(ctx, r.Playbook, r.Inventory, opts...)
		}
	}
	if err != nil {
		d.log.Error("dispatch: propose apply: "+err.Error(), zap.String("run_id", r.ID))
		d.finalize(r, run.StatusFailed, nil, "propose apply: "+mask.redactString(err.Error()))
		return run.StatusFailed
	}

	disposition := "queued to apply"
	if proposal.Status == run.StatusPendingApproval {
		disposition = "held for approval"
	}
	effect := fmt.Sprintf("plan would destroy %d resource(s)", destroys)
	if !read {
		effect = "plan summary could not be read"
	}
	note := fmt.Sprintf("switchtender: %s; proposed apply %s %s.\n",
		effect, proposal.ID, disposition)
	if err := d.store.AppendLog(ctx, r.ID, []byte(note)); err != nil {
		d.log.Error("dispatch: plan gate note: "+err.Error(), zap.String("run_id", r.ID))
	}
	code := 0
	d.finalize(r, run.StatusSucceeded, &code, "")
	return run.StatusSucceeded
}

// ProposeApplyFor builds and stores the apply a plan run proposes, deciding the hold from policies.
//
// It exists for the relay. A worker has no path to create a run, deliberately: the save endpoint
// refuses an unknown id because a worker only ever reports on what it claimed. That left the
// plan-content gate unable to complete anywhere but the control node, so a gated terraform apply
// executed on a worker failed instead of waiting for an approver. The worker now reports what its plan
// found and the control node builds the proposal from the plan run it already holds, which is narrower
// than letting a worker submit a run: the apply's command, target, credentials, image, and commit come
// from the stored plan rather than from the worker's request.
//
// One apply per plan, always the same one. A worker whose 201 never arrived retries, which is
// legitimate, so the second call has to return the proposal the first one made rather than mint a
// second real apply. The key is derived from the plan and carries the server's reserved prefix, which
// no caller may supply, so the store's unique index settles it whichever process asks.
//
// created reports whether this call stored the proposal, false when a retry found the one an
// earlier call made, so a caller announcing the proposal announces it once.
func ProposeApplyFor(ctx context.Context, store run.Store, policies []*policy.Policy, plan *run.Run,
	destroys int, read bool, sealedPlan string) (proposal *run.Run, created bool, err error) {
	if plan == nil {
		return nil, false, fmt.Errorf("propose apply: no plan run")
	}
	if sealedPlan == "" {
		return nil, false, fmt.Errorf("%w: the plan saved no plan file, so there is no plan for an "+
			"apply to carry out", ErrPlanFile)
	}
	proposal = &run.Run{
		ID: run.NewID(), Playbook: plan.Playbook, Inventory: plan.Inventory,
		Status: run.StatusPending, CreatedAt: time.Now(),
		IdempotencyKey: applyKeyFor(plan.ID),
	}
	run.ApplyOptions(proposal, append(applyOptions(plan, policies, destroys, read),
		run.WithPlanFile(sealedPlan)))
	if err := gateProposal(policies, proposal); err != nil {
		return nil, false, err
	}

	err = store.Save(ctx, proposal)
	if errors.Is(err, run.ErrDuplicateKey) {
		existing, ferr := store.ByIdempotencyKey(ctx, proposal.IdempotencyKey)
		if ferr != nil {
			return nil, false, fmt.Errorf("propose apply: %w", ferr)
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("propose apply: %w", err)
	}
	return proposal, true, nil
}

// gateProposal puts a proposed apply in front of the rules every other submission faces, holding it
// in place when a blanket rule requires approval and refusing it when a deny rule matches.
//
// The apply faces the rules every other submission faces. This path wrote straight to the store, so
// a deny rule never refused it, a blanket approval rule never held it, and the rule set in force
// was never recorded: the same install refused the apply when the control node claimed the plan and
// ran it when a worker did. The plan-content threshold applyOptions weighs is one rule among them,
// not the only one. A review's preview of the apply decides through this same function, so the
// answer a pull request is shown cannot drift from the answer the apply later gets.
func gateProposal(policies []*policy.Policy, proposal *run.Run) error {
	stampPolicySet(proposal, policies)
	// Graded like every other submission, for the reason above: this path faces the same rules, so
	// it has to see what they see. A rule written on whether a change can be taken back would
	// otherwise apply everywhere except here.
	gp := gradedLocally(proposal)
	if p := policy.Denying(policies, gp); p != nil {
		return fmt.Errorf("%w: policy %q refuses this apply", ErrPolicyDenied,
			maskPolicyText(gp, p.Label()))
	}
	// What a policy noted about the apply is recorded on it, held or not, exactly as on a
	// submission: a rule that notes a large destroy rather than holding it has to leave that on the
	// run that destroys.
	proposal.PolicyNotes = policyNotes(policies, gp)
	if p := policy.Requiring(policies, gp); p != nil {
		proposal.Status = run.StatusPendingApproval
		proposal.HeldByPolicy = maskPolicyText(gp, p.Label())
		proposal.RequireDistinctApprover = proposal.RequireDistinctApprover ||
			policy.RequireDistinct(policies, gp)
		proposal.RequireReason = decision.Stricter(proposal.RequireReason,
			policy.ReasonRequirement(policies, gp))
	}
	return nil
}

// previewProposal returns the decision the plan gate would make on the apply it proposes from a
// plan of apply that destroys the given number of resources. It builds the proposal exactly as
// ProposeApplyFor does and gates it with gateProposal, without storing anything.
func previewProposal(policies []*policy.Policy, apply *run.Run, destroys int, read bool) ApplyPreview {
	proposal := &run.Run{
		ID: "run_preview", Playbook: apply.Playbook, Inventory: apply.Inventory,
		Status: run.StatusPending,
	}
	run.ApplyOptions(proposal, applyOptions(apply, policies, destroys, read))
	if err := gateProposal(policies, proposal); err != nil {
		var rule string
		gp := gradedLocally(proposal)
		if p := policy.Denying(policies, gp); p != nil {
			rule = maskPolicyText(gp, p.Label())
		}
		return ApplyPreview{Outcome: ApplyDenied, Rule: rule, Stage: StagePlan, PlanGated: true}
	}
	if proposal.Status == run.StatusPendingApproval {
		return ApplyPreview{
			Outcome: ApplyHeld, Rule: proposal.HeldByPolicy, Stage: StagePlan, PlanGated: true,
			RequireDistinctApprover: proposal.RequireDistinctApprover,
		}
	}
	return ApplyPreview{Outcome: ApplyRuns, PlanGated: true}
}

// applyKeyFor is the idempotency key the apply proposed from one plan holds, so a plan can only ever
// have the one apply.
func applyKeyFor(planID string) string {
	return "st:apply:" + planID
}

// applyProposer is a store that can create the apply a plan proposes on its behalf. A relay-backed
// store implements it because a worker cannot create runs itself.
type applyProposer interface {
	// ProposeApply asks the control node to create the apply for the named plan run, carrying out the
	// plan file the plan saved, which the control node seals.
	ProposeApply(ctx context.Context, planID string, destroys int, read bool, plan []byte) (*run.Run,
		error)
}

// applyOptions builds the submit options for the apply a plan proposes: everything about the plan run
// that decides what the apply does, who asked for it, and which code it runs.
func applyOptions(r *run.Run, policies []*policy.Policy, destroys int, read bool) []run.SubmitOption {
	// The apply is the plan's own spec run for real, so it starts from the one description of how a
	// run executes rather than from a list kept here. A list kept here is how the apply lost the
	// plan's timeout, the way every other hand-kept copy of the spec lost a field.
	opts := append(r.ExecutionOptions(), run.WithDryRun(false), run.WithProposedFrom(r.ID))
	// A plan held for destroying too much records the rule and the count, since "why did this
	// wait" is answered by the threshold it crossed, not merely by the rule's name. A plan whose
	// summary could not be read is held as well: this run reached here only because a plan-content
	// policy scopes it, and a plan nobody could weigh against the destroy limit has not passed that
	// limit. Queuing it would apply an unmeasured plan as though it destroyed nothing.
	if !read {
		opts = append(opts, run.WithRequireApproval(true), run.WithHeldByPolicy(
			"plan summary unreadable, so the destroy count was never weighed against the limit"))
	} else {
		// The count is recorded on the apply, where the grade reads it. Without it the apply graded
		// like any command naming a directory, costly and medium, so a rule holding irreversible or
		// high risk changes let an apply that destroys resources run straight through.
		opts = append(opts, run.WithPlanDestroys(destroys))
		// Graded like every other evaluation: the hold decision and the release decision must see
		// the same run, or a rule with a risk or reversibility floor can hold an apply whose
		// second-approver requirement was computed blind to that floor. The run weighed is the apply
		// as proposed, carrying the count, on a copy so the plan run keeps its own record. Weighed
		// as the plan run, a rule pairing a destroy limit with a floor read the grade as costly and
		// never held a plan past its limit.
		gr := gradedLocally(r)
		if gr != nil {
			apply := *gr
			apply.PlanDestroys = &destroys
			gr = &apply
		}
		if p := policy.Exceeding(policies, gr, destroys); p != nil {
			// The second-approver requirement travels with the hold, and it is the strictest
			// answer across every rule the count exceeded, not the first one's. policy.Requiring,
			// which the dispatcher's pass consults, only considers rules with no destroy limit, so
			// the rules that held this apply are exactly the ones that pass excludes: without
			// deciding here nothing would, and whoever asked for the destroy could release it.
			opts = append(opts, run.WithRequireApproval(true),
				run.WithRequireDistinctApprover(policy.ExceedingDistinct(policies, gr, destroys)),
				run.WithRequireReason(policy.ExceedingReason(policies, gr, destroys)),
				run.WithHeldByPolicy(maskPolicyText(gr, fmt.Sprintf(
					"%s (plan destroys %d, limit %d)", p.Label(), destroys, p.MaxDestroy))))
		}
	}
	// The apply is proposed while executing the plan, long after the plan's request returned, so
	// the executor's context carries no receipt. The plan run's receipt is the truthful one: the
	// request that submitted the plan is what set this apply in motion, and the apply is the run
	// that actually destroys things, so its evidence must not read as having no origin.
	if r.AuditReceipt != "" {
		opts = append(opts, run.WithAuditReceiptOf(r.AuditReceipt))
	}
	// The apply is proposed by the executor, whose context carries no submitting org, so the plan
	// run's org is the truthful one: the apply belongs to the same tenant as the plan that spawned
	// it, and a plan of an objectless working directory would otherwise leave the apply readable
	// across every tenant.
	opts = append(opts, run.WithOrgID(r.OrgID))
	// The apply inherits the plan's actor. It is created by the executor, so nothing filled this in
	// and the run that actually destroys infrastructure was attributed to nobody: an actor-scoped
	// approval policy could not match it, and its chain entries named no requester. The plan's actor is
	// the truthful answer, for the same reason its receipt and its organization are.
	if r.Actor != "" {
		// The account travels with the name. Separation of duties compares accounts, so an apply that
		// inherited only the display name let the person who submitted the plan release the apply that
		// destroys the infrastructure: the distinct-approver rule compared a credential label against
		// a username, never matched, and the chain then recorded the release as correctly approved.
		// This is the highest blast radius run the gate governs, so it is the last place to lose it.
		opts = append(opts, run.WithActor(r.Actor), run.WithActorType(r.ActorType),
			run.WithActorAccount(r.ActorUserID))
		// An agent's plan proposes an apply that is still the agent's change, so the apply carries
		// the same identity evidence: which agent, the account it is bound to, and who provisioned
		// it. The apply is built by the executor, where no request context carries any of it.
		opts = append(opts, run.WithInitiator(r.Initiator))
	}
	// It carries the plan's origin and labels too, because it is the second half of the same request.
	// Without them the run that actually destroys infrastructure showed a blank origin in the runs
	// list, fell out of the history of the template that launched its plan, and lost the ticket label
	// its plan was filed under.
	if r.Source != "" {
		opts = append(opts, run.WithSource(r.Source, r.SourceID))
	}
	opts = append(opts, run.WithLabels(r.Labels))
	// The notification targets were set for the change, and the apply is the run that makes it.
	// The execution spec leaves them to each caller, and without them the apply told none of the
	// channels the request named that it held, finished, or failed. A plain-language request travels
	// too, since it is what the apply's approver checks the plan against.
	opts = append(opts, run.WithNotifications(r.Notifications), run.WithIntent(r.Intent))
	// And the inventory the plan was made against, the snapshot the plan run was submitted with,
	// since the apply carries out that plan.
	opts = append(opts, run.WithInventorySnapshot(r.InventorySnapshot, r.InventorySealed))
	// And it is pinned to the commit the plan was read from. An approver reads a plan and releases the
	// apply on the strength of what it said it would destroy; without a pin the apply re-syncs the
	// project and takes whatever the branch head is by then, so an approval of one plan could release
	// an apply of different code with nothing in the record showing the substitution.
	if r.CommitSHA != "" {
		opts = append(opts, run.WithPinnedCommit(r.CommitSHA))
	}
	return opts
}

// planReadCap bounds how much plan output is held in memory to read the summary from.
//
// The whole plan was buffered with no limit while the run's stored log is capped, so a large or
// deliberately inflated plan escaped the container's memory limit into the server's heap, and
// plan.String copied it again. Several gated applies at once could then take the process down, and
// with it every other run, the API, and the UI. A few megabytes is far more than any real summary
// needs and small enough that a fleet of them costs nothing.
const planReadCap = 4 << 20

// cappedBuffer accumulates up to cap bytes and remembers that it stopped, so a caller can tell a
// complete answer from a partial one rather than reading a truncated copy as though it were whole.
type cappedBuffer struct {
	// buf holds what fit.
	buf bytes.Buffer
	// cap is the most that will be held.
	cap int
	// truncated reports that output was discarded, so what is held is not the whole of it.
	truncated bool
}

// Write stores what fits and discards the rest, always reporting the full length so the writer it
// tees from is never told its output was short.
func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.cap - b.buf.Len(); room > 0 {
		if len(p) <= room {
			b.buf.Write(p)
			return len(p), nil
		}
		b.buf.Write(p[:room])
	}
	b.truncated = true
	return len(p), nil
}

// String returns what was held.
func (b *cappedBuffer) String() string { return b.buf.String() }
