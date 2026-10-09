package importer

import (
	"fmt"
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

// TestWorkflowReadsCredentialsFromTheRelatedBlock pins the shape awxkit writes. A job template's
// credentials and a node's own credentials are exportable relations, so a real export carries both
// under related and never at the top level. A workflow carries the credentials of the templates it
// runs and of its nodes from there, including one a node supplies because its template prompts.
func TestWorkflowReadsCredentialsFromTheRelatedBlock(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the subtest.
		Name string
		// A and B are the fields spliced into the two job templates.
		A, B string
		// Extra is spliced into the workflow, before its nodes.
		Extra string
		// Nodes replaces the workflow's nodes when set.
		Nodes string
		// WantCount is how many credentials the workflow template carries.
		WantCount int
	}{{ // Test 0: Both job templates carry their credential under related.
		Name: "job templates under related",
		A:    `, "related": {"credentials": [{"name": "vault"}]}`,
		B:    `, "related": {"credentials": [{"name": "aws"}]}`, WantCount: 2,
	}, { // Test 1: A node adds a credential under related, on a template that names none.
		Name: "node under related",
		Nodes: `[{"identifier": "first", "unified_job_template": {"name": "a"},
		  "related": {"credentials": [{"name": "vault"}], "success_nodes": [{"identifier": "second"}]}},
		 {"identifier": "second", "unified_job_template": {"name": "b"}}]`,
		WantCount: 1,
	}, { // Test 2: The top-level shape still reads, so a hand-written export is unchanged.
		Name: "node at the top level",
		Nodes: `[{"identifier": "first", "unified_job_template": {"name": "a"},
		  "credentials": [{"name": "vault"}], "success_nodes": [{"identifier": "second"}]},
		 {"identifier": "second", "unified_job_template": {"name": "b"}}]`,
		WantCount: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			export := `{
			  "credentials": [
			    {"name": "vault", "credential_type": "Vault", "inputs": {}},
			    {"name": "aws", "credential_type": "Amazon Web Services", "inputs": {}}
			  ],
			  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
			  "job_templates": [` + jobTemplate("a", test.A) + `,` + jobTemplate("b", test.B) + `],
			  "workflow_job_templates": [{"name": "rollout", "related": {"workflow_nodes": ` +
				nodesOrDefault(test.Nodes) + `}}]
			}`
			plan, err := FromAWX([]byte(export), time.Unix(0, 0).UTC())
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			tpl := workflowTemplate(t, plan)
			if got := len(tpl.CredentialIDs); got != test.WantCount {
				t.Errorf("credentials = %d, want %d; a step with none cannot authenticate and "+
					"nothing in the plan says why.\nwarnings: %v", got, test.WantCount, plan.Warnings)
			}
			if _, ok := warningContaining(t, plan.Warnings, "related.credentials"); ok {
				t.Errorf("the credentials are still reported as a field this importer does not read: %v",
					plan.Warnings)
			}
		})
	}
}

