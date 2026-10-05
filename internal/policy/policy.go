// Package policy decides which runs must be held for approval, turning approval from an opt-in flag
// into an enforced rule. A policy matches runs by tool, command, and target; a matched run is held
// at submission so an operator cannot skip the gate.
package policy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/decision"
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
	// CommandContains matches when a run's command contains this text, ignoring case. Empty matches
	// any. Case is ignored because "drop database" and "DROP DATABASE" are the same statement, so a
	// case-sensitive rule refuses one spelling and waves the other through.
	CommandContains string `json:"command_contains,omitempty"`
	// InventoryID matches a run targeting this stored inventory, directly or through a composed
	// inventory that drew hosts from it. Empty matches any.
	InventoryID string `json:"inventory_id,omitempty"`
	// Queue matches a run routed to this worker queue. Empty matches any.
	//
	// A queue decides which segment of the estate executes the run, so it is the criterion a rule
	// needs to say "hold anything headed for production". Nothing could match on it, which left the
	// queue governable only by a grant; this makes it governable by a rule as well, for an install
	// that has not turned strict grants on.
	Queue string `json:"queue,omitempty"`
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
	// Reversibility matches only runs at least as hard to undo as this class: reversible, costly,
	// or irreversible. Empty matches any run.
	//
	// It is a floor, so "costly" covers costly and irreversible and leaves a dry run alone. This is
	// the criterion for the rule an operator actually wants to write, which is that a change nobody
	// can take back needs a second person to agree to it. Risk cannot express that: a fleet restart
	// grades high and undoes itself, while deleting a backup set grades quietly and is forever.
	Reversibility string `json:"reversibility,omitempty"`
	// Effect is what a matched blanket policy does: require_approval holds the run, deny refuses
	// the submission. Empty means require_approval.
	Effect string `json:"effect,omitempty"`
	// ExcludeDryRun leaves dry-run runs unmatched, so a no-change preview does not need approval. A
	// dry run that is not a no-change preview stays matched: an Ansible dry run whose playbook sets
	// check_mode to anything but true somewhere runs that work for real under --check, and one whose
	// playbook could not be read in full may, so neither is exempt.
	ExcludeDryRun bool `json:"exclude_dry_run,omitempty"`
	// RequireDistinctApprover refuses a decision made by the person who asked for the change, which
	// is separation of duties: an approval gate the requester can release themselves records a
	// signature but stops nothing. The requirement is copied onto each run this rule holds, so a
	// later edit to the rule cannot weaken a decision that is already pending.
	RequireDistinctApprover bool `json:"require_distinct_approver,omitempty"`
	// RequireReason makes a decision on a run this rule holds carry the approver's stated reason:
	// "denials" for a denial, "always" for an approval and a denial alike. Empty asks for none, the
	// default. Like the distinct-approver requirement it is copied onto each run the rule holds, so a
	// later edit to the rule cannot weaken a decision already pending. It sets no criterion, so it is
	// allowed beside a Rego policy, where it applies to every run that policy holds.
	RequireReason string `json:"require_reason,omitempty"`
	// MaxDestroy holds a matched terraform or opentofu run for approval when its plan would destroy
	// more than this many resources. A negative value disables the plan-content check, the safe
	// default, so a policy without a threshold is a blanket rule rather than one that holds on any
	// destroy.
	MaxDestroy int `json:"max_destroy"`
	// Rego is the compiled Rego bundle that decides for this policy, nil for a criteria rule. A Rego
	// policy sets no criteria of its own: its package decides whether a run is denied, held, needs a
	// distinct approver, or must be planned first, and every function below asks it. It is only
	// ever read from the policy file, and a database store refuses to save one.
	Rego *RegoProgram `json:"rego,omitempty"`
	// CreatedAt is when the policy was created.
	CreatedAt time.Time `json:"created_at"`
}

