package dispatch

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// WithPolicies enforces approval policies on submitted runs: a run matching any stored policy is
// held for approval instead of dispatched, so the gate cannot be skipped by omitting the flag.
func WithPolicies(store policy.Store) Option {
	return func(c *config) { c.policies = store }
}

// holdRequested is the reason recorded for a run held because its submission asked for approval,
// rather than because a stored rule matched it. The register has to distinguish the two.
const holdRequested = "requested at submission"

// requiresApproval decides whether any stored policy holds r for approval, writes the decision onto
// r, and returns it. It refuses the submission when it cannot tell.
//
// A lookup failure used to be logged and treated as no match, on the reasoning that a policy store
// failure implied a broader store failure the run's own save would surface. That held while policies
// were rows in the same database as runs. It stopped holding the moment policies could come from a
// file: a deleted file, a half-written deploy, or one typo in a merged change makes every gate
// disappear while runs save perfectly well, and the only signal is one log line per submission.
//
// A gate that cannot be evaluated is not a gate that passed. The submission fails instead, loudly,
// which is disruptive exactly once and in the direction that does not execute something an approver
// was meant to see.
func (d *Dispatcher) requiresApproval(ctx context.Context, r *run.Run) (gateVerdict, error) {
	policies, err := d.listPolicies(ctx)
	if err != nil {
		return gateVerdict{}, fmt.Errorf("%w: approval policies could not be read, so the run is "+
			"refused rather than run past a gate that could not be checked: %w",
			ErrPolicyUnavailable, err)
	}
	v, rec := d.decideHold(policies, r)
	v.apply(r, rec)
	return v, nil
}

// gateVerdict is what the rules decided about a submission, as the values the run stores. It is one
// comparable value so that a held run's regrade at its pinned commit is compared with the gate's
// decision whole: a field added here later is written by apply and compared by the regrade without
// anyone listing it a second time.
type gateVerdict struct {
	// Status is the status the run carried into the gate, which the plan gate's rule reads.
	Status run.Status
	// PolicySet is the digest of the rule set the decision was made under.
	PolicySet string
	// Denying names the deny rule that matches the run or any step of its workflow, empty when none
	// does.
	Denying string
	// Held reports whether a rule holds the run for approval.
	Held bool
	// HeldByPolicy is what the run records as having held it.
	HeldByPolicy string
	// RequireDistinctApprover reports whether the approval must come from somebody other than the
	// requester.
	RequireDistinctApprover bool
	// RequireReason is the reason requirement a decision on the run must meet.
	RequireReason string
}

// gateRecord is what the gate's grade wrote on a run for its approver to read. A held run's regrade
// replaces it rather than comparing it, since its text names the commit it read.
type gateRecord struct {
	// DryRunScans is what the gate's scans read of the run's dry runs.
	DryRunScans []run.DryRunScan
	// PolicyNotes is what the rules noted about the run without holding it.
	PolicyNotes []string
	// HoldNote says why the rule that held the run did so, when the rule wrote one.
	HoldNote string
}

// apply writes the verdict v and the record rec onto r.
func (v gateVerdict) apply(r *run.Run, rec gateRecord) {
	if v.Held {
		r.Status = run.StatusPendingApproval
	}
	r.HeldByPolicy = v.HeldByPolicy
	r.RequireDistinctApprover = v.RequireDistinctApprover
	r.RequireReason = v.RequireReason
	r.DryRunScans = rec.DryRunScans
	r.PolicyNotes = rec.PolicyNotes
	r.HoldNote = rec.HoldNote
}

// settle completes the verdict v with what every held run records: a run that arrived held, because
// its caller asked for approval or because it is a child of a held parent, never consulted a rule
// for its hold, and it is recorded as held at submission. Such runs used to store an empty rule,
// which the register renders as "nothing held it" beside an outcome showing the change waited for
// an approver: the exact inverse of what happened.
func (v *gateVerdict) settle() {
	if (v.Held || v.Status == run.StatusPendingApproval) && v.HeldByPolicy == "" {
		v.HeldByPolicy = holdRequested
	}
}

