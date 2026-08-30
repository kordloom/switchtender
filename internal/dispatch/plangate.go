package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

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

// executePlanGate runs r as a plan, then proposes an apply cloned from it instead of applying in
// place. The plan's output becomes r's log so an approver reviews exactly what would change, and the
// proposed apply carries r's id in ProposedFrom so it never re-gates. This run finalizes as succeeded
// because it ran a plan; a plan that cannot run finalizes as failed or canceled and proposes nothing.
func (d *Dispatcher) executePlanGate(ctx context.Context, r *run.Run, policies []*policy.Policy) run.Status {
	plan := &cappedBuffer{cap: planReadCap}
	return d.streamSpec(ctx, r, true, plan,
		func(res roundhouse.Result, runErr error, mask *masker, _ *run.SummaryFold) run.Status {
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
			}
			destroys, read := parsePlanDestroys(plan.String())
			// A plan too large to hold was never weighed, so it is not reported as weighed. The
			// summary sits at the end and the parser also requires every summary line in the output
			// to agree, so judging a truncated copy could both miss the answer and miss a
			// disagreement. Declining is the existing fail-safe: an unreadable summary holds the
			// apply for a person, which is what an unmeasured plan deserves.
			if plan.truncated {
				read = false
			}
			return d.proposeApply(ctx, r, policies, destroys, read, mask)
		})
}

// proposeApply builds the apply proposed from a completed plan run and finalizes the plan run as
// succeeded. The proposal is a clone of r run for real, held for approval when destroys exceeds a
// scoping policy's threshold and queued to run otherwise, and it carries r's id so it never re-gates.
// The read argument reports whether destroys came from a summary the parser actually found, and a
// plan that could not be read is held rather than queued. A synthesized log line records the
// decision beneath the plan output. A failure to create the proposal fails the plan run so the
// missing apply is visible rather than silently dropped.
func (d *Dispatcher) proposeApply(
	ctx context.Context, r *run.Run, policies []*policy.Policy, destroys int, read bool, mask *masker,
) run.Status {
	// A relay-backed store cannot create a run, so the control node is asked to build the proposal
	// from the plan it already holds. Everywhere else this submits directly, unchanged.
	var proposal *run.Run
	var err error
	if proposer, ok := d.store.(applyProposer); ok {
		proposal, err = proposer.ProposeApply(ctx, r.ID, destroys, read)
	} else {
		proposal, err = d.Submit(ctx, r.Playbook, r.Inventory, applyOptions(r, policies, destroys, read)...)
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
// One apply per plan, always the same one. A worker whose 201 never arrived retries, which is
// legitimate, so the second call has to return the proposal the first one made rather than mint a
// second real apply. The key is derived from the plan and carries the server's reserved prefix, which
// no caller may supply, so the store's unique index settles it whichever process asks.
func ProposeApplyFor(ctx context.Context, store run.Store, policies []*policy.Policy, plan *run.Run,
	destroys int, read bool) (*run.Run, error) {
	if plan == nil {
		return nil, fmt.Errorf("propose apply: no plan run")
	}
	proposal := &run.Run{
		ID: run.NewID(), Playbook: plan.Playbook, Inventory: plan.Inventory,
		Status: run.StatusPending, CreatedAt: time.Now(),
		IdempotencyKey: applyKeyFor(plan.ID),
	}
	run.ApplyOptions(proposal, applyOptions(plan, policies, destroys, read))

	// The apply faces the rules every other submission faces. This path wrote straight to the store, so
	// a deny rule never refused it, a blanket approval rule never held it, and the rule set in force was
	// never recorded: the same install refused the apply when the control node claimed the plan and ran
	// it when a worker did. The plan-content threshold applyOptions weighs is one rule among them, not
	// the only one.
	stampPolicySet(proposal, policies)
	if p := policy.Denying(policies, proposal); p != nil {
		return nil, fmt.Errorf("%w: policy %q refuses this apply", ErrPolicyDenied, p.Label())
	}
	if p := policy.Requiring(policies, proposal); p != nil {
		proposal.Status = run.StatusPendingApproval
		proposal.HeldByPolicy = p.Label()
		proposal.RequireDistinctApprover = proposal.RequireDistinctApprover ||
			policy.RequireDistinct(policies, proposal)
	}

	err := store.Save(ctx, proposal)
	if errors.Is(err, run.ErrDuplicateKey) {
		existing, ferr := store.ByIdempotencyKey(ctx, proposal.IdempotencyKey)
		if ferr != nil {
			return nil, fmt.Errorf("propose apply: %w", ferr)
		}
		return existing, nil
	}
	if err != nil {
		return nil, fmt.Errorf("propose apply: %w", err)
	}
	return proposal, nil
}

// applyKeyFor is the idempotency key the apply proposed from one plan holds, so a plan can only ever
// have the one apply.
func applyKeyFor(planID string) string {
	return "st:apply:" + planID
}

// applyProposer is a store that can create the apply a plan proposes on its behalf. A relay-backed
// store implements it because a worker cannot create runs itself.
type applyProposer interface {
	// ProposeApply asks the control node to create the apply for the named plan run.
	ProposeApply(ctx context.Context, planID string, destroys int, read bool) (*run.Run, error)
}

// applyOptions builds the submit options for the apply a plan proposes: everything about the plan run
// that decides what the apply does, who asked for it, and which code it runs.
func applyOptions(r *run.Run, policies []*policy.Policy, destroys int, read bool) []run.SubmitOption {
	opts := []run.SubmitOption{
		run.WithTool(r.Tool),
		run.WithCommand(r.Command),
		run.WithDryRun(false),
		run.WithProposedFrom(r.ID),
	}
	// A plan held for destroying too much records the rule and the count, since "why did this
	// wait" is answered by the threshold it crossed, not merely by the rule's name. A plan whose
	// summary could not be read is held as well: this run reached here only because a plan-content
	// policy scopes it, and a plan nobody could weigh against the destroy limit has not passed that
	// limit. Queuing it would apply an unmeasured plan as though it destroyed nothing.
	if !read {
		opts = append(opts, run.WithRequireApproval(true), run.WithHeldByPolicy(
			"plan summary unreadable, so the destroy count was never weighed against the limit"))
	} else if p := policy.Exceeding(policies, r, destroys); p != nil {
		// The rule's own second-approver requirement travels with the hold. policy.Requiring, which
		// the dispatcher's pass consults, only considers rules with no destroy limit, so the rule
		// that held this apply is the one rule that pass excludes: without copying it here nothing
		// would, and whoever asked for the destroy could release it themselves.
		opts = append(opts, run.WithRequireApproval(true),
			run.WithRequireDistinctApprover(p.RequireDistinctApprover),
			run.WithHeldByPolicy(fmt.Sprintf(
				"%s (plan destroys %d, limit %d)", p.Label(), destroys, p.MaxDestroy)))
	}
	if r.ProjectID != "" {
		opts = append(opts, run.WithProject(r.ProjectID))
	}
	if r.InventoryID != "" {
		opts = append(opts, run.WithInventory(r.InventoryID))
	}
	if len(r.CredentialIDs) > 0 {
		opts = append(opts, run.WithCredentialIDs(r.CredentialIDs))
	}
	if len(r.ExtraVars) > 0 {
		opts = append(opts, run.WithExtraVars(r.ExtraVars))
	}
	if r.Queue != "" {
		opts = append(opts, run.WithQueue(r.Queue))
	}
	if r.Image != "" {
		opts = append(opts, run.WithImage(r.Image, r.PullCredentialID))
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
	}
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