// Matches reports whether the policy's criteria match r. Every non-empty criterion must match,
// so a policy narrows the runs it gates rather than widening them.
func (p *Policy) Matches(r *run.Run) bool {
	if p.Rego != nil {
		d := p.Rego.Decide(r)
		return d.Err != nil || len(d.Deny) > 0 || len(d.Hold) > 0
	}
	return p.matchesBeforeGrade(r) && p.meetsGradeFloors(r)
}

// matchesBeforeGrade reports whether every criterion but the risk and reversibility floors matches
// r. Those two are grades computed from the run, and a terraform or opentofu apply has no honest
// grade until its plan has said what it destroys, so the plan gate asks this question first.
func (p *Policy) matchesBeforeGrade(r *run.Run) bool {
	// The run's effective mode, not the flag it was submitted with. A dry run whose playbook forces
	// real work is a change, and exempting it on the strength of the flag let any change through a
	// rule that excludes dry runs by asking for it as a preview.
	if p.ExcludeDryRun && r.ChangeFree() {
		return false
	}
	if p.Tool != "" && run.NormalizeTool(p.Tool) != run.NormalizeTool(r.Tool) {
		return false
	}
	if p.CommandContains != "" && !containsFold(r.Command, p.CommandContains) {
		return false
	}
	// A run reaches an inventory's hosts by naming it, or through a smart or constructed inventory
	// that drew hosts from it, and a rule scoped to the inventory governs both.
	if p.InventoryID != "" && !r.TargetsInventory(p.InventoryID) {
		return false
	}
	if p.Queue != "" && p.Queue != r.Queue {
		return false
	}
	return p.matchesActor(r)
}

// meetsGradeFloors reports whether r's grade clears the policy's risk and reversibility floors. A
// policy with neither floor has nothing to clear.
func (p *Policy) meetsGradeFloors(r *run.Run) bool {
	if p.MinRisk != "" && !meetsRiskFloor(run.AssessRisk(r).Level, p.MinRisk) {
		return false
	}
	if p.Reversibility != "" && !run.MeetsReversibilityFloor(reversibilityOf(r), p.Reversibility) {
		return false
	}
	return true
}

// containsFold reports whether text contains want, ignoring case. Command criteria are matched this
// way because the shell and every database that accepts "drop database" accepts "DROP DATABASE" as
// the same statement, so a case-sensitive rule is a refusal anyone can step around by holding down
// shift. The risk grader already lowercases before hunting for the same markers, so matching on
// case here also kept the two halves of the product disagreeing about whether case matters.
func containsFold(text, want string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(want))
}

