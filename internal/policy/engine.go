package policy

import (
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/paid"
	"github.com/kordloom/switchtender/internal/run"
)

// Engine evaluates the rules a Pro or Team license adds to the one plain hold: deny, the risk and
// reversibility floors, distinct-approver separation of duties, and how those compose across a rule
// set. The paid module registers it. A Community build has no rule evaluation code, so its engine
// holds every run an advanced rule's free criteria cover, names the tier on the hold, and never
// refuses.
//
// Every method takes the whole rule set, Rego policies included, because the first rule in list
// order is the one named in evidence and that cannot survive splitting the list.
type Engine interface {
	// Denying returns the first rule that refuses r outright, or nil.
	Denying(policies []*Policy, r *run.Run) *Policy
	// Requiring returns the first stored blanket rule that holds r, or nil. The built-in agent hold
	// is asked after it, by the caller.
	Requiring(policies []*Policy, r *run.Run) *Policy
	// RequireDistinct reports whether any rule covering r demands a distinct approver.
	RequireDistinct(policies []*Policy, r *run.Run) bool
	// ReasonRequirement returns the strictest reason requirement among the rules that would hold r.
	ReasonRequirement(policies []*Policy, r *run.Run) string
	// PlanGated reports whether a rule sends r, an unplanned apply, to the plan gate. The caller has
	// already answered the preconditions that do not depend on a rule's criteria.
	PlanGated(policies []*Policy, r *run.Run) bool
	// Exceeding returns the first plan-content rule a plan of this size violates, or nil.
	Exceeding(policies []*Policy, r *run.Run, destroys int) *Policy
	// ExceedingDistinct reports whether any rule the destroy count exceeds demands a distinct
	// approver.
	ExceedingDistinct(policies []*Policy, r *run.Run, destroys int) bool
	// ExceedingReason returns the strictest reason requirement among the plan-content rules a plan of
	// this size violates.
	ExceedingReason(policies []*Policy, r *run.Run, destroys int) string
}

// engineSlot holds the rule engine, the Community stub until the paid module registers one.
var engineSlot = paid.NewSlot[Engine]("rule engine", communityEngine{},
	license.FeaturePolicyRules, license.FeaturePolicyFull)

// RegisterEngine registers the paid rule engine. A second registration panics.
func RegisterEngine(e Engine) { engineSlot.Set(e) }

// CurrentEngine returns the rule engine, the Community stub when none is registered.
func CurrentEngine() Engine { return engineSlot.Get() }

// EngineRegistered reports whether a paid rule engine is registered.
func EngineRegistered() bool { return engineSlot.Registered() }

// communityEngine is the rule engine of a build without the paid module. It fails closed: a rule it
// cannot evaluate holds every run the rule's free criteria cover, its actor and account scope
// ignored, labeled with the tier that would evaluate it, and refuses nothing. A deny it cannot
// evaluate holds, a floor it cannot read applies, and a distinct-approver flag on any rule covering
// the run is enforced. That keeps the promise that a restored backup carries its policies back on
// any tier, without evaluating what Community did not buy.
type communityEngine struct{}

// Denying returns the Rego policy that refuses r or could not decide. No criteria rule refuses: deny
// is a Pro rule, and one this build cannot evaluate holds instead.
func (communityEngine) Denying(policies []*Policy, r *run.Run) *Policy {
	for _, p := range policies {
		if p == nil || p.Rego == nil {
			continue
		}
		if d := p.Rego.Decide(r); d.Err != nil || len(d.Deny) > 0 {
			return p.RegoVerdict(EffectDeny, d.Deny, d.Err)
		}
	}
	return nil
}

// Requiring returns the first rule holding r: a Rego hold, a plain blanket rule, or an advanced
// blanket rule whose free criteria cover r, which is returned labeled with its tier.
func (communityEngine) Requiring(policies []*Policy, r *run.Run) *Policy {
	for _, p := range policies {
		switch {
		case p == nil:
		case p.Rego != nil:
			if d := p.Rego.Decide(r); d.Err != nil || len(d.Hold) > 0 {
				return p.RegoVerdict(EffectRequireApproval, d.Hold, d.Err)
			}
		case p.Exempts() || p.MaxDestroy >= 0 || !p.covers(r):
		case p.Advanced():
			return p.tierVerdict()
		default:
			return p
		}
	}
	return nil
}

// RequireDistinct reports whether any rule covering r on its free criteria carries the flag, or any
// Rego policy demands it or could not decide. Strict, since this build cannot read the floors that
// would narrow the rule.
func (communityEngine) RequireDistinct(policies []*Policy, r *run.Run) bool {
	for _, p := range policies {
		switch {
		case p == nil:
		case p.Rego != nil:
			if d := p.Rego.Decide(r); d.Err != nil || d.RequireDistinctApprover {
				return true
			}
		case p.RequireDistinctApprover && !p.Exempts() && p.covers(r):
			return true
		}
	}
	return false
}

