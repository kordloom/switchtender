package dispatch

import (
	"context"
	"fmt"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// WithPolicies enforces approval policies on submitted runs: a run matching any stored policy is
// held for approval instead of dispatched, so the gate cannot be skipped by omitting the flag.
func WithPolicies(store policy.Store) Option {
	return func(c *config) { c.policies = store }
}

// holdRequested is the reason recorded for a run held because its submission asked for approval,
// rather than because a stored rule matched it. The register has to distinguish the two.
const holdRequested = "requested at submission"

// requiresApproval reports whether any stored policy requires approval for r, and refuses the
// submission when it cannot tell.
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
func (d *Dispatcher) requiresApproval(ctx context.Context, r *run.Run) (bool, error) {
	if d.policies == nil {
		return false, nil
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return false, fmt.Errorf("%w: approval policies could not be read, so the run is refused "+
			"rather than run past a gate that could not be checked: %w", ErrPolicyUnavailable, err)
	}
	// The rule that held the run is recorded on it here, while the rule is in hand. Looking it up
	// when the evidence is read would answer with today's policies rather than the one that
	// actually stopped the change, and would answer with nothing at all once it is deleted.
	if p := policy.Requiring(policies, r); p != nil {
		r.HeldByPolicy = p.Label()
		r.RequireDistinctApprover = p.RequireDistinctApprover
		return true, nil
	}
	return false, nil
}

// recordHold makes sure a held run says what held it.
//
// A run can arrive already held, because the caller asked for approval at submission or because it
// is a child of a held parent, and those paths never consult a policy. They used to store an empty
// rule, which the register renders as "nothing held it" beside an outcome showing the change waited
// for an approver: the exact inverse of what happened. Every held run now names its reason.
func recordHold(r *run.Run, reason string) {
	if r.Status == run.StatusPendingApproval && r.HeldByPolicy == "" {
		r.HeldByPolicy = reason
	}
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
	if p := policy.Denying(policies, r); p != nil {
		return fmt.Errorf("%w: policy %q refuses this submission", ErrPolicyDenied, p.Label())
	}
	return nil
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
		return nil
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return fmt.Errorf("%w: approval policies could not be read, so the pipeline is refused "+
			"rather than run past a gate that could not be checked: %w", ErrPolicyUnavailable, err)
	}
	if p := policy.Denying(policies, parent); p != nil {
		return fmt.Errorf("%w: policy %q refuses this submission", ErrPolicyDenied, p.Label())
	}
	for i, step := range steps {
		if p := policy.Denying(policies, stepRun(parent, step, i, 0, baseStepVars(parent))); p != nil {
			return fmt.Errorf("%w: policy %q refuses step %q", ErrPolicyDenied, p.Label(), step.Name)
		}
	}
	return nil
}

// pipelineRequiresApproval reports whether a pipeline must be held, which it must when the parent
// itself matches a blanket policy or when any of its steps does. A pipeline is submitted through a
// different path than a single run, so without this the same command an operator gated would execute
// freely by being wrapped in a one-step workflow. The whole pipeline is held rather than the matching
// step, because the graph walk cannot park a step midway and a partly applied change is worse than
// one that never started.
func (d *Dispatcher) pipelineRequiresApproval(ctx context.Context, parent *run.Run,
	steps []run.PipelineStep) (bool, error) {
	if d.policies == nil {
		return false, nil
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return false, fmt.Errorf("%w: approval policies could not be read, so the pipeline is "+
			"refused rather than run past a gate that could not be checked: %w",
			ErrPolicyUnavailable, err)
	}
	if p := policy.Requiring(policies, parent); p != nil {
		parent.HeldByPolicy = p.Label()
		parent.RequireDistinctApprover = p.RequireDistinctApprover
		return true, nil
	}
	for i, step := range steps {
		// A pipeline held because one of its steps matches records that rule too: the whole graph
		// is held, so the evidence has to say which step's rule stopped it.
		if p := policy.Requiring(policies, stepRun(parent, step, i, 0, baseStepVars(parent))); p != nil {
			parent.HeldByPolicy = p.Label()
			parent.RequireDistinctApprover = p.RequireDistinctApprover
			return true, nil
		}
	}
	return false, nil
}
