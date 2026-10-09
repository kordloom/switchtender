package importer

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// The kinds AWX gives the objects a schedule fires, as its natural keys spell them.
const (
	awxTypeJobTemplate = "job_template"
	awxTypeWorkflow    = "workflow_job_template"
)

// awxTypedRef is a natural-key reference that also keeps the kind of object it names. A top-level
// schedule's unified_job_template declares one, and the kind is what decides whether the schedule
// belongs to a job template, a workflow, or something that has nothing here to fire.
type awxTypedRef struct {
	// awxRef is the natural key the reference names.
	awxRef
	// Type is the kind the reference declares, such as job_template, or empty when it declares
	// none, which a reference spelled as a bare name does.
	Type string
}

// UnmarshalJSON decodes the reference the way awxRef does and keeps the kind it declares.
func (r *awxTypedRef) UnmarshalJSON(b []byte) error {
	if err := r.awxRef.UnmarshalJSON(b); err != nil {
		return err
	}
	var key struct {
		// Type is the kind the natural key declares.
		Type string `json:"type"`
	}
	r.Type = ""
	if json.Unmarshal(b, &key) == nil {
		r.Type = key.Type
	}
	return nil
}

// awxTopSchedules holds the schedules an export writes at its top level, grouped by the object each
// one fires, until the template or workflow it belongs to takes them.
//
// awxkit writes schedules twice from AWX 22 on: nested under the job template, workflow, or
// inventory source each belongs to, and again as one top-level list. A top-level schedule that
// duplicates a nested one is the same schedule, and one only the top level carries imports with its
// owner. One that fires a project update, an inventory sync, or a system job is named as not
// imported, since nothing here stands in for those, and so is one whose reference matches no job
// template or workflow in the export.
type awxTopSchedules struct {
	// byOwner holds the schedules of each job template and workflow that the top level carries,
	// keyed by kind, organization, and name.
	byOwner map[string][]awxSchedule
	// other holds the schedules that fire a project update, an inventory sync, or a system job.
	other []awxSchedule
	// unmatched holds the schedules whose reference names no single job template or workflow.
	unmatched []awxUnmatchedSchedule
}

// awxUnmatchedSchedule is a top-level schedule whose reference matches no single job template or
// workflow in the export, with the words that say what it fires and why it was not placed.
type awxUnmatchedSchedule struct {
	// schedule is the schedule as the export wrote it.
	schedule awxSchedule
	// fires words what the reference names, such as "Deploy" in organization "Ops".
	fires string
	// why says why the reference was not placed with a template or workflow.
	why string
}

// newAWXTopSchedules groups an export's top-level schedules by what each fires. A reference that
// declares its kind is placed by it. One that declares none is placed with the job template or the
// workflow it resolves to the way every other reference resolves, by organization when it names
// one and by name alone otherwise, and only when exactly one of them answers to it.
func newAWXTopSchedules(export awxExport) *awxTopSchedules {
	top := &awxTopSchedules{byOwner: map[string][]awxSchedule{}}
	jobs, workflows := awxIDs{}, awxIDs{}
	for _, jt := range export.JobTemplates {
		jobs.set(jt.Organization.Name, jt.Name, jt.Organization.Name+orgKeySep+jt.Name)
	}
	for _, wf := range export.Workflows {
		workflows.set(wf.Organization.Name, wf.Name, wf.Organization.Name+orgKeySep+wf.Name)
	}
	owners := map[string]awxIDs{awxTypeJobTemplate: jobs, awxTypeWorkflow: workflows}
	for _, s := range export.Schedules {
		ref := s.UnifiedJobTemplate
		switch ref.Type {
		case "project", "inventory_source", "system_job_template":
			top.other = append(top.other, s)
		case awxTypeJobTemplate, awxTypeWorkflow:
			top.placeTyped(s, owners[ref.Type])
		case "":
			top.placeUntyped(s, jobs, workflows)
		default:
			top.unmatched = append(top.unmatched, awxUnmatchedSchedule{schedule: s,
				fires: fmt.Sprintf("%s of the kind %s", quoteName(ref.Name), quoteName(ref.Type)),
				why:   "which nothing here stands in for"})
		}
	}
	return top
}

