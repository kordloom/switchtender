package importer

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// TestAWXApprovalNodeBecomesAnApprovalStep pins the shape an AWX approval node imports as: a named
// approval step carrying what AWX showed its approver and its timeout, the nodes after it waiting
// for the approval, and its failure nodes on its deny path. A graph that came across as anything
// else would run work at a different moment from the one the estate's owner set.
func TestAWXApprovalNodeBecomesAnApprovalStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Nodes are the workflow's AWX nodes.
		Nodes string
		// WantSteps are the steps the workflow imports as.
		WantSteps []run.PipelineStep
	}{{ // Test 0: Build, an approval with a description and a timeout, then ship or page.
		Nodes: `{"identifier": "build", "unified_job_template": "a",
			"related": {"success_nodes": ["approve"]}},
			{"identifier": "approve", "related": {"success_nodes": ["ship"], "failure_nodes": ["page"],
			"create_approval_template": {"name": "Approve prod", "description": "Check the canary",
			"timeout": 3600}}},
			{"identifier": "ship", "unified_job_template": "b"},
			{"identifier": "page", "unified_job_template": "a"}`,
		WantSteps: []run.PipelineStep{
			{Name: "build", Playbook: "a.yml"},
			{Name: "approve", Type: run.StepApproval, Description: "Approve prod: Check the canary",
				ApprovalTimeout: 3600, DependsOn: []string{"build"}},
			{Name: "ship", Playbook: "b.yml", DependsOn: []string{"approve"}},
			{Name: "page", Playbook: "a.yml", IfDenied: []string{"approve"}},
		},
	}, { // Test 1: A REST approval node wired by id carries the name its summary gives it.
		Nodes: `{"id": 1, "unified_job_template": "a", "success_nodes": [2]},
			{"id": 2, "unified_job_template": 7, "success_nodes": [3],
			"summary_fields": {"unified_job_template": {"id": 7, "name": "Approve prod",
			"unified_job_type": "workflow_approval"}}},
			{"id": 3, "unified_job_template": "b"}`,
		WantSteps: []run.PipelineStep{
			{Name: "node-1", Playbook: "a.yml"},
			{Name: "node-2", Type: run.StepApproval, Description: "Approve prod",
				DependsOn: []string{"node-1"}},
			{Name: "node-3", Playbook: "b.yml", DependsOn: []string{"node-2"}},
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan := workflowPlanFor(t, workflowDoc(test.Nodes))
			var got []run.PipelineStep
			for _, tpl := range plan.Templates {
				if tpl.Name == "rollout" {
					got = tpl.Steps
				}
			}
			if diff := cmp.Diff(test.WantSteps, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("steps mismatch (-want +got):\n%s\nwarnings: %v", diff, plan.Warnings)
			}
			if err := run.ValidatePipeline(got); err != nil {
				t.Errorf("the imported graph does not validate: %v", err)
			}
			if len(plan.Assess().Governance.CarriedGates) != 1 {
				t.Errorf("carried gates = %v, want the workflow named once",
					plan.Assess().Governance.CarriedGates)
			}
		})
	}
}
