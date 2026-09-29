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
// past that is Team, and Advanced is where that line is drawn. Twice a criterion was added to the
// policy, wired into the engine, sold as Team, and never added here: actor scoping first, then
// reversibility. Both were free through --policy-file, which is the path an install that takes
// policy seriously actually uses.
//
// The failure is silent in the direction that costs money and costs it quietly. So rather than
// trusting the next person to remember, every field that can narrow a match is listed here and has
// to be classified deliberately: either it is part of the blanket gate Community buys, or Advanced
// has to notice it.
func TestEveryMatchingCriterionBeyondTheBlanketGateIsLicensed(t *testing.T) {
	t.Parallel()

	// The blanket gate: a rule that says "hold runs that look like this" without narrowing who may
	// release it or how severe it has to be. These are free.
	community := map[string]bool{
		"id": true, "name": true, "created_at": true, "queue": true,
		"tool": true, "command_contains": true, "inventory_id": true,
		"exclude_dry_run": true, "max_destroy": true,
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
	}
}
