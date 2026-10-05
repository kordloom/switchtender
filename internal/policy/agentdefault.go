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
// the criteria a Community rule may set, and on the named agent and its account when it names them.
//
// An exemption naming an agent's label without its account is refused where it is written, and
// skipped here as well, so one that reached a store some other way still exempts nothing: a label
// repeats across accounts, and an exemption keyed on it alone fails open.
func Exempting(policies []*Policy, r *run.Run) *Policy {
	for _, p := range policies {
		if p == nil || p.Rego != nil || !p.Exempts() || (p.Actor != "" && p.Account == "") {
			continue
		}
		if p.matchesBeforeGrade(r) {
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
// or which exemption let r go ahead without it, naming the account the agent is bound to. It is
// empty for a run no agent requested and for a dry run shown to change nothing, which the hold
// never covers.
func AgentNote(policies []*Policy, r *run.Run) string {
	if !AgentRequested(r) || r.ChangeFree() {
		return ""
	}
	p := Exempting(policies, r)
	if p == nil {
		return AgentDefaultName
	}
	if account, known := accountOf(r); known && account != "" {
		return fmt.Sprintf("requested by an agent bound to account %q, exempt from the default "+
			"hold by policy %q", account, p.Label())
	}
	return fmt.Sprintf("requested by an agent, exempt from the default hold by policy %q",
		p.Label())
}

// validateExemption checks an exemption's shape. An exemption says which agent runs may go ahead
// without the built-in hold, so it takes the criteria that pick runs out and nothing that only
// means something on a rule that holds: a risk or reversibility floor would exempt the riskiest
// runs and leave the routine ones held, and an approver requirement has no decision to apply to.
func (p *Policy) validateExemption() error {
	// A token's label is chosen by whoever mints it and is unique only within one account. Keyed on
	// the label alone, an exemption also covered a token minted for any other account under the
	// same name, so two teams that each called an agent ci-agent exempted each other, and an admin
	// could hand the exemption to any account by minting a token with the right label. A hold or a
	// deny matching too much fails safe. An exemption matching too much fails open, so it names the
	// account as well.
	if p.Actor != "" && p.Account == "" {
		return fmt.Errorf("%w: actor %q is a token label, which is not unique across accounts, "+
			"so the label alone would also exempt a token minted for another account under the "+
			"same name. Name the account the agent's token is bound to as well", ErrExemptionAccount,
			p.Actor)
	}
	if p.ActorKind != "" && p.ActorKind != ActorKindAgent {
		return fmt.Errorf("an exemption lifts the hold on agent runs, so actor_kind must be empty "+
			"or %q, not %q", ActorKindAgent, p.ActorKind)
	}
	if p.MinRisk != "" || p.Reversibility != "" || p.ExcludeDryRun ||
		p.RequireDistinctApprover || p.RequireReason != "" || p.MaxDestroy >= 0 {
		return fmt.Errorf("an exemption takes only tool, command_contains, inventory_id, queue, " +
			"account, and actor: min_risk, reversibility, exclude_dry_run, " +
			"require_distinct_approver, require_reason, and max_destroy belong on a rule that holds")
	}
	return nil
}
