package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// regoStore writes a policy file loading one Rego module with the given body and returns the
// store serving it.
func regoStore(t *testing.T, body string) *policy.FileStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gate.rego"),
		[]byte("package switchtender\n\n"+body+"\n"), 0o600); err != nil {
		t.Fatalf("write module: %v", err)
	}
	path := filepath.Join(dir, "policies.yml")
	if err := os.WriteFile(path, []byte("rego:\n  - name: rego gate\n    files: [gate.rego]\n"),
		0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	store, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	return store
}

// TestARegoPolicyGatesASubmission drives the dispatcher's submit path with a Rego policy in force
// and checks what lands on the run: the hold, the rule and bundle that held it, the second-approver
// requirement, and the in-force record naming the bundle; and for a refusal, the reason.
func TestARegoPolicyGatesASubmission(t *testing.T) {
	t.Parallel()
	policies := regoStore(t, `deny contains "agents may not run bash" if {
	input.actor.kind == "agent"
	input.run.tool == "bash"
}

hold contains "terraform needs a person" if input.run.tool == "terraform"

require_distinct_approver if input.run.tool == "terraform"`)
	listed, err := policies.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	digest := listed[0].Rego.Digest()
	store := run.NewMemStore()
	d := New(store, &fakeRunnerLister{hosts: []string{"a"}}, nil, WithPolicies(policies))
	defer d.Close()
	ctx := context.Background()

	held, err := d.Submit(ctx, "", "", run.WithTool("terraform"), run.WithCommand("infra/prod"))
	if err != nil {
		t.Fatalf("Submit(terraform) error = %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("status = %q, want held", held.Status)
	}
	wantHeld := "rego gate (terraform needs a person, rego sha256:" + digest[:12] + ")"
	if held.HeldByPolicy != wantHeld {
		t.Errorf("held by %q, want %q", held.HeldByPolicy, wantHeld)
	}
	if !held.RequireDistinctApprover {
		t.Error("the Rego policy's second-approver rule did not reach the run")
	}
	if held.PolicySet == nil || held.PolicySet.Count != 1 ||
		!strings.Contains(strings.Join(held.PolicySet.Rules, "\n"), "bundle sha256:"+digest) {
		t.Errorf("policy set = %+v, want it to name the bundle digest in full", held.PolicySet)
	}

	_, err = d.Submit(ctx, "", "", run.WithTool("bash"), run.WithCommand("uptime"),
		run.WithActorType("agent"), run.WithActor("bot"))
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Submit(agent bash) error = %v, want ErrPolicyDenied", err)
	}
	if !strings.Contains(err.Error(), "agents may not run bash") {
		t.Errorf("refusal %q does not say why", err)
	}

	free, err := d.Submit(ctx, "", "", run.WithTool("bash"), run.WithCommand("uptime"))
	if err != nil {
		t.Fatalf("Submit(person bash) error = %v", err)
	}
	if free.Status == run.StatusPendingApproval || free.HeldByPolicy != "" {
		t.Errorf("a run no rule covers was held: %+v", free)
	}
}

// TestARegoPolicyThatCannotDecideRefusesTheSubmission pins failing closed through the dispatcher:
// a runtime error in the policy refuses every submission it judges, naming the failure.
func TestARegoPolicyThatCannotDecideRefusesTheSubmission(t *testing.T) {
	t.Parallel()
	policies := regoStore(t, `hold contains x if { x := to_number(input.run.command) }`)
	d := New(run.NewMemStore(), &fakeRunnerLister{hosts: []string{"a"}}, nil,
		WithPolicies(policies))
	defer d.Close()
	_, err := d.Submit(context.Background(), "", "", run.WithTool("bash"),
		run.WithCommand("not a number"))
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Submit() error = %v, want ErrPolicyDenied", err)
	}
	if !strings.Contains(err.Error(), "evaluation failed") {
		t.Errorf("refusal %q does not say the policy could not decide", err)
	}
}

