package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
)

// TestEveryPolicyFieldAFileCanSetTheApiCanSetToo is the guard on a gap nothing else could see.
//
// A policy has three ways in: a YAML file, this API, and the page. They are meant to describe the
// same rule, and for one field they did not. policy.Policy carried Reversibility, the engine
// evaluated it, a YAML file could set it, and createPolicyRequest had no such field, so the request
// was refused as carrying an unknown field.
//
// That made the single rule this product tells operators to write, that a change nobody can take
// back needs a second person to agree, unreachable from the API and the page both. It worked only
// for somebody who hand-edited a file, and nothing failed to say so: the feature was built, graded,
// documented, and sold, and could not be turned on the way anyone would try to turn it on.
//
// Comparing the two structs by their json tags is what catches the next one. A field added to the
// stored policy without a way to set it fails here, by name.
func TestEveryPolicyFieldAFileCanSetTheApiCanSetToo(t *testing.T) {
	t.Parallel()

	// Fields the API deliberately does not take: the server assigns them.
	assigned := map[string]bool{"id": true, "created_at": true}

	tags := func(v any) map[string]bool {
		out := map[string]bool{}
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if name != "" && name != "-" {
				out[name] = true
			}
		}
		return out
	}

	stored := tags(policy.Policy{})
	accepted := tags(createPolicyRequest{})
	for name := range stored {
		if assigned[name] || accepted[name] {
			continue
		}
		t.Errorf("policy.Policy has %q and createPolicyRequest does not, so a rule using it can "+
			"only be written by hand-editing a file and the API refuses it as an unknown field",
			name)
	}
}
