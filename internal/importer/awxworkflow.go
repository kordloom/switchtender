package importer

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
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
	SuccessNodes []awxNodeRef `json:"success_nodes"`
	// FailureNodes are the nodes that run when this one fails, which a pipeline cannot express.
	FailureNodes []awxNodeRef `json:"failure_nodes"`
	// AlwaysNodes are the nodes that run whatever this one did.
	AlwaysNodes []awxNodeRef `json:"always_nodes"`
	// Related carries the outgoing edges when the export nests them, which awxkit does.
	Related *awxWorkflowNodeRelated `json:"related"`
	// ExtraData are the node's own extra vars, which AWX layers over its job template's.
	ExtraData json.RawMessage `json:"extra_data"`
	// Credentials are credentials the node adds on top of its job template's, by natural key.
	Credentials []awxRef `json:"credentials"`
}

// awxWorkflowNodeRelated holds a node's nested outgoing edges.
type awxWorkflowNodeRelated struct {
	// SuccessNodes, FailureNodes, and AlwaysNodes are the edges awxkit writes as natural keys.
	SuccessNodes []awxNodeRef `json:"success_nodes"`
	FailureNodes []awxNodeRef `json:"failure_nodes"`
	AlwaysNodes  []awxNodeRef `json:"always_nodes"`
}

// awxNodeRef references a workflow node by whichever key the export used.
//
// A top-level export wires the graph with integer ids. awxkit strips every id and wires it with
// identifiers instead, so a decoder that reads only integers saw every node as id zero and refused
// any workflow with more than one node. Both shapes normalize to one string here so the graph can be
// keyed once.
type awxNodeRef string

// UnmarshalJSON decodes a node reference from an id number, an identifier string, or an object
// carrying either.
func (r *awxNodeRef) UnmarshalJSON(b []byte) error {
	var id int64
	if json.Unmarshal(b, &id) == nil {
		*r = awxNodeRef(nodeKeyForID(id))
		return nil
	}
	var str string
	if json.Unmarshal(b, &str) == nil {
		*r = awxNodeRef(str)
		return nil
	}
	var obj struct {
		Identifier string `json:"identifier"`
		ID         int64  `json:"id"`
	}
	if json.Unmarshal(b, &obj) == nil {
		if obj.Identifier != "" {
			*r = awxNodeRef(obj.Identifier)
			return nil
		}
		if obj.ID != 0 {
			*r = awxNodeRef(nodeKeyForID(obj.ID))
		}
	}
	return nil
}

// nodeKeyForID spells a numeric node id as a graph key, kept in one place so the key a node
// registers and the key an edge resolves against cannot drift apart.
func nodeKeyForID(id int64) string { return fmt.Sprintf("id-%d", id) }

// successors returns the node's success and always edges, from whichever place the export carried
// them. Both are plain dependencies, so they are read together.
func (n awxWorkflowNode) successors() []awxNodeRef {
	out := append([]awxNodeRef(nil), n.SuccessNodes...)
	out = append(out, n.AlwaysNodes...)
	if n.Related != nil {
		out = append(out, n.Related.SuccessNodes...)
		out = append(out, n.Related.AlwaysNodes...)
	}
	return out
}

// failures returns the node's failure edges from whichever place the export carried them.
func (n awxWorkflowNode) failures() []awxNodeRef {
	out := append([]awxNodeRef(nil), n.FailureNodes...)
	if n.Related != nil {
		out = append(out, n.Related.FailureNodes...)
	}
	return out
}

// continues reports whether the node has an always edge, which makes it a step downstream work may
// proceed past even when it fails.
func (n awxWorkflowNode) continues() bool {
	if len(n.AlwaysNodes) > 0 {
		return true
	}
	return n.Related != nil && len(n.Related.AlwaysNodes) > 0
}

// keys returns every key an edge may use to name this node, so a graph wired by id resolves against
// a node carrying an identifier and the other way round.
func (n awxWorkflowNode) keys() []string {
	out := make([]string, 0, 2)
	if n.Identifier != "" {
		out = append(out, n.Identifier)
	}
	if n.ID != 0 {
		out = append(out, nodeKeyForID(n.ID))
	}
	return out
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
	projectIDs, inventoryIDs, credentialIDs map[string]string) {
	if len(export.Workflows) == 0 {
		return
	}
	jobs := make(map[string]awxJobTemplate, len(export.JobTemplates))
	for _, jt := range export.JobTemplates {
		jobs[jt.Name] = jt
	}
	for _, wf := range export.Workflows {
		p.addWorkflow(wf, jobs, now, projectIDs, inventoryIDs, credentialIDs)
	}
}

