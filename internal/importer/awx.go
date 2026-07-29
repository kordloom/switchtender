package importer

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// awxExport is the top level of an awx export document, keyed by asset type.
type awxExport struct {
	// Projects are the source control projects.
	Projects []awxProject `json:"projects"`
	// Inventory holds inventories under AWX's singular key.
	Inventory []awxInventory `json:"inventory"`
	// Inventories holds inventories under the plural key some exports use.
	Inventories []awxInventory `json:"inventories"`
	// JobTemplates are the job templates.
	JobTemplates []awxJobTemplate `json:"job_templates"`
	// Credentials are the credentials, with secrets omitted by AWX.
	Credentials []awxCredential `json:"credentials"`
	// InventorySources are dynamic inventory sources exported at the top level. Some exports nest them
	// under each inventory's related block instead.
	InventorySources []awxInventorySource `json:"inventory_sources"`
}

// awxProject is an AWX project.
type awxProject struct {
	// Name is the project name.
	Name string `json:"name"`
	// ScmType is the source control type; only git is imported.
	ScmType string `json:"scm_type"`
	// ScmURL is the repository URL.
	ScmURL string `json:"scm_url"`
	// ScmBranch is the branch, tag, or commit.
	ScmBranch string `json:"scm_branch"`
}

// awxInventory is an AWX inventory with its hosts and groups.
type awxInventory struct {
	// Name is the inventory name.
	Name string `json:"name"`
	// Hosts are the top level hosts.
	Hosts []awxHost `json:"hosts"`
	// Groups are the named host groups.
	Groups []awxGroup `json:"groups"`
	// Related carries dynamic inventory sources when the export nests them under the inventory.
	Related *awxInventoryRelated `json:"related"`
}

// awxInventoryRelated holds an inventory's nested related assets.
type awxInventoryRelated struct {
	// InventorySources are the inventory's dynamic sources.
	InventorySources []awxInventorySource `json:"inventory_sources"`
}

// awxInventorySource is an AWX dynamic inventory source: a file in a project or a cloud plugin that
// generates hosts at refresh time.
type awxInventorySource struct {
	// Name labels the source.
	Name string `json:"name"`
	// Source is the AWX source type: scm for a file in a project, or a cloud plugin such as ec2.
	Source string `json:"source"`
	// SourcePath is the inventory file or plugin config path within the project, for scm sources.
	SourcePath string `json:"source_path"`
	// SourceProject references the project holding the config, for scm sources.
	SourceProject awxRef `json:"source_project"`
	// Credential references the credential that authenticates the plugin.
	Credential awxRef `json:"credential"`
	// Inventory references the inventory this source feeds, kept only for context.
	Inventory awxRef `json:"inventory"`
}

// awxHost is an inventory host with optional variables.
type awxHost struct {
	// Name is the host name.
	Name string `json:"name"`
	// Variables holds host variables as a map or a YAML string.
	Variables json.RawMessage `json:"variables"`
}

// awxGroup is a named group of hosts.
type awxGroup struct {
	// Name is the group name.
	Name string `json:"name"`
	// Hosts are the group's members.
	Hosts []awxHost `json:"hosts"`
}

// awxJobTemplate is an AWX job template.
type awxJobTemplate struct {
	// Name is the template name.
	Name string `json:"name"`
	// Playbook is the playbook path within the project.
	Playbook string `json:"playbook"`
	// Project references the source project by natural key.
	Project awxRef `json:"project"`
	// Inventory references the inventory by natural key.
	Inventory awxRef `json:"inventory"`
	// ExtraVars are the template extra vars as a YAML or JSON string.
	ExtraVars string `json:"extra_vars"`
	// JobSliceCount is AWX's job slicing count, mapped to shard count.
	JobSliceCount int `json:"job_slice_count"`
	// Credentials references credentials by natural key.
	Credentials []awxRef `json:"credentials"`
	// SurveySpec is the survey when exported at the top level.
	SurveySpec *awxSurvey `json:"survey_spec"`
	// Related carries the survey and schedules when exported nested.
	Related *awxRelated `json:"related"`
}

// awxRelated holds a job template's nested related assets.
type awxRelated struct {
	// SurveySpec is the survey.
	SurveySpec *awxSurvey `json:"survey_spec"`
	// Schedules are the template's schedules.
	Schedules []awxSchedule `json:"schedules"`
}

// awxSurvey is an AWX survey specification.
type awxSurvey struct {
	// Spec lists the survey fields.
	Spec []awxSurveyField `json:"spec"`
}

// awxSurveyField is one AWX survey question.
type awxSurveyField struct {
	// Variable is the extra var name.
	Variable string `json:"variable"`
	// QuestionName is the human prompt.
	QuestionName string `json:"question_name"`
	// Type is the AWX field type.
	Type string `json:"type"`
	// Required rejects a launch that omits the field.
	Required bool `json:"required"`
	// Default is the default answer.
	Default any `json:"default"`
	// Choices lists allowed values as a list or a newline separated string.
	Choices any `json:"choices"`
}