// nodesOrDefault returns the workflow nodes a test gave, or two nodes wired first to second that
// add no credential of their own.
func nodesOrDefault(nodes string) string {
	if nodes != "" {
		return nodes
	}
	return `[{"identifier": "first", "unified_job_template": {"name": "a"},
	  "related": {"success_nodes": [{"identifier": "second"}]}},
	 {"identifier": "second", "unified_job_template": {"name": "b"}}]`
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

// TestWorkflowTagsAreCarriedOrRefused checks the job tags, which a pipeline holds once the same way
// it holds the host limit.
//
// A node's tags decide which plays and tasks of its playbook actually run, so dropping them is not a
// cosmetic loss. A node running only the tasks tagged "config" imports as a node running the whole
// playbook, and a node skipping the tasks tagged "destroy" imports as one that runs them. They were
// read from each job template and then never used, with nothing in the plan to say so, which is the
// worst shape a migration defect takes: the import reports success and the first run does more than
// the job it replaced.
func TestWorkflowTagsAreCarriedOrRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		A, B         string
		WantTags     []string
		WantSkipTags []string
		Refused      bool
	}{
		{Name: "no tags anywhere", A: "", B: ""},
		{
			Name: "both nodes agree", A: `, "job_tags": "config"`, B: `, "job_tags": "config"`,
			WantTags: []string{"config"},
		},
		{
			Name: "both nodes skip the same", A: `, "skip_tags": "destroy"`,
			B:            `, "skip_tags": "destroy"`,
			WantSkipTags: []string{"destroy"},
		},
		{
			Name: "nodes run different tags", A: `, "job_tags": "config"`,
			B: `, "job_tags": "deploy"`, Refused: true,
		},
		{
			Name: "one node is untagged", A: `, "job_tags": "config"`, B: "", Refused: true,
		},
		{
			Name: "nodes skip different tags", A: `, "skip_tags": "destroy"`,
			B: `, "skip_tags": "migrate"`, Refused: true,
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			plan := workflowExport(t, jobTemplate("a", test.A), jobTemplate("b", test.B), "")
			if test.Refused {
				for _, tpl := range plan.Templates {
					if tpl.Name == "rollout" {
						t.Fatalf("a workflow whose nodes carry different tags imported anyway, "+
							"with tags %v and skip tags %v", tpl.Tags, tpl.SkipTags)
					}
				}
				if !strings.Contains(strings.Join(plan.Warnings, "\n"), "tags") {
					t.Errorf("no warning said why it was refused: %v", plan.Warnings)
				}
				return
			}
			tpl := workflowTemplate(t, plan)
			if diff := cmp.Diff(test.WantTags, tpl.Tags, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("tags mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantSkipTags, tpl.SkipTags, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("skip tags mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestWorkflowInventoryIsCarriedOrRefused covers the fourth thing AWX scopes per node, and the only
// one this importer never resolved.
//
// A pipeline holds one inventory and a step names none, so a workflow's nodes either agree on where
// they run or the workflow cannot be expressed. That is the same rule the limit, the tags and the
// extra vars already follow. Inventory was not resolved at all: the workflow's own was read and every
// node's job template inventory was discarded silently.
//
// Two shapes were wrong because of it. A workflow naming no inventory of its own imported with none,
// which is a run against no hosts rather than the run AWX performed. And a workflow whose nodes
// targeted different fleets imported as though they targeted one, with nothing said.
func TestWorkflowInventoryIsCarriedOrRefused(t *testing.T) {
	t.Parallel()
	// Its own export rather than the shared helper, because the inventories have to be declared at
	// the top level and that helper splices only into the workflow object.
	build := func(t *testing.T, nodeA, nodeB, workflow string) *Plan {
		t.Helper()
		export := `{
		  "inventories": [
		    {"name": "prod", "host_vars": {}, "hosts": [{"name": "p1"}]},
		    {"name": "staging", "host_vars": {}, "hosts": [{"name": "s1"}]}
		  ],
		  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
		  "job_templates": [` + jobTemplate("a", nodeA) + `,` + jobTemplate("b", nodeB) + `],
		  "workflow_job_templates": [{
		    "name": "rollout",` + workflow + `
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
	tests := []struct {
		// Name says what the case proves.
		Name string
		// A and B are the two nodes' job template fields.
		A, B string
		// Workflow is what the workflow itself names, spliced in beside its name.
		Workflow string
		// WantInventory is the inventory the imported template must target, by name.
		WantInventory string
		// Refused is set when the workflow must not import at all.
		Refused bool
		// WantWarning is a phrase the report must carry.
		WantWarning string
	}{{
		Name: "nodes agree and the workflow names none",
		A:    `, "inventory": "prod"`, B: `, "inventory": "prod"`,
		WantInventory: "prod",
	}, {
		Name: "nodes disagree",
		A:    `, "inventory": "prod"`, B: `, "inventory": "staging"`,
		Refused: true, WantWarning: "runs against inventory",
	}, {
		Name: "the workflow's own inventory wins and says so",
		A:    `, "inventory": "staging"`, B: `, "inventory": "staging"`,
		Workflow: ` "inventory": "prod",`, WantInventory: "prod",
		WantWarning: "wins here",
	}, {
		Name: "a node naming none does not refuse, because AWX would take the workflow's",
		A:    `, "inventory": "prod"`, B: "",
		WantInventory: "prod",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			plan := build(t, test.A, test.B, test.Workflow)
			warnings := strings.Join(plan.Warnings, "\n")
			if test.WantWarning != "" && !strings.Contains(warnings, test.WantWarning) {
				t.Errorf("no warning mentioned %q, so the loss is not in the report: %v",
					test.WantWarning, plan.Warnings)
			}
			if test.Refused {
				for _, tpl := range plan.Templates {
					if tpl.Name == "rollout" {
						t.Fatalf("a workflow whose nodes run against different inventories imported "+
							"anyway, targeting inventory id %q, so some step now runs against hosts "+
							"its operator did not choose", tpl.InventoryID)
					}
				}
				return
			}
			tpl := workflowTemplate(t, plan)
			if tpl.InventoryID == "" {
				t.Fatalf("the workflow imported with no inventory, so it runs against no hosts "+
					"rather than the fleet AWX ran it against. warnings: %v", plan.Warnings)
			}
			var gotName string
			for _, inv := range plan.Inventories {
				if inv.ID == tpl.InventoryID {
					gotName = inv.Name
				}
			}
			if gotName != test.WantInventory {
				t.Errorf("workflow targets inventory %q, want %q", gotName, test.WantInventory)
			}
		})
	}
}
