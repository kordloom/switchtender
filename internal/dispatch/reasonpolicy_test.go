package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAHoldCarriesTheRulesReasonRequirement pins that the reason requirement of the rules that hold
// a run is copied onto it at the hold and enforced at the decision, the strictest of them, so a
// rule edited while the run waits cannot loosen it.
func TestAHoldCarriesTheRulesReasonRequirement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Requirements are what each rule in force asks.
		Requirements []string
		// WantRequirement is what the held run carries.
		WantRequirement string
		// WantBareApproval is the error an approval with no reason meets.
		WantBareApproval error
	}{{ // Test 0: No rule asks, and a bare approval goes through.
		Requirements: []string{""}, WantRequirement: "", WantBareApproval: nil,
	}, { // Test 1: A denial-only requirement leaves a bare approval free.
		Requirements: []string{"denials"}, WantRequirement: "denials", WantBareApproval: nil,
	}, { // Test 2: The strictest rule wins, and a bare approval is refused.
		Requirements: []string{"denials", "always"}, WantRequirement: "always",
		WantBareApproval: ErrReasonRequired,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			policies := policy.NewMemStore()
			for i, req := range test.Requirements {
				if err := policies.Save(ctx, &policy.Policy{ID: fmt.Sprintf("pol_%d", i),
					Name: fmt.Sprintf("rule %d", i), MaxDestroy: policy.DisabledMaxDestroy,
					RequireReason: req, CreatedAt: time.Now()}); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
			}
			d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithPolicies(policies),
				WithNoJanitor())
			t.Cleanup(d.Close)
			held, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
				run.WithQueue("served-by-nobody"), run.WithActor("laptop"), run.WithActorType("token"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if diff := cmp.Diff(test.WantRequirement, held.RequireReason); diff != "" {
				t.Errorf("RequireReason mismatch (-want +got):\n%s", diff)
			}
			_, err = d.DecideRun(ctx, held.ID, RunDecision{Approve: true, By: approver})
			if !errors.Is(err, test.WantBareApproval) {
				t.Errorf("bare approval error = %v, want %v", err, test.WantBareApproval)
			}
		})
	}
}

// TestAnApprovalStepCarriesItsWorkflowsReasonRequirement pins that the record an approval step asks
// through carries the workflow's reason requirement, the way it carries the distinct-approver one.
func TestAnApprovalStepCarriesItsWorkflowsReasonRequirement(t *testing.T) {
	t.Parallel()
	parent := &run.Run{ID: "run_wf", Kind: run.KindPipeline, RequireReason: "denials",
		RequireDistinctApprover: true,
		Steps:                   []run.PipelineStep{{Name: "gate", Type: run.StepApproval}}}
	node := approvalStepRun(parent, parent.Steps[0], 0, time.Now())
	if node.RequireReason != "denials" || !node.RequireDistinctApprover {
		t.Errorf("step requirement = %q distinct %v, want the workflow's denials and distinct",
			node.RequireReason, node.RequireDistinctApprover)
	}
}
