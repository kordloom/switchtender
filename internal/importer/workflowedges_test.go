package importer

import (
	"strings"
	"testing"
)

// TestANodeThatRunsOneThingAlwaysAndAnotherOnSuccessIsRefused covers a graph that imported running a
// step after the failure it was meant to stop at.
//
// An always edge is carried by letting the upstream step fail, and continue-on-failure is a property of
// the step, not of one edge leaving it. So a node with an always edge to one place and a success edge to
// another has no faithful translation: setting the flag lets both proceed after a failure. Build, then
// deploy on success and notify always, is the shape most AWX shops have, and it imported as a deploy
// that runs after a failed build with nothing warned.
func TestANodeThatRunsOneThingAlwaysAndAnotherOnSuccessIsRefused(t *testing.T) {
	t.Parallel()
	const nodes = `{"id": 1, "identifier": "build", "unified_job_template": "a",
        "success_nodes": [2], "always_nodes": [3]},
      {"id": 2, "identifier": "deploy", "unified_job_template": "a"},
      {"id": 3, "identifier": "notify", "unified_job_template": "b"}`
	plan := workflowPlanFor(t, workflowDoc(nodes))
	if workflowWasImported(plan) {
		t.Fatal("the workflow imported. Its deploy step depends on a build that is allowed to fail, " +
			"so a failed build now deploys.")
	}
	why, ok := warningContaining(t, plan.Warnings, "whatever it does")
	if !ok {
		t.Fatalf("the conflict was not reported.\nwarnings: %v", plan.Warnings)
	}
	for _, want := range []string{`"notify"`, `"deploy"`} {
		if !strings.Contains(why, want) {
			t.Errorf("the refusal does not name %s, so the operator cannot see which two edges "+
				"disagree: %s", want, why)
		}
	}
}

// TestBothEdgeKindsPointingAtOneNodeIsNotAConflict holds the refusal to the case that is actually
// ambiguous.
//
// Two edges naming the same node are two statements about one step, always is the stronger of them, and
// AWX runs it either way. Refusing here would drop a graph that translates exactly.
func TestBothEdgeKindsPointingAtOneNodeIsNotAConflict(t *testing.T) {
	t.Parallel()
	const nodes = `{"id": 1, "identifier": "build", "unified_job_template": "a",
        "success_nodes": [2], "always_nodes": [2]},
      {"id": 2, "identifier": "report", "unified_job_template": "b"}`
	plan := workflowPlanFor(t, workflowDoc(nodes))
	tpl := workflowTemplate(t, plan)
	if len(tpl.Steps) != 2 {
		t.Fatalf("steps = %d, want 2: %v", len(tpl.Steps), plan.Warnings)
	}
	if !tpl.Steps[0].ContinueOnFailure {
		t.Error("the always edge was not carried, so the report step does not run when the build fails")
	}
	if len(tpl.Steps[1].DependsOn) != 1 {
		t.Errorf("the report step depends on %v, want the build once", tpl.Steps[1].DependsOn)
	}
}

// TestAWorkflowCarriesTheRuntimeCapItsNodesHad covers the fourth per-node field, which was dropped in
// silence while the other three were resolved.
//
// A job template's timeout imports on a plain template. On a workflow it went nowhere: a node AWX would
// have killed after five minutes became a step with no cap, so a hung task runs until something else
// stops it. A pipeline holds one cap, so nodes that disagree take the longest and the report says which
// node it came from, since a step whose cap grew is a step whose hang now lasts longer than it could.
func TestAWorkflowCarriesTheRuntimeCapItsNodesHad(t *testing.T) {
	t.Parallel()
	t.Run("one cap, carried", func(t *testing.T) {
		t.Parallel()
		const doc = `{
          "projects": [{"name": "one", "scm_type": "git", "scm_url": "https://e.com/one.git"}],
          "job_templates": [
            {"name": "a", "playbook": "a.yml", "project": "one", "timeout": 300},
            {"name": "b", "playbook": "b.yml", "project": "one", "timeout": 300}
          ],
          "workflow_job_templates": [{"name": "rollout", "workflow_nodes": [
            {"id": 1, "identifier": "first", "unified_job_template": "a", "success_nodes": [2]},
            {"id": 2, "identifier": "second", "unified_job_template": "b"}
          ]}]
        }`
		tpl := workflowTemplate(t, workflowPlanFor(t, doc))
		if tpl.Timeout != 300 {
			t.Errorf("timeout = %d, want 300. Without it a hung step runs past the cap its operator "+
				"set in AWX.", tpl.Timeout)
		}
	})
	t.Run("caps that disagree take the longest and say so", func(t *testing.T) {
		t.Parallel()
		const doc = `{
          "projects": [{"name": "one", "scm_type": "git", "scm_url": "https://e.com/one.git"}],
          "job_templates": [
            {"name": "a", "playbook": "a.yml", "project": "one", "timeout": 300},
            {"name": "b", "playbook": "b.yml", "project": "one", "timeout": 1800}
          ],
          "workflow_job_templates": [{"name": "rollout", "workflow_nodes": [
            {"id": 1, "identifier": "quick", "unified_job_template": "a", "success_nodes": [2]},
            {"id": 2, "identifier": "slow", "unified_job_template": "b"}
          ]}]
        }`
		plan := workflowPlanFor(t, doc)
		tpl := workflowTemplate(t, plan)
		if tpl.Timeout != 1800 {
			t.Errorf("timeout = %d, want the longest of the two caps so every step is still bounded",
				tpl.Timeout)
		}
		if _, ok := warningContaining(t, plan.Warnings, "different runtime caps"); !ok {
			t.Errorf("a step's cap grew and nothing said so.\nwarnings: %v", plan.Warnings)
		}
	})
}