// decideHold grades r under policies and returns what the gate decides about it and what it records
// on it. The gate and a held run's regrade at its pinned commit both decide through it, so the two
// decisions compare as one value.
func (d *Dispatcher) decideHold(policies []*policy.Policy, r *run.Run) (gateVerdict, gateRecord) {
	// The rule that held the run is recorded on it here, while the rule is in hand. Looking it up
	// when the evidence is read would answer with today's policies rather than the one that
	// actually stopped the change, and would answer with nothing at all once it is deleted.
	gr := d.graded(r)
	v := gateVerdict{
		Status: r.Status, PolicySet: policy.InForce(policies).Digest, HeldByPolicy: r.HeldByPolicy,
		RequireDistinctApprover: r.RequireDistinctApprover, RequireReason: r.RequireReason,
	}
	// What the gate's scan read is recorded on the run itself, held or not, so the evidence says
	// what a dry run was found to run, and why it waited or went through. So is what a policy noted
	// rather than held on, for the same reason: a run nothing holds is the run whose record has to
	// say what it was warned about.
	rec := gateRecord{DryRunScans: gr.DryRunScans, PolicyNotes: policyNotes(policies, gr),
		HoldNote: r.HoldNote}
	if p := policy.Denying(policies, gr); p != nil {
		v.Denying = maskPolicyText(gr, p.Label())
	}
	// A terraform or opentofu apply a rule holds is planned first, and the request is not held: the
	// apply its plan proposes is, carrying the saved plan, after the rules decide on it. A request
	// held here would be approved without its plan and then plan when it ran.
	//
	// An agent's apply is the exception. Planning it runs provider code and data sources with this
	// server's credentials before anybody approved anything, so it is held here, before anything
	// plans. Its release is a release to plan: the plan gate plans it then, and the apply its plan
	// proposes is held again carrying the saved plan.
	if p := policy.Requiring(policies, gr); p != nil && (policy.AgentPlansFirst(policies, gr) ||
		r.Status == run.StatusPendingApproval || !policy.PlanGated(policies, gr)) {
		v.Held = true
		v.HeldByPolicy = maskPolicyText(gr, p.Label())
		rec.HoldNote = agentHoldNote(policies, gr, exemptionHoldNote(policies, gr, p))
		if policy.AgentPlansFirst(policies, gr) {
			rec.HoldNote = withUnreadModules(rec.HoldNote, d.unreadModules(r))
		}
		// The label names the first rule for the evidence; the flag is the OR of every matching
		// rule, and an explicitly requested distinct approver is never lowered by a policy.
		v.RequireDistinctApprover = v.RequireDistinctApprover || policy.RequireDistinct(policies, gr)
		// The reason requirement composes the same way: the strictest of every rule covering the
		// run, copied now so a rule edited while the run waits cannot loosen it.
		v.RequireReason = decision.Stricter(v.RequireReason, policy.ReasonRequirement(policies, gr))
	}
	v.settle()
	return v, rec
}

// agentHoldNote returns the hold note for the graded run gr: why the built-in hold keeps it waiting
// when the hold covers it and what the agent asked for runs code with this server's credentials,
// and note, the note any other rule wrote, otherwise. What the gate's scans read of a dry run is
// recorded on the run either way, and decides nothing for an agent.
func agentHoldNote(policies []*policy.Policy, gr *run.Run, note string) string {
	if !policy.AgentHolds(policies, gr) {
		return note
	}
	if why := policy.AgentHoldReason(gr); why != "" {
		return maskPolicyText(gr, why)
	}
	return note
}

