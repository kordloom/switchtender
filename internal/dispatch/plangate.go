package dispatch

import (
	"bytes"
	"context"
	"fmt"

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
	var plan bytes.Buffer
	return d.streamSpec(ctx, r, true, &plan,
		func(res roundhouse.Result, runErr error, mask *masker) run.Status {
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
		opts = append(opts, run.WithRequireApproval(true), run.WithHeldByPolicy(fmt.Sprintf(
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
	proposal, err := d.Submit(ctx, r.Playbook, r.Inventory, opts...)
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
