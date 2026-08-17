// Package policy decides which runs must be held for approval, turning approval from an opt-in flag
// into an enforced rule. A policy matches runs by tool, command, and target; a matched run is held
// at submission so an operator cannot skip the gate.
package policy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/run"
)

// DisabledMaxDestroy is the MaxDestroy value that turns a policy's plan-content check off. It is the
// safe default, so a policy created without a destroy threshold never holds a run on plan content.
const DisabledMaxDestroy = -1

// Policy effects. An empty effect means EffectRequireApproval, so every policy written before
// effects existed keeps its meaning.
const (
	// EffectRequireApproval holds a matched run for a person's sign-off, the default.
	EffectRequireApproval = "require_approval"
	// EffectDeny refuses a matched submission outright, so the run is never created. The refused
	// request is still on the chain: the gate records it before any handler acts.
	EffectDeny = "deny"
)

// Actor kinds a policy can match. An empty kind matches any actor.
const (
	// ActorKindAgent matches runs an AI agent submitted, identified by its minted token kind,
	// never guessed from how the request looks.
	ActorKindAgent = "agent"
	// ActorKindHuman matches runs a person submitted: a browser session, an owner-held API token,
	// or the command line. A run fired by a webhook or a schedule is neither kind, so it is
	// matched only by a policy that leaves ActorKind empty.
	ActorKindHuman = "human"
)

// humanActorTypes are the authentication types that mean a person asked, in the audit chain's
// vocabulary.
var humanActorTypes = map[string]bool{"session": true, "token": true, "cli": true}

// Policy is a rule that requires approval for the runs it matches. Each criterion is optional; an
// empty criterion matches any value, so a policy with no criteria requires approval for every run.
// A policy is a blanket rule, held at submission, unless it sets a non-negative MaxDestroy, which
// makes it a plan-content rule enforced at execution by the plan gate instead.
type Policy struct {
	// ID is the unique policy identifier.
	ID string `json:"id"`
	// Name labels the policy for humans, for example "prod terraform destroy".
	Name string `json:"name"`
	// Tool matches a run's execution tool: ansible, bash, terraform, opentofu, python, powershell, or go. Empty matches any.
	Tool string `json:"tool,omitempty"`
	// CommandContains matches when a run's command contains this text. Empty matches any.
	CommandContains string `json:"command_contains,omitempty"`
	// InventoryID matches a run targeting this stored inventory. Empty matches any.
	InventoryID string `json:"inventory_id,omitempty"`
	// ActorKind matches who fired the run: agent for an AI agent's token, human for a person.
	// Empty matches any actor. This is what turns a policy into an authorization boundary for a
	// machine principal, distinct from the rules that bind people.
	ActorKind string `json:"actor_kind,omitempty"`
	// Actor matches the exact requesting actor recorded on the run, for a rule scoped to one named
	// principal. Empty matches any.
	Actor string `json:"actor,omitempty"`
	// MinRisk matches only runs whose assessed risk is at least this level: low, medium, or high.
	// Empty matches any risk. It turns the advisory risk grade into an enforceable criterion.
	MinRisk string `json:"min_risk,omitempty"`
	// Effect is what a matched blanket policy does: require_approval holds the run, deny refuses
	// the submission. Empty means require_approval.
	Effect string `json:"effect,omitempty"`
	// ExcludeDryRun leaves dry-run runs unmatched, so a no-change preview does not need approval.
	ExcludeDryRun bool `json:"exclude_dry_run,omitempty"`
	// MaxDestroy holds a matched terraform or opentofu run for approval when its plan would destroy
	// more than this many resources. A negative value disables the plan-content check, the safe
	// default, so a policy without a threshold is a blanket rule rather than one that holds on any
	// destroy.
	MaxDestroy int `json:"max_destroy"`
	// CreatedAt is when the policy was created.
	CreatedAt time.Time `json:"created_at"`
}

// Matches reports whether the policy's criteria match r. Every non-empty criterion must match,
// so a policy narrows the runs it gates rather than widening them.
func (p *Policy) Matches(r *run.Run) bool {
	if p.ExcludeDryRun && r.DryRun {
		return false
	}
	if p.Tool != "" && run.NormalizeTool(p.Tool) != run.NormalizeTool(r.Tool) {
		return false
	}
	if p.CommandContains != "" && !strings.Contains(r.Command, p.CommandContains) {
		return false
	}
	if p.InventoryID != "" && p.InventoryID != r.InventoryID {
		return false
	}
	if !p.matchesActor(r) {
		return false
	}
	if p.MinRisk != "" && riskRank(run.AssessRisk(r).Level) < riskRank(p.MinRisk) {
		return false
	}
	return true
}

// matchesActor reports whether the policy's actor criteria match who fired r. A run whose actor
// type is unknown, such as one fired by a webhook or a schedule, matches neither named kind, so an
// actor-scoped rule never fires on a request it cannot attribute.
func (p *Policy) matchesActor(r *run.Run) bool {
	if p.Actor != "" && p.Actor != r.Actor {
		return false
	}
	switch p.ActorKind {
	case "":
		return true
	case ActorKindAgent:
		return r.ActorType == ActorKindAgent
	case ActorKindHuman:
		return humanActorTypes[r.ActorType]
	default:
		// An unknown kind matches nothing: a typo must not silently widen a rule to every run.
		return false
	}
}

