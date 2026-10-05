package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
	"github.com/kordloom/switchtender/internal/util"
)

// PlanCounts is what a Terraform or OpenTofu plan's change summary line reported.
type PlanCounts struct {
	// Add is the number of resources the plan would create.
	Add int `json:"add"`
	// Change is the number of resources the plan would update in place.
	Change int `json:"change"`
	// Destroy is the number of resources the plan would destroy, the count the plan-content rules
	// weigh.
	Destroy int `json:"destroy"`
	// Import is the number of resources the plan would import.
	Import int `json:"import"`
	// Total is the sum of every count clause on the summary line.
	Total int `json:"total"`
}

// PlanReadCap is how much plan output the plan gate weighs. Output past it is not read, and a
// caller reporting a plan's destroy count must report a longer plan as unread, as the gate does.
const PlanReadCap = planReadCap

// PlanSummary reads the change summary out of a plan's captured output with the parser the plan
// gate uses, so a count reported anywhere else is the count the rules weigh. The second result is
// false when no summary was read, which is not the same as a plan that changes nothing.
func PlanSummary(out string) (PlanCounts, bool) {
	c, ok := parsePlanSummary(out)
	counts := PlanCounts{
		Add: c.Add, Change: c.Change, Destroy: c.Destroy, Import: c.Import, Total: c.Total,
	}
	return counts, ok
}

// Apply decision outcomes a preview reports.
const (
	// ApplyRuns means no rule in force would hold or refuse the apply.
	ApplyRuns = "run"
	// ApplyHeld means a rule would hold the apply for a person's approval.
	ApplyHeld = "hold"
	// ApplyDenied means a rule would refuse the apply outright.
	ApplyDenied = "deny"
)

// Stages at which a previewed decision lands.
const (
	// StageSubmission is the decision made when the apply is submitted, before anything executes.
	StageSubmission = "submission"
	// StagePlan is the decision the plan gate makes on the apply it proposes from a plan.
	StagePlan = "plan"
)

// ApplyPreview is the decision the rules in force would give the apply a plan stands for. It is a
// prediction made against today's rules and today's plan: the apply itself is decided again, by the
// same functions, when it is actually submitted.
type ApplyPreview struct {
	// Outcome is run, hold, or deny.
	Outcome string `json:"outcome"`
	// Rule names the rule behind a hold or a refusal, as evidence would name it. Empty for run.
	Rule string `json:"rule,omitempty"`
	// Stage is where the decision lands: at submission, or at the plan gate after the apply is
	// planned again. Empty for run.
	Stage string `json:"stage,omitempty"`
	// RequireDistinctApprover reports that the approver must be a different person from whoever
	// requests the apply.
	RequireDistinctApprover bool `json:"require_distinct_approver,omitempty"`
	// PlanGated reports that a plan-content rule scopes the apply, so it is planned and weighed on
	// its destroy count before it may run.
	PlanGated bool `json:"plan_gated,omitempty"`
}

// PreviewApply returns the decision the rules in force would give apply, the real run a review plan
// stands for, given the destroy count that plan reported. read is false when the plan's summary
// could not be read, which the plan gate treats as a plan that must be held.
//
// It walks the same functions the dispatcher walks, in the same order: a deny rule at submission,
// a blanket hold at submission, and, for a terraform or opentofu apply a plan-content rule scopes,
// the proposal the plan gate would build from this plan, gated exactly as a real one is. The apply
// is graded as a submission is, reading an Ansible playbook from the project's checkout, which
// holds the branch rather than the pull request until the pull request merges. Nothing is stored
// and nothing executes. A rule store that cannot be read is an error, never a preview of no rules.
func (d *Dispatcher) PreviewApply(ctx context.Context, apply *run.Run, destroys int,
	read bool) (ApplyPreview, error) {
	var policies []*policy.Policy
	if d.policies != nil {
		var err error
		if policies, err = d.policies.List(ctx); err != nil {
			return ApplyPreview{}, fmt.Errorf("%w: %w", ErrPolicyUnavailable, err)
		}
	}
	// The rule a preview names is what the pull request's comment prints, and a Rego verdict is
	// labeled with the messages its module built, which can quote the apply's command, so it is
	// masked as the hold the apply would record is.
	gr := d.graded(apply)
	if p := policy.Denying(policies, gr); p != nil {
		return ApplyPreview{Outcome: ApplyDenied, Rule: maskPolicyText(gr, p.Label()),
			Stage: StageSubmission}, nil
	}
	gated := policy.PlanGated(policies, apply)
	if p := policy.Requiring(policies, gr); p != nil {
		return ApplyPreview{
			Outcome:                 ApplyHeld,
			Rule:                    maskPolicyText(gr, p.Label()),
			Stage:                   StageSubmission,
			PlanGated:               gated,
			RequireDistinctApprover: policy.RequireDistinct(policies, gr),
		}, nil
	}
	if !gated {
		return ApplyPreview{Outcome: ApplyRuns}, nil
	}
	return previewProposal(policies, apply, destroys, read), nil
}

