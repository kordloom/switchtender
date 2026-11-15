package importer

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// ascenderNodeKey writes the natural key ascender-kit names a node of the rollout workflow by.
func ascenderNodeKey(identifier string) string {
	return `{"workflow_job_template": {"organization": {"name": "Default", "type": "organization"},
		"name": "rollout", "type": "workflow_job_template"}, "identifier": "` + identifier + `",
		"type": "workflow_job_template_node"}`
}

// ascenderApproval writes an approval node the way ascender-kit writes one, with the settings given
// spliced into its create_approval_template block.
func ascenderApproval(settings string) string {
	return `{"identifier": "approve", "max_retries": 0, "related": {
		"success_nodes": [` + ascenderNodeKey("ship") + `], "failure_nodes": [], "always_nodes": [],
		"condition_nodes": [], "create_approval_template": {"name": "Approve prod",
		"description": "", "timeout": 600` + settings + `}}}`
}

// ascenderShip is the node an approval in these tests releases.
const ascenderShip = `{"identifier": "ship", "unified_job_template": "b", "max_retries": 0,
	"related": {"success_nodes": [], "failure_nodes": [], "always_nodes": [], "condition_nodes": []}}`

// TestAscenderWorkflowGate pins what an Ascender 25.5.0 or later workflow imports as. A conditional
// connector and an approval needing more than one approver change what runs if carried as a plain
// step, so each refuses the workflow by name. An approval that approves itself on a timeout and an
// approval's context template import with a warning. Every node ascender-kit writes carries a
// connector list, empty when it has none, and that list alone must not refuse anything.
//
//nolint:funlen // Test function.
func TestAscenderWorkflowGate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Nodes are the workflow's nodes.
		Nodes string
		// WantImported reports whether the workflow imports.
		WantImported bool
		// WantLeftOut is a fragment of a warning the report must count as not coming across.
		WantLeftOut string
		// WantReview is a fragment of a warning the report must count as worth reviewing.
		WantReview string
		// WantLostGate reports whether the assessment lists the workflow's gate as lost.
		WantLostGate bool
	}{{ // Test 0: An empty connector list on every node, as ascender-kit writes it, imports.
		Nodes: `{"identifier": "build", "unified_job_template": "a", "max_retries": 0,
			"related": {"success_nodes": [` + ascenderNodeKey("ship") + `], "failure_nodes": [],
			"always_nodes": [], "condition_nodes": []}}, ` + ascenderShip,
		WantImported: true,
	}, { // Test 1: A conditional connector refuses the workflow and names both ends.
		Nodes: `{"identifier": "build", "unified_job_template": "a", "related": {
			"success_nodes": [], "condition_nodes": [{"id": ` + ascenderNodeKey("ship") + `,
			"trigger": "success", "artifact_key": "deploy", "operator": "eq",
			"expected_value": "yes"}]}}, ` + ascenderShip,
		WantLeftOut: `node build leads to "ship" through a conditional connector`,
	}, { // Test 2: A REST connector names its target by id, and the target is named as its step.
		Nodes: `{"id": 1, "unified_job_template": "a", "condition_nodes": [2]},
			{"id": 2, "unified_job_template": "b"}`,
		WantLeftOut: `node node-1 leads to "node-2" through a conditional connector`,
	}, { // Test 3: A connector whose target cannot be read still refuses.
		Nodes: `{"identifier": "build", "unified_job_template": "a",
			"related": {"condition_nodes": [{"trigger": "success"}]}}`,
		WantLeftOut: "leads to 1 node it does not name through a conditional connector",
	}, { // Test 4: An approval needing two approvers refuses the workflow.
		Nodes: ascenderApproval(`, "required_approvals": 2, "on_timeout": "deny"`) + ", " +
			ascenderShip,
		WantLeftOut:  `approval node approve, "Approve prod", waits for 2 distinct people`,
		WantLostGate: true,
	}, { // Test 5: One approver is the approval step this imports as.
		Nodes: ascenderApproval(`, "required_approvals": 1, "on_timeout": "deny",
			"context_template": ""`) + ", " + ascenderShip,
		WantImported: true,
	}, { // Test 6: Zero needs one person's approval in Ascender as well.
		Nodes:        ascenderApproval(`, "required_approvals": 0`) + ", " + ascenderShip,
		WantImported: true,
	}, { // Test 7: A REST approval names its approvers in its summary.
		Nodes: `{"id": 1, "unified_job_template": 7, "success_nodes": [2],
			"summary_fields": {"unified_job_template": {"id": 7, "name": "Approve prod",
			"unified_job_type": "workflow_approval", "required_approvals": 3}}},
			{"id": 2, "unified_job_template": "b"}`,
		WantLeftOut:  `approval node node-1, "Approve prod", waits for 3 distinct people`,
		WantLostGate: true,
	}, { // Test 8: An approval that approves itself on a timeout imports and is named.
		Nodes: ascenderApproval(`, "required_approvals": 1, "on_timeout": "approve"`) + ", " +
			ascenderShip,
		WantImported: true,
		WantReview:   `approval node approve, "Approve prod", approves itself in Ascender`,
	}, { // Test 9: A context template does not come across and is named.
		Nodes: ascenderApproval(`, "context_template": "Deploying {{ version }}"`) + ", " +
			ascenderShip,
		WantImported: true,
		WantLeftOut:  `approval node approve, "Approve prod", shows its approver a context template`,
	}, { // Test 10: A REST approval's summary says it approves itself on a timeout.
		Nodes: `{"id": 1, "unified_job_template": 7, "success_nodes": [2],
			"summary_fields": {"unified_job_template": {"id": 7, "name": "Approve prod",
			"unified_job_type": "workflow_approval", "on_timeout": "approve",
			"context_template": "{{ version }}"}}},
			{"id": 2, "unified_job_template": "b"}`,
		WantImported: true,
		WantReview:   `approval node node-1, "Approve prod", approves itself in Ascender`,
		WantLeftOut:  `approval node node-1, "Approve prod", shows its approver a context template`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan := workflowPlanFor(t, workflowDoc(test.Nodes))
			if got := workflowWasImported(plan); got != test.WantImported {
				t.Fatalf("workflow imported = %v, want %v.\nwarnings:\n%s", got, test.WantImported,
					strings.Join(plan.Warnings, "\n"))
			}
			r := plan.Report()
			if test.WantLeftOut != "" {
				if _, ok := warningContaining(t, r.LeftOut, `workflow "rollout"`,
					test.WantLeftOut); !ok {
					t.Errorf("no warning counted as left out says %q.\nleft out:\n%s",
						test.WantLeftOut, strings.Join(r.LeftOut, "\n"))
				}
			}
			if test.WantReview != "" {
				if _, ok := warningContaining(t, r.NeedsReview, `workflow "rollout"`,
					test.WantReview); !ok {
					t.Errorf("no warning counted as worth reviewing says %q.\nto review:\n%s",
						test.WantReview, strings.Join(r.NeedsReview, "\n"))
				}
			}
			// A refused workflow's gate is not one that comes across.
			if !test.WantImported && slices.Contains(plan.carriedGates, "rollout") {
				t.Errorf("a refused workflow is listed as carrying its gate: %v", plan.carriedGates)
			}
			// A gate refused for its own shape is a lost gate the assessment names up front.
			if got := slices.Contains(plan.gates, "rollout"); got != test.WantLostGate {
				t.Errorf("workflow listed as a lost gate = %v, want %v: %v", got, test.WantLostGate,
					plan.gates)
			}
			// Only the cases that name a setting carry an Ascender warning.
			for _, w := range plan.Warnings {
				wanted := (test.WantLeftOut != "" && strings.Contains(w, test.WantLeftOut)) ||
					(test.WantReview != "" && strings.Contains(w, test.WantReview))
				for _, part := range []string{"approves itself in Ascender",
					"shows its approver a context template", "conditional connector",
					"distinct people"} {
					if strings.Contains(w, part) && !wanted {
						t.Errorf("an Ascender warning nobody asked for: %s", w)
					}
				}
			}
		})
	}
}