// unreadModules returns the registry and remote modules of the Terraform or OpenTofu configuration
// r would plan that the gate cannot read, by the scan that downloads nothing. It reads the project
// at the commit the run is pinned to, or the working directory on this server.
func (d *Dispatcher) unreadModules(r *run.Run) []string {
	_, needs := scanConfigurationNeeds(r, d.checkoutReader())
	return needs.Modules
}

// withUnreadModules appends to an agent's hold note each module the gate did not read.
//
// An agent's apply is held before anything plans, and its release is a release to plan, so the
// approver decides before the run's own init fetches its registry and remote modules. Nothing in
// the note said those modules existed, so an approver read a configuration of local modules as the
// whole of what would run. The note is unchanged when every module was read.
func withUnreadModules(note string, modules []string) string {
	if len(modules) == 0 {
		return note
	}
	list := "The gate did not read these modules: " + strings.Join(modules, ", ") +
		". The run's own init resolves these when it starts."
	if note == "" {
		return list
	}
	return note + " " + list
}

// listPolicies returns the stored policies, or none when the dispatcher has no policy store. An
// install with no store still has the built-in agent hold, so a caller asks the rules either way
// rather than reading a missing store as a pass.
func (d *Dispatcher) listPolicies(ctx context.Context) ([]*policy.Policy, error) {
	if d.policies == nil {
		return nil, nil
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return nil, err
	}
	return policies, nil
}

// policyNotes returns what the rules record about the graded run g without holding it: the
// warnings Rego policies set to warn: note, and what the built-in agent hold did, with g's own
// secrets masked. The agent note is recorded whether the hold applied or an exemption lifted it,
// so the outcome a receipt discloses says which, and names the exemption.
func policyNotes(policies []*policy.Policy, g *run.Run) []string {
	notes := policy.Noting(policies, g)
	if note := policy.AgentNote(policies, g); note != "" {
		notes = append(notes, note)
	}
	for i, note := range notes {
		notes[i] = maskPolicyText(g, note)
	}
	return notes
}

// maskPolicyText hides r's own secrets in text a policy wrote about r: the values its variables and
// script carry, and any secret-looking assignment in the text itself.
//
// A Rego module builds its messages from the input document, and the document carries a script's
// text, so a message can quote a token written inline in a command. The text it builds is written
// onto the run as a hold or a note, the dossier handed to an auditor shows both, and a note is
// committed with the run's outcome and disclosed by every receipt. This is the reading
// RedactRunText gives text leaving the server, short of the stored credentials, which a policy
// never sees.
func maskPolicyText(r *run.Run, text string) string {
	if r == nil || text == "" {
		return text
	}
	m := &masker{}
	m.set(runOwnSecrets(r.ExtraVars, r.Command))
	masked, _ := util.RedactAssignments(m.redactString(text), MaskToken)
	return masked
}

// planFirstOnRequest turns a Terraform or OpenTofu apply whose own submission asked for approval
// from a held request into one that plans first, and records the ask on it. Held as it was, the
// request would be approved and then plan when it ran, so the approver would never see the plan
// that applied. Planned first, the apply its plan proposes is held instead, carrying the saved
// plan, and the approval binds the plan that applies. Any other run is left as it was submitted.
func planFirstOnRequest(r *run.Run) {
	tool := run.NormalizeTool(r.Tool)
	if tool != run.ToolTerraform && tool != run.ToolOpenTofu {
		return
	}
	if r.Status != run.StatusPendingApproval || r.DryRun || r.ProposedFrom != "" ||
		r.ParentID != nil || r.PlanSHA256 != "" {
		return
	}
	r.Status, r.ApprovalRequested = run.StatusPending, true
}

