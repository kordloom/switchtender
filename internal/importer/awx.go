package importer

import (
	"bytes"
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
	// The rest are counted, not mapped. They are decoded as raw messages so the report can say how
	// many of each an export held and what will not come across, rather than staying silent about
	// the part of an AWX install a team's orchestration actually lives in.
	Workflows             []awxWorkflow     `json:"workflow_job_templates"`
	Organizations         []json.RawMessage `json:"organizations"`
	Teams                 []json.RawMessage `json:"teams"`
	NotificationTemplates []json.RawMessage `json:"notification_templates"`
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
	// Hosts are the inventory's hosts when the export nests them, which awxkit does.
	Hosts []awxHost `json:"hosts"`
	// Groups are the inventory's groups when the export nests them, which awxkit does.
	Groups []awxGroup `json:"groups"`
}

// hosts returns the inventory's hosts from whichever place the export carried them.
//
// awxkit, the tool the migration guide tells an operator to run, writes hosts and groups under the
// inventory's related block rather than at the top level. Reading only the top level meant every
// inventory from a real export arrived empty, and silently: the import reported success, the
// inventory existed, and it had no hosts in it. That is the first thing an evaluator does, so it is
// the first thing they saw fail.
func (i awxInventory) hosts() []awxHost {
	if len(i.Hosts) > 0 {
		return i.Hosts
	}
	if i.Related != nil {
		return i.Related.Hosts
	}
	return nil
}

// groups returns the inventory's groups from whichever place the export carried them.
func (i awxInventory) groups() []awxGroup {
	if len(i.Groups) > 0 {
		return i.Groups
	}
	if i.Related != nil {
		return i.Related.Groups
	}
	return nil
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
	// Hosts are the group's members when the export carries them at the top level.
	Hosts []awxHost `json:"hosts"`
	// Related carries the group's members when the export nests them, which awxkit does.
	Related *awxGroupRelated `json:"related"`
}

// awxGroupRelated holds a group's nested related assets.
type awxGroupRelated struct {
	// Hosts are the group's members.
	Hosts []awxHost `json:"hosts"`
}

// hosts returns the group's members from whichever place the export carried them.
//
// Reading only the top level was the same bug the inventory accessors above fix, one level down and
// with a worse failure: the hosts still imported, because they also appear on the inventory, so an
// import reported the right host count with every group empty. A template carrying limit "web" then
// matched nothing, and the run reported success having touched no hosts at all.
func (g awxGroup) hosts() []awxHost {
	if len(g.Hosts) > 0 {
		return g.Hosts
	}
	if g.Related != nil {
		return g.Related.Hosts
	}
	return nil
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
	// Limit narrows the run to matching hosts, the same pattern ansible-playbook takes.
	Limit string `json:"limit"`
	// JobTags and SkipTags select and skip tagged plays and tasks.
	JobTags  string `json:"job_tags"`
	SkipTags string `json:"skip_tags"`
	// Verbosity is AWX's 0 to 4 logging level.
	Verbosity int `json:"verbosity"`
	// Forks is how many hosts Ansible addresses in parallel.
	Forks int `json:"forks"`
	// Timeout caps a job's runtime in seconds.
	Timeout int `json:"timeout"`
	// JobType is "run" or "check"; check is Ansible's no-change mode.
	JobType string `json:"job_type"`
	// DiffMode shows the before and after of each change.
	DiffMode bool `json:"diff_mode"`
	// Credentials references credentials by natural key when the export carries them at the top
	// level.
	Credentials []awxRef `json:"credentials"`
	// SurveySpec is the survey when exported at the top level.
	SurveySpec *awxSurvey `json:"survey_spec"`
	// Related carries the survey, schedules, and credentials when exported nested.
	Related *awxRelated `json:"related"`
}

// credentials returns the template's credentials from whichever place the export carried them.
//
// awxkit never writes them at the top level: credentials are an exportable relation, so they arrive
// under related as natural keys. Reading only the top level meant every template from a real export
// arrived with no credentials and no warning, and the first run failed to authenticate with nothing
// to point at.
func (t awxJobTemplate) credentials() []awxRef {
	if len(t.Credentials) > 0 {
		return t.Credentials
	}
	if t.Related != nil {
		return t.Related.Credentials
	}
	return nil
}

// checkMode reports whether the template runs in Ansible's no-change mode. AWX spells it as a job
// type rather than a flag, and both the template and the workflow import paths ask the same way.
func (t awxJobTemplate) checkMode() bool { return strings.EqualFold(t.JobType, "check") }

