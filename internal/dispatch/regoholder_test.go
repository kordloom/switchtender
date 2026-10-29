package dispatch

import (
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/policytest"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAStoredRegoPolicyFetchesThePlaybook pins that a Rego policy this build cannot evaluate gets
// the fresh project checkout a policy reading the reversibility grade gets. The build cannot tell
// what the bundle reads, so it prepares everything a policy could read rather than judging on less.
func TestAStoredRegoPolicyFetchesThePlaybook(t *testing.T) {
	t.Parallel()
	set := []*policy.Policy{{ID: "pol_r", Name: "rego gate", MaxDestroy: policy.DisabledMaxDestroy,
		Rego: policytest.StoredRego(t)}}
	r := &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml"}
	if !readsPlaybook(set, r) {
		t.Error("readsPlaybook() = false for a Rego policy this build cannot read, want true")
	}
	if readsPlaybook(nil, r) {
		t.Error("readsPlaybook() = true with no policy, want false, the control")
	}
}
