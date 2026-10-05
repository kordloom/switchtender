package importer

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// awxWorkflow is an AWX workflow job template: a graph of nodes, each running a job template, wired
// by success, failure, and always edges.
type awxWorkflow struct {
	// Name is the workflow name.
	Name string `json:"name"`
	// Organization is the organization the workflow belongs to, which scopes its name.
	Organization awxRef `json:"organization"`
	// ExtraVars are the workflow's own extra vars as a YAML or JSON string.
	ExtraVars string `json:"extra_vars"`
	// Inventory references the inventory the workflow runs against, by natural key.
	Inventory awxRef `json:"inventory"`
	// SurveySpec is the workflow survey when exported at the top level.
	SurveySpec *awxSurvey `json:"survey_spec"`
	// Schedules are the workflow's own schedules when the export carries them at the top level. A
	// workflow job template is scheduled in AWX exactly as a job template is, and these are what make
	// the graph fire at all.
	Schedules []awxSchedule `json:"schedules"`
	// Nodes are the graph's nodes when the export carries them at the top level.
	Nodes []awxWorkflowNode `json:"workflow_nodes"`
	// Related carries the nodes, survey, and schedules when the export nests them instead.
	Related *awxWorkflowRelated `json:"related"`
}

// awxWorkflowRelated holds a workflow's nested assets.
type awxWorkflowRelated struct {
	awxNotifyRelated
	// WorkflowNodes are the graph's nodes.
	WorkflowNodes []awxWorkflowNode `json:"workflow_nodes"`
	// SurveySpec is the workflow survey.
	SurveySpec *awxSurvey `json:"survey_spec"`
	// Schedules are the workflow's own schedules, which awxkit writes here.
	Schedules []awxSchedule `json:"schedules"`
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
	// SummaryFields is the REST API's summary of what the node references. A node wired by id says
	// only there what kind of object its unified_job_template is.
	SummaryFields *awxNodeSummary `json:"summary_fields"`
	// templateType is the kind of object the natural key in unified_job_template declares, such as
	// job_template or workflow_job_template, or empty when the reference declared none.
	templateType string
}

// awxNodeSummary is the part of a node's REST summary fields the importer reads.
type awxNodeSummary struct {
	// UnifiedJobTemplate describes the object the node runs.
	UnifiedJobTemplate awxNodeSummaryTemplate `json:"unified_job_template"`
}

// awxNodeSummaryTemplate is the REST API's description of the object a node runs.
type awxNodeSummaryTemplate struct {
	// Name is the object's name.
	Name string `json:"name"`
	// UnifiedJobType is the kind of job the object launches, such as job, workflow_job, or
	// workflow_approval.
	UnifiedJobType string `json:"unified_job_type"`
}

// UnmarshalJSON decodes a node and records the kind of object its template reference declares.
//
// AWX keeps job templates, workflow job templates, projects, and inventory sources in separate
// namespaces, so one name can mean several objects. The reference decodes to its name, which is all
// the rest of the importer keys on, and a node running a nested workflow or a project sync resolved
// by that name alone to any job template that shared it, so the import ran different work in its
// place. The declared kind is kept beside the name so a node that runs anything but a job template
// can be refused.
func (n *awxWorkflowNode) UnmarshalJSON(b []byte) error {
	type plain awxWorkflowNode
	if err := json.Unmarshal(b, (*plain)(n)); err != nil {
		return err
	}
	var typed struct {
		UnifiedJobTemplate json.RawMessage `json:"unified_job_template"`
	}
	if err := json.Unmarshal(b, &typed); err != nil {
		return err
	}
	// A reference spelled as a name or an id declares no kind, which is not an error.
	var key struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(typed.UnifiedJobTemplate, &key) == nil {
		n.templateType = key.Type
	}
	return nil
}

// awxWorkflowNodeRelated holds a node's nested outgoing edges.
type awxWorkflowNodeRelated struct {
	// SuccessNodes, FailureNodes, and AlwaysNodes are the edges awxkit writes as natural keys.
	SuccessNodes []awxNodeRef `json:"success_nodes"`
	FailureNodes []awxNodeRef `json:"failure_nodes"`
	AlwaysNodes  []awxNodeRef `json:"always_nodes"`
	// CreateApprovalTemplate is where awxkit writes an approval node. AWX creates an approval
	// through its own endpoint, so the node carries this in place of a template reference.
	CreateApprovalTemplate *awxApprovalTemplate `json:"create_approval_template"`
}