// denied refuses r's submission when a deny policy matches it, naming the rule. It fails closed
// exactly as requiresApproval does: a gate that cannot be evaluated is not a gate that passed.
func (d *Dispatcher) denied(ctx context.Context, r *run.Run) error {
	if d.policies == nil {
		// An install with no policy store has no rules, and the run says so: recording nothing would
		// leave a run under no rules indistinguishable from one whose rules were never captured.
		stampPolicySet(r, nil)
		return nil
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return fmt.Errorf("%w: approval policies could not be read, so the run is refused "+
			"rather than run past a gate that could not be checked: %w", ErrPolicyUnavailable, err)
	}
	// The list just read is the set in force for this submission, so it is recorded here rather than
	// read again. Every submit path passes through this check, including a run born held, which is what
	// makes the record complete rather than a property of the gated ones.
	stampPolicySet(r, policies)
	// The first rule check of every submission path, so the fetch happens once, here, and the hold
	// check that follows reads the same commit.
	if err := d.refreshForGate(policies, r); err != nil {
		return err
	}
	gr := d.graded(r)
	if p := policy.Denying(policies, gr); p != nil {
		d.recordRefusal(ctx, r, policies, p)
		// A Rego verdict is labeled with the messages its module built, which can quote the run's
		// command, so the refusal the caller reads is masked like every other text a policy writes.
		return fmt.Errorf("%w: policy %q refuses this submission", ErrPolicyDenied,
			maskPolicyText(gr, p.Label()))
	}
	return nil
}

// recordRefusal commits a deny policy's refusal of r to the chain, naming the rule and the bundle
// that decided. The submission is refused whether or not the entry lands, since refusing is the
// direction a gate fails in, and a chain that cannot record it is logged.
func (d *Dispatcher) recordRefusal(ctx context.Context, r *run.Run, policies []*policy.Policy,
	decided *policy.Policy) {
	if d.audits == nil {
		return
	}
	rule, bundle := decided.Label(), ""
	if decided.Rego != nil {
		bundle = decided.Rego.Digest()
		// A Rego verdict is a copy labeled with its reasons, so the rule is named by the policy the
		// verdict came from.
		for _, p := range policies {
			if p.Rego == decided.Rego {
				rule = p.Label()
			}
		}
	}
	if err := outcome.CommitRefusal(ctx, d.audits, r, rule, bundle, d.now); err != nil {
		d.log.Error("dispatch: record a policy refusal: "+err.Error(), zap.String("run_id", r.ID))
	}
}

// stampPolicySet records the rule set in force on the run.
func stampPolicySet(r *run.Run, policies []*policy.Policy) {
	set := policy.InForce(policies)
	r.PolicySet = &run.PolicySet{Digest: set.Digest, Count: set.Count, Rules: set.Rules}
}

// pipelineDenied refuses a pipeline when a deny policy matches the parent or any of its steps, for
// the same reason pipelineRequiresApproval holds one: wrapping a refused command in a one-step
// workflow must not launder it past the rule.
func (d *Dispatcher) pipelineDenied(ctx context.Context, parent *run.Run, steps []run.PipelineStep) error {
	if d.policies == nil {
		// Recorded for the reason denied records it, and so that a held workflow's regrade has the
		// set the gate decided under to compare against.
		stampPolicySet(parent, nil)
		return nil
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return fmt.Errorf("%w: approval policies could not be read, so the pipeline is refused "+
			"rather than run past a gate that could not be checked: %w", ErrPolicyUnavailable, err)
	}
	stampPolicySet(parent, policies)
	units, names := policyUnits(parent, steps)
	// The pipeline's first rule check, as denied is a single run's, so the steps are graded on the
	// commit fetched here.
	if err := d.refreshForGate(policies, append([]*run.Run{parent}, units...)...); err != nil {
		return err
	}
	gp := d.graded(parent)
	if p := policy.Denying(policies, gp); p != nil {
		d.recordRefusal(ctx, parent, policies, p)
		return fmt.Errorf("%w: policy %q refuses this submission", ErrPolicyDenied,
			maskPolicyText(gp, p.Label()))
	}
	for i, unit := range units {
		gs := d.graded(unit)
		if p := policy.Denying(policies, gs); p != nil {
			d.recordRefusal(ctx, parent, policies, p)
			return fmt.Errorf("%w: policy %q refuses step %q", ErrPolicyDenied,
				maskPolicyText(gs, p.Label()), names[i])
		}
	}
	return nil
}