// riskRank orders risk levels so MinRisk can compare them. An unknown level ranks above high, so a
// policy naming a level this build does not know fails toward holding rather than passing.
func riskRank(level string) int {
	switch level {
	case run.RiskLow:
		return 1
	case run.RiskMedium:
		return 2
	case run.RiskHigh:
		return 3
	default:
		return 4
	}
}

// Denies reports whether the policy refuses matched submissions outright.
func (p *Policy) Denies() bool { return p.Effect == EffectDeny }

// Denying returns the first deny policy matching r, or nil when none does, so the rule that
// refused a submission can be named in the refusal and in the evidence.
func Denying(policies []*Policy, r *run.Run) *Policy {
	for _, p := range policies {
		if p.Denies() && p.Matches(r) {
			return p
		}
	}
	return nil
}

// Validate checks the policy's declared vocabulary, so a rule with a typo is refused where it is
// written rather than silently matching nothing, or worse, everything.
func (p *Policy) Validate() error {
	switch p.Effect {
	case "", EffectRequireApproval, EffectDeny:
	default:
		return fmt.Errorf("effect must be %q or %q, not %q", EffectRequireApproval, EffectDeny, p.Effect)
	}
	switch p.ActorKind {
	case "", ActorKindAgent, ActorKindHuman:
	default:
		return fmt.Errorf("actor_kind must be %q or %q, not %q", ActorKindAgent, ActorKindHuman, p.ActorKind)
	}
	switch p.MinRisk {
	case "", run.RiskLow, run.RiskMedium, run.RiskHigh:
	default:
		return fmt.Errorf("min_risk must be %q, %q, or %q, not %q",
			run.RiskLow, run.RiskMedium, run.RiskHigh, p.MinRisk)
	}
	if p.Denies() && p.MaxDestroy >= 0 {
		return fmt.Errorf("a deny policy cannot set max_destroy: a plan-content rule holds an " +
			"apply for review, and a denied run is never planned at all")
	}
	return nil
}

// Requires reports whether any blanket policy requires approval for r at submission. A plan-content
// policy, one with a non-negative MaxDestroy, is not a blanket rule: it is enforced at execution by
// the plan gate, which plans the run and holds the apply only when its plan destroys too much, so it
// is skipped here.
func Requires(policies []*Policy, r *run.Run) bool {
	return Requiring(policies, r) != nil
}

// Requiring returns the first blanket policy requiring approval for r, or nil when none does. The
// rule that held a run is evidence: an auditor asking why a change waited wants the rule named, and
// the answer has to be recorded when the hold happens, since a policy can be renamed or deleted
// long before anyone reads the register. Deny policies are not approval rules and are skipped;
// Denying finds those, and the dispatcher consults it first.
func Requiring(policies []*Policy, r *run.Run) *Policy {
	for _, p := range policies {
		if p.MaxDestroy < 0 && !p.Denies() && p.Matches(r) {
			return p
		}
	}
	return nil
}

// Label returns how a policy should be named in evidence: its name, or its id when it has none.
func (p *Policy) Label() string {
	if p == nil {
		return ""
	}
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// PlanGated reports whether any plan-content policy scopes r, meaning r's apply must be planned and
// checked before it runs. A policy is plan-content when its MaxDestroy is non-negative, and it scopes
// r when it also matches r. This is separate from Matches, which is unchanged.
func PlanGated(policies []*Policy, r *run.Run) bool {
	for _, p := range policies {
		if p.MaxDestroy >= 0 && p.Matches(r) {
			return true
		}
	}
	return false
}

// PlanExceeds reports whether a plan that destroys the given number of resources violates any
// plan-content policy scoping r. A policy participates only when it is enabled, its MaxDestroy
// non-negative, and it matches r; it is violated when destroys is greater than its threshold. A run
// whose plan exceeds a threshold is held for approval rather than applied.
func PlanExceeds(policies []*Policy, r *run.Run, destroys int) bool {
	return Exceeding(policies, r, destroys) != nil
}

// Exceeding returns the first plan-content policy a plan of this size violates, or nil when none
// does, so the rule that held the apply can be recorded on it.
func Exceeding(policies []*Policy, r *run.Run, destroys int) *Policy {
	for _, p := range policies {
		if p.MaxDestroy >= 0 && p.Matches(r) && destroys > p.MaxDestroy {
			return p
		}
	}
	return nil
}

// Store persists approval policies. Implementations must be safe for concurrent use.
type Store interface {
	// Save stores a policy, inserting or replacing by id.
	Save(ctx context.Context, p *Policy) error
	// Get returns the policy with the given id, or ErrNotFound when it does not exist.
	Get(ctx context.Context, id string) (*Policy, error)
	// List returns every policy, oldest first.
	List(ctx context.Context) ([]*Policy, error)
	// Delete removes a policy by id, returning ErrNotFound when it does not exist.
	Delete(ctx context.Context, id string) error
}

// NewPolicy returns a policy with the given name and a fresh id, its plan-content check disabled so a
// policy created without a destroy threshold never holds a run on plan content. Callers set the
// matching criteria and, to enable the plan-content gate, a non-negative MaxDestroy.
func NewPolicy(name string) *Policy {
	return &Policy{ID: NewID(), Name: name, MaxDestroy: DisabledMaxDestroy, CreatedAt: time.Now()}
}

// NewID returns a random policy identifier prefixed with "pol_".
func NewID() string {
	return idgen.New("pol_", 6)
}