// awxSchedule is an AWX schedule with an iCalendar rule.
type awxSchedule struct {
	// Name is the schedule name.
	Name string `json:"name"`
	// RRule is the iCalendar recurrence rule.
	RRule string `json:"rrule"`
	// Enabled reports whether the schedule is active; absent means enabled.
	Enabled *bool `json:"enabled"`
}

// awxCredential is an AWX credential shell, without secret material.
type awxCredential struct {
	// Name is the credential name.
	Name string `json:"name"`
	// CredentialType is the AWX credential type name.
	CredentialType string `json:"credential_type"`
	// Inputs are the credential's configured values. AWX replaces every secret among them with the
	// literal "$encrypted$", so what survives an export is the non-secret settings: which user to
	// connect as, how to become root, which region or endpoint to talk to.
	Inputs map[string]any `json:"inputs,omitempty"`
}

// awxRef is an AWX natural-key reference that decodes from a name string, a natural-key array whose
// last element is the name, or an object with a name field.
type awxRef string

// UnmarshalJSON decodes the several shapes AWX uses for a natural-key reference into the name.
func (r *awxRef) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*r = awxRef(s)
		return nil
	}
	var arr []string
	if json.Unmarshal(b, &arr) == nil && len(arr) > 0 {
		*r = awxRef(arr[len(arr)-1])
		return nil
	}
	var obj struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(b, &obj) == nil {
		*r = awxRef(obj.Name)
	}
	return nil
}

// FromAWX maps an awx export document into a Plan of SwitchTender objects with cross-references wired
// by generated id. It never fails on a single unmappable asset: it records a warning and continues,
// so a partial export still migrates what it can.
func FromAWX(data []byte, now time.Time) (*Plan, error) {
	var export awxExport
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, fmt.Errorf("parse awx export: %w", err)
	}
	plan := &Plan{}

	projectIDs := map[string]string{}
	for _, p := range export.Projects {
		if p.ScmType != "git" || p.ScmURL == "" {
			plan.warn("project %q skipped: only git projects import (scm_type=%q)", p.Name, p.ScmType)
			continue
		}
		obj := &project.Project{
			ID: project.NewID(), Name: p.Name, RepoURL: p.ScmURL, Branch: p.ScmBranch,
			InstallDeps: true, CreatedAt: now,
		}
		plan.Projects = append(plan.Projects, obj)
		projectIDs[p.Name] = obj.ID
	}

	inventoryIDs := map[string]string{}
	var nestedSources []awxInventorySource
	for _, inv := range append(export.Inventory, export.Inventories...) {
		obj := &inventory.Inventory{
			ID: inventory.NewID(), Name: inv.Name,
			Content:   buildInventoryINI(convertHosts(inv.Hosts), convertGroups(inv.Groups)),
			CreatedAt: now,
		}
		plan.Inventories = append(plan.Inventories, obj)
		inventoryIDs[inv.Name] = obj.ID
		if inv.Related != nil {
			nestedSources = append(nestedSources, inv.Related.InventorySources...)
		}
	}

	credentialIDs := map[string]string{}
	for _, c := range export.Credentials {
		kind, exact := mapCredentialKind(c.CredentialType, c.Inputs)
		if !exact {
			plan.warn("credential %q type %q mapped to %q; verify it is correct",
				c.Name, c.CredentialType, kind)
		}
		obj := &credential.Credential{ID: credential.NewID(), Name: c.Name, Kind: kind, CreatedAt: now}
		plan.Credentials = append(plan.Credentials, obj)
		credentialIDs[c.Name] = obj.ID
		if settings := publicInputs(c.Inputs); len(settings) > 0 {
			plan.warn("credential %q needs its secret re-entered; exports omit secrets by design. "+
				"AWX also recorded %s, which the secret should carry",
				c.Name, strings.Join(settings, ", "))
		} else {
			plan.warn("credential %q needs its secret re-entered; exports omit secrets by design", c.Name)
		}
	}

	for _, s := range export.InventorySources {
		plan.addSource(s, now, projectIDs, credentialIDs)
	}
	for _, s := range nestedSources {
		plan.addSource(s, now, projectIDs, credentialIDs)
	}

	for _, jt := range export.JobTemplates {
		plan.addTemplate(jt, now, projectIDs, inventoryIDs, credentialIDs)
	}
	return plan, nil
}

