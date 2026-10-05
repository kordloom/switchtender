package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// refusalEntry is the part of a recorded refusal the test compares.
type refusalEntry struct {
	// Actor, ActorType, and OnBehalfOf are who the chain names.
	Actor, ActorType, OnBehalfOf string
	// Suffix is the path after the refused run's id.
	Suffix string
}

// TestADenyPolicyRecordsItsRefusalInTheChain pins the chain entry a refusal leaves. A refused
// submission used to leave only the request the middleware records, so an exported chain could say
// a launch was attempted and nothing about the rule, or the exact Rego bundle, that turned it away,
// and the rule in force could be edited a minute later.
func TestADenyPolicyRecordsItsRefusalInTheChain(t *testing.T) {
	t.Parallel()
	rego := regoStore(t, `deny contains "agents may not run bash" if {
	input.actor.kind == "agent"
	input.run.tool == "bash"
}`)
	listed, err := rego.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	yaml := policy.NewMemStore()
	if err := yaml.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "no agent bash/shells", ActorKind: policy.ActorKindAgent,
		Tool: "bash", Effect: policy.EffectDeny, MaxDestroy: policy.DisabledMaxDestroy,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	tests := []struct {
		// Policies are the rules in force.
		Policies policy.Store
		// Steps, when set, submit a workflow whose step the rule refuses.
		Steps []run.PipelineStep
		// WantEntries are the refusals the chain must hold.
		WantEntries []refusalEntry
	}{{ // Test 0: A Rego refusal names the policy and the full bundle digest.
		Policies: rego,
		WantEntries: []refusalEntry{{
			Actor: "system:policy", ActorType: "system", OnBehalfOf: "bot",
			Suffix: "/decision/refused/rego/sha256:" + listed[0].Rego.Digest() + "/policy/rego gate",
		}},
	}, { // Test 1: A YAML refusal names the rule, and carries no bundle.
		Policies: yaml,
		WantEntries: []refusalEntry{{
			Actor: "system:policy", ActorType: "system", OnBehalfOf: "bot",
			Suffix: "/decision/refused/policy/no agent bash/shells",
		}},
	}, { // Test 2: A workflow refused for one of its steps records the refusal on the workflow.
		Policies: yaml,
		Steps:    []run.PipelineStep{{Name: "shell", Tool: "bash", Command: "uptime"}},
		WantEntries: []refusalEntry{{
			Actor: "system:policy", ActorType: "system", OnBehalfOf: "bot",
			Suffix: "/decision/refused/policy/no agent bash/shells",
		}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			audits := audit.NewMemStore()
			d := New(run.NewMemStore(), &fakeRunnerLister{hosts: []string{"a"}}, nil,
				WithPolicies(test.Policies), WithAudits(audits))
			defer d.Close()
			var err error
			if test.Steps != nil {
				_, err = d.SubmitPipeline(context.Background(), "workflow", "", test.Steps,
					run.WithActor("bot"), run.WithActorType("agent"))
			} else {
				_, err = d.Submit(context.Background(), "", "", run.WithTool("bash"),
					run.WithCommand("uptime"), run.WithActor("bot"), run.WithActorType("agent"))
			}
			if !errors.Is(err, ErrPolicyDenied) {
				t.Fatalf("submit error = %v, want ErrPolicyDenied", err)
			}
			entries, err := audits.List(context.Background(), 100)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var got []refusalEntry
			for _, e := range entries {
				if e.Method != audit.MethodDecision {
					continue
				}
				rest, ok := strings.CutPrefix(e.Path, "/runs/")
				_, suffix, found := strings.Cut(rest, "/")
				if !ok || !found || e.ContentDigest == "" {
					t.Errorf("refusal entry %q carries no run or no committed body", e.Path)
				}
				got = append(got, refusalEntry{Actor: e.Actor, ActorType: e.ActorType,
					OnBehalfOf: e.OnBehalfOf, Suffix: "/" + suffix})
			}
			if diff := cmp.Diff(test.WantEntries, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("refusals in the chain (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRefusalPath pins the refusal path's shape: the bundle, when there is one, before the rule, so
// a rule's name cannot pose as a digest. The register and the dossier read it as a decision they do
// not credit to anybody.
func TestRefusalPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Rule is the refusing rule's name.
		Rule string
		// Bundle is the Rego bundle digest, empty for a YAML rule.
		Bundle string
		// WantResult is the path.
		WantResult string
	}{{ // Test 0: A YAML rule.
		Rule: "no prod", WantResult: "/runs/run_1/decision/refused/policy/no prod",
	}, { // Test 1: A Rego policy and its bundle.
		Rule: "guardrails", Bundle: "ab12",
		WantResult: "/runs/run_1/decision/refused/rego/sha256:ab12/policy/guardrails",
	}, { // Test 2: A name that tries to pose as a bundle stays inside the rule's segment.
		Rule:       "x/rego/sha256:forged",
		WantResult: "/runs/run_1/decision/refused/policy/x/rego/sha256:forged",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := outcome.RefusalPath("run_1", test.Rule, test.Bundle); got != test.WantResult {
				t.Errorf("RefusalPath() = %q, want %q", got, test.WantResult)
			}
		})
	}
}
