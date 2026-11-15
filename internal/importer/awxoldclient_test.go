package importer

import (
	"fmt"
	"strings"
	"testing"
)

// oldClientWarning is a fragment only the old-client warning carries.
const oldClientWarning = "the awx client of the Tower 3.8 era"

// oldClientDoc builds an export holding one job template and one workflow over it, in the shape
// awxkit writes. natural is the workflow's natural_key value, or empty for none, nodes are its
// nodes, and top is spliced in ahead of the other top-level keys.
func oldClientDoc(natural, nodes, top string) string {
	key := ""
	if natural != "" {
		key = `"natural_key": ` + natural + `, `
	}
	return `{` + top + `
	  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
	  "job_templates": [{"name": "a", "playbook": "a.yml", "project": "infra"}],
	  "workflow_job_templates": [{"name": "rollout", ` + key +
		`"related": {"workflow_nodes": [` + nodes + `]}}]
	}`
}

// TestAWXOldClientExportIsNamedInThePreview pins when the preview says an export looks like one
// from the awx client of the Tower 3.8 era, which leaves approval nodes out of its export.
//
// The case it exists for is an approval at the start of a workflow. The client drops the node, the
// work it guarded and its deny path arrive as steps with no dependency, and nothing in the file
// says a gate was there. The warning is the only notice, so it must fire on that file and stay
// quiet on one that proves a newer client wrote it.
func TestAWXOldClientExportIsNamedInThePreview(t *testing.T) {
	t.Parallel()
	const (
		wfKey = `{"organization": {"name": "Default", "type": "organization"}, ` +
			`"name": "rollout", "type": "workflow_job_template"}`
		jobNode = `{"identifier": "deploy", ` +
			`"unified_job_template": {"name": "a", "type": "job_template"}}`
		denyRun = `{"identifier": "page", ` +
			`"unified_job_template": {"name": "a", "type": "job_template"}}`
		gate = `{"identifier": "approve", "related": {"success_nodes": ["deploy"],
			"create_approval_template": {"name": "Approve prod", "description": "", "timeout": 0}}}`
	)
	tests := []struct {
		// Natural is the workflow's natural_key value, or empty for none.
		Natural string
		// Nodes are the workflow's nodes.
		Nodes string
		// Top is spliced in as more top-level keys.
		Top string
		// WantWarning reports whether the preview must carry the old-client warning.
		WantWarning bool
	}{{ // Test 0: The Tower 3.8 shape, with the approval that guarded both nodes gone.
		Natural: wfKey, Nodes: jobNode + ", " + denyRun, WantWarning: true,
	}, { // Test 1: An approval node proves the client is 21.4.0 or later.
		Natural: wfKey, Nodes: gate + ", " + jobNode,
	}, { // Test 2: A top-level schedules list proves a full export from 21.3.0 or later.
		Natural: wfKey, Nodes: jobNode, Top: `"schedules": [],`,
	}, { // Test 3: A null is not a list, so it is no proof.
		Natural: wfKey, Nodes: jobNode, Top: `"schedules": null,`, WantWarning: true,
	}, { // Test 4: A workflow with no natural key was not written by awxkit.
		Nodes: jobNode,
	}, { // Test 5: A null natural key is no key.
		Natural: "null", Nodes: jobNode,
	}, { // Test 6: An approval named by its natural key counts as an approval node.
		Natural: wfKey, Nodes: jobNode + `, {"identifier": "approve",
			"unified_job_template": {"name": "Approve prod", "type": "workflow_approval_template"},
			"related": {"success_nodes": ["deploy"]}}`,
	}, { // Test 7: A refused workflow is still a workflow the old client wrote.
		Natural: wfKey, Nodes: `{"identifier": "build", "unified_job_template": {"name": "a",
			"type": "job_template"}, "related": {"success_nodes": ["approve"]}}`, WantWarning: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan := workflowPlanFor(t, oldClientDoc(test.Natural, test.Nodes, test.Top))
			w, got := warningContaining(t, plan.Warnings, oldClientWarning)
			if got != test.WantWarning {
				t.Fatalf("old-client warning = %v, want %v.\nwarnings:\n%s", got,
					test.WantWarning, strings.Join(plan.Warnings, "\n"))
			}
			if !got {
				return
			}
			for _, part := range []string{"Check each workflow against the source",
				"add any missing approval back as an approval step"} {
				if !strings.Contains(w, part) {
					t.Errorf("the warning does not tell the operator what to do (%q): %s", part, w)
				}
			}
			// It is a check, not a drop the import made, and it leads the items to review so the
			// assessment's short list cannot leave it out.
			r := plan.Report()
			if len(r.NeedsReview) == 0 || r.NeedsReview[0] != w {
				t.Errorf("the warning is not the first item to review: %v", r.NeedsReview)
			}
		})
	}
}

// TestAWXOldClientWarningNeedsWorkflows pins that an awxkit export holding no workflow has nothing
// an old client could have dropped, so its preview does not carry the warning.
func TestAWXOldClientWarningNeedsWorkflows(t *testing.T) {
	t.Parallel()
	plan, err := FromAWX([]byte(`{"projects": [{"name": "infra", "scm_type": "git",
		"scm_url": "https://e.com/i.git", "natural_key": {"name": "infra", "type": "project"}}]}`),
		importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if w, ok := warningContaining(t, plan.Warnings, oldClientWarning); ok {
		t.Errorf("an export with no workflows carries the old-client warning: %s", w)
	}
}