// TestAscenderNodeRetriesBecomeStepRetries pins that a node Ascender runs again after a failure
// imports as a step that gets the same retries, held to the ceiling Ascender itself sets. An
// approval node is never retried in Ascender, and an approval step takes no retries here.
func TestAscenderNodeRetriesBecomeStepRetries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Nodes are the workflow's nodes.
		Nodes string
		// WantSteps are the steps the workflow imports as.
		WantSteps []run.PipelineStep
	}{{ // Test 0: Three retries carry, and a node with none keeps none.
		Nodes: `{"identifier": "build", "unified_job_template": "a", "max_retries": 3,
			"related": {"success_nodes": ["ship"], "condition_nodes": []}},
			{"identifier": "ship", "unified_job_template": "b", "max_retries": 0}`,
		WantSteps: []run.PipelineStep{
			{Name: "build", Playbook: "a.yml", Retries: 3},
			{Name: "ship", Playbook: "b.yml", DependsOn: []string{"build"}},
		},
	}, { // Test 1: More than Ascender allows is held to its ceiling, and a negative is none.
		Nodes: `{"identifier": "build", "unified_job_template": "a", "max_retries": 500,
			"related": {"success_nodes": ["ship"]}},
			{"identifier": "ship", "unified_job_template": "b", "max_retries": -2}`,
		WantSteps: []run.PipelineStep{
			{Name: "build", Playbook: "a.yml", Retries: maxAscenderRetries},
			{Name: "ship", Playbook: "b.yml", DependsOn: []string{"build"}},
		},
	}, { // Test 2: An approval node's retries are not carried onto its approval step.
		Nodes: `{"identifier": "approve", "max_retries": 2, "related": {"success_nodes": ["ship"],
			"create_approval_template": {"name": "Approve prod", "required_approvals": 1}}},
			{"identifier": "ship", "unified_job_template": "b", "max_retries": 1}`,
		WantSteps: []run.PipelineStep{
			{Name: "approve", Type: run.StepApproval, Description: "Approve prod"},
			{Name: "ship", Playbook: "b.yml", Retries: 1, DependsOn: []string{"approve"}},
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
		})
	}
}
