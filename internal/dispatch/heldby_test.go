package dispatch

import (
	"context"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestHeldRunNamesTheRuleThatStoppedIt pins that a run held for approval records which rule held
// it, by the name that rule carried at the hold.
//
// The register asks "what stopped this change". Answering it by matching today's policies would
// answer with a rule that may have been renamed since, or with nothing once it is deleted, and the
// run whose evidence is being read is exactly the one whose policy is most likely to have changed.
func TestHeldRunNamesTheRuleThatStoppedIt(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: "pol_1", Name: "prod terraform destroy", Tool: "terraform",
		MaxDestroy: -1,
	}); err != nil {
		t.Fatalf("Save() policy error = %v", err)
	}
	d := New(store, &fakeRunnerLister{hosts: []string{"a"}}, nil, WithPolicies(policies))
	defer d.Close()

	r, err := d.Submit(context.Background(), "", "inv",
		run.WithTool("terraform"), run.WithCommand("infra/prod"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if r.Status != run.StatusPendingApproval {
		t.Fatalf("status = %q, want the run held", r.Status)
	}
	if r.HeldByPolicy != "prod terraform destroy" {
		t.Errorf("held by = %q, want the rule that held it", r.HeldByPolicy)
	}

	// A run nothing holds names no rule, so an empty value means "nothing stopped this" rather
	// than "we did not record it".
	free, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if free.HeldByPolicy != "" {
		t.Errorf("unheld run named rule %q, want none", free.HeldByPolicy)
	}
}