// addSource maps one AWX inventory source into a dynamic source and the backing inventory it
// maintains, wiring the project and credential references by id. A file source keeps its path; a
// cloud plugin source has no file, so it imports with the plugin name and a warning that a config
// must be set before it can refresh.
func (p *Plan) addSource(s awxInventorySource, now time.Time, projectIDs, credentialIDs map[string]string) {
	if s.Name == "" {
		p.warn("inventory source skipped: it has no name")
		return
	}
	src := &invsource.Source{ID: invsource.NewID(), Name: s.Name, CreatedAt: now}
	switch {
	case s.SourcePath != "":
		src.Source = s.SourcePath
	case s.Source != "":
		src.Source = s.Source
		p.warn("inventory source %q imports the %q plugin as its source; point it at a plugin config file before refreshing",
			s.Name, s.Source)
	default:
		p.warn("inventory source %q skipped: it has no source path or plugin type", s.Name)
		return
	}
	if name := string(s.SourceProject); name != "" {
		if id, ok := projectIDs[name]; ok {
			src.ProjectID = id
		} else {
			p.warn("inventory source %q references unknown project %q", s.Name, name)
		}
	}
	if name := string(s.Credential); name != "" {
		if id, ok := credentialIDs[name]; ok {
			src.CredentialID = id
		} else {
			p.warn("inventory source %q references unknown credential %q", s.Name, name)
		}
	}
	inv := &inventory.Inventory{
		ID: inventory.NewID(), Name: s.Name + " (dynamic)", Content: "{}", CreatedAt: now,
	}
	src.InventoryID = inv.ID
	p.Inventories = append(p.Inventories, inv)
	p.Sources = append(p.Sources, src)
}

// addTemplate maps one job template and its schedules into the plan, wiring project, inventory, and
// credential references by id and warning on anything unresolved.
func (p *Plan) addTemplate(jt awxJobTemplate, now time.Time,
	projectIDs, inventoryIDs, credentialIDs map[string]string) {
	tpl := &template.Template{
		ID: template.NewID(), Name: jt.Name, Playbook: jt.Playbook, CreatedAt: now,
	}
	if name := string(jt.Project); name != "" {
		if id, ok := projectIDs[name]; ok {
			tpl.ProjectID = id
		} else {
			p.warn("template %q references unknown project %q", jt.Name, name)
		}
	}
	if name := string(jt.Inventory); name != "" {
		if id, ok := inventoryIDs[name]; ok {
			tpl.InventoryID = id
		} else {
			p.warn("template %q references unknown inventory %q", jt.Name, name)
		}
	}
	if jt.JobSliceCount >= 2 {
		tpl.Shards = jt.JobSliceCount
	}
	for _, ref := range jt.Credentials {
		if id, ok := credentialIDs[string(ref)]; ok {
			tpl.CredentialIDs = append(tpl.CredentialIDs, id)
		} else if ref != "" {
			p.warn("template %q references unknown credential %q", jt.Name, string(ref))
		}
	}
	if vars, err := parseExtraVars(jt.ExtraVars); err != nil {
		p.warn("template %q extra_vars could not be parsed: %v", jt.Name, err)
	} else {
		tpl.ExtraVars = vars
	}
	tpl.Survey = p.mapSurvey(jt)

	p.Templates = append(p.Templates, tpl)
	p.addSchedules(jt, tpl.ID, now)
}

// mapSurvey converts a job template's survey, whether top level or nested under related, into
// SwitchTender survey fields, warning on any inexact type mapping.
func (p *Plan) mapSurvey(jt awxJobTemplate) []template.SurveyField {
	survey := jt.SurveySpec
	if survey == nil && jt.Related != nil {
		survey = jt.Related.SurveySpec
	}
	if survey == nil {
		return nil
	}
	var fields []template.SurveyField
	for _, f := range survey.Spec {
		fieldType, exact := mapSurveyType(f.Type)
		if !exact {
			p.warn("survey field %q of template %q: type %q mapped to %q",
				f.Variable, jt.Name, f.Type, fieldType)
		}
		fields = append(fields, template.SurveyField{
			Var: f.Variable, Label: f.QuestionName, Type: fieldType,
			Required: f.Required, Default: f.Default, Choices: choicesFrom(f.Choices),
		})
	}
	return fields
}

// addSchedules maps a job template's schedules into the plan, converting each RRULE to cron and
// warning on any that cron cannot express.
func (p *Plan) addSchedules(jt awxJobTemplate, templateID string, now time.Time) {
	if jt.Related == nil {
		return
	}
	for _, s := range jt.Related.Schedules {
		cron, ok := RRULEToCron(s.RRule)
		if !ok {
			p.warn("schedule %q of template %q skipped: cannot express %q as cron",
				s.Name, jt.Name, s.RRule)
			continue
		}
		enabled := s.Enabled == nil || *s.Enabled
		p.Schedules = append(p.Schedules, &schedule.Schedule{
			ID: schedule.NewID(), Name: s.Name, Cron: cron, TemplateID: templateID,
			Enabled: enabled, CreatedAt: now,
		})
	}
}

// convertHosts adapts AWX hosts to the shared import host shape, decoding host variables.
func convertHosts(hosts []awxHost) []importHost {
	out := make([]importHost, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, importHost{Name: h.Name, Variables: decodeVars(h.Variables)})
	}
	return out
}

// convertGroups adapts AWX groups to the shared import group shape.
func convertGroups(groups []awxGroup) []importGroup {
	out := make([]importGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, importGroup{Name: g.Name, Hosts: convertHosts(g.Hosts)})
	}
	return out
}

// decodeVars decodes host variables from either a JSON object or a YAML or JSON string.
func decodeVars(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err == nil {
		return asMap
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if vars, err := parseExtraVars(asString); err == nil {
			return vars
		}
	}
	return nil
}