// refuseAgentWorkflowApply refuses an agent's workflow that carries a Terraform or OpenTofu apply
// step no exemption covers, recording the refusal on the chain the way a deny rule's is. A
// workflow's approval binds its steps as written, and an apply step plans and applies when it
// runs, so the approver never sees the plan the step applies. An agent's apply is held before it
// plans and again carrying the saved plan, and a workflow wrapping the apply would trade those two
// approvals for one that binds no plan. Each step is judged as the run it would become, carrying
// the agent's identity and account, which is also what tells an agent's workflow from a person's.
// It fails closed as every other check here does: a gate that cannot be evaluated has not been
// passed.
func (d *Dispatcher) refuseAgentWorkflowApply(ctx context.Context, parent *run.Run,
	steps []run.PipelineStep) error {
	policies, err := d.listPolicies(ctx)
	if err != nil {
		return fmt.Errorf("%w: approval policies could not be read, so the pipeline is refused "+
			"rather than run past a gate that could not be checked: %w", ErrPolicyUnavailable, err)
	}
	units, names := policyUnits(parent, steps)
	for i, unit := range units {
		if !policy.AgentWorkflowApplies(policies, unit) {
			continue
		}
		d.recordRefusal(ctx, parent, policies, policy.AgentWorkflowApply())
		return fmt.Errorf("%w: %w: step %q applies %s for an agent, and a workflow's approval "+
			"does not show the plan a step applies. Ask for the apply as its own run, which plans "+
			"first and waits for approval of the saved plan, or write a policy with effect exempt "+
			"that covers the step", ErrPolicyDenied, ErrAgentWorkflowApply, names[i],
			toolName(unit.Tool))
	}
	return nil
}

// toolName names an infrastructure tool the way a refusal says it.
func toolName(tool string) string {
	if run.NormalizeTool(tool) == run.ToolOpenTofu {
		return "OpenTofu"
	}
	return "Terraform"
}

// policyUnits builds the run each executable step of a pipeline would execute as, with its name,
// for the rules to grade. An approval step executes nothing, so it is not a unit: graded as a run
// it reads as an Ansible run of no playbook, which a blanket rule on Ansible would hold or refuse,
// and a workflow would be stopped by the very step that exists to stop it.
func policyUnits(parent *run.Run, steps []run.PipelineStep) ([]*run.Run, []string) {
	units := make([]*run.Run, 0, len(steps))
	names := make([]string, 0, len(steps))
	for i, step := range steps {
		if step.IsApproval() {
			continue
		}
		units = append(units, stepRun(parent, step, i, 0, baseStepVars(parent)))
		names = append(names, step.Name)
	}
	return units, names
}

// pipelineRequiresApproval decides whether a pipeline must be held, writes the decision onto the
// parent, and returns it. A pipeline is submitted through a different path than a single run, so
// without this the same command an operator gated would execute freely by being wrapped in a
// one-step workflow.
func (d *Dispatcher) pipelineRequiresApproval(ctx context.Context, parent *run.Run,
	steps []run.PipelineStep) (gateVerdict, error) {
	policies, err := d.listPolicies(ctx)
	if err != nil {
		return gateVerdict{}, fmt.Errorf("%w: approval policies could not be read, so the pipeline "+
			"is refused rather than run past a gate that could not be checked: %w",
			ErrPolicyUnavailable, err)
	}
	v, rec := d.decidePipeline(policies, parent, steps)
	v.apply(parent, rec)
	return v, nil
}