// addWorkflow maps one workflow, or reports why it could not be mapped.
func (p *Plan) addWorkflow(wf awxWorkflow, jobs map[string]awxJobTemplate, now time.Time,
	projectIDs, inventoryIDs, credentialIDs map[string]string) {
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
		if len(n.failures()) > 0 {
			p.warn("workflow %q was not imported: node %s runs other nodes on failure, which a "+
				"pipeline cannot express. Rebuild it on the Workflows page, where a step can be set "+
				"to continue on failure.", name, nodeLabel(n))
			return
		}
	}

	// Every node must resolve to a job template in this export, or its step has no work to do.
	steps := make([]run.PipelineStep, 0, len(nodes))
	stepName := make(map[string]string, len(nodes))
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
		keys := n.keys()
		if len(keys) == 0 {
			p.warn("workflow %q was not imported: a node carries neither an id nor an identifier, "+
				"so its place in the graph cannot be resolved", name)
			return
		}
		for _, k := range keys {
			if _, taken := stepName[k]; taken {
				p.warn("workflow %q was not imported: two nodes share the key %q", name, k)
				return
			}
			stepName[k] = label
		}
		// A node running a check-mode job template must stay check mode. A step carries its own
		// DryRun and the dispatcher honors it, so dropping this imported a node that made no changes
		// as one that makes them, against whatever the workflow targets.
		steps = append(steps, run.PipelineStep{
			Name: label, Playbook: jt.Playbook, DryRun: jt.checkMode(),
		})
	}

	// Wire the edges. A success edge is a plain dependency. An always edge is a dependency whose
	// upstream is allowed to fail, which is what continue-on-failure means on the upstream step.
	byKey := make(map[string]int, len(nodes)*2)
	for i, n := range nodes {
		for _, k := range n.keys() {
			byKey[k] = i
		}
	}
	for i, n := range nodes {
		for _, next := range n.successors() {
			j, ok := byKey[string(next)]
			if !ok {
				p.warn("workflow %q was not imported: node %s points at node %q, which is not in "+
					"the workflow", name, nodeLabel(n), oneLine(string(next)))
				return
			}
			steps[j].DependsOn = append(steps[j].DependsOn, steps[i].Name)
		}
		if n.continues() {
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

	// What a pipeline holds once but AWX scoped per node. Each is resolved before the template
	// exists, so a workflow that cannot be expressed is refused whole rather than planned and then
	// abandoned.
	limit, err := workflowLimit(nodes, jobs)
	if err != nil {
		p.warn("workflow %q was not imported: %v", name, err)
		return
	}
	vars, err := p.workflowVars(wf, nodes, jobs)
	if err != nil {
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
	tpl.Limit = limit
	tpl.ExtraVars = vars
	creds, widened := p.workflowCredentials(name, nodes, jobs, credentialIDs)
	tpl.CredentialIDs = creds
	if widened {
		p.warn("workflow %q imported with the credentials of all its nodes on every step. AWX gave "+
			"each node its own; a workflow template gives every step one set, so a step can now reach "+
			"a credential it could not before. Split it into separate workflows if that matters.", name)
	}
	if s := wf.survey(); s != nil {
		tpl.Survey = p.mapSurvey(awxJobTemplate{Name: name, SurveySpec: s})
	}
	p.Templates = append(p.Templates, tpl)
	p.warn("workflow %q imported as a workflow template with %d steps. Check the graph before you "+
		"run it: AWX node convergence and per-node prompts do not carry across.", name, len(steps))
}

// workflowLimit returns the one host limit a workflow's nodes agree on.
//
// A limit is pipeline wide here: every child run copies the parent's and a step never names its own.
// So nodes limited differently, or a limit on some nodes and not others, cannot be expressed. Both
// are refused rather than resolved, because every way of resolving them runs some node against hosts
// its operator had excluded.
func workflowLimit(nodes []awxWorkflowNode, jobs map[string]awxJobTemplate) (string, error) {
	limit := jobs[string(nodes[0].UnifiedJobTemplate)].Limit
	for _, n := range nodes[1:] {
		if l := jobs[string(n.UnifiedJobTemplate)].Limit; l != limit {
			return "", fmt.Errorf("node %s is limited to %q while another is limited to %q, and a "+
				"workflow template applies one limit to every step",
				nodeLabel(n), oneLine(l), oneLine(limit))
		}
	}
	return limit, nil
}

// workflowVars merges the extra vars a workflow's nodes carry into the one set its steps share.
//
// Every step receives the pipeline's extra vars and carries none of its own, so the vars a node held
// on its job template or on the node itself have nowhere else to go. Merging is faithful while the
// nodes agree, since a variable a step never reads costs it nothing. Two nodes setting one key to
// different values is a real conflict: one of them would run with the other's value and neither the
// import nor the run would say so, and a variable is exactly the kind of thing that decides which
// environment a playbook touches.
func (p *Plan) workflowVars(wf awxWorkflow, nodes []awxWorkflowNode,
	jobs map[string]awxJobTemplate) (map[string]any, error) {
	merged := map[string]any{}
	add := func(src string, in map[string]any) error {
		for _, k := range slices.Sorted(maps.Keys(in)) {
			if old, seen := merged[k]; seen && !reflect.DeepEqual(old, in[k]) {
				return fmt.Errorf("%s sets extra var %q to a value another node sets differently, and "+
					"a workflow template gives every step one set of vars", src, oneLine(k))
			}
			merged[k] = in[k]
		}
		return nil
	}

	own, err := parseExtraVars(wf.ExtraVars)
	if err != nil {
		return nil, fmt.Errorf("its own extra_vars could not be parsed: %w", err)
	}
	if err := add("the workflow", own); err != nil {
		return nil, err
	}
	for _, n := range nodes {
		jt := jobs[string(n.UnifiedJobTemplate)]
		tv, err := parseExtraVars(jt.ExtraVars)
		if err != nil {
			return nil, fmt.Errorf("the extra_vars of job template %q could not be parsed: %w",
				jt.Name, err)
		}
		nv, err := parseNodeData(n.ExtraData)
		if err != nil {
			return nil, fmt.Errorf("the extra_data of node %s could not be parsed: %w", nodeLabel(n), err)
		}
		// AWX layers a node's own extra_data over its job template's vars, so within one node the
		// node wins and that is settled before anything is compared. Only the value the node actually
		// runs with is then held against the other nodes.
		effective := make(map[string]any, len(tv)+len(nv))
		maps.Copy(effective, tv)
		maps.Copy(effective, nv)
		if err := add(fmt.Sprintf("node %s", nodeLabel(n)), effective); err != nil {
			return nil, err
		}
	}
	return merged, nil
}

// parseNodeData reads a workflow node's extra_data, which AWX writes as a JSON object rather than
// the string form job templates use.
func parseNodeData(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// workflowCredentials returns the credentials every step of the imported workflow will hold, and
// whether that is more than any single node held.
//
// Credentials are pipeline wide: each child run receives the parent's set. AWX scopes them per node,
// so the union is the only expressible mapping and it widens what a step can reach. Importing none
// instead would leave every step unable to authenticate, which is why the union is taken, but the
// widening is a real change in what a step may touch, so the caller says so rather than leaving the
// operator to discover it.
func (p *Plan) workflowCredentials(name string, nodes []awxWorkflowNode,
	jobs map[string]awxJobTemplate, credentialIDs map[string]string) (ids []string, widened bool) {
	seen := map[string]bool{}
	perNode := 0
	for _, n := range nodes {
		refs := append(append([]awxRef(nil), jobs[string(n.UnifiedJobTemplate)].Credentials...),
			n.Credentials...)
		count := 0
		for _, ref := range refs {
			if ref == "" {
				continue
			}
			id, ok := credentialIDs[string(ref)]
			if !ok {
				p.warn("workflow %q node %s references unknown credential %q, so its steps import "+
					"without it", name, nodeLabel(n), oneLine(string(ref)))
				continue
			}
			count++
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if count > perNode {
			perNode = count
		}
	}
	return ids, len(ids) > perNode
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
