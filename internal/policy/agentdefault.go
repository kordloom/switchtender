package policy

import (
	"fmt"

	"github.com/kordloom/switchtender/internal/run"
)

// The built-in agent hold. It is the rule every install starts with: a run an AI agent requested
// waits for a person unless a stored exemption says that run may go ahead. It is not a stored
// policy, so it cannot be deleted, it is not in the policy list, and it counts against no license's
// policy limit.
const (
	// AgentDefaultID identifies the built-in agent hold wherever a rule is named by id.
	AgentDefaultID = "builtin_agent_hold"
	// AgentDefaultName is how the built-in agent hold is named in evidence: on the held run, in the
	// approval view, and in the run's receipt and dossier.
	AgentDefaultName = "requested by an agent, held by default"
)

// AgentDefault returns the built-in agent hold as a rule, so a hold it places is recorded the way a
// stored rule's is. It sets no criteria and no requirements: AgentHolds decides when it applies.
func AgentDefault() *Policy {
	return &Policy{ID: AgentDefaultID, Name: AgentDefaultName, MaxDestroy: DisabledMaxDestroy}
}

// IsAgentDefault reports whether p is the built-in agent hold.
func IsAgentDefault(p *Policy) bool {
	return p != nil && p.ID == AgentDefaultID && p.Rego == nil
}

// AgentRequested reports whether an AI agent asked for r. That is true for a run its token
// submitted, and for a run that carries the identity of the agent whose request it derives from,
// such as the apply an agent's gated plan proposes or a shard of an agent's split. Who asked is
// read from the minted token's kind, never guessed from how the request looked.
func AgentRequested(r *run.Run) bool {
	return r != nil && (r.ActorType == ActorKindAgent || r.Initiator != nil)
}

// Exempting returns the first exemption covering r, or nil when none does. An exemption matches on
// the criteria a Community rule may set, and on the named agent when it names one.
func Exempting(policies []*Policy, r *run.Run) *Policy {
	for _, p := range policies {
		if p != nil && p.Rego == nil && p.Exempts() && p.matchesBeforeGrade(r) {
			return p
		}
	}
	return nil
}

// AgentHolds reports whether the built-in agent hold holds r: an agent requested it, it is not a
// dry run the gate's scans prove changes nothing, and no exemption covers it.
//
// The dry-run reading is the one exclude_dry_run uses, so the two cannot disagree about what a
// preview is. A dry run whose playbook forces real work under check mode, or whose Terraform or
// OpenTofu configuration runs a program while it plans, is a change, and so is one the gate could
// not read in full.
func AgentHolds(policies []*Policy, r *run.Run) bool {
	return AgentRequested(r) && !r.ChangeFree() && Exempting(policies, r) == nil
}

// AgentNote returns what the evidence records about the built-in agent hold for r: that it held r,
// or which exemption let r go ahead without it. It is empty for a run no agent requested and for a
// dry run shown to change nothing, which the hold never covers.
func AgentNote(policies []*Policy, r *run.Run) string {
	if !AgentRequested(r) || r.ChangeFree() {
		return ""
	}
	if p := Exempting(policies, r); p != nil {
		return fmt.Sprintf("requested by an agent, exempt from the default hold by policy %q",
			p.Label())
	}
	return AgentDefaultName
}

// validateExemption checks an exemption's shape. An exemption says which agent runs may go ahead
// without the built-in hold, so it takes the criteria that pick runs out and nothing that only
// means something on a rule that holds: a risk or reversibility floor would exempt the riskiest runs
// and leave the routine ones held, and an approver requirement has no decision to apply to.
func (p *Policy) validateExemption() error {
	if p.ActorKind != "" && p.ActorKind != ActorKindAgent {
		return fmt.Errorf("an exemption lifts the hold on agent runs, so actor_kind must be empty "+
			"or %q, not %q", ActorKindAgent, p.ActorKind)
	}
	if p.MinRisk != "" || p.Reversibility != "" || p.ExcludeDryRun ||
		p.RequireDistinctApprover || p.RequireReason != "" || p.MaxDestroy >= 0 {
		return fmt.Errorf("an exemption takes only tool, command_contains, inventory_id, queue, " +
			"and actor: min_risk, reversibility, exclude_dry_run, require_distinct_approver, " +
			"require_reason, and max_destroy belong on a rule that holds")
	}
	return nil
}