// decidePipeline grades a pipeline under policies and returns what the gate decides about it and
// what it records on the parent. The gate and a held workflow's regrade at its pinned commit both
// decide through it, so the two decisions compare as one value.
//
// A pipeline must be held when the parent itself matches a blanket policy or when any of its steps
// does. The whole pipeline is held rather than the matching step, because the graph walk cannot
// park a step midway and a partly applied change is worse than one that never started.
func (d *Dispatcher) decidePipeline(policies []*policy.Policy, parent *run.Run,
	steps []run.PipelineStep) (gateVerdict, gateRecord) {
	gp := d.graded(parent)
	v := gateVerdict{
		Status: parent.Status, PolicySet: policy.InForce(policies).Digest,
		HeldByPolicy: parent.HeldByPolicy, RequireDistinctApprover: parent.RequireDistinctApprover,
		RequireReason: parent.RequireReason,
	}
	rec := gateRecord{DryRunScans: gp.DryRunScans, PolicyNotes: policyNotes(policies, gp),
		HoldNote: parent.HoldNote}
	if p := policy.Denying(policies, gp); p != nil {
		v.Denying = maskPolicyText(gp, p.Label())
	}
	if p := policy.Requiring(policies, gp); p != nil {
		v.Held = true
		v.HeldByPolicy = maskPolicyText(gp, p.Label())
		rec.HoldNote = exemptionHoldNote(policies, gp, p)
	}
	units := []*run.Run{gp}
	stepUnits, unitNames := policyUnits(parent, steps)
	for i, unit := range stepUnits {
		gs := d.graded(unit)
		units = append(units, gs)
		if p := policy.Denying(policies, gs); p != nil && v.Denying == "" {
			v.Denying = fmt.Sprintf("%s in step %q", maskPolicyText(gs, p.Label()), unitNames[i])
		}
		// What the gate's scan of each step read is recorded on the pipeline, which is what an
		// approver decides on, named by the step it belongs to.
		named := stepNamed(gs, unitNames[i])
		rec.DryRunScans = append(rec.DryRunScans, named.DryRunScans...)
		// So is what a policy noted about a step, under the step's prefix, so each step run takes
		// back its own share.
		for _, note := range policyNotes(policies, gs) {
			rec.PolicyNotes = append(rec.PolicyNotes, run.StepPrefix(unitNames[i])+note)
		}
		// A pipeline held because one of its steps matches records that rule too: the whole graph
		// is held, so the evidence has to say which step's rule stopped it.
		if !v.Held {
			if p := policy.Requiring(policies, gs); p != nil {
				v.Held = true
				v.HeldByPolicy = maskPolicyText(gs, p.Label())
				rec.HoldNote = exemptionHoldNote(policies, named, p)
			}
		}
	}
	// Approving the parent releases every step, so the separation-of-duties answer must be the
	// strictest across the parent and all steps, not the answer of whichever unit happened to match
	// first. Computed from the first match, a pipeline held by a plain rule on the parent never asked
	// its steps, and a step whose own rule demanded a second person released on the requester's say
	// so. The reason requirement composes the same way.
	//
	// A workflow's approval steps take the same answer, from the rules in force at submission whether
	// or not they hold the workflow. A rule that demands a second person or a reason for a step's
	// change demands it for the approval that releases that step, and a workflow nothing held would
	// otherwise reach its approval step with no separation at all.
	if v.Held || run.HasApproval(steps) {
		for _, gu := range units {
			if policy.RequireDistinct(policies, gu) {
				v.RequireDistinctApprover = true
				break
			}
		}
		for _, gu := range units {
			v.RequireReason = decision.Stricter(v.RequireReason, policy.ReasonRequirement(policies, gu))
		}
	}
	v.settle()
	return v, rec
}

// stepNamed returns a copy of the graded step gs whose scans name the step, the way the pipeline
// records them, so a hold note about the step says which step it is.
func stepNamed(gs *run.Run, name string) *run.Run {
	named := *gs
	named.DryRunScans = run.ScansForStep(name, gs.DryRunScans)
	return &named
}
