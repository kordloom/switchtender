package policy

import (
	"reflect"
	"strings"
	"testing"
)

// TestEveryMatchingCriterionBeyondTheBlanketGateIsLicensed is the guard on a leak that has now
// happened twice.
//
// Community buys one plain require-approval policy. Everything that narrows or strengthens a rule
// past that is Pro or Team, and ProRules and TeamRules are where those lines are drawn. Twice a
// criterion was added to the policy, wired into the engine, sold as paid, and never added to the
// license question: actor scoping first, then reversibility. Both were free through --policy-file,
// which is the path an install that takes policy seriously actually uses.
//
// The failure is silent in the direction that costs money and costs it quietly. So rather than
// trusting the next person to remember, every field that can narrow a match is listed here and has
// to be classified deliberately: part of the blanket gate Community buys, a Pro rule, or a Team
// rule, and a field on one side trips only its own predicate.
func TestEveryMatchingCriterionBeyondTheBlanketGateIsLicensed(t *testing.T) {
	t.Parallel()

	// The blanket gate: a rule that says "hold runs that look like this" without narrowing who may
	// release it or how severe it has to be. These are free.
	community := map[string]bool{
		"id": true, "name": true, "created_at": true, "queue": true,
		"tool": true, "command_contains": true, "inventory_id": true,
		"exclude_dry_run": true, "max_destroy": true,
		// Asking the approver to say why narrows nothing a rule matches and loosens nothing: it
		// adds a record to a decision the blanket gate already asks for, so it is part of the gate.
		"require_reason": true,
	}
	// Pro strengthens what a rule does to the runs it matches. Team says who the rule is about,
	// who may release it, or hands the decision to Rego.
	pro := map[string]bool{"effect": true, "min_risk": true, "reversibility": true}
	team := map[string]bool{
		"require_distinct_approver": true, "actor_kind": true, "actor": true, "account": true,
		"rego": true,
	}

	rt := reflect.TypeOf(Policy{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || community[name] {
			continue
		}
		// Setting this field alone must make the policy Advanced. Built by reflection so a new
		// field cannot be quietly omitted from the check.
		p := &Policy{Name: "probe"}
		v := reflect.ValueOf(p).Elem().Field(i)
		switch v.Kind() {
		case reflect.String:
			// An effect is free at its default and Team only when it refuses outright, so the
			// probe has to carry the value that actually crosses the line.
			if name == "effect" {
				v.SetString(EffectDeny)
			} else {
				v.SetString("x")
			}
		case reflect.Bool:
			v.SetBool(true)
		case reflect.Pointer:
			// The only pointer is the Rego bundle, which can deny, scope to actors, and demand a
			// second approver, so carrying one at all crosses the line.
			if name != "rego" {
				t.Errorf("field %q is a pointer this guard does not know; classify it above", name)
				continue
			}
			v.Set(reflect.ValueOf(uncompiledRego()))
		default:
			t.Errorf("field %q is neither a string nor a bool, so this guard cannot classify it; "+
				"decide whether it is part of the blanket gate and add it above", name)
			continue
		}
		if !p.Advanced() {
			t.Errorf("a policy setting only %q is not Advanced, so it is free on Community. "+
				"Either it belongs in the blanket gate Community buys, or Advanced has to see it",
				name)
		}
		if !pro[name] && !team[name] {
			t.Errorf("field %q is paid but neither Pro nor Team here; classify it above", name)
			continue
		}
		if got := p.ProRules(); got != pro[name] {
			t.Errorf("a policy setting only %q: ProRules() = %v, want %v", name, got, pro[name])
		}
		if got := p.TeamRules(); got != team[name] {
			t.Errorf("a policy setting only %q: TeamRules() = %v, want %v", name, got, team[name])
		}
	}
}
