package importer

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/template"
)

// workflowExport builds an awxkit shaped export of one workflow whose two nodes run the job
// templates given, so each test varies only the thing it is about.
func workflowExport(t *testing.T, nodeA, nodeB, extra string) *Plan {
	t.Helper()
	export := `{
	  "credentials": [
	    {"name": "vault", "credential_type": "Vault", "inputs": {}},
	    {"name": "aws", "credential_type": "Amazon Web Services", "inputs": {}}
	  ],
	  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
	  "job_templates": [` + nodeA + `,` + nodeB + `],
	  "workflow_job_templates": [{
	    "name": "rollout",` + extra + `
	    "workflow_nodes": [
	      {"id": 1, "identifier": "first", "unified_job_template": "a", "success_nodes": [2]},
	      {"id": 2, "identifier": "second", "unified_job_template": "b"}
	    ]
	  }]
	}`
	plan, err := FromAWX([]byte(export), time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	return plan
}

// jobTemplate writes one awxkit job template with the fields a test cares about spliced in.
func jobTemplate(name, fields string) string {
	return `{"name": "` + name + `", "playbook": "` + name + `.yml", "project": "infra"` + fields + `}`
}

// workflowTemplate returns the imported workflow template, or fails with the plan's warnings so a
// refusal reports why rather than as a nil dereference.
func workflowTemplate(t *testing.T, p *Plan) *template.Template {
	t.Helper()
	for _, tpl := range p.Templates {
		if tpl.Name == "rollout" {
			return tpl
		}
	}
	t.Fatalf("the workflow was not imported; warnings: %s", strings.Join(p.Warnings, "\n"))
	return nil
}

// TestWorkflowNodeKeepsCheckMode is the safety case. A node running a check-mode job template must
// import as a step that still makes no changes.
//
// A pipeline step carries its own DryRun and the dispatcher honors it, so nothing forced this to be
// dropped. Dropping it turned a workflow an operator ran to preview changes into one that applies
// them, and the import reported success, so the first sign would be the changes themselves.
func TestWorkflowNodeKeepsCheckMode(t *testing.T) {
	t.Parallel()
	plan := workflowExport(t,
		jobTemplate("a", `, "job_type": "check"`),
		jobTemplate("b", `, "job_type": "run"`), "")
	tpl := workflowTemplate(t, plan)

	if len(tpl.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(tpl.Steps))
	}
	if !tpl.Steps[0].DryRun {
		t.Error("the check-mode node imported as a step that makes changes")
	}
	if tpl.Steps[1].DryRun {
		t.Error("the run-mode node imported as a check-mode step")
	}
}

// TestWorkflowLimitIsCarriedOrRefused checks the host limit, which a pipeline holds once and applies
// to every step.
//
// Agreeing nodes carry their limit onto the template. Nodes that disagree, or a limit on one node
// and not the other, cannot be expressed: every resolution runs some node against hosts its operator
// had excluded, so the workflow is refused instead.
func TestWorkflowLimitIsCarriedOrRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		A, B      string
		WantLimit string
		Refused   bool
	}{
		{Name: "no limit anywhere", A: "", B: ""},
		{
			Name: "both nodes agree", A: `, "limit": "canary"`, B: `, "limit": "canary"`,
			WantLimit: "canary",
		},
		{Name: "nodes disagree", A: `, "limit": "canary"`, B: `, "limit": "web"`, Refused: true},
		{Name: "one node is unlimited", A: `, "limit": "canary"`, B: "", Refused: true},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			plan := workflowExport(t, jobTemplate("a", test.A), jobTemplate("b", test.B), "")
			if test.Refused {
				for _, tpl := range plan.Templates {
					if tpl.Name == "rollout" {
						t.Fatalf("a workflow whose nodes carry different limits imported anyway, "+
							"with limit %q", tpl.Limit)
					}
				}
				if !strings.Contains(strings.Join(plan.Warnings, "\n"), "limited to") {
					t.Errorf("no warning said why it was refused: %v", plan.Warnings)
				}
				return
			}
			if got := workflowTemplate(t, plan).Limit; got != test.WantLimit {
				t.Errorf("limit = %q, want %q", got, test.WantLimit)
			}
		})
	}
}

