package importer

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// workflowDoc builds an export whose single workflow carries the nodes given, alongside two job
// templates and a spare object so an unimported workflow does not empty the plan.
func workflowDoc(nodes string) string {
	return `{
      "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
      "job_templates": [
        {"name": "a", "playbook": "a.yml", "project": "infra"},
        {"name": "b", "playbook": "b.yml", "project": "infra"}
      ],
      "workflow_job_templates": [{"name": "rollout", "workflow_nodes": [` + nodes + `]}]
    }`
}

// workflowPlanFor maps one workflow export and fails the test if the document itself is refused.
func workflowPlanFor(t *testing.T, doc string) *Plan {
	t.Helper()
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v\ndocument:\n%s", err, doc)
	}
	return plan
}

// workflowWasImported reports whether the plan holds the workflow template.
func workflowWasImported(p *Plan) bool {
	for _, tpl := range p.Templates {
		if tpl.Name == "rollout" {
			return true
		}
	}
	return false
}

// TestWorkflowIsImportedWholeOrNotAtAll pins every refusal in the workflow mapper. A partially
// mapped graph is the dangerous outcome: it looks like the workflow the operator had and runs a
// subset of it, so anything this cannot place refuses the whole workflow rather than reducing it.
//
//nolint:funlen // Test function.
func TestWorkflowIsImportedWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Nodes        string
		WantImported bool
		WantWarning  string
	}{
		{Name: "two nodes wired by id",
			Nodes: `{"id": 1, "unified_job_template": "a", "success_nodes": [2]},
				{"id": 2, "unified_job_template": "b"}`,
			WantImported: true}, // Test 0.
		{Name: "failure edge refused",
			Nodes: `{"id": 1, "unified_job_template": "a", "failure_nodes": [2]},
				{"id": 2, "unified_job_template": "b"}`,
			WantWarning: "runs other nodes on failure"}, // Test 1.
		{Name: "nested failure edge refused",
			Nodes: `{"id": 1, "unified_job_template": "a",
				"related": {"failure_nodes": [2]}}, {"id": 2, "unified_job_template": "b"}`,
			WantWarning: "runs other nodes on failure"}, // Test 2.
		{Name: "unknown job template refused",
			Nodes:       `{"id": 1, "unified_job_template": "missing"}`,
			WantWarning: "which is not a job template in this export"}, // Test 3.
		{Name: "edge to a node outside the graph refused",
			Nodes:       `{"id": 1, "unified_job_template": "a", "success_nodes": [99]}`,
			WantWarning: "which is not in the workflow"}, // Test 4.
		{Name: "node with neither id nor identifier refused",
			Nodes:       `{"unified_job_template": "a"}`,
			WantWarning: "carries neither an id nor an identifier"}, // Test 5.
		{Name: "two nodes sharing a key refused",
			Nodes: `{"id": 1, "identifier": "same", "unified_job_template": "a"},
				{"id": 2, "identifier": "same", "unified_job_template": "b"}`,
			WantWarning: "two nodes share the key"}, // Test 6.
		{Name: "always edge makes the upstream continue on failure",
			Nodes: `{"id": 1, "unified_job_template": "a", "always_nodes": [2]},
				{"id": 2, "unified_job_template": "b"}`,
			WantImported: true}, // Test 7.
		{Name: "identifier wired graph",
			Nodes: `{"identifier": "first", "unified_job_template": "a",
				"related": {"success_nodes": ["second"]}},
				{"identifier": "second", "unified_job_template": "b"}`,
			WantImported: true}, // Test 8: awxkit strips every id and wires by identifier.
		{Name: "cycle refused",
			Nodes: `{"id": 1, "unified_job_template": "a", "success_nodes": [2]},
				{"id": 2, "unified_job_template": "b", "success_nodes": [1]}`,
			WantWarning: "was not imported"}, // Test 9: the dispatcher's own rule refuses it.
		{Name: "self edge refused",
			Nodes:       `{"id": 1, "unified_job_template": "a", "success_nodes": [1]}`,
			WantWarning: "was not imported"}, // Test 10.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan := workflowPlanFor(t, workflowDoc(test.Nodes))
			if got := workflowWasImported(plan); got != test.WantImported {
				t.Fatalf("workflow imported = %v, want %v.\nwarnings: %v",
					got, test.WantImported, plan.Warnings)
			}
			if test.WantWarning == "" {
				return
			}
			if _, ok := warningContaining(t, plan.Warnings, `workflow "rollout"`,
				test.WantWarning); !ok {
				t.Errorf("missing %q.\nwarnings: %v", test.WantWarning, plan.Warnings)
			}
		})
	}
}

