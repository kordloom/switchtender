package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"strconv"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// parsePlanDestroys returns the destroy count from a plan's summary line, or zero when no summary is
// found. It reuses the plan summary pattern the drift check reads, taking only the resources a plan
// would destroy so the plan-content gate weighs destruction rather than total change.
func parsePlanDestroys(out string) int {
	m := planSummary.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[3])
	return n
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
			return d.proposeApply(ctx, r, policies, parsePlanDestroys(plan.String()), mask)
		})
}

// proposeApply builds the apply proposed from a completed plan run and finalizes the plan run as
// succeeded. The proposal is a clone of r run for real, held for approval when destroys exceeds a
// scoping policy's threshold and queued to run otherwise, and it carries r's id so it never re-gates.
// A synthesized log line records the decision beneath the plan output. A failure to create the
// proposal fails the plan run so the missing apply is visible rather than silently dropped.
func (d *Dispatcher) proposeApply(
	ctx context.Context, r *run.Run, policies []*policy.Policy, destroys int, mask *masker,
) run.Status {
	opts := []run.SubmitOption{
		run.WithTool(r.Tool),
		run.WithCommand(r.Command),
		run.WithDryRun(false),
		run.WithProposedFrom(r.ID),
	}
	if policy.PlanExceeds(policies, r, destroys) {
		opts = append(opts, run.WithRequireApproval(true))
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
	note := fmt.Sprintf("switchtender: plan would destroy %d resource(s); proposed apply %s %s.\n",
		destroys, proposal.ID, disposition)
	if err := d.store.AppendLog(ctx, r.ID, []byte(note)); err != nil {
		d.log.Error("dispatch: plan gate note: "+err.Error(), zap.String("run_id", r.ID))
	}
	code := 0
	d.finalize(r, run.StatusSucceeded, &code, "")
	return run.StatusSucceeded
}