// awxApprovalTemplate is an AWX approval node: the workflow stops there until a person approves or
// denies, and only then do the nodes after it run.
type awxApprovalTemplate struct {
	// Name is what AWX shows the approver.
	Name string `json:"name"`
	// Description is the context the author gave the approver.
	Description string `json:"description"`
	// Timeout is how many seconds the node waits before it fails the workflow, or zero for no limit.
	Timeout int64 `json:"timeout"`
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

// approvalGate returns the approval a node waits on, or nil when the node runs a template.
//
// awxkit writes an approval as the template to create in the node's place. A natural key can also
// name an approval template outright, and the REST API writes the approval template's id and says
// what it is only in the node's summary. Each is the same person the workflow waits for.
func (n awxWorkflowNode) approvalGate() *awxApprovalTemplate {
	if n.Related != nil && n.Related.CreateApprovalTemplate != nil {
		return n.Related.CreateApprovalTemplate
	}
	if n.templateType == "workflow_approval_template" {
		return &awxApprovalTemplate{Name: n.UnifiedJobTemplate.Name}
	}
	if s := n.SummaryFields; s != nil && s.UnifiedJobTemplate.UnifiedJobType == "workflow_approval" {
		return &awxApprovalTemplate{Name: s.UnifiedJobTemplate.Name}
	}
	return nil
}

// otherWork names what a node runs when the export says it is not a job template, or returns the
// empty string when it is one or the export does not say.
func (n awxWorkflowNode) otherWork() string {
	kind := n.templateType
	if kind == "" && n.SummaryFields != nil {
		kind = n.SummaryFields.UnifiedJobTemplate.UnifiedJobType
	}
	switch kind {
	case "", "job_template", "job":
		return ""
	case "workflow_job_template", "workflow_job":
		return "a nested workflow"
	case "project", "project_update":
		return "a project sync"
	case "inventory_source", "inventory_update":
		return "an inventory sync"
	case "system_job_template", "system_job":
		return "a system job"
	}
	return fmt.Sprintf("an AWX %s", kind)
}

// templateName is the name of what the node runs. The REST summary's name wins when there is one,
// since a reference spelled as an id says nothing a reader can find.
func (n awxWorkflowNode) templateName() string {
	if n.SummaryFields != nil && n.SummaryFields.UnifiedJobTemplate.Name != "" {
		return n.SummaryFields.UnifiedJobTemplate.Name
	}
	return n.UnifiedJobTemplate.Name
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

// schedules returns the workflow's own schedules from whichever place the export carried them.
//
// The struct read neither place, so a workflow's schedules were dropped without a word: the
// workflow imported, its steps and graph were right, and it never fired again. Every other thing
// this importer declines to carry is named in the report, and this one decided when the work ran.
func (w awxWorkflow) schedules() []awxSchedule {
	if len(w.Schedules) > 0 {
		return w.Schedules
	}
	if w.Related != nil {
		return w.Related.Schedules
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
	projectIDs, inventoryIDs, credentialIDs awxIDs) {
	if len(export.Workflows) == 0 {
		return
	}
	jobs := newAWXJobs(export.JobTemplates)
	workflowName := orgQualifier(awxOrgNames(export.Workflows, func(wf awxWorkflow) (string, string) {
		return wf.Organization.Name, wf.Name
	})...)
	for _, wf := range export.Workflows {
		id := p.addWorkflow(wf, workflowName(wf.Organization.Name, wf.Name), jobs, now, projectIDs,
			inventoryIDs, credentialIDs)
		if id == "" {
			p.refused++
		}
		p.addWorkflowSchedules(wf, id, inventoryIDs, now)
		if id != "" {
			p.awxNotify().templateOrg[id] = wf.Organization.Name
			if wf.Related != nil {
				p.attachAWXNotifications(fmt.Sprintf("workflow %q", wf.Name),
					&wf.Related.awxNotifyRelated, id, now)
			}
		}
	}
}

// addWorkflowSchedules maps a workflow's own schedules onto the template it imported as.
//
// A schedule fires a stored template, and a workflow template is one, so the cadence carries across
// whole rather than being reported as unmappable. The schedules are read here rather than inside
// addWorkflow because a refused workflow has no template for them to fire, and that outcome has to
// be reported rather than left as a second silent loss on top of the first.
func (p *Plan) addWorkflowSchedules(wf awxWorkflow, templateID string, inventoryIDs awxIDs,
	now time.Time) {
	scheds := wf.schedules()
	if len(scheds) == 0 {
		return
	}
	// A nameless workflow is refused before it reaches here, and quoting the name it does not have
	// would print workflow "" at the operator, naming nothing they can find in the export.
	owner := fmt.Sprintf("workflow %q", wf.Name)
	if wf.Name == "" {
		owner = "the workflow job template without a name"
	}
	if templateID == "" {
		p.warn("%s was not imported, so its %d schedule%s did not import either. The cadence it ran "+
			"on goes with it: set it again on the workflow you rebuild.",
			owner, len(scheds), plural(len(scheds)))
		return
	}
	p.addSchedules(owner, scheds, templateID, inventoryIDs, now)
}

// addWorkflow maps one workflow and returns the id of the template it created, or reports why it
// could not be mapped and returns the empty string.
func (p *Plan) addWorkflow(wf awxWorkflow, name string, jobs awxJobs, now time.Time,
	projectIDs, inventoryIDs, credentialIDs awxIDs) string {
	if name == "" {
		p.warn("a workflow job template without a name was skipped")
		return ""
	}
	nodes := wf.nodes()
	if len(nodes) == 0 {
		p.warn("workflow %q carries no nodes, so there is nothing to import", name)
		return ""
	}

	// An approval node is a person the workflow waits for partway through, and it imports as an
	// approval step: the steps after it wait for the decision, and its failure edges become the
	// steps that run when it is denied or times out. An always edge is the one shape that cannot
	// come across, since it would run the next step whatever the approver said, which is no gate,
	// so a workflow carrying one is refused and the gate is named as lost rather than weakened.
	for _, n := range nodes {
		gate := n.approvalGate()
		if gate == nil {
			continue
		}
		if len(n.alwaysEdges()) > 0 {
			p.gates = append(p.gates, name)
			p.warn("workflow %q was not imported: approval node %s, %q, runs other nodes whatever "+
				"the approver decides. An approval step here releases its approve path only on an "+
				"approval and its deny path only on a denial or a timeout, so carrying that edge "+
				"would run work nobody approved. Rebuild it on the Workflows page with the work on "+
				"the path it belongs to.", name, nodeLabel(n), oneLine(gate.Name))
			return ""
		}
	}

	// A step runs a playbook, so a node running anything else is refused before any node is looked
	// up by name. A nested workflow, a project sync, or an inventory sync can share its name with a
	// job template, and resolved by name it imported as that job template: the node ran some other
	// playbook, and a nested workflow's own approval gates were dropped with it.
	for _, n := range nodes {
		if n.approvalGate() != nil {
			continue
		}
		if work := n.otherWork(); work != "" {
			p.warn("workflow %q was not imported: node %s runs %s, %q, and a pipeline step runs a "+
				"playbook. Rebuild it on the Workflows page with that work written as steps.",
				name, nodeLabel(n), work, oneLine(n.templateName()))
			return ""
		}
	}

	// A failure edge runs work precisely because something failed. A pipeline step runs when its
	// dependencies allow it, and there is no run-because-it-failed step for a job, so a workflow
	// using one cannot be expressed and is refused rather than imported without its error handling.
	// An approval node's failure edge is its deny path, which an approval step does carry.
	for _, n := range nodes {
		if n.approvalGate() == nil && len(n.failures()) > 0 {
			p.warn("workflow %q was not imported: node %s runs other nodes on failure, which a "+
				"pipeline cannot express. Rebuild it on the Workflows page, where a step can be set "+
				"to continue on failure.", name, nodeLabel(n))
			return ""
		}
	}

	// The per-node settings a workflow template holds once are read from the nodes that run a job.
	// An approval node runs none, and reading one as a job template with no tags, limit, or
	// inventory would make every real node look like it disagreed with it.
	jobNodes := make([]awxWorkflowNode, 0, len(nodes))
	gateNames := []string{}
	for _, n := range nodes {
		if n.approvalGate() != nil {
			gateNames = append(gateNames, nodeLabel(n))
			continue
		}
		jobNodes = append(jobNodes, n)
	}
	if len(jobNodes) == 0 {
		p.warn("workflow %q was not imported: it carries only approval nodes, so there is no work "+
			"for an approval to release", name)
		return ""
	}

	// Every node must resolve to a job template in this export, or its step has no work to do.
	steps := make([]run.PipelineStep, 0, len(nodes))
	stepName := make(map[string]string, len(nodes))
	projectID := ""
	for _, n := range nodes {
		if gate := n.approvalGate(); gate != nil {
			label := nodeLabel(n)
			keys := n.keys()
			if len(keys) == 0 {
				p.warn("workflow %q was not imported: a node carries neither an id nor an identifier, "+
					"so its place in the graph cannot be resolved", name)
				return ""
			}
			for _, k := range keys {
				if _, taken := stepName[k]; taken {
					p.warn("workflow %q was not imported: two nodes share the key %q", name, k)
					return ""
				}
				stepName[k] = label
			}
			steps = append(steps, approvalStepOf(label, gate))
			continue
		}
		jt, ok := jobs.get(n.UnifiedJobTemplate)
		if !ok {
			p.warn("workflow %q was not imported: node %s runs %s, which is not a job template in "+
				"this export, so the step would have no work to do.",
				name, nodeLabel(n), jobs.keys.unresolved(n.UnifiedJobTemplate))
			return ""
		}
		if jt.Playbook == "" {
			p.warn("workflow %q was not imported: the job template %q it runs has no playbook",
				name, jt.Name)
			return ""
		}
		// A pipeline sources every step from one project, so a workflow spanning two of them cannot
		// be expressed as one template.
		if id, _ := projectIDs.get(jt.Project); id != "" {
			if projectID != "" && projectID != id {
				p.warn("workflow %q was not imported: its nodes span more than one project, and a "+
					"workflow template sources every step from one.", name)
				return ""
			}
			projectID = id
		}
		label := nodeLabel(n)
		keys := n.keys()
		if len(keys) == 0 {
			p.warn("workflow %q was not imported: a node carries neither an id nor an identifier, "+
				"so its place in the graph cannot be resolved", name)
			return ""
		}
		for _, k := range keys {
			if _, taken := stepName[k]; taken {
				p.warn("workflow %q was not imported: two nodes share the key %q", name, k)
				return ""
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
				return ""
			}
			steps[j].DependsOn = append(steps[j].DependsOn, steps[i].Name)
		}
		// An always edge is carried by letting the upstream step fail, and that flag belongs to the
		// step rather than to one edge leaving it. So a node with an always edge to one place and a
		// success edge to another cannot be expressed: setting the flag lets BOTH proceed after a
		// failure, and the step that was meant to run only on success ran on failure too. Deploy on
		// success beside notify always is an everyday shape, and it imported as deploy after a failed
		// build, silently.
		//
		// Both edges pointing at the same node is not that case. There the two statements agree about
		// one step and always is the stronger of them, which is what AWX does too, so it is carried.
		if always, successOnly := n.edgeConflict(byKey, steps); len(always) > 0 && len(successOnly) > 0 {
			p.warn("workflow %q was not imported: node %s runs %s whatever it does, and %s only when "+
				"it succeeds. A pipeline step lets everything downstream continue or nothing, so "+
				"carrying the first would let the second run after a failure. Split the node in two, "+
				"or make the always edge a success edge if that is what it meant.",
				name, nodeLabel(n), strings.Join(always, ", "), strings.Join(successOnly, ", "))
			return ""
		}
		if n.continues() {
			steps[i].ContinueOnFailure = true
		}
		// An approval node's failure edges are its deny path: the node they point at runs when the
		// approver denies the step or its timeout passes.
		if n.approvalGate() != nil {
			for _, next := range n.failures() {
				j, ok := byKey[string(next)]
				if !ok {
					p.warn("workflow %q was not imported: node %s points at node %q, which is not in "+
						"the workflow", name, nodeLabel(n), oneLine(string(next)))
					return ""
				}
				steps[j].IfDenied = append(steps[j].IfDenied, steps[i].Name)
			}
		}
	}
	for i := range steps {
		sort.Strings(steps[i].DependsOn)
		steps[i].DependsOn = dedupeStrings(steps[i].DependsOn)
		sort.Strings(steps[i].IfDenied)
		steps[i].IfDenied = dedupeStrings(steps[i].IfDenied)
	}

	// The graph is validated through the same rule the dispatcher runs it through, so an import can
	// never create a template that refuses to launch.
	if err := run.ValidatePipeline(steps); err != nil {
		p.warn("workflow %q was not imported: %v", name, err)
		return ""
	}

	// What a pipeline holds once but AWX scoped per node. Each is resolved before the template
	// exists, so a workflow that cannot be expressed is refused whole rather than planned and then
	// abandoned.
	limit, err := workflowLimit(jobNodes, jobs)
	if err != nil {
		p.warn("workflow %q was not imported: %v", name, err)
		return ""
	}
	vars, err := p.workflowVars(wf, jobNodes, jobs)
	if err != nil {
		p.warn("workflow %q was not imported: %v", name, err)
		return ""
	}
	tags, skipTags, err := workflowTags(jobNodes, jobs)
	if err != nil {
		p.warn("workflow %q was not imported: %v", name, err)
		return ""
	}
	nodeInventory, err := workflowInventory(jobNodes, jobs)
	if err != nil {
		p.warn("workflow %q was not imported: %v", name, err)
		return ""
	}
	timeout := p.workflowTimeout(name, jobNodes, jobs)

	tpl := &template.Template{
		ID: template.NewID(), Name: name, Steps: steps, ProjectID: projectID, CreatedAt: now,
		Tags: splitAWXTags(tags), SkipTags: splitAWXTags(skipTags), Timeout: timeout,
	}
	inv := wf.Inventory
	if inv.Name == "" {
		// The workflow names none, so its steps ran against whatever their own job templates named.
		// That was read nowhere and the workflow imported with no inventory at all, which is a run
		// against no hosts rather than the run AWX performed.
		inv = nodeInventory
	} else if nodeInventory.Name != "" && nodeInventory != inv {
		p.warn("workflow %q runs against inventory %q and its nodes' job templates name %q. The "+
			"workflow's own inventory wins here, as it does in AWX when the workflow sets one, so "+
			"every step now targets %q.", name, inv.Name, nodeInventory.Name, inv.Name)
	}
	if inv.Name != "" {
		if id, ok := inventoryIDs.get(inv); ok {
			tpl.InventoryID = id
		} else {
			p.warn("workflow %q references unknown inventory %s, so it imports with none", name,
				inventoryIDs.unresolved(inv))
		}
	}
	tpl.Limit = limit
	tpl.ExtraVars = vars
	creds, widened := p.workflowCredentials(name, jobNodes, jobs, credentialIDs)
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
	if len(gateNames) > 0 {
		p.carriedGates = append(p.carriedGates, name)
		p.warn("workflow %q keeps its approval %s, %s, as approval %s: the workflow stops there "+
			"until an admin approves or denies, and a denial or a timeout takes the failure path. "+
			"Who may approve follows your roles and approval policies, not the AWX approval role.",
			name, "node"+plural(len(gateNames)), strings.Join(gateNames, ", "),
			"step"+plural(len(gateNames)))
	}
	return tpl.ID
}

// workflowTags returns the tags and skip tags every node in the workflow shares, refusing when they
// disagree.
//
// A pipeline step carries no tags of its own, and the template holds one set for the whole workflow,
// so per-node tags either agree or the workflow cannot be expressed. They were read from each job
// template and then dropped, which is the change that does not look like one: a node running only
// the tasks tagged "config" imported as a node running the whole playbook, and a node skipping the
// tasks tagged "destroy" imported as a node that runs them. It is the same class as the limit, and
// it is refused the same way rather than planned and quietly widened.
// sameTagSet reports whether two AWX tag fields name the same tags. The field is an unordered,
// comma separated list somebody typed, so "deploy,config" and "config, deploy" are one set written
// two ways, and comparing the raw strings refused a workflow whose nodes agree.
func sameTagSet(a, b string) bool {
	left, right := splitAWXTags(a), splitAWXTags(b)
	if len(left) != len(right) {
		return false
	}
	sorted := append([]string(nil), left...)
	other := append([]string(nil), right...)
	sort.Strings(sorted)
	sort.Strings(other)
	return slices.Equal(sorted, other)
}

func workflowTags(nodes []awxWorkflowNode, jobs awxJobs) (tags, skip string, err error) {
	first := jobs.of(nodes[0].UnifiedJobTemplate)
	tags, skip = first.JobTags, first.SkipTags
	for _, n := range nodes[1:] {
		jt := jobs.of(n.UnifiedJobTemplate)
		if !sameTagSet(jt.JobTags, tags) {
			return "", "", fmt.Errorf("node %s runs the tags %q while another runs %q, and a "+
				"workflow template applies one set of tags to every step",
				nodeLabel(n), oneLine(jt.JobTags), oneLine(tags))
		}
		if !sameTagSet(jt.SkipTags, skip) {
			return "", "", fmt.Errorf("node %s skips the tags %q while another skips %q, and a "+
				"workflow template applies one set of skipped tags to every step",
				nodeLabel(n), oneLine(jt.SkipTags), oneLine(skip))
		}
	}
	return tags, skip, nil
}

// workflowLimit returns the one host limit a workflow's nodes agree on.
//
// A limit is pipeline wide here: every child run copies the parent's and a step never names its own.
// So nodes limited differently, or a limit on some nodes and not others, cannot be expressed. Both
// are refused rather than resolved, because every way of resolving them runs some node against hosts
// its operator had excluded.
func workflowLimit(nodes []awxWorkflowNode, jobs awxJobs) (string, error) {
	limit := jobs.of(nodes[0].UnifiedJobTemplate).Limit
	for _, n := range nodes[1:] {
		if l := jobs.of(n.UnifiedJobTemplate).Limit; l != limit {
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
	jobs awxJobs) (map[string]any, error) {
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
		jt := jobs.of(n.UnifiedJobTemplate)
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
	jobs awxJobs, credentialIDs awxIDs) (ids []string, widened bool) {
	seen := map[string]bool{}
	perNode := 0
	for _, n := range nodes {
		refs := append(append([]awxRef(nil), jobs.of(n.UnifiedJobTemplate).Credentials...),
			n.Credentials...)
		count := 0
		for _, ref := range refs {
			if ref.Name == "" {
				continue
			}
			id, ok := credentialIDs.get(ref)
			if !ok {
				p.warn("workflow %q node %s references unknown credential %s, so its steps import "+
					"without it", name, nodeLabel(n), oneLine(credentialIDs.unresolved(ref)))
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

// successEdges returns the edges taken only when this node succeeds, from wherever the export put them.
func (n awxWorkflowNode) successEdges() []awxNodeRef {
	out := append([]awxNodeRef(nil), n.SuccessNodes...)
	if n.Related != nil {
		out = append(out, n.Related.SuccessNodes...)
	}
	return out
}

// alwaysEdges returns the edges taken whatever this node did, from wherever the export put them.
func (n awxWorkflowNode) alwaysEdges() []awxNodeRef {
	out := append([]awxNodeRef(nil), n.AlwaysNodes...)
	if n.Related != nil {
		out = append(out, n.Related.AlwaysNodes...)
	}
	return out
}

// edgeConflict returns the step names this node runs unconditionally and the ones it runs only on
// success, resolved through the graph so two keys naming one node are seen as one node.
//
// A step named by both kinds of edge is not in the second list. The two statements are about the same
// step, always is the stronger, and carrying it is what AWX does.
func (n awxWorkflowNode) edgeConflict(byKey map[string]int,
	steps []run.PipelineStep) (always, successOnly []string) {
	alwaysAt := map[int]bool{}
	for _, next := range n.alwaysEdges() {
		if j, ok := byKey[string(next)]; ok {
			alwaysAt[j] = true
		}
	}
	for j := range alwaysAt {
		always = append(always, quoteName(steps[j].Name))
	}
	seen := map[int]bool{}
	for _, next := range n.successEdges() {
		j, ok := byKey[string(next)]
		if !ok || alwaysAt[j] || seen[j] {
			continue
		}
		seen[j] = true
		successOnly = append(successOnly, quoteName(steps[j].Name))
	}
	sort.Strings(always)
	sort.Strings(successOnly)
	return always, successOnly
}

// workflowTimeout returns the runtime cap a workflow's nodes agree on, warning when they do not.
//
// A job template's timeout imports on a plain template and was the one per-node field a workflow threw
// away in silence: a node capped at five minutes became a step with no cap at all, so a hung task that
// AWX would have killed runs until something else stops it. The pipeline holds one cap, so nodes that
// disagree cannot all be honored.
//
// The longest is taken rather than refusing the workflow, because a cap is a safety net and the longest
// of them still bounds every step, while refusing would drop a graph over a field that does not change
// what runs. The report names what changed for which node, since a step whose cap grew is a step whose
// hang now lasts longer than its operator set.
func (p *Plan) workflowTimeout(name string, nodes []awxWorkflowNode,
	jobs awxJobs) int {
	longest, at := 0, ""
	shortened := false
	for _, n := range nodes {
		seconds := int(jobs.of(n.UnifiedJobTemplate).Timeout)
		if seconds <= 0 {
			continue
		}
		if longest == 0 {
			longest, at = seconds, nodeLabel(n)
			continue
		}
		if seconds != longest {
			shortened = true
		}
		if seconds > longest {
			longest, at = seconds, nodeLabel(n)
		}
	}
	if shortened {
		p.warn("workflow %q has nodes with different runtime caps, and a workflow template holds one, "+
			"so every step is capped at the longest of them: %d seconds, from node %s. A step that had "+
			"a shorter cap can now run longer than it could in AWX.", name, longest, at)
	}
	return longest
}

// workflowInventory returns the one inventory a workflow's nodes agree on.
//
// A pipeline holds one inventory and a step names none, so this is the fourth thing AWX scopes per
// node that has to be resolved before the template exists. It was the only one that was not: the
// workflow's own inventory was read and every node's job template inventory was discarded without a
// word. A workflow that named no inventory of its own therefore imported with none, which is not the
// run AWX performed but a run against nothing, and a workflow whose nodes targeted different fleets
// imported as though they targeted one.
//
// Nodes that disagree refuse the workflow, for the reason the limit does: every way of resolving them
// runs some step against hosts its operator had not chosen. A node whose job template names no
// inventory does not force a refusal, because AWX would have taken the workflow's own in that case.
func workflowInventory(nodes []awxWorkflowNode, jobs awxJobs) (awxRef, error) {
	var found awxRef
	for _, n := range nodes {
		inv := jobs.of(n.UnifiedJobTemplate).Inventory
		if inv.Name == "" {
			continue
		}
		if found.Name == "" {
			found = inv
			continue
		}
		// Compared with its organization, so two teams' inventories that share a name are two
		// inventories, which is what they are.
		if inv != found {
			return awxRef{}, fmt.Errorf("node %s runs against inventory %q while another runs "+
				"against %q, and a workflow template applies one inventory to every step",
				nodeLabel(n), oneLine(inv.Name), oneLine(found.Name))
		}
	}
	return found, nil
}

// approvalStepOf maps an AWX approval node to an approval step. AWX shows its approver the
// approval's name and description, so both reach the step's description, and its timeout carries as
// the step's, which takes the deny path when it passes exactly as AWX takes the failure path.
func approvalStepOf(label string, gate *awxApprovalTemplate) run.PipelineStep {
	description := gate.Name
	if gate.Description != "" {
		if description != "" {
			description += ": "
		}
		description += gate.Description
	}
	if len(description) > run.MaxApprovalDescription {
		// Cut on the byte limit the step is validated against, then drop any rune the cut split.
		description = strings.ToValidUTF8(description[:run.MaxApprovalDescription], "")
	}
	timeout := gate.Timeout
	if timeout < 0 {
		timeout = 0
	}
	if timeout > math.MaxInt32 {
		timeout = math.MaxInt32
	}
	return run.PipelineStep{Name: label, Type: run.StepApproval, Description: description,
		ApprovalTimeout: int(timeout)}
}