// meetsRiskFloor reports whether a run assessed at level clears a policy's floor.
//
// A floor this build cannot rank holds every run rather than none. Comparing ranks directly put an
// unknown floor above every level a run can be graded, so a rule written on a newer build, or
// restored from a backup taken across a version change, read as an active gate on screen and
// matched nothing at all. An unrankable floor is a rule this build does not understand, and the
// only safe reading of a rule you do not understand is that it applies.
func meetsRiskFloor(level, floor string) bool {
	if riskRank(floor) > riskRank(run.RiskHigh) {
		return true
	}
	return riskRank(level) >= riskRank(floor)
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

// Advanced reports whether a policy uses the full engine: a deny effect, a risk floor, actor
// scoping, or distinct-approver separation of duties. One plain require-approval policy is the
// Community gate; everything past it is what a Team license covers.
//
// This is meant to be the one definition every enforcement point shares. It said so before it was,
// which is how the drift lived: the create and update handlers each carried their own copy of the
// condition and this carried a third, and none of the three tested for actor scoping while the
// licensing terms, the API reference, the FAQ, and the comment on license.FeaturePolicyFull all sold
// it as Team. A rule scoped to agents as a class was free through the API and free through
// --policy-file, which is the path an install that takes policy seriously actually uses.
//
// Actor scoping belongs here because it is the criterion that turns a blanket hold into an
// authorization boundary around a machine principal, which is the thing being sold.
// Reversibility belongs here for the same reason MinRisk does, and it drifted the same way: the
// grade was added, the engine evaluated it, and this was not updated, so a rule holding on
// irreversibility was free through --policy-file while the identical rule through the API was
// refused as Team. The feature this product sells hardest was the one leaking.
//
// A Rego policy is the full engine by construction: it can deny, scope to actors, and demand a
// second approver, and nothing about it can be read before it runs to say that it does not.
func (p *Policy) Advanced() bool {
	return p.Rego != nil ||
		p.Effect == EffectDeny ||
		p.MinRisk != "" ||
		p.Reversibility != "" ||
		p.RequireDistinctApprover ||
		p.ActorKind != "" ||
		p.Actor != ""
}

// riskRank orders risk levels so MinRisk can compare them. An unknown level ranks above high, so a
// run this build cannot grade fails toward holding rather than passing. An unknown floor is handled
// by meetsRiskFloor, since ranking alone would push it out of reach of every run.
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
//
// A Rego policy that refuses r is returned as a copy labeled with its reasons and its bundle digest,
// and so is one that could not decide: a gate that cannot be evaluated has not been passed.
func Denying(policies []*Policy, r *run.Run) *Policy {
	for _, p := range policies {
		if p.Rego != nil {
			if d := p.Rego.Decide(r); d.Err != nil || len(d.Deny) > 0 {
				return p.regoVerdict(EffectDeny, d.Deny, d.Err)
			}
			continue
		}
		if p.Denies() && p.Matches(r) {
			return p
		}
	}
	return nil
}

// Validate checks the policy's declared vocabulary, so a rule with a typo is refused where it is
// written rather than silently matching nothing, or worse, everything.
func (p *Policy) Validate() error {
	if !decision.ValidRequirement(p.RequireReason) {
		return fmt.Errorf("require_reason must be %q or %q, not %q", decision.RequireDenials,
			decision.RequireAlways, p.RequireReason)
	}
	if p.Rego != nil {
		return p.validateRego()
	}
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
	// Refused at load rather than at match time. An unrecognized floor matches nothing, so a
	// misspelled class would leave the rule loaded, listed, and silently never firing, which is the
	// worst shape for a control whose whole job is to stop a change nobody can take back.
	if p.Reversibility != "" && !run.ValidReversibility(p.Reversibility) {
		return fmt.Errorf("reversibility must be %q, %q, or %q, not %q",
			run.Reversible, run.ReversibleCostly, run.Irreversible, p.Reversibility)
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
//
// A Rego policy holding r is returned as a copy labeled with its reasons and bundle digest. One that
// could not decide holds too, though the dispatcher's deny pass has already refused it by then.
func Requiring(policies []*Policy, r *run.Run) *Policy {
	for _, p := range policies {
		if p.Rego != nil {
			if d := p.Rego.Decide(r); d.Err != nil || len(d.Hold) > 0 {
				return p.regoVerdict(EffectRequireApproval, d.Hold, d.Err)
			}
			continue
		}
		if p.MaxDestroy < 0 && !p.Denies() && p.Matches(r) {
			return p
		}
	}
	return nil
}

// Noting returns what the Rego policies set to warn: note recorded about r, one entry for each such
// policy whose warn rule fired, named as a hold is named: the policy, its messages, and its bundle.
//
// A note holds and refuses nothing. It is the warning a policy chose to record on the run rather
// than wait on, so the run page, the outcome record, and a receipt can say what was flagged and
// that the run went ahead. A policy that could not decide notes nothing here, because Denying has
// already refused the run for it: setting warn to note softens a warning, never a failure.
func Noting(policies []*Policy, r *run.Run) []string {
	var out []string
	for _, p := range policies {
		if p == nil || p.Rego == nil || p.Rego.Warn() != RegoWarnNote {
			continue
		}
		if d := p.Rego.Decide(r); d.Err == nil && len(d.Note) > 0 {
			out = append(out, p.noteLabel(d.Note))
		}
	}
	return out
}

// RequireDistinct reports whether any matching non-deny rule demands a distinct approver.
//
// The flag used to be copied from whichever matching rule Requiring returned first, so a stricter
// separation-of-duties rule later in the list silently did nothing, and reordering policies could
// lower a control nobody meant to lower. Separation of duties composes by OR: if any rule that
// covers this run demands a second person, the run demands a second person. Plan-content rules
// count too, since the plan gate holds under the same requirement.
//
// A Rego policy demands one when its require_distinct_approver rule holds for r, or when it could
// not decide, since the stricter answer is the only safe one to a question nobody could answer.
func RequireDistinct(policies []*Policy, r *run.Run) bool {
	for _, p := range policies {
		if p.Rego != nil {
			if d := p.Rego.Decide(r); d.Err != nil || d.RequireDistinctApprover {
				return true
			}
			continue
		}
		if p.RequireDistinctApprover && !p.Denies() && p.Matches(r) {
			return true
		}
	}
	return false
}

// ReasonRequirement returns the strictest reason requirement among the rules that would hold r, so
// it composes the way separation of duties does: if any rule covering the run asks for a reason,
// the run asks for one. A Rego policy asks when it holds r or cannot decide; a deny rule never
// holds anything and is skipped. Plan-content rules count too, since the plan gate holds under the
// same rules.
func ReasonRequirement(policies []*Policy, r *run.Run) string {
	out := ""
	for _, p := range policies {
		if p.RequireReason == "" || p.Denies() {
			continue
		}
		if p.Rego != nil {
			if d := p.Rego.Decide(r); d.Err != nil || len(d.Hold) > 0 {
				out = decision.Stricter(out, p.RequireReason)
			}
			continue
		}
		if p.Matches(r) {
			out = decision.Stricter(out, p.RequireReason)
		}
	}
	return out
}

// ExceedingReason returns the strictest reason requirement among the plan-content rules a plan of
// this size violates, which is what the apply the plan gate holds carries.
func ExceedingReason(policies []*Policy, r *run.Run, destroys int) string {
	out := ""
	for _, p := range policies {
		if p.MaxDestroy >= 0 && p.RequireReason != "" && p.Matches(r) && destroys > p.MaxDestroy {
			out = decision.Stricter(out, p.RequireReason)
		}
	}
	return out
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

// PlanGated reports whether r's apply must be planned and checked before it runs. A plan-content
// policy, one with a non-negative MaxDestroy, sends r there when it matches r.
//
// So does a rule with a risk or reversibility floor that matches a terraform or opentofu apply on
// every criterion but the grade, when the grade does not yet clear the floor. Such an apply is
// graded from its command until a plan says what it destroys, which made every apply costly and
// medium: a rule holding irreversible changes, or high risk ones, never matched an apply that
// destroyed everything, and the apply ran. Planned first, the apply it proposes carries the plan's
// destroy count, its grade says what the plan found, and the rule decides on that.
//
// And so does any approval rule that would hold the apply, or the apply's own submission asking for
// approval. An apply held before it was planned was approved as a request and planned only when it
// ran, so the approver never saw the plan that applied. Planned first, the apply it proposes
// carries the saved plan, the rules decide on that proposal, it is held when its submission asked,
// and the approval binds the plan that runs. A run still held at submission, and a run already
// released by a decision, are left to the hold they had, and so is a step of a workflow, which its
// workflow's approval governs.
//
// Only a Terraform or OpenTofu apply that has not been planned yet is ever planned first, so any
// other run is never plan gated, whatever the rules say. The dispatcher leaves a run it reads as
// plan gated unheld, trusting the plan gate to hold the apply it proposes, and the plan gate never
// takes a dry run, a run of another tool, or an apply a plan already proposed. A plan-content rule
// that matched one of those used to release it from every hold: a destroy limit written without a
// tool let an Ansible run past a rule holding everything, and one written for Terraform let the
// apply a plan proposed past the hold its own rules placed.
func PlanGated(policies []*Policy, r *run.Run) bool {
	tool := run.NormalizeTool(r.Tool)
	graded := (tool == run.ToolTerraform || tool == run.ToolOpenTofu) && !r.DryRun &&
		r.ProposedFrom == ""
	if !graded {
		return false
	}
	if r.ParentID == nil && r.Status != run.StatusPendingApproval && r.DecisionID == "" &&
		r.ApprovedSpecDigest == "" && (r.ApprovalRequested || Requiring(policies, r) != nil) {
		return true
	}
	for _, p := range policies {
		// A Rego policy plans an apply when its plan_gate rule says so, or when it cannot decide:
		// the apply it then proposes faces the same policy again, which refuses it, so an
		// undecidable policy never lets an unplanned apply through.
		if p.Rego != nil {
			if d := p.Rego.Decide(r); d.Err != nil || d.PlanGate {
				return true
			}
			continue
		}
		if p.MaxDestroy >= 0 && p.Matches(r) {
			return true
		}
		if (p.MinRisk != "" || p.Reversibility != "") && p.matchesBeforeGrade(r) &&
			!p.meetsGradeFloors(r) {
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

// ExceedingDistinct reports whether any rule the destroy count exceeds demands a distinct
// approver. The plan gate holds an apply on the first exceeding rule, but the release is governed
// by the strictest rule the count exceeded, however the list is ordered: copied from the first
// match, a loose rule listed ahead of a strict one silently dropped the second person, which is
// the same ordering drop RequireDistinct exists to prevent on the submission pass.
func ExceedingDistinct(policies []*Policy, r *run.Run, destroys int) bool {
	for _, p := range policies {
		if p.MaxDestroy >= 0 && p.RequireDistinctApprover && p.Matches(r) && destroys > p.MaxDestroy {
			return true
		}
	}
	return false
}

// Store persists approval policies. Implementations must be safe for concurrent use.
type Store interface {
	// Save stores a policy, inserting or replacing by id. A Rego policy is refused with
	// ErrRegoNotStored, since it is read from the policy file and has nowhere else to live.
	Save(ctx context.Context, p *Policy) error
	// Get returns the policy with the given id, or ErrNotFound when it does not exist.
	Get(ctx context.Context, id string) (*Policy, error)
	// List returns every policy, oldest first.
	List(ctx context.Context) ([]*Policy, error)
	// Delete removes a policy by id, returning ErrNotFound when it does not exist.
	Delete(ctx context.Context, id string) error
}

// ReadOnlyStore is a Store whose policies are changed somewhere else, so every write through it is
// refused. A caller asks before it runs any other check, since no other answer applies.
type ReadOnlyStore interface {
	// ReadOnly returns why writes are refused, wrapping ErrReadOnly.
	ReadOnly() error
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

// reversibilityOf reads the grade already attached to a run, falling back to grading it here.
//
// A grade computed from the run alone is blind to an Ansible playbook, because the work is inside a
// file and the run carries only its path. A caller that could read the playbook attaches the richer
// grade first, and this prefers it: without that, a rule written to hold anything that cannot be
// undone would never fire on the tool this product exists to run.
func reversibilityOf(r *run.Run) string {
	if r.Reversibility != nil {
		return r.Reversibility.Class
	}
	return run.AssessReversibility(r).Class
}

// validateRego checks a Rego policy's shape. Its package decides everything, so a criterion set
// beside it would read as narrowing the rule while changing nothing, and is refused.
func (p *Policy) validateRego() error {
	criteria := p.Tool != "" || p.CommandContains != "" || p.InventoryID != "" || p.Queue != "" ||
		p.ActorKind != "" || p.Actor != "" || p.MinRisk != "" || p.Reversibility != "" ||
		p.Effect != "" || p.ExcludeDryRun || p.RequireDistinctApprover || p.MaxDestroy >= 0
	if criteria {
		return fmt.Errorf("%w: a Rego policy decides in its package and cannot also set criteria",
			ErrRego)
	}
	return nil
}