// RedactRunText returns text with every secret value this server can attribute to r masked, the
// same values the run's log masker held: the run's own credentials and those of its stored
// inventory, its registry pull login, secret-looking values in its stored inventory, and the
// secrets its own variables and command carry. Secret-looking assignments are masked on top, which
// is the reading the receipt and the inventory redactor apply to free text.
//
// It exists for text that is about to leave the server for somewhere this install does not control,
// such as a pull request comment. The stored log was masked when it was written, so this is a
// second pass over it: a log written by an older worker, or before a credential was attached to an
// inventory, is not trusted to have been.
//
// A credential whose value comes from an external source is not resolved here. Resolving it would
// mint a fresh secret, or run a command, purely to find out what to hide, and a dynamic secret's
// new value is not the one the run printed anyway. Its value was masked at execution by the run's
// own masker.
func (d *Dispatcher) RedactRunText(ctx context.Context, r *run.Run, text string) string {
	if r == nil || text == "" {
		return text
	}
	values := runOwnSecrets(r.ExtraVars, r.Command)
	values = append(values, d.storedSecretValues(ctx, r)...)
	if r.InventoryID != "" && d.inventories != nil {
		if inv, err := d.inventories.Get(ctx, r.InventoryID); err == nil &&
			secretsource.NormalizeKind(inv.ContentSource) == secretsource.KindLocal {
			values = append(values, inventorySecrets(inv.Content)...)
		}
	}
	m := &masker{}
	m.set(values)
	masked := m.redactString(text)
	masked, _ = util.RedactAssignments(masked, maskToken)
	return masked
}

// storedSecretValues opens every locally sealed credential r executes with, including its stored
// inventory's and its registry pull credential, and returns the values a tool could print: the
// whole value, each value of a KEY=VALUE bundle, and every string inside a JSON value, which covers
// the structured kinds and custom types without naming each one. A credential that cannot be opened
// contributes nothing, since nothing it holds reached the run either.
func (d *Dispatcher) storedSecretValues(ctx context.Context, r *run.Run) []string {
	if d.credentials == nil || d.sealer == nil {
		return nil
	}
	ids := d.effectiveCredentialIDs(ctx, r)
	if r.PullCredentialID != "" {
		ids = append(ids, r.PullCredentialID)
	}
	var out []string
	for _, id := range ids {
		// The source is checked before anything is opened, so a credential an external engine holds
		// is never resolved here. A local one opens through the one path every credential takes,
		// which for a local source resolves to the sealed value with no lease.
		stored, err := d.credentials.Get(ctx, id)
		if err != nil || secretsource.NormalizeKind(stored.Source) != secretsource.KindLocal {
			continue
		}
		c, plain, lease, err := d.openCredential(ctx, id)
		d.revokeLease(lease)
		if err != nil {
			continue
		}
		out = append(out, plain)
		for _, line := range credential.EnvLines(plain) {
			if _, val, ok := strings.Cut(line, "="); ok {
				out = append(out, val)
			}
		}
		out = append(out, jsonStrings(plain)...)
		if c.Kind == credential.KindRegistry {
			user, pass := credential.RegistryLogin(plain)
			out = append(out, user, pass)
		}
	}
	return out
}

// jsonStrings returns every string leaf of text when it is a JSON object or array, and nothing
// otherwise. Masking is the only use, so a field that is not secret costs a little over-masking in
// text that is leaving the server, which is the direction to err.
func jsonStrings(text string) []string {
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(v)
	return out
}
