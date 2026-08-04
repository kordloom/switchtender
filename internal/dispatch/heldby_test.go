package dispatch

import (
	"context"
	"strings"
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

// TestEveryHeldRunSaysWhatHeldIt pins that no run waits for an approver while its evidence says
// nothing stopped it. A run can arrive already held without any policy being consulted, and those
// paths used to store an empty rule, which the register renders as "nothing held it" beside an
// outcome showing the change waited.
func TestEveryHeldRunSaysWhatHeldIt(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &fakeRunnerLister{hosts: []string{"a", "b"}}, nil)
	defer d.Close()

	// Held because the submission asked for it, with no policy in play at all.
	r, err := d.Submit(context.Background(), "play.yml", "inv", run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if r.Status != run.StatusPendingApproval {
		t.Fatalf("status = %q, want held", r.Status)
	}
	if r.HeldByPolicy == "" {
		t.Error("a run held at its own request records no reason, so the register reads it as " +
			"a change nothing stopped")
	}

	// Every shard of a held split says the same thing as the split.
	parent, err := d.SubmitSplit(context.Background(), "play.yml", "inv", 2,
		run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	shards, err := store.Shards(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) == 0 {
		t.Fatal("no shards were created")
	}
	for _, sh := range shards {
		if sh.HeldByPolicy != parent.HeldByPolicy {
			t.Errorf("shard %s reason = %q, want its parent's %q", sh.ID, sh.HeldByPolicy,
				parent.HeldByPolicy)
		}
	}
}

// TestPipelineHeldByAStepNamesTheRule pins that a pipeline held because one of its steps matches
// records the rule, since the parent's own attributes do not explain the hold.
func TestPipelineHeldByAStepNamesTheRule(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: "pol_tf", Name: "terraform needs sign off", Tool: "terraform", MaxDestroy: -1,
	}); err != nil {
		t.Fatalf("Save() policy error = %v", err)
	}
	d := New(store, &fakeRunnerLister{hosts: []string{"a"}}, nil, WithPolicies(policies))
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "release", "inv", []run.PipelineStep{
		{Name: "configure", Playbook: "site.yml"},
		{Name: "provision", Tool: "terraform", Command: "infra/prod"},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if parent.Status != run.StatusPendingApproval {
		t.Fatalf("pipeline status = %q, want held by its terraform step", parent.Status)
	}
	if parent.HeldByPolicy == "" {
		t.Fatal("a pipeline held by a step's rule records no reason")
	}
	if !strings.Contains(parent.HeldByPolicy, "terraform needs sign off") {
		t.Errorf("reason = %q, want it to name the rule", parent.HeldByPolicy)
	}
}