// awxRelated holds a job template's nested related assets.
type awxRelated struct {
	// SurveySpec is the survey.
	SurveySpec *awxSurvey `json:"survey_spec"`
	// Schedules are the template's schedules.
	Schedules []awxSchedule `json:"schedules"`
	// Credentials references the template's credentials by natural key.
	Credentials []awxRef `json:"credentials"`
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
	// CredentialType is the AWX credential type, which arrives as a natural-key reference like every
	// other cross-object field. It was typed as a plain string, and a real export serializes it as an
	// object, so encoding/json failed on the whole document and an export from a live AWX imported
	// nothing at all rather than importing partially.
	CredentialType awxRef `json:"credential_type"`
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
	// UseNumber keeps JSON numbers as json.Number rather than float64, so a host variable or survey
	// choice that is a large integer survives to the inventory verbatim instead of being reformatted
	// through float64, which loses precision past 2^53 and prints in scientific notation.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&export); err != nil {
		return nil, fmt.Errorf("parse awx export: %w", err)
	}
	plan := &Plan{}

	projectIDs := map[string]string{}
	for _, p := range export.Projects {
		if p.ScmType != "git" || p.ScmURL == "" {
			plan.warn("project %q skipped: only git projects import (scm_type=%q)", p.Name, p.ScmType)
			continue
		}
		// The same check the API applies when a person creates a project. Skipping it here let an
		// export create stored projects the API itself would have refused, which then fail at clone
		// time with an error about the repository rather than about the import that made them.
		if err := project.ValidateRepoURL(p.ScmURL); err != nil {
			plan.warn("project %q skipped: %v", p.Name, err)
			continue
		}
		if _, dup := projectIDs[p.Name]; dup {
			plan.warn("project %q appears more than once; the later one is what templates naming "+
				"it will use", p.Name)
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
		hosts, groups := inv.hosts(), inv.groups()
		// An inventory that arrives with nothing in it is reported, whatever the reason.
		//
		// AWX writes hosts in more than one place and this importer reads the two it knows. When a
		// real export put them somewhere else, every inventory was created empty and the import
		// still said it had succeeded, so an operator migrated a fleet and got a set of inventories
		// that targeted no hosts. Chasing shapes does not close that: the next export format the
		// importer has not seen fails the same silent way. Reporting the outcome does, because the
		// outcome is the same whichever shape the hosts were in, and an inventory that is genuinely
		// empty in AWX is worth a line to the operator too.
		if len(hosts) == 0 && len(groups) == 0 {
			plan.warn("inventory %q imported with no hosts and no groups. If it is not empty in "+
				"AWX, this export puts them somewhere this importer does not read, and the "+
				"inventory will target nothing", inv.Name)
		}
		obj := &inventory.Inventory{
			ID: inventory.NewID(), Name: inv.Name,
			Content:   buildInventoryINI(plan, inv.Name, convertHosts(hosts), convertGroups(groups)),
			CreatedAt: now,
		}
		if _, dup := inventoryIDs[inv.Name]; dup {
			plan.warn("inventory %q appears more than once; the later one is what templates naming "+
				"it will use", inv.Name)
		}
		plan.Inventories = append(plan.Inventories, obj)
		inventoryIDs[inv.Name] = obj.ID
		if inv.Related != nil {
			nestedSources = append(nestedSources, inv.Related.InventorySources...)
		}
	}

	credentialIDs := map[string]string{}
	for _, c := range export.Credentials {
		kind, exact := mapCredentialKind(string(c.CredentialType), c.Inputs)
		if !exact {
			plan.warn("credential %q type %q mapped to %q; verify it is correct",
				c.Name, string(c.CredentialType), kind)
		}
		if _, dup := credentialIDs[c.Name]; dup {
			plan.warn("credential %q appears more than once; the later one is what templates naming "+
				"it will use, and the two may not be the same kind", c.Name)
		}
		obj := &credential.Credential{ID: credential.NewID(), Name: c.Name, Kind: kind, CreatedAt: now}
		// AWX keeps the vault label as a non-secret input; carrying it means a multi-vault setup
		// imports with its --vault-id labels intact instead of every password turning unlabeled.
		if kind == credential.KindVaultPassword {
			if label := strings.TrimSpace(fmt.Sprint(c.Inputs["vault_id"])); label != "" &&
				label != "<nil>" && credential.ValidVaultID(label) {
				obj.VaultID = label
			}
		}
		// The export's non-secret inputs land as settings on the credential itself, so a machine
		// credential arrives knowing its connection user and become method and only the secret needs
		// entering. They used to survive only as warning text an operator had to copy by hand.
		settings, refused := credentialSettings(kind, c.Inputs)
		obj.Settings = settings
		plan.Credentials = append(plan.Credentials, obj)
		credentialIDs[c.Name] = obj.ID
		const base = "credential %q needs its secret re-entered; exports omit secrets by design"
		switch {
		case len(settings) > 0 && len(refused) > 0:
			plan.warn(base+". Its non-secret settings (%s) were stored on the credential; these AWX "+
				"inputs could not be stored and must be set by hand: %s",
				c.Name, settingsList(settings), strings.Join(refused, ", "))
		case len(settings) > 0:
			plan.warn(base+". Its non-secret settings (%s) were stored on the credential",
				c.Name, settingsList(settings))
		case len(refused) > 0:
			plan.warn(base+". These AWX inputs could not be stored and must be set by hand: %s",
				c.Name, strings.Join(refused, ", "))
		default:
			plan.warn(base, c.Name)
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
	// Workflows come after the job templates they run, since each node's step inlines the playbook
	// of the template it points at.
	plan.addWorkflows(export, now, projectIDs, inventoryIDs, credentialIDs)
	reportUnmapped(plan, export)
	if err := plan.requireObjects("projects, inventories, credentials, job templates, or " +
		"schedules"); err != nil {
		return nil, err
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
		// The execution settings AWX holds on the job template. Dropping these silently changed what
		// the template does: a check-mode template imported as a live one, and a template limited to
		// a canary host imported targeting the whole inventory, both without a word in the report.
		Limit:     jt.Limit,
		Tags:      splitAWXTags(jt.JobTags),
		SkipTags:  splitAWXTags(jt.SkipTags),
		Verbosity: jt.Verbosity,
		Forks:     jt.Forks,
		Timeout:   jt.Timeout,
		DiffMode:  jt.DiffMode,
		DryRun:    jt.checkMode(),
	}
	if name := string(jt.Project); name != "" {
		if id, ok := projectIDs[name]; ok {
			tpl.ProjectID = id
		} else {
			// The template is not created. In AWX every job template is scoped to a project, so its
			// playbook path is relative to that checkout and is held inside it at run time. A
			// template with no project has no checkout to be held inside, and dispatch skips the
			// containment check entirely when there is no project, so importing one converts a path
			// that was contained into one that is resolved against the server's own directory.
			p.warn("template %q was not imported: it references project %q, which is not in this "+
				"export, and a template with no project has no checkout to resolve its playbook "+
				"against", jt.Name, name)
			return
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
	for _, ref := range jt.credentials() {
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
		// AWX's password survey type prompts for a secret and stores it obscured. A survey field here
		// is plain text whose answer is kept on the run and injected as an extra var, so importing one
		// would quietly turn a password prompt into a stored plaintext value, and AWX exports the
		// field's default alongside it. Refusing and naming it is honest; a silent downgrade hands the
		// operator a migration that looks complete and is less safe than what they left.
		if strings.EqualFold(f.Type, "password") {
			p.warn("survey field %q of template %q is a password prompt and was NOT imported. Store "+
				"its value as a credential instead: importing it as a survey field would keep the "+
				"answer in plain text on every run.", f.Variable, jt.Name)
			continue
		}
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
			// A rule that bounds itself is the common case here, and its remedy is different from a
			// cadence cron cannot express, so it says so: a cron entry has no end, and creating one
			// from a rule that was meant to stop would leave a job firing forever.
			p.warn("schedule %q of template %q skipped: %s (%q)", s.Name, jt.Name, rruleProblem(s.RRule),
				s.RRule)
			continue
		}
		enabled := s.Enabled == nil || *s.Enabled
		// AWX records the zone on the rule. Keeping it is what makes an imported 2am window still
		// fire at 2am where the operator lives, and follow that zone's daylight saving shifts. A zone
		// this build cannot resolve is reported and the schedule still imports, in server time,
		// because a job that runs at the wrong hour is recoverable and one that was never created is
		// easy to miss.
		zone := dtstartZone(s.RRule)
		if zone != "" {
			if _, err := time.LoadLocation(zone); err != nil {
				p.warn("schedule %q of template %q names the timezone %q, which this system cannot "+
					"resolve, so it imports in the server's local time: %v",
					s.Name, jt.Name, oneLine(zone), err)
				zone = ""
			}
		}
		p.addSchedule(&schedule.Schedule{
			ID: schedule.NewID(), Name: s.Name, Cron: cron, Timezone: zone, TemplateID: templateID,
			Enabled: enabled, CreatedAt: now,
		}, "this AWX export", now)
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
		out = append(out, importGroup{Name: g.Name, Hosts: convertHosts(g.hosts())})
	}
	return out
}

// decodeVars decodes host variables from either a JSON object or a YAML or JSON string.
func decodeVars(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var asMap map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&asMap); err == nil {
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

// reportUnmapped names the AWX objects an export held that this importer does not create, so the
// report says what is not coming across instead of leaving the operator to discover it later.
//
// Workflows matter most: an AWX shop's orchestration lives in workflow job templates, and an import
// that recreates every job template while silently dropping the graph that sequences them looks
// complete and is not.
func reportUnmapped(plan *Plan, export awxExport) {
	for _, item := range []struct {
		Count int
		What  string
		Why   string
	}{
		{len(export.Organizations), "organization",
			"create them with POST /v1/orgs and add members, which carries the same ownership"},
		{len(export.Teams), "team",
			"create them with POST /v1/teams and grant access per object"},
		{len(export.NotificationTemplates), "notification template",
			"set notifications on each template, or configure the server-wide channels"},
	} {
		if item.Count == 0 {
			continue
		}
		plural := "s"
		if item.Count == 1 {
			plural = ""
		}
		plan.warn("this export holds %d %s%s, which are not imported: %s",
			item.Count, item.What, plural, item.Why)
	}
}

// splitAWXTags turns AWX's comma separated tag string into the list a template holds, dropping the
// blanks a trailing or doubled comma leaves behind.
func splitAWXTags(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}