// TestWorkflowVarsMergeOrRefuse checks the extra vars a node held, which have nowhere per-step to go.
//
// Non-conflicting vars merge, since a variable a step never reads costs it nothing. A key two nodes
// set differently is refused: one step would silently run with the other's value, and a variable is
// often what decides which environment a playbook touches.
func TestWorkflowVarsMergeOrRefuse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		A, B     string
		Workflow string
		Want     map[string]any
		Refused  bool
	}{
		{
			Name: "vars from both nodes and the workflow merge",
			A:    `, "extra_vars": "region: us-east"`,
			B:    `, "extra_vars": "tier: web"`,
			Workflow: `
	    "extra_vars": "release: 1.2.3",`,
			Want: map[string]any{"region": "us-east", "tier": "web", "release": "1.2.3"},
		},
		{
			Name: "the same value on both nodes is not a conflict",
			A:    `, "extra_vars": "region: us-east"`,
			B:    `, "extra_vars": "region: us-east"`,
			Want: map[string]any{"region": "us-east"},
		},
		{
			Name:    "two nodes set one key differently",
			A:       `, "extra_vars": "region: us-east"`,
			B:       `, "extra_vars": "region: eu-west"`,
			Refused: true,
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			plan := workflowExport(t, jobTemplate("a", test.A), jobTemplate("b", test.B), test.Workflow)
			if test.Refused {
				for _, tpl := range plan.Templates {
					if tpl.Name == "rollout" {
						t.Fatalf("a workflow whose nodes conflict on a var imported anyway, with %v",
							tpl.ExtraVars)
					}
				}
				if !strings.Contains(strings.Join(plan.Warnings, "\n"), "sets extra var") {
					t.Errorf("no warning said why it was refused: %v", plan.Warnings)
				}
				return
			}
			got := workflowTemplate(t, plan).ExtraVars
			if diff := cmp.Diff(test.Want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("extra vars mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestWorkflowNodeExtraDataReachesTheTemplate checks a node's own extra_data, which AWX writes as a
// JSON object rather than the string form a job template uses, is read at all. Before, a node's vars
// were dropped and the step ran without the values the workflow was built to pass it.
func TestWorkflowNodeExtraDataReachesTheTemplate(t *testing.T) {
	t.Parallel()
	export := `{
	  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
	  "job_templates": [` + jobTemplate("a", `, "extra_vars": "batch_size: 1\nregion: us-east"`) + `],
	  "workflow_job_templates": [{
	    "name": "rollout",
	    "workflow_nodes": [
	      {"id": 1, "identifier": "first", "unified_job_template": "a",
	       "extra_data": {"batch_size": 5, "drain": true}}
	    ]
	  }]
	}`
	plan, err := FromAWX([]byte(export), time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	// batch_size is set on both, and AWX layers the node over its job template, so 5 is the value
	// this node ran with and 1 is the one it overrode.
	want := map[string]any{"batch_size": float64(5), "drain": true, "region": "us-east"}
	if diff := cmp.Diff(want, workflowTemplate(t, plan).ExtraVars); diff != "" {
		t.Errorf("node extra_data did not reach the template (-want +got):\n%s", diff)
	}
}

// TestWorkflowCredentialsUnionIsDisclosed checks the union of the nodes' credentials reaches the
// template, and that widening it is said out loud.
//
// A pipeline hands every step one credential set, so the union is the only expressible mapping and
// importing none would leave every step unable to authenticate. It does mean a step can reach a
// credential AWX kept from it, which is a real change in what the step may touch, so an operator has
// to be told rather than left to find out.
func TestWorkflowCredentialsUnionIsDisclosed(t *testing.T) {
	t.Parallel()
	plan := workflowExport(t,
		jobTemplate("a", `, "credentials": ["vault"]`),
		jobTemplate("b", `, "credentials": ["aws"]`), "")
	tpl := workflowTemplate(t, plan)

	if len(tpl.CredentialIDs) != 2 {
		t.Fatalf("credentials = %d, want both nodes' credentials so every step can authenticate",
			len(tpl.CredentialIDs))
	}
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "could not before") {
		t.Errorf("the widening was not disclosed: %v", plan.Warnings)
	}
}

// TestWorkflowSharedCredentialIsNotAWidening checks the disclosure is not printed when every node
// already held the same credential, so the warning keeps meaning something.
func TestWorkflowSharedCredentialIsNotAWidening(t *testing.T) {
	t.Parallel()
	plan := workflowExport(t,
		jobTemplate("a", `, "credentials": ["vault"]`),
		jobTemplate("b", `, "credentials": ["vault"]`), "")
	tpl := workflowTemplate(t, plan)

	if len(tpl.CredentialIDs) != 1 {
		t.Errorf("credentials = %d, want the one both nodes shared", len(tpl.CredentialIDs))
	}
	if strings.Contains(strings.Join(plan.Warnings, "\n"), "could not before") {
		t.Errorf("a shared credential was reported as a widening: %v", plan.Warnings)
	}
}