// TestWorkflowWithoutAName pins the two shapes that have nothing to import, so neither becomes a
// nameless template nor a silently dropped workflow.
func TestWorkflowWithoutAName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Workflow    string
		WantWarning string
	}{
		{Name: "no name", Workflow: `{"workflow_nodes": [{"id": 1,
			"unified_job_template": "a"}]}`,
			WantWarning: "a workflow job template without a name was skipped"}, // Test 0.
		{Name: "no nodes", Workflow: `{"name": "rollout"}`,
			WantWarning: `workflow "rollout" carries no nodes`}, // Test 1.
		{Name: "empty node list", Workflow: `{"name": "rollout", "workflow_nodes": []}`,
			WantWarning: `workflow "rollout" carries no nodes`}, // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := `{
				"projects": [{"name": "infra", "scm_type": "git",
					"scm_url": "https://e.com/i.git"}],
				"job_templates": [{"name": "a", "playbook": "a.yml", "project": "infra"}],
				"workflow_job_templates": [` + test.Workflow + `]}`
			plan := workflowPlanFor(t, doc)
			if workflowWasImported(plan) {
				t.Fatalf("a workflow with nothing to import was created.\nwarnings: %v",
					plan.Warnings)
			}
			if _, ok := warningContaining(t, plan.Warnings, test.WantWarning); !ok {
				t.Errorf("missing %q.\nwarnings: %v", test.WantWarning, plan.Warnings)
			}
		})
	}
}

// TestWorkflowRefusesANodeWithNoPlaybook pins that a step with no work to do is never created. A
// step carrying an empty playbook would report success without running anything, which is the shape
// this importer refuses everywhere else too.
func TestWorkflowRefusesANodeWithNoPlaybook(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
      "job_templates": [{"name": "a", "playbook": "", "project": "infra"}],
      "workflow_job_templates": [{"name": "rollout",
        "workflow_nodes": [{"id": 1, "unified_job_template": "a"}]}]
    }`
	plan := workflowPlanFor(t, doc)
	if workflowWasImported(plan) {
		t.Fatal("a workflow whose node has no playbook was imported")
	}
	if _, ok := warningContaining(t, plan.Warnings, "has no playbook"); !ok {
		t.Errorf("the missing playbook was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestWorkflowSpanningTwoProjectsIsRefused pins that a workflow whose nodes live in different
// projects is not imported. A pipeline sources every step from one project, so any single project
// chosen for it would run some node's playbook out of a checkout that does not hold it.
func TestWorkflowSpanningTwoProjectsIsRefused(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [
        {"name": "one", "scm_type": "git", "scm_url": "https://e.com/one.git"},
        {"name": "two", "scm_type": "git", "scm_url": "https://e.com/two.git"}
      ],
      "job_templates": [
        {"name": "a", "playbook": "a.yml", "project": "one"},
        {"name": "b", "playbook": "b.yml", "project": "two"}
      ],
      "workflow_job_templates": [{"name": "rollout", "workflow_nodes": [
        {"id": 1, "unified_job_template": "a", "success_nodes": [2]},
        {"id": 2, "unified_job_template": "b"}
      ]}]
    }`
	plan := workflowPlanFor(t, doc)
	if workflowWasImported(plan) {
		t.Fatal("a workflow spanning two projects was imported")
	}
	if _, ok := warningContaining(t, plan.Warnings, "span more than one project"); !ok {
		t.Errorf("the project conflict was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestWorkflowDependenciesAreSortedAndDeduped pins the edge wiring. A node reached by both a success
// and an always edge must be depended on once, and the order has to be stable so re-importing the
// same export twice produces the same template rather than a diff nobody caused.
func TestWorkflowDependenciesAreSortedAndDeduped(t *testing.T) {
	t.Parallel()
	const nodes = `{"id": 1, "identifier": "zeta", "unified_job_template": "a",
        "success_nodes": [3], "always_nodes": [3]},
      {"id": 2, "identifier": "alpha", "unified_job_template": "a", "success_nodes": [3]},
      {"id": 3, "identifier": "last", "unified_job_template": "b"}`
	plan := workflowPlanFor(t, workflowDoc(nodes))
	tpl := workflowTemplate(t, plan)
	if len(tpl.Steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(tpl.Steps))
	}
	var last []string
	for _, step := range tpl.Steps {
		if step.Name == "last" {
			last = step.DependsOn
		}
	}
	if diff := cmp.Diff([]string{"alpha", "zeta"}, last, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("dependencies mismatch (-want +got):\n%s", diff)
	}
	if !tpl.Steps[0].ContinueOnFailure {
		t.Error("the node with an always edge did not import as continue on failure")
	}
	if tpl.Steps[1].ContinueOnFailure {
		t.Error("a node with only a success edge imported as continue on failure")
	}
}

// TestWorkflowNodeRefIsReadInEveryShape pins the node reference decoder. A top-level export wires
// the graph with integer ids while awxkit strips every id and wires it with identifiers, so a
// decoder reading only integers saw every node as id zero and refused any workflow with more than
// one node.
func TestWorkflowNodeRefIsReadInEveryShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		JSON    string
		WantRef string
	}{
		{Name: "integer id", JSON: `7`, WantRef: "id-7"},               // Test 0.
		{Name: "identifier string", JSON: `"first"`, WantRef: "first"}, // Test 1.
		{Name: "object with identifier", JSON: `{"identifier": "first", "id": 7}`,
			WantRef: "first"}, // Test 2: the identifier is the name a person recognizes.
		{Name: "object with only an id", JSON: `{"id": 7}`, WantRef: "id-7"}, // Test 3.
		{Name: "object with neither", JSON: `{"other": 1}`, WantRef: ""},     // Test 4.
		{Name: "zero id", JSON: `0`, WantRef: "id-0"},                        // Test 5.
		{Name: "null", JSON: `null`, WantRef: "id-0"},                        // Test 6: json
		// decodes null into an int as a no-op, leaving the zero id spelling.
		{Name: "empty string", JSON: `""`, WantRef: ""}, // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var ref awxNodeRef
			if err := json.Unmarshal([]byte(test.JSON), &ref); err != nil {
				t.Fatalf("UnmarshalJSON(%s) error = %v, want none", test.JSON, err)
			}
			if string(ref) != test.WantRef {
				t.Errorf("UnmarshalJSON(%s) = %q, want %q", test.JSON, string(ref), test.WantRef)
			}
		})
	}
}

// TestWorkflowNodeExtraDataThatCannotBeReadRefusesTheWorkflow pins that unreadable node vars refuse
// the whole workflow rather than importing it without them. A step running with a different set of
// variables than it used to is a step that may touch a different environment.
func TestWorkflowNodeExtraDataThatCannotBeReadRefusesTheWorkflow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Extra       string
		WantWarning string
	}{
		{Name: "node extra data is a string",
			Extra:       `"extra_data": "not an object"`,
			WantWarning: "extra_data of node"}, // Test 0.
		{Name: "node extra data is a list",
			Extra:       `"extra_data": [1, 2]`,
			WantWarning: "extra_data of node"}, // Test 1.
		{Name: "node extra data null is fine",
			Extra: `"extra_data": null`}, // Test 2.
		{Name: "node extra data empty object is fine",
			Extra: `"extra_data": {}`}, // Test 3.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			nodes := `{"id": 1, "unified_job_template": "a", ` + test.Extra + `}`
			plan := workflowPlanFor(t, workflowDoc(nodes))
			if got := workflowWasImported(plan); got == (test.WantWarning != "") {
				t.Fatalf("workflow imported = %v with %s.\nwarnings: %v",
					got, test.Extra, plan.Warnings)
			}
			if test.WantWarning == "" {
				return
			}
			if _, ok := warningContaining(t, plan.Warnings, test.WantWarning); !ok {
				t.Errorf("missing %q.\nwarnings: %v", test.WantWarning, plan.Warnings)
			}
		})
	}
}

// TestWorkflowOwnExtraVarsThatCannotBeParsedRefuseTheWorkflow pins the same rule one level up, for
// the vars the workflow itself carries.
func TestWorkflowOwnExtraVarsThatCannotBeParsedRefuseTheWorkflow(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
      "job_templates": [{"name": "a", "playbook": "a.yml", "project": "infra"}],
      "workflow_job_templates": [{"name": "rollout", "extra_vars": "a: [1, 2",
        "workflow_nodes": [{"id": 1, "unified_job_template": "a"}]}]
    }`
	plan := workflowPlanFor(t, doc)
	if workflowWasImported(plan) {
		t.Fatal("a workflow whose own extra vars could not be parsed was imported")
	}
	if _, ok := warningContaining(t, plan.Warnings,
		"its own extra_vars could not be parsed"); !ok {
		t.Errorf("the parse failure was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestWorkflowNodeVarsWinOverItsJobTemplates pins the layering AWX applies. Within one node the
// node's own extra_data wins over its job template's vars, and only the value the node actually runs
// with is held against the other nodes, so two nodes agreeing after layering is not a conflict.
func TestWorkflowNodeVarsWinOverItsJobTemplates(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
      "job_templates": [
        {"name": "a", "playbook": "a.yml", "project": "infra", "extra_vars": "region: us-east"},
        {"name": "b", "playbook": "b.yml", "project": "infra", "extra_vars": "region: us-west"}
      ],
      "workflow_job_templates": [{"name": "rollout", "workflow_nodes": [
        {"id": 1, "unified_job_template": "a", "extra_data": {"region": "shared"}},
        {"id": 2, "unified_job_template": "b", "extra_data": {"region": "shared"}}
      ]}]
    }`
	plan := workflowPlanFor(t, doc)
	tpl := workflowTemplate(t, plan)
	want := map[string]any{"region": "shared"}
	if diff := cmp.Diff(want, tpl.ExtraVars, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("extra vars mismatch (-want +got):\n%s", diff)
	}
}

// TestWorkflowInventoryIsWiredOrReported pins that a workflow naming an inventory this export does
// not hold still imports, with the missing reference named. A workflow silently targeting nothing
// would look created and reach no hosts.
func TestWorkflowInventoryIsWiredOrReported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Inventory   string
		WantWired   bool
		WantWarning bool
	}{
		{Name: "known", Inventory: `"inventory": "prod",`, WantWired: true},     // Test 0.
		{Name: "unknown", Inventory: `"inventory": "gone",`, WantWarning: true}, // Test 1.
		{Name: "none", Inventory: ""},                                           // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := `{
				"projects": [{"name": "infra", "scm_type": "git",
					"scm_url": "https://e.com/i.git"}],
				"inventory": [{"name": "prod", "hosts": [{"name": "web01"}]}],
				"job_templates": [{"name": "a", "playbook": "a.yml", "project": "infra"}],
				"workflow_job_templates": [{"name": "rollout", ` + test.Inventory +
				`"workflow_nodes": [{"id": 1, "unified_job_template": "a"}]}]}`
			plan := workflowPlanFor(t, doc)
			tpl := workflowTemplate(t, plan)
			if got := tpl.InventoryID != ""; got != test.WantWired {
				t.Errorf("inventory wired = %v, want %v", got, test.WantWired)
			}
			_, warned := warningContaining(t, plan.Warnings, "references unknown inventory")
			if warned != test.WantWarning {
				t.Errorf("inventory warning = %v, want %v.\nwarnings: %v",
					warned, test.WantWarning, plan.Warnings)
			}
		})
	}
}

// TestWorkflowUnknownCredentialIsReportedWithoutRefusingTheWorkflow pins that a credential this
// export does not hold leaves the step without it and says so. Refusing the workflow outright would
// lose the graph over one reference, and importing quietly would leave a step unable to
// authenticate with nothing to point at.
func TestWorkflowUnknownCredentialIsReportedWithoutRefusingTheWorkflow(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
      "job_templates": [{"name": "a", "playbook": "a.yml", "project": "infra"}],
      "workflow_job_templates": [{"name": "rollout", "workflow_nodes": [
        {"id": 1, "unified_job_template": "a", "credentials": ["gone", ""]}
      ]}]
    }`
	plan := workflowPlanFor(t, doc)
	tpl := workflowTemplate(t, plan)
	if len(tpl.CredentialIDs) != 0 {
		t.Errorf("CredentialIDs = %v, want none", tpl.CredentialIDs)
	}
	if _, ok := warningContaining(t, plan.Warnings, `unknown credential "gone"`); !ok {
		t.Errorf("the unknown credential was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestWorkflowSurveyRefusesAPasswordFieldToo pins that a workflow's survey goes through the same
// refusal a job template's does. A password prompt imported as a survey field would keep the answer
// in plain text on every run, whichever kind of template carried it.
func TestWorkflowSurveyRefusesAPasswordFieldToo(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
      "job_templates": [{"name": "a", "playbook": "a.yml", "project": "infra"}],
      "workflow_job_templates": [{"name": "rollout",
        "related": {"survey_spec": {"spec": [
          {"variable": "secret", "type": "password"},
          {"variable": "env", "type": "text"}]}},
        "workflow_nodes": [{"id": 1, "unified_job_template": "a"}]}]
    }`
	plan := workflowPlanFor(t, doc)
	tpl := workflowTemplate(t, plan)
	if len(tpl.Survey) != 1 || tpl.Survey[0].Var != "env" {
		t.Fatalf("survey = %+v, want only the non-secret field", tpl.Survey)
	}
	if _, ok := warningContaining(t, plan.Warnings, `survey field "secret"`,
		"NOT imported"); !ok {
		t.Errorf("the password field was not named.\nwarnings: %v", plan.Warnings)
	}
}

// TestWorkflowImportAlwaysTellsTheOperatorToCheckTheGraph pins the closing warning. AWX node
// convergence and per-node prompts do not carry across, so an imported workflow is a thing to look
// at before it runs rather than a finished migration.
func TestWorkflowImportAlwaysTellsTheOperatorToCheckTheGraph(t *testing.T) {
	t.Parallel()
	const nodes = `{"id": 1, "unified_job_template": "a", "success_nodes": [2]},
      {"id": 2, "unified_job_template": "b"}`
	plan := workflowPlanFor(t, workflowDoc(nodes))
	if _, ok := warningContaining(t, plan.Warnings,
		"imported as a workflow template with 2 steps", "Check the graph"); !ok {
		t.Errorf("the closing warning is missing.\nwarnings: %v", plan.Warnings)
	}
}