// ReasonRequirement returns the strictest reason requirement among the Rego policies holding r and
// the rules whose free criteria cover it. A deny rule counts, since this build holds what it would
// have refused.
func (communityEngine) ReasonRequirement(policies []*Policy, r *run.Run) string {
	out := ""
	for _, p := range policies {
		switch {
		case p == nil || p.RequireReason == "":
		case p.Rego != nil:
			if d := p.Rego.Decide(r); d.Err != nil || len(d.Hold) > 0 {
				out = decision.Stricter(out, p.RequireReason)
			}
		case !p.Exempts() && p.covers(r):
			out = decision.Stricter(out, p.RequireReason)
		}
	}
	return out
}

// PlanGated plans r first when a Rego policy says so or could not decide, when a plan-content rule
// covers it, or when an advanced rule covers it on its free criteria: a floor this build cannot
// read plans first, and Requiring then holds the apply the plan proposes.
func (communityEngine) PlanGated(policies []*Policy, r *run.Run) bool {
	for _, p := range policies {
		switch {
		case p == nil:
		case p.Rego != nil:
			if d := p.Rego.Decide(r); d.Err != nil || d.PlanGate {
				return true
			}
		case p.Exempts() || !p.covers(r):
		case p.MaxDestroy >= 0 || p.Advanced():
			return true
		}
	}
	return false
}

// Exceeding returns the first plan-content rule covering r on its free criteria whose threshold the
// plan exceeds, labeled with its tier when it is advanced.
func (communityEngine) Exceeding(policies []*Policy, r *run.Run, destroys int) *Policy {
	exceeded := exceededPlain(policies, r, destroys)
	if len(exceeded) == 0 {
		return nil
	}
	if p := exceeded[0]; p.Advanced() {
		return p.tierVerdict()
	}
	return exceeded[0]
}

// ExceedingDistinct reports whether any exceeded plan-content rule covering r carries the flag.
func (communityEngine) ExceedingDistinct(policies []*Policy, r *run.Run, destroys int) bool {
	for _, p := range exceededPlain(policies, r, destroys) {
		if p.RequireDistinctApprover {
			return true
		}
	}
	return false
}

// ExceedingReason returns the strictest reason requirement among the exceeded plan-content rules
// covering r.
func (communityEngine) ExceedingReason(policies []*Policy, r *run.Run, destroys int) string {
	out := ""
	for _, p := range exceededPlain(policies, r, destroys) {
		out = decision.Stricter(out, p.RequireReason)
	}
	return out
}

// exceededPlain returns, in list order, the plan-content rules whose free criteria cover r and
// whose threshold a plan of this size exceeds.
func exceededPlain(policies []*Policy, r *run.Run, destroys int) []*Policy {
	var out []*Policy
	for _, p := range policies {
		if p != nil && p.Rego == nil && p.MaxDestroy >= 0 && destroys > p.MaxDestroy && p.covers(r) {
			out = append(out, p)
		}
	}
	return out
}

// tierVerdict returns a copy of an advanced rule standing for the hold a Community build places in
// its stead: an approval rule named for the rule, the tier that evaluates it, and what happened.
func (p *Policy) tierVerdict() *Policy {
	f := license.FeaturePolicyRules
	if p.TeamRules() {
		f = license.FeaturePolicyFull
	}
	scope := ""
	if p.scoped() {
		scope = "; its actor scope is not applied on this build"
	}
	cp := *p
	cp.Effect = EffectRequireApproval
	cp.Name = p.Label() + " (held because this build cannot evaluate it: " +
		license.Unlicensed(f).Error() + scope + ")"
	return &cp
}

// covers reports whether a rule covers r on this build. A plain rule or an exemption matches on
// every criterion it has. An advanced rule matches on its free criteria alone, since this build
// cannot apply its scope.
func (p *Policy) covers(r *run.Run) bool {
	if p.Advanced() {
		return p.coversUnscoped(r)
	}
	return p.MatchesPlain(r)
}

// coversUnscoped reports whether an advanced rule's free criteria cover r, with its actor, actor
// kind, and account criteria ignored. A build without the rule engine cannot apply a rule's scope,
// so the rule applies to every run its free criteria cover, which holds more than the rule asked
// and never less.
func (p *Policy) coversUnscoped(r *run.Run) bool {
	cp := *p
	cp.Actor, cp.ActorKind, cp.Account = "", "", ""
	return cp.MatchesPlain(r)
}

// scoped reports whether a rule names an actor, an actor kind, or an account.
func (p *Policy) scoped() bool {
	return p.Actor != "" || p.ActorKind != "" || p.Account != ""
}
