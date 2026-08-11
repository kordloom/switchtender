package importer

import (
	"fmt"
	"sort"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// awxWorkflow is an AWX workflow job template: a graph of nodes, each running a job template, wired
// by success, failure, and always edges.
type awxWorkflow struct {
	// Name is the workflow name.
	Name string `json:"name"`
	// ExtraVars are the workflow's own extra vars as a YAML or JSON string.
	ExtraVars string `json:"extra_vars"`
	// Inventory references the inventory the workflow runs against, by natural key.
	Inventory awxRef `json:"inventory"`
	// SurveySpec is the workflow survey when exported at the top level.
	SurveySpec *awxSurvey `json:"survey_spec"`
	// Nodes are the graph's nodes when the export carries them at the top level.
	Nodes []awxWorkflowNode `json:"workflow_nodes"`
	// Related carries the nodes and survey when the export nests them instead.
	Related *awxWorkflowRelated `json:"related"`
}

// awxWorkflowRelated holds a workflow's nested assets.
type awxWorkflowRelated struct {
	// WorkflowNodes are the graph's nodes.
	WorkflowNodes []awxWorkflowNode `json:"workflow_nodes"`
	// SurveySpec is the workflow survey.
	SurveySpec *awxSurvey `json:"survey_spec"`
}

// awxWorkflowNode is one node of a workflow graph. AWX identifies a node by an id and wires the
// graph with lists of the ids that follow it on each outcome.
type awxWorkflowNode struct {
	// ID identifies the node within its workflow.
	ID int64 `json:"id"`
	// Identifier is AWX's stable node name, used as the step name when present.
	Identifier string `json:"identifier"`
	// UnifiedJobTemplate names the job template this node runs, by natural key.
	UnifiedJobTemplate awxRef `json:"unified_job_template"`
	// SuccessNodes are the nodes that run when this one succeeds.
	SuccessNodes []int64 `json:"success_nodes"`
	// FailureNodes are the nodes that run when this one fails, which a pipeline cannot express.
	FailureNodes []int64 `json:"failure_nodes"`
	// AlwaysNodes are the nodes that run whatever this one did.
	AlwaysNodes []int64 `json:"always_nodes"`
}

// nodes returns the workflow's graph nodes from whichever place the export carried them.
func (w awxWorkflow) nodes() []awxWorkflowNode {
	if len(w.Nodes) > 0 {
		return w.Nodes
	}
	if w.Related != nil {
		return w.Related.WorkflowNodes
	}
	return nil
}

// survey returns the workflow's survey from whichever place the export carried it.
func (w awxWorkflow) survey() *awxSurvey {
	if w.SurveySpec != nil {
		return w.SurveySpec
	}
	if w.Related != nil {
		return w.Related.SurveySpec
	}
	return nil
}

// addWorkflows maps AWX workflow job templates into saved workflow templates: one template carrying
// a pipeline graph, which every launch, schedule, and trigger fires the same way.
//
// A workflow is imported whole or not at all. A partially mapped graph is the dangerous outcome: it
// looks like the workflow the operator had and runs a subset of it, so a node this importer cannot
// place means the whole workflow is reported and skipped rather than reduced. jobs indexes the
// export's job templates by name so a node's referenced playbook and project can be inlined onto its
// step, since a pipeline step carries its own work rather than pointing at another template.
func (p *Plan) addWorkflows(export awxExport, now time.Time,
	projectIDs, inventoryIDs map[string]string) {
	if len(export.Workflows) == 0 {
		return
	}
	jobs := make(map[string]awxJobTemplate, len(export.JobTemplates))
	for _, jt := range export.JobTemplates {
		jobs[jt.Name] = jt
	}
	for _, wf := range export.Workflows {
		p.addWorkflow(wf, jobs, now, projectIDs, inventoryIDs)
	}
}

// addWorkflow maps one workflow, or reports why it could not be mapped.
func (p *Plan) addWorkflow(wf awxWorkflow, jobs map[string]awxJobTemplate, now time.Time,
	projectIDs, inventoryIDs map[string]string) {
	name := wf.Name
	if name == "" {
		p.warn("a workflow job template without a name was skipped")
		return
	}
	nodes := wf.nodes()
	if len(nodes) == 0 {
		p.warn("workflow %q carries no nodes, so there is nothing to import", name)
		return
	}

	// A failure edge runs work precisely because something failed. A pipeline step runs when its
	// dependencies allow it, and there is no run-because-it-failed step, so a workflow using one
	// cannot be expressed and is refused rather than imported without its error handling.
	for _, n := range nodes {
		if len(n.FailureNodes) > 0 {
			p.warn("workflow %q was not imported: node %s runs other nodes on failure, which a "+
				"pipeline cannot express. Rebuild it on the Workflows page, where a step can be set "+
				"to continue on failure.", name, nodeLabel(n))
			return
		}
	}

	// Every node must resolve to a job template in this export, or its step has no work to do.
	steps := make([]run.PipelineStep, 0, len(nodes))
	stepName := make(map[int64]string, len(nodes))
	projectID := ""
	for _, n := range nodes {
		jt, ok := jobs[string(n.UnifiedJobTemplate)]
		if !ok {
			p.warn("workflow %q was not imported: node %s runs %q, which is not a job template in "+
				"this export, so the step would have no work to do.",
				name, nodeLabel(n), oneLine(string(n.UnifiedJobTemplate)))
			return
		}
		if jt.Playbook == "" {
			p.warn("workflow %q was not imported: the job template %q it runs has no playbook",
				name, jt.Name)
			return
		}
		// A pipeline sources every step from one project, so a workflow spanning two of them cannot
		// be expressed as one template.
		if id := projectIDs[string(jt.Project)]; id != "" {
			if projectID != "" && projectID != id {
				p.warn("workflow %q was not imported: its nodes span more than one project, and a "+
					"workflow template sources every step from one.", name)
				return
			}
			projectID = id
		}
		label := nodeLabel(n)
		if _, taken := stepName[n.ID]; taken {
			p.warn("workflow %q was not imported: two nodes share the id %d", name, n.ID)
			return
		}
		stepName[n.ID] = label
		steps = append(steps, run.PipelineStep{Name: label, Playbook: jt.Playbook})
	}

	// Wire the edges. A success edge is a plain dependency. An always edge is a dependency whose
	// upstream is allowed to fail, which is what continue-on-failure means on the upstream step.
	byID := make(map[int64]int, len(nodes))
	for i, n := range nodes {
		byID[n.ID] = i
	}
	for i, n := range nodes {
		for _, next := range append(append([]int64(nil), n.SuccessNodes...), n.AlwaysNodes...) {
			j, ok := byID[next]
			if !ok {
				p.warn("workflow %q was not imported: node %s points at node %d, which is not in "+
					"the workflow", name, nodeLabel(n), next)
				return
			}
			steps[j].DependsOn = append(steps[j].DependsOn, steps[i].Name)
		}
		if len(n.AlwaysNodes) > 0 {
			steps[i].ContinueOnFailure = true
		}
	}
	for i := range steps {
		sort.Strings(steps[i].DependsOn)
		steps[i].DependsOn = dedupeStrings(steps[i].DependsOn)
	}

	// The graph is validated through the same rule the dispatcher runs it through, so an import can
	// never create a template that refuses to launch.
	if err := run.ValidatePipeline(steps); err != nil {
		p.warn("workflow %q was not imported: %v", name, err)
		return
	}

	tpl := &template.Template{
		ID: template.NewID(), Name: name, Steps: steps, ProjectID: projectID, CreatedAt: now,
	}
	if inv := string(wf.Inventory); inv != "" {
		if id, ok := inventoryIDs[inv]; ok {
			tpl.InventoryID = id
		} else {
			p.warn("workflow %q references unknown inventory %q, so it imports with none", name, inv)
		}
	}
	if vars, err := parseExtraVars(wf.ExtraVars); err != nil {
		p.warn("workflow %q extra_vars could not be parsed: %v", name, err)
	} else {
		tpl.ExtraVars = vars
	}
	if s := wf.survey(); s != nil {
		tpl.Survey = p.mapSurvey(awxJobTemplate{Name: name, SurveySpec: s})
	}
	p.Templates = append(p.Templates, tpl)
	p.warn("workflow %q imported as a workflow template with %d steps. Check the graph before you "+
		"run it: AWX node convergence and per-node prompts do not carry across.", name, len(steps))
}

// nodeLabel names a workflow node for a step and for a warning. AWX's own identifier is used when
// the export carries one, since it is the name a person recognizes, and the node id otherwise.
func nodeLabel(n awxWorkflowNode) string {
	if n.Identifier != "" {
		return n.Identifier
	}
	return fmt.Sprintf("node-%d", n.ID)
}

// dedupeStrings removes repeats from a sorted slice, so a node reached by both a success and an
// always edge is depended on once.
func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