// TestARegoPlanGateHoldsLikeADestroyThreshold runs the same Terraform apply through the plan gate
// under a YAML destroy threshold and under its Rego twin, and requires the same outcome for each
// plan: held over the limit, applied under it, held when the summary cannot be read.
func TestARegoPlanGateHoldsLikeADestroyThreshold(t *testing.T) {
	t.Parallel()
	rego := `plan_gate if input.run.tool == "terraform"

hold contains msg if {
	input.run.tool == "terraform"
	input.plan.planned
	input.plan.destroys > 3
	msg := sprintf("plan destroys %d, limit 3", [input.plan.destroys])
}`
	tests := []struct {
		// Summary is the plan output the fake runner writes.
		Summary string
		// WantHeld is whether the proposed apply must wait for a person.
		WantHeld bool
	}{{ // Test 0: Over the limit.
		Summary: "Plan: 0 to add, 0 to change, 5 to destroy.\n", WantHeld: true,
	}, { // Test 1: Under the limit.
		Summary: "Plan: 1 to add, 0 to change, 2 to destroy.\n", WantHeld: false,
	}, { // Test 2: A summary nobody could read.
		Summary: "something this parser does not know\n", WantHeld: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			yaml := policy.NewMemStore()
			if err := yaml.Save(context.Background(), &policy.Policy{
				ID: policy.NewID(), Name: "tf-guard", Tool: run.ToolTerraform, MaxDestroy: 3,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			yamlHeld := planGateOutcome(t, yaml, test.Summary)
			regoHeld := planGateOutcome(t, regoStore(t, rego), test.Summary)
			if yamlHeld != test.WantHeld || regoHeld != test.WantHeld {
				t.Errorf("held: YAML %v, Rego %v, want %v", yamlHeld, regoHeld, test.WantHeld)
			}
		})
	}
}

// planGateOutcome submits a Terraform apply under policies, lets the plan gate run against a
// runner reporting summary, and reports whether the proposed apply was held. An apply that was not
// held must also have run, and one that was held must not have.
func planGateOutcome(t *testing.T, policies policy.Store, summary string) bool {
	t.Helper()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := &planGateRunner{summary: summary}
	d := New(store, runner, nil, WithPolicies(policies))
	defer d.Close()
	created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
		run.WithCommand("infra/prod"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if plan := waitTerminal(t, store, created.ID); plan.Status != run.StatusSucceeded {
		t.Fatalf("plan run status = %q, want succeeded", plan.Status)
	}
	proposal := waitProposal(t, store, created.ID)
	stored, err := store.Get(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Get(proposal) error = %v", err)
	}
	if stored.Status == run.StatusPendingApproval {
		time.Sleep(100 * time.Millisecond)
		if n := runner.applies.Load(); n != 0 {
			t.Errorf("%d applies ran while the apply was held", n)
		}
		return true
	}
	if applied := waitTerminal(t, store, stored.ID); applied.Status != run.StatusSucceeded {
		t.Fatalf("proposed apply status = %q, want succeeded", applied.Status)
	}
	return false
}

// TestARegoPolicyReadingReversibilityFetchesThePlaybook pins that a Rego policy deciding on the
// reversibility grade gets the same fresh project checkout a YAML reversibility floor gets, since
// the grade reads the playbook, and that one which never reads the grade does not cost a fetch for
// a real run. A dry run is read under every Rego policy, because whether its playbook forces real
// tasks under check mode decides whether the policy judges it as the real run it partly is.
func TestARegoPolicyReadingReversibilityFetchesThePlaybook(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the module body.
		Body string
		// DryRun makes the judged run a dry run.
		DryRun bool
		// WantGrades is whether the gate must fetch the playbook before judging.
		WantGrades bool
	}{{ // Test 0: Reads the grade.
		Body:       `hold contains "x" if input.reversibility.class == "irreversible"`,
		WantGrades: true,
	}, { // Test 1: Never reads it.
		Body:       `hold contains "x" if input.run.tool == "bash"`,
		WantGrades: false,
	}, { // Test 2: A dry run is read whatever the module reads.
		Body:       `hold contains "x" if input.run.tool == "bash"`,
		DryRun:     true,
		WantGrades: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			list, err := regoStore(t, test.Body).List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			r := &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml", DryRun: test.DryRun}
			if got := readsPlaybook(list, r); got != test.WantGrades {
				t.Errorf("readsPlaybook() = %v, want %v", got, test.WantGrades)
			}
		})
	}
}