// placeTyped files a schedule whose reference declares a job template or a workflow. A reference
// that matches none of that kind is still filed under the owner it names, so the report says the
// owner did not import, unless more than one organization holds that name and the reference does
// not say which.
func (top *awxTopSchedules) placeTyped(s awxSchedule, owners awxIDs) {
	ref := s.UnifiedJobTemplate
	kind := "job template"
	if ref.Type == awxTypeWorkflow {
		kind = "workflow"
	}
	if owners.ambiguous(ref.awxRef) {
		top.unmatched = append(top.unmatched, awxUnmatchedSchedule{schedule: s,
			fires: kind + " " + quoteName(ref.Name),
			why:   "a name more than one organization uses, and the reference does not say which"})
		return
	}
	key, ok := owners.get(ref.awxRef)
	if !ok {
		key = ref.Org + orgKeySep + ref.Name
	}
	top.byOwner[ref.Type+orgKeySep+key] = append(top.byOwner[ref.Type+orgKeySep+key], s)
}

// placeUntyped files a schedule whose reference declares no kind, which a bare name or an id does.
// It goes with the one job template or workflow the reference resolves to. A reference that more
// than one answers to, an id, and a name the export does not hold are each kept for the report.
func (top *awxTopSchedules) placeUntyped(s awxSchedule, jobs, workflows awxIDs) {
	ref := s.UnifiedJobTemplate.awxRef
	jobKey, isJob := jobs.get(ref)
	wfKey, isWorkflow := workflows.get(ref)
	ambiguous := jobs.ambiguous(ref) || workflows.ambiguous(ref)
	switch {
	case isJob && !isWorkflow && !ambiguous:
		top.byOwner[awxTypeJobTemplate+orgKeySep+jobKey] = append(
			top.byOwner[awxTypeJobTemplate+orgKeySep+jobKey], s)
		return
	case isWorkflow && !isJob && !ambiguous:
		top.byOwner[awxTypeWorkflow+orgKeySep+wfKey] = append(
			top.byOwner[awxTypeWorkflow+orgKeySep+wfKey], s)
		return
	}
	miss := awxUnmatchedSchedule{schedule: s, fires: quoteName(ref.Name),
		why: "which this export does not hold"}
	if ref.Org != "" {
		miss.fires += " in organization " + quoteName(ref.Org)
	}
	id, isID := strings.CutPrefix(ref.Name, "id-")
	switch {
	case isJob || isWorkflow || ambiguous:
		miss.why = "a name more than one job template or workflow in this export answers to, and " +
			"the reference does not say which"
	case isID && id != "":
		miss.fires = "AWX id " + id
		miss.why = "and a top-level schedule is placed with its template by name, not by id"
	}
	top.unmatched = append(top.unmatched, miss)
}

// take returns the schedules of one owner: the nested ones it was given, plus the top-level ones of
// that owner whose names the nested list does not already carry. The owner's top-level schedules
// are handed over once, so what is left at the end is what no owner took.
func (top *awxTopSchedules) take(kind, org, name string, nested []awxSchedule) []awxSchedule {
	if top == nil {
		return nested
	}
	key := kind + orgKeySep + org + orgKeySep + name
	extra, ok := top.byOwner[key]
	if !ok {
		return nested
	}
	delete(top.byOwner, key)
	out := nested
	for _, s := range extra {
		carried := false
		for _, n := range nested {
			if n.Name == s.Name {
				carried = true
				break
			}
		}
		if !carried {
			out = append(out, s)
		}
	}
	return out
}

// report names the top-level schedules nothing took: those of a template or workflow that did not
// import, each beside its owner, those whose reference matches no single template or workflow, each
// with the reason, and those that fire something with no equivalent here, in one line, so a reader
// can tell a schedule that went with its owner from one that had nowhere to go.
func (top *awxTopSchedules) report(p *Plan) {
	if top == nil {
		return
	}
	for _, key := range slices.Sorted(maps.Keys(top.byOwner)) {
		parts := strings.SplitN(key, orgKeySep, 3)
		owner := "job template"
		if parts[0] == awxTypeWorkflow {
			owner = "workflow"
		}
		for _, s := range top.byOwner[key] {
			p.warn("schedule %q fires %s %s, which did not import, so it was not imported either",
				s.Name, owner, quoteName(parts[2]))
			p.refused++
		}
	}
	for _, m := range top.unmatched {
		p.warn("schedule %q fires %s, %s, so it was not imported", m.schedule.Name, m.fires, m.why)
		p.refused++
	}
	if len(top.other) == 0 {
		return
	}
	names := make([]string, 0, len(top.other))
	for _, s := range top.other {
		names = append(names, strconv.Quote(oneLine(s.Name)))
	}
	which, outcome := fmt.Sprintf("%d schedules at the top level of the export fire",
		len(top.other)), "they were"
	if len(top.other) == 1 {
		which, outcome = "1 schedule at the top level of the export fires", "it was"
	}
	p.warn("%s a project update, an inventory sync, or a system job, which nothing here stands in "+
		"for, so %s not imported: %s", which, outcome, strings.Join(names, ", "))
	p.refused += len(top.other)
}
