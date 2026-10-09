package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
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
	// CredentialTypes are the custom credential types, which a credential's credential_type names.
	CredentialTypes []awxCredentialType `json:"credential_types"`
	// InventorySources are dynamic inventory sources exported at the top level. Some exports nest them
	// under each inventory's related block instead.
	InventorySources []awxInventorySource `json:"inventory_sources"`
	// The rest are counted, not mapped. They are decoded as raw messages so the report can say how
	// many of each an export held and what will not come across, rather than staying silent about
	// the part of an AWX install a team's orchestration actually lives in.
	Workflows     []awxWorkflow     `json:"workflow_job_templates"`
	Organizations []json.RawMessage `json:"organizations"`
	Teams         []json.RawMessage `json:"teams"`
	// NotificationTemplates are the named notification channels, imported as notification targets
	// and attached wherever the export attaches them.
	NotificationTemplates []awxNotificationTemplate `json:"notification_templates"`
	// Schedules are the schedules awxkit also writes at the top level, each naming what it fires.
	// One that belongs to a job template or a workflow imports with it, and the rest are reported.
	Schedules []awxSchedule `json:"schedules"`
}

// awxProject is an AWX project.
type awxProject struct {
	// Name is the project name.
	Name string `json:"name"`
	// Organization is the organization the project belongs to, which scopes its name.
	Organization awxRef `json:"organization"`
	// ScmType is the source control type; only git is imported.
	ScmType string `json:"scm_type"`
	// ScmURL is the repository URL.
	ScmURL string `json:"scm_url"`
	// ScmBranch is the branch, tag, or commit.
	ScmBranch string `json:"scm_branch"`
	// Credential references the source control credential a private repository syncs with.
	Credential awxRef `json:"credential"`
	// Related carries the notification templates attached to the project, which hear about project
	// updates and are reported rather than imported.
	Related *awxProjectRelated `json:"related"`
}

// awxInventory is an AWX inventory with its hosts and groups.
type awxInventory struct {
	// Name is the inventory name.
	Name string `json:"name"`
	// Organization is the organization the inventory belongs to, which scopes its name.
	Organization awxRef `json:"organization"`
	// Hosts are the top level hosts.
	Hosts []awxHost `json:"hosts"`
	// Groups are the named host groups.
	Groups []awxGroup `json:"groups"`
	// Variables holds inventory-wide variables as a map or a YAML string, which become [all:vars].
	Variables json.RawMessage `json:"variables"`
	// Related carries dynamic inventory sources when the export nests them under the inventory.
	Related *awxInventoryRelated `json:"related"`
	// Kind is empty for an ordinary inventory, smart for one defined by a host filter, or
	// constructed for one the constructed plugin builds from input inventories.
	Kind string `json:"kind"`
	// HostFilter is a smart inventory's filter over the hosts of the organization's inventories.
	HostFilter string `json:"host_filter"`
	// InputInventories are a constructed inventory's inputs, when the export carries them at the
	// top level.
	InputInventories []awxRef `json:"input_inventories"`
	// SourceVars are a constructed inventory's plugin options, when the export flattens them onto
	// the inventory the way AWX's constructed_inventories endpoint serves them.
	SourceVars json.RawMessage `json:"source_vars"`
	// Limit narrows a constructed inventory, when the export flattens it onto the inventory.
	Limit string `json:"limit"`
}

// inputInventories returns a constructed inventory's inputs from whichever place the export
// carried them.
func (i awxInventory) inputInventories() []awxRef {
	if len(i.InputInventories) > 0 {
		return i.InputInventories
	}
	if i.Related != nil {
		return i.Related.InputInventories
	}
	return nil
}

// awxInventoryRelated holds an inventory's nested related assets.
type awxInventoryRelated struct {
	// InventorySources are the inventory's dynamic sources.
	InventorySources []awxInventorySource `json:"inventory_sources"`
	// Hosts are the inventory's hosts when the export nests them, which awxkit does.
	Hosts []awxHost `json:"hosts"`
	// Groups are the inventory's groups when the export nests them, which awxkit does.
	Groups []awxGroup `json:"groups"`
	// InputInventories are a constructed inventory's inputs when the export nests them.
	InputInventories []awxRef `json:"input_inventories"`
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
	// Inventory references the inventory this source feeds. A constructed source is matched to its
	// constructed inventory by it.
	Inventory awxRef `json:"inventory"`
	// SourceVars are the source's plugin options, a YAML or JSON string. A constructed source's are
	// the constructed inventory's options; any other source's are reported, since a source here
	// takes its options from its config file.
	SourceVars json.RawMessage `json:"source_vars"`
	// Limit narrows a constructed source to the hosts a pattern matches.
	Limit string `json:"limit"`
}

// constructed reports whether the source is the one AWX keeps behind a constructed inventory.
func (s awxInventorySource) constructed() bool {
	return strings.EqualFold(s.Source, "constructed")
}

// awxHost is an inventory host with optional variables.
type awxHost struct {
	// Name is the host name.
	Name string `json:"name"`
	// Variables holds host variables as a map or a YAML string.
	Variables json.RawMessage `json:"variables"`
	// Enabled reports whether AWX runs anything against this host; absent means enabled.
	Enabled *bool `json:"enabled"`
}

// awxGroup is a named group of hosts.
type awxGroup struct {
	// Name is the group name.
	Name string `json:"name"`
	// Hosts are the group's members when the export carries them at the top level.
	Hosts []awxHost `json:"hosts"`
	// Related carries the group's members when the export nests them, which awxkit does.
	Related *awxGroupRelated `json:"related"`
	// Variables holds the group's variables as a map or a YAML string, which become [name:vars].
	Variables json.RawMessage `json:"variables"`
	// Children names the groups nested under this one, which become [name:children].
	Children []string `json:"children"`
}

// awxGroupRelated holds a group's nested related assets.
type awxGroupRelated struct {
	// Hosts are the group's members.
	Hosts []awxHost `json:"hosts"`
	// Children are the groups nested under this one. awxkit writes them here as whole group objects,
	// while a hand-written export names them as strings at the top level, so both shapes arrive.
	Children []awxGroup `json:"children"`
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

// childNames returns the groups nested under this one, from whichever place the export carried them.
//
// The top-level form names them as strings and was the only form read. awxkit nests them as whole group
// objects instead, so a real export imported every group flat: the [name:children] sections were empty,
// and a play targeting a parent group reached none of the hosts underneath it while the host count and
// the group count both came out right.
func (g awxGroup) childNames() []string {
	if len(g.Children) > 0 {
		return g.Children
	}
	if g.Related == nil {
		return nil
	}
	out := make([]string, 0, len(g.Related.Children))
	for _, child := range g.Related.Children {
		if name := strings.TrimSpace(child.Name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// maxGroupNesting bounds how deep the walk below follows a group's children. AWX itself allows deep
// nesting and an export is untrusted input, so the walk is bounded rather than trusting the file.
const maxGroupNesting = 32

// flattenGroups returns every group an inventory holds, including the ones that appear only nested
// inside another group's related block.
//
// A nested child is a group in its own right: it has its own hosts and its own variables. Reading only
// the list at the inventory level meant those existed nowhere in the imported inventory, so the hosts
// under a subgroup were missing entirely unless they happened to be listed at the inventory level too.
//
// The fuller record of a group wins when it appears twice, since awxkit writes the whole object in one
// place and a bare reference in the other, and which one comes first is not something to depend on.
func flattenGroups(groups []awxGroup) []awxGroup {
	var out []awxGroup
	at := map[string]int{}
	var walk func(gs []awxGroup, depth int)
	walk = func(gs []awxGroup, depth int) {
		if depth > maxGroupNesting {
			return
		}
		for _, g := range gs {
			name := strings.TrimSpace(g.Name)
			if name == "" {
				continue
			}
			if idx, dup := at[name]; dup {
				if len(g.hosts()) > len(out[idx].hosts()) {
					out[idx] = g
				}
				continue
			}
			at[name] = len(out)
			out = append(out, g)
			if g.Related != nil {
				walk(g.Related.Children, depth+1)
			}
		}
	}
	walk(groups, 0)
	return out
}

// dropSkippedHosts takes each host the lenient decoder left out of an inventory's own host list out
// of every group in that inventory too. It returns, by position in skipped, the names of the groups
// each such host was taken out of.
//
// awxkit lists a host on its inventory and again, by name, under each group it belongs to. A group
// that still named a host skipped for a malformed field would put it back in the inventory without
// its variables and with its enabled field read as on, so a run against the group would reach a
// host the report says was left out.
func (e *awxExport) dropSkippedHosts(skipped []lenientSkip) map[int][]string {
	out := map[int][]string{}
	for i, s := range skipped {
		if s.Name == "" || len(s.Path) < 2 {
			continue
		}
		var inventories []awxInventory
		switch s.Path[0].Key {
		case "inventory":
			inventories = e.Inventory
		case "inventories":
			inventories = e.Inventories
		default:
			continue
		}
		at := s.Path[0].Index
		if rest := stepKeys(s.Path[1:]); at < 0 || at >= len(inventories) ||
			(rest != "hosts" && rest != "related.hosts") {
			continue
		}
		inv := &inventories[at]
		// A host listed twice, once readably, is still a host of the inventory, so its groups keep it.
		if slices.ContainsFunc(inv.hosts(), func(h awxHost) bool { return h.Name == s.Name }) {
			continue
		}
		var from []string
		dropMember(inv.Groups, s.Name, &from, 0)
		if inv.Related != nil {
			dropMember(inv.Related.Groups, s.Name, &from, 0)
		}
		if len(from) > 0 {
			slices.Sort(from)
			out[i] = slices.Compact(from)
		}
	}
	return out
}

// dropMember takes the named host out of each group and every group nested under it, appending to
// from the name of each group that listed it.
func dropMember(groups []awxGroup, name string, from *[]string, depth int) {
	if depth > maxGroupNesting {
		return
	}
	named := func(h awxHost) bool { return h.Name == name }
	for i := range groups {
		g := &groups[i]
		listed := slices.ContainsFunc(g.Hosts, named)
		g.Hosts = slices.DeleteFunc(g.Hosts, named)
		if g.Related != nil {
			listed = listed || slices.ContainsFunc(g.Related.Hosts, named)
			g.Related.Hosts = slices.DeleteFunc(g.Related.Hosts, named)
			dropMember(g.Related.Children, name, from, depth+1)
		}
		if listed {
			*from = append(*from, g.Name)
		}
	}
}

// awxJobTemplate is an AWX job template.
type awxJobTemplate struct {
	// Name is the template name.
	Name string `json:"name"`
	// Organization is the organization the template belongs to, which scopes its name.
	Organization awxRef `json:"organization"`
	// Playbook is the playbook path within the project.
	Playbook string `json:"playbook"`
	// Project references the source project by natural key.
	Project awxRef `json:"project"`
	// Inventory references the inventory by natural key.
	Inventory awxRef `json:"inventory"`
	// ExtraVars are the template extra vars as a YAML or JSON string.
	ExtraVars string `json:"extra_vars"`
	// JobSliceCount is AWX's job slicing count, mapped to shard count.
	JobSliceCount looseInt `json:"job_slice_count"`
	// Limit narrows the run to matching hosts, the same pattern ansible-playbook takes.
	Limit string `json:"limit"`
	// JobTags and SkipTags select and skip tagged plays and tasks.
	JobTags  string `json:"job_tags"`
	SkipTags string `json:"skip_tags"`
	// Verbosity is AWX's 0 to 4 logging level.
	Verbosity looseInt `json:"verbosity"`
	// Forks is how many hosts Ansible addresses in parallel.
	Forks looseInt `json:"forks"`
	// Timeout caps a job's runtime in seconds.
	Timeout looseInt `json:"timeout"`
	// JobType is "run" or "check"; check is Ansible's no-change mode.
	JobType string `json:"job_type"`
	// DiffMode shows the before and after of each change.
	DiffMode bool `json:"diff_mode"`
	// SurveyEnabled is whether AWX asks the survey at launch. AWX keeps a survey that was switched
	// off and exports it, so the spec alone does not say it is asked. Absent reads as asked, which
	// is how an export built by hand without the field has always imported.
	SurveyEnabled *bool `json:"survey_enabled"`
	// BecomeEnabled is whether AWX runs the playbook with privilege escalation.
	BecomeEnabled bool `json:"become_enabled"`
	// UseFactCache is whether AWX keeps the facts each job gathers and serves them to the next.
	UseFactCache bool `json:"use_fact_cache"`
	// AllowCallbacks is whether a host in the inventory may launch the template against itself.
	AllowCallbacks bool `json:"allow_callbacks"`
	// HostConfigKey is the key a host presents to call back. AWX exports it in the clear.
	HostConfigKey string `json:"host_config_key"`
	// ID is the AWX job template id, which an export taken from the API carries. It is kept raw so
	// a value that is not a number cannot keep the template from importing.
	ID json.RawMessage `json:"id"`
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
	awxNotifyRelated
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
	// Description is the schedule's free text description. It has no effect on a fire.
	Description string `json:"description"`
	// UnifiedJobTemplate names what the schedule fires, by natural key with its kind, which awxkit
	// writes on every schedule and which a schedule at the top level of the export is placed by.
	UnifiedJobTemplate awxTypedRef `json:"unified_job_template"`
	// RRule is the iCalendar recurrence rule.
	RRule string `json:"rrule"`
	// Enabled reports whether the schedule is active; absent means enabled.
	Enabled *bool `json:"enabled"`
	// ExtraData are the variables the schedule launches with, its survey answers among them.
	ExtraData json.RawMessage `json:"extra_data"`
	// Inventory replaces the template's inventory for this schedule's runs, when set.
	Inventory awxRef `json:"inventory"`
	// Limit replaces the template's host limit for this schedule's runs, when set.
	Limit *string `json:"limit"`
	// JobTags and SkipTags replace the template's tag selection for this schedule's runs, when set.
	JobTags  *string `json:"job_tags"`
	SkipTags *string `json:"skip_tags"`
	// JobType is run or check for this schedule's runs, when set.
	JobType *string `json:"job_type"`
	// DiffMode replaces the template's diff setting for this schedule's runs, when set.
	DiffMode *bool `json:"diff_mode"`
	// Verbosity, Forks, and Timeout replace the template's own for this schedule's runs, when set.
	Verbosity *looseInt `json:"verbosity"`
	Forks     *looseInt `json:"forks"`
	Timeout   *looseInt `json:"timeout"`
}

// awxCredential is an AWX credential shell, without secret material.
type awxCredential struct {
	// Name is the credential name.
	Name string `json:"name"`
	// Organization is the organization the credential belongs to, which scopes its name.
	Organization awxRef `json:"organization"`
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
// last element is the name, or an object with a name field and, for an object that belongs to one,
// the organization it belongs to. The REST API writes the same reference as the target's integer id,
// which decodes into the id-N spelling the workflow node reference already uses, since no natural key
// resolves it.
type awxRef struct {
	// Name is the referenced object's name, or id-N for a bare integer id.
	Name string
	// Org is the organization the reference names, empty when it names none.
	Org string
}

// opaqueToUnread marks a reference as read whole: its keys are how AWX spells a pointer, not fields of
// the object, so the unread scan does not descend into it.
func (awxRef) opaqueToUnread() {}

// UnmarshalJSON decodes the several shapes AWX uses for a natural-key reference.
//
// An integer id keeps a name of its own rather than decoding to the empty string, because the empty
// name means "no reference was given" and an id means "this reference could not be resolved". Read
// as absent, a numeric project and inventory slipped past the fail-closed refusal in addTemplate and
// the template imported with neither, silently: dispatch skips its containment check when a template
// has no project, so an export taken from the REST API rather than through awxkit converted every
// contained playbook path into one resolved against the server's own directory, with no warning.
//
// The organization is kept because two organizations may each hold an object of the same name, and a
// reference resolved by name alone wired a template to the other organization's project.
func (r *awxRef) UnmarshalJSON(b []byte) error {
	*r = awxRef{}
	var s string
	if json.Unmarshal(b, &s) == nil {
		r.Name = s
		return nil
	}
	var id json.Number
	if json.Unmarshal(b, &id) == nil {
		r.Name = "id-" + id.String()
		return nil
	}
	var arr []string
	if json.Unmarshal(b, &arr) == nil && len(arr) > 0 {
		r.Name = arr[len(arr)-1]
		return nil
	}
	var obj struct {
		// Name is the referenced object's name.
		Name string `json:"name"`
		// Organization is the natural key of the organization the object belongs to.
		Organization json.RawMessage `json:"organization"`
	}
	if json.Unmarshal(b, &obj) == nil {
		r.Name = obj.Name
		var org awxRef
		if len(obj.Organization) > 0 && json.Unmarshal(obj.Organization, &org) == nil {
			r.Org = org.Name
		}
	}
	return nil
}

// FromAWX maps an awx export document into a Plan of SwitchTender objects with cross-references wired
// by generated id. It never fails on a single unmappable asset: it records a warning and continues,
// so a partial export still migrates what it can.
func FromAWX(data []byte, now time.Time) (*Plan, error) {
	return fromAWX(data, now, nil)
}

// fromAWX is FromAWX with ids, AWX's job template list, standing in for the AWX id an export does
// not carry. ids is nil when there is no list.
func fromAWX(data []byte, now time.Time, ids *awxTemplateIDs) (*Plan, error) {
	data, err := textOf(data)
	if err != nil {
		return nil, err
	}
	var export awxExport
	// Numbers stay json.Number rather than float64, so a host variable or survey choice that is a
	// large integer survives to the inventory verbatim instead of being reformatted through float64,
	// which loses precision past 2^53 and prints in scientific notation.
	//
	// An inventory and a group are containers, with the related blocks awxkit nests their hosts
	// under, so one malformed host costs that host alone and not the inventory and every template
	// on it.
	skipped, err := decodeLenient(data, &export, reflect.TypeFor[awxInventory](),
		reflect.TypeFor[awxInventoryRelated](), reflect.TypeFor[awxGroup](),
		reflect.TypeFor[awxGroupRelated]())
	if err != nil {
		return nil, fmt.Errorf("parse awx export: %w", err)
	}
	// A decoder reads one value and stops, so the rest of the file has to be checked separately. See
	// wholedoc.go: this refusal came free from json.Unmarshal until the move to a decoder above.
	if err := refuseJSONTail(data); err != nil {
		return nil, err
	}
	plan := &Plan{awxIDs: ids}
	// A skipped entry counts as refused, so an export whose every entry was malformed reports each
	// one and why, rather than claiming nothing in it was recognized.
	ungrouped := export.dropSkippedHosts(skipped)
	for i, s := range skipped {
		text := s.Text
		if groups := ungrouped[i]; len(groups) > 0 {
			quoted := make([]string, len(groups))
			for j, g := range groups {
				quoted[j] = quoteName(g)
			}
			lists := "lists"
			if len(groups) > 1 {
				lists = "list"
			}
			text += fmt.Sprintf(". It is left out of group%s %s too, which %s it, so no play "+
				"reaches it", plural(len(groups)), clipNames(quoted), lists)
		}
		plan.warn("%s", text)
		plan.refused++
	}

	projectIDs := awxIDs{}
	var projectCreds []projectCredential
	projectName := orgQualifier(awxOrgNames(export.Projects, func(p awxProject) (string, string) {
		return p.Organization.Name, p.Name
	})...)
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
		obj := &project.Project{
			ID: project.NewID(), Name: projectName(p.Organization.Name, p.Name), RepoURL: p.ScmURL,
			Branch: p.ScmBranch, InstallDeps: true, CreatedAt: now,
		}
		if projectIDs.set(p.Organization.Name, p.Name, obj.ID) {
			plan.warn("project %q appears more than once; the later one is what templates naming "+
				"it will use", obj.Name)
		}
		plan.Projects = append(plan.Projects, obj)
		if p.Credential.Name != "" {
			projectCreds = append(projectCreds, projectCredential{project: obj, ref: p.Credential})
		}
	}

	inventoryIDs := awxIDs{}
	var nestedSources []awxInventorySource
	allInventories := append(export.Inventory, export.Inventories...)
	inventoryName := orgQualifier(awxOrgNames(allInventories, func(inv awxInventory) (string, string) {
		return inv.Organization.Name, inv.Name
	})...)
	var composed []awxInventory
	for _, inv := range allInventories {
		// A smart or constructed inventory holds no hosts of its own and names other inventories,
		// so it is created once every inventory it can name has an id.
		if kind := strings.ToLower(strings.TrimSpace(inv.Kind)); kind == inventory.KindSmart ||
			kind == inventory.KindConstructed {
			composed = append(composed, inv)
			continue
		}
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
			ID: inventory.NewID(), Name: inventoryName(inv.Organization.Name, inv.Name),
			Content: buildInventoryINI(plan, inv.Name,
				plan.convertHosts(hosts, "inventory "+quoteName(inv.Name)),
				plan.convertGroups(flattenGroups(groups), "inventory "+quoteName(inv.Name)),
				decodeVars(inv.Variables)),
			CreatedAt: now,
		}
		if inventoryIDs.set(inv.Organization.Name, inv.Name, obj.ID) {
			plan.warn("inventory %q appears more than once; the later one is what templates naming "+
				"it will use", obj.Name)
		}
		plan.Inventories = append(plan.Inventories, obj)
		if inv.Related != nil {
			nestedSources = append(nestedSources, inv.Related.InventorySources...)
		}
	}
	constructedSources := map[string]awxInventorySource{}
	for _, inv := range composed {
		if inv.Related == nil {
			continue
		}
		for _, src := range inv.Related.InventorySources {
			if src.constructed() {
				constructedSources[inv.Organization.Name+orgKeySep+inv.Name] = src
			} else {
				nestedSources = append(nestedSources, src)
			}
		}
	}
	for _, src := range export.InventorySources {
		if src.constructed() {
			constructedSources[src.Inventory.Org+orgKeySep+src.Inventory.Name] = src
		}
	}
	for _, inv := range composed {
		plan.addComposed(inv, inventoryName(inv.Organization.Name, inv.Name), now, inventoryIDs,
			constructedSources)
	}

	credentialIDs := awxIDs{}
	credentialName := orgQualifier(awxOrgNames(export.Credentials, func(c awxCredential) (string, string) {
		return c.Organization.Name, c.Name
	})...)
	customTypes, refusedTypes := plan.addCredentialTypes(export.CredentialTypes, now)
	for _, c := range export.Credentials {
		// A credential of a custom type the export also carries becomes a credential of the imported
		// type. It arrives as an empty shell like every other credential, and its type decides how
		// its values reach a run, files included, exactly as the type did in AWX.
		if typ, ok := customTypes[c.CredentialType.Name]; ok {
			obj := &credential.Credential{
				ID: credential.NewID(), Name: credentialName(c.Organization.Name, c.Name),
				TypeID: typ.ID, CreatedAt: now,
			}
			if credentialIDs.set(c.Organization.Name, c.Name, obj.ID) {
				plan.warn("credential %q appears more than once; the later one is what templates "+
					"naming it will use, and the two may not be the same kind", obj.Name)
			}
			plan.Credentials = append(plan.Credentials, obj)
			plan.warn("%s", typedCredentialWarning(c.Name, typ))
			plan.noteKubeconfigSwitch(c.Name, obj.Name, typ)
			continue
		}
		kind, exact := mapCredentialKind(c.CredentialType.Name, c.Inputs)
		// A credential of a custom type that did not come across is a guess whatever the name
		// says. A type called Templated Token maps to the token kind as an exact match, yet a run
		// that read API_TOKEN from the type's injector gets the token kind's own variable here, so
		// the credential carries a line saying so.
		switch injectors, refused := refusedTypes[c.CredentialType.Name]; {
		case refused:
			plan.warn("credential %q is of the custom type %q, which did not come across, so its "+
				"kind %q is a guess from the type's name: verify it is correct. Its AWX injectors "+
				"(%s) are replaced by what the %q kind injects here", c.Name, c.CredentialType.Name,
				kind, injectors, kind)
		case !exact:
			plan.warn("credential %q type %q mapped to %q; verify it is correct",
				c.Name, c.CredentialType.Name, kind)
		}
		obj := &credential.Credential{
			ID: credential.NewID(), Name: credentialName(c.Organization.Name, c.Name), Kind: kind,
			CreatedAt: now,
		}
		if credentialIDs.set(c.Organization.Name, c.Name, obj.ID) {
			plan.warn("credential %q appears more than once; the later one is what templates naming "+
				"it will use, and the two may not be the same kind", obj.Name)
		}
		// AWX keeps the vault label as a non-secret input; carrying it means a multi-vault setup
		// imports with its --vault-id labels intact instead of every password turning unlabeled. The
		// label is rendered by jsonScalarString, which reads a JSON null as empty, so an absent label
		// no longer has to be told apart from a real one by the "<nil>" text Go prints for it.
		if kind == credential.KindVaultPassword {
			if label := strings.TrimSpace(jsonScalarString(c.Inputs["vault_id"])); label != "" &&
				credential.ValidVaultID(label) {
				obj.VaultID = label
			}
		}
		// The export's non-secret inputs land as settings on the credential itself, so a machine
		// credential arrives knowing its connection user and become method and only the secret needs
		// entering. They used to survive only as warning text an operator had to copy by hand.
		settings, refused := credentialSettings(kind, c.Inputs)
		obj.Settings = settings
		plan.Credentials = append(plan.Credentials, obj)
		// A machine credential in AWX can hold a become password beside its connection secret. A
		// credential here holds one secret, so the become password arrives as a shell of its own,
		// attached wherever the first one is. It went unmentioned, and escalation failed at run time
		// on a password nobody had been asked to enter.
		if kind != credential.KindBecomePassword &&
			strings.TrimSpace(jsonScalarString(c.Inputs["become_password"])) == "$encrypted$" {
			become := &credential.Credential{
				ID: credential.NewID(), Name: obj.Name + " (become)", Kind: credential.KindBecomePassword,
				CreatedAt: now,
			}
			plan.Credentials = append(plan.Credentials, become)
			if plan.becomeFor == nil {
				plan.becomeFor = map[string]string{}
			}
			plan.becomeFor[obj.ID] = become.ID
			plan.warn("credential %q also held a become password, which arrives as the separate "+
				"credential %q attached wherever %q is; set its secret too", c.Name, become.Name, c.Name)
		}
		base := "credential %q needs its secret re-entered; an export never carries secret values"
		if !awxHeldSecret(c.Inputs) {
			// AWX writes every secret it holds as "$encrypted$", and this credential carries none: a
			// machine credential that only names a user, or a Galaxy credential for the public
			// server. Telling the operator to re-enter its secret sent them looking for one that
			// never existed.
			base = "credential %q had no secret in AWX, so there is nothing to re-enter. A run needs " +
				"a credential to hold a secret, so set one or detach it from the templates that use it"
		}
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

	// A project's source control credential is wired once the credentials exist, since the export
	// lists projects first. It was never read, so every private repository failed its first sync.
	for _, pc := range projectCreds {
		id, ok := credentialIDs.get(pc.ref)
		if !ok {
			plan.warn("project %q references unknown credential %s, so it imports without one and a "+
				"private repository will not sync until one is attached", pc.project.Name,
				credentialIDs.unresolved(pc.ref))
			continue
		}
		if kind := plan.credentialKind(id); kind != credential.KindSSHKey {
			plan.warn("project %q syncs with credential %q, which imports as a %s credential rather "+
				"than an SSH key. A project here syncs a private repository over SSH with a key, so "+
				"it imports without one: point it at an SSH remote and attach a key credential",
				pc.project.Name, pc.ref.Name, kind)
			continue
		}
		pc.project.CredentialID = id
	}

	for _, s := range export.InventorySources {
		// The source AWX keeps behind a constructed inventory became that inventory's options above.
		if s.constructed() {
			continue
		}
		plan.addSource(s, now, projectIDs, credentialIDs)
	}
	// Notification templates come before the objects attached to them, so each attachment can be
	// wired to the target its template became.
	plan.addNotificationTemplates(export.NotificationTemplates, now)
	plan.reportProjectNotifications(export.Projects)
	for _, s := range nestedSources {
		plan.addSource(s, now, projectIDs, credentialIDs)
	}
	// Placed once every inventory exists, so a smart inventory's organization takes in all of them.
	plan.placeAWXSmartInventories(export, inventoryIDs, now)

	templateName := orgQualifier(awxOrgNames(export.JobTemplates, func(jt awxJobTemplate) (string, string) {
		return jt.Organization.Name, jt.Name
	})...)
	plan.topSchedules = newAWXTopSchedules(export)
	for _, jt := range export.JobTemplates {
		plan.addTemplate(jt, templateName(jt.Organization.Name, jt.Name), now, projectIDs, inventoryIDs,
			credentialIDs)
	}
	// Workflows come after the job templates they run, since each node's step inlines the playbook
	// of the template it points at.
	plan.addWorkflows(export, now, projectIDs, inventoryIDs, credentialIDs)
	plan.topSchedules.report(plan)
	plan.attachAWXOrganizations(export.Organizations, now)
	plan.reportAWXPlacement()
	reportUnmapped(plan, export)
	// What the struct never had a field for, which reportUnmapped cannot see: it names the kinds this
	// importer knows it drops, and a field it does not know about is exactly the one nobody wrote down.
	reportUnread(plan, data, export)
	if err := plan.requireObjects("projects, inventories, credentials, job templates, " +
		"workflows, schedules, or notification templates"); err != nil {
		return nil, err
	}
	return plan, nil
}

// addSource maps one AWX inventory source into a dynamic source and the backing inventory it
// maintains, wiring the project and credential references by id. A file source keeps its path; a
// cloud plugin source has no file, so it imports with the plugin name and a warning that a config
// must be set before it can refresh.
func (p *Plan) addSource(s awxInventorySource, now time.Time, projectIDs, credentialIDs awxIDs) {
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
	if s.SourceProject.Name != "" {
		if id, ok := projectIDs.get(s.SourceProject); ok {
			src.ProjectID = id
		} else {
			p.warn("inventory source %q references unknown project %s", s.Name,
				projectIDs.unresolved(s.SourceProject))
		}
	}
	if s.Credential.Name != "" {
		if id, ok := credentialIDs.get(s.Credential); ok {
			src.CredentialID = id
		} else {
			p.warn("inventory source %q references unknown credential %s", s.Name,
				credentialIDs.unresolved(s.Credential))
		}
	}
	if vars := sourceVarsText(s.SourceVars); vars != "" {
		p.warn("inventory source %q carries source_vars in AWX, which were not imported: a source "+
			"here reads its options from its config file, so move them into %s", s.Name,
			quoteName(src.Source))
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
func (p *Plan) addTemplate(jt awxJobTemplate, name string, now time.Time,
	projectIDs, inventoryIDs, credentialIDs awxIDs) {
	tpl := &template.Template{
		ID: template.NewID(), Name: name, Playbook: jt.Playbook, CreatedAt: now,
		// The execution settings AWX holds on the job template. Dropping these silently changed what
		// the template does: a check-mode template imported as a live one, and a template limited to
		// a canary host imported targeting the whole inventory, both without a word in the report.
		Limit:     jt.Limit,
		Tags:      splitAWXTags(jt.JobTags),
		SkipTags:  splitAWXTags(jt.SkipTags),
		Verbosity: int(jt.Verbosity),
		Forks:     int(jt.Forks),
		Timeout:   int(jt.Timeout),
		DiffMode:  jt.DiffMode,
		DryRun:    jt.checkMode(),
	}
	if jt.Project.Name != "" {
		if id, ok := projectIDs.get(jt.Project); ok {
			tpl.ProjectID = id
		} else {
			// The template is not created. In AWX every job template is scoped to a project, so its
			// playbook path is relative to that checkout and is held inside it at run time. A
			// template with no project has no checkout to be held inside, and dispatch skips the
			// containment check entirely when there is no project, so importing one converts a path
			// that was contained into one that is resolved against the server's own directory.
			p.warn("template %q was not imported: it references project %s, which is not in this "+
				"export, and a template with no project has no checkout to resolve its playbook "+
				"against", name, projectIDs.unresolved(jt.Project))
			p.refused++
			return
		}
	}
	if jt.Inventory.Name != "" {
		if id, ok := inventoryIDs.get(jt.Inventory); ok {
			tpl.InventoryID = id
		} else {
			p.warn("template %q references unknown inventory %s", name,
				inventoryIDs.unresolved(jt.Inventory))
		}
	}
	if jt.JobSliceCount >= 2 {
		tpl.Shards = int(jt.JobSliceCount)
	}
	for _, ref := range jt.credentials() {
		if id, ok := credentialIDs.get(ref); ok {
			tpl.CredentialIDs = append(tpl.CredentialIDs, id)
			if become, split := p.becomeFor[id]; split {
				tpl.CredentialIDs = append(tpl.CredentialIDs, become)
			}
		} else if ref.Name != "" {
			p.warn("template %q references unknown credential %s", name, credentialIDs.unresolved(ref))
		}
	}
	if vars, err := parseExtraVars(jt.ExtraVars); err != nil {
		p.warn("template %q extra_vars could not be parsed: %v", jt.Name, err)
	} else {
		tpl.ExtraVars = vars
	}
	// AWX's become switch adds --become to the command line. A template has no such switch here,
	// so the same escalation rides as the connection variable, which is what the imported run
	// printed as become=UNSET without it. The report says where the two differ.
	if jt.BecomeEnabled {
		if _, set := tpl.ExtraVars["ansible_become"]; !set {
			if tpl.ExtraVars == nil {
				tpl.ExtraVars = map[string]any{}
			}
			tpl.ExtraVars["ansible_become"] = true
			p.warn("template %q runs with privilege escalation in AWX, so it carries ansible_become: "+
				"true in its extra vars. Unlike AWX's switch, an extra var also outranks a play that "+
				"sets become: false", name)
		}
	}
	p.mapFactCacheAndCallbacks(jt, tpl, name)
	if spec := jt.surveySpec(); jt.SurveyEnabled != nil && !*jt.SurveyEnabled && spec != nil &&
		len(spec.Spec) > 0 {
		// A survey switched off in AWX is never asked there, and its defaults are not applied either.
		// Importing it made every launch here demand answers the template never needed.
		p.warn("template %q has a survey that is switched off in AWX, so it was not imported: a "+
			"launch there asks nothing, and importing it would make every launch here require "+
			"answers", name)
	} else {
		tpl.Survey = p.mapSurvey(jt)
	}

	p.Templates = append(p.Templates, tpl)
	p.awxNotify().templateOrg[tpl.ID] = jt.Organization.Name
	var nested []awxSchedule
	if jt.Related != nil {
		nested = jt.Related.Schedules
	}
	scheds := p.topSchedules.take(awxTypeJobTemplate, jt.Organization.Name, jt.Name, nested)
	p.addSchedules(fmt.Sprintf("template %q", name), scheds, tpl.ID, inventoryIDs, now)
	if jt.Related != nil {
		// After the schedules, so an attachment also reaches the copy an overriding schedule fires.
		p.attachAWXNotifications(fmt.Sprintf("template %q", name), &jt.Related.awxNotifyRelated,
			tpl.ID, now)
	}
}

// mapFactCacheAndCallbacks carries AWX's fact cache switch and provisioning callback settings onto
// an imported template. Both come across as they are, and what an operator has to do afterward is
// said in the report: a callback URL is an address on the old server baked into every host's boot
// script, so it changes, and a key only comes across sealed on an install that has an encryption
// key.
func (p *Plan) mapFactCacheAndCallbacks(jt awxJobTemplate, tpl *template.Template, name string) {
	tpl.UseFactCache = jt.UseFactCache
	if jt.UseFactCache && tpl.InventoryID == "" {
		p.warn("template %q uses the fact cache in AWX, and its inventory did not come across, so "+
			"its runs gather facts without the cache until it names a stored inventory", name)
	}
	tpl.AllowCallbacks = jt.AllowCallbacks
	key := strings.TrimSpace(jt.HostConfigKey)
	// awxkit writes an encrypted field as this marker when the exporting account could not read it.
	if key == "$encrypted$" {
		key = ""
	}
	if key != "" {
		if p.callbackKeys == nil {
			p.callbackKeys = map[string]string{}
		}
		p.callbackKeys[tpl.ID] = key
	}
	if !jt.AllowCallbacks {
		return
	}
	switch key {
	case "":
		p.warn("template %q accepts provisioning callbacks in AWX, and the export carries no host "+
			"config key, so it refuses every callback until a key is minted for it", name)
	default:
		p.warn("template %q accepts provisioning callbacks in AWX. Its host config key comes "+
			"across sealed when this install has an encryption key; without one, mint a key after "+
			"the import", name)
	}
	p.mapAWXCallback(jt, tpl, name)
	if tpl.InventoryID == "" {
		p.warn("template %q accepts provisioning callbacks in AWX, and its inventory did not come "+
			"across, so no host can be matched until it names a stored inventory", name)
	}
}

// projectCredential is a project whose source control credential is resolved after the credentials
// are imported.
type projectCredential struct {
	// project is the imported project.
	project *project.Project
	// ref names the credential in the export.
	ref awxRef
}

// credentialName returns the name of the planned credential with the given id, or the id when no
// planned credential has it.
func (p *Plan) credentialName(id string) string {
	for _, c := range p.Credentials {
		if c.ID == id {
			return c.Name
		}
	}
	return id
}

// credentialKind returns the kind of the planned credential with the given id, empty when none has it.
func (p *Plan) credentialKind(id string) credential.Kind {
	for _, c := range p.Credentials {
		if c.ID == id {
			return c.Kind
		}
	}
	return ""
}

// surveySpec returns the job template's survey, top level or nested under related, or nil.
func (jt awxJobTemplate) surveySpec() *awxSurvey {
	if jt.SurveySpec != nil {
		return jt.SurveySpec
	}
	if jt.Related != nil {
		return jt.Related.SurveySpec
	}
	return nil
}

// mapSurvey converts a job template's survey, whether top level or nested under related, into
// SwitchTender survey fields, warning on any inexact type mapping.
func (p *Plan) mapSurvey(jt awxJobTemplate) []template.SurveyField {
	survey := jt.surveySpec()
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
		field := template.SurveyField{
			Var: f.Variable, Label: f.QuestionName, Type: fieldType,
			Required: f.Required, Default: f.Default, Choices: choicesFrom(f.Choices),
		}
		// A password prompt imports as a secret field, whose answer is sealed the way AWX encrypts
		// it. Its default is the one part that cannot come across: AWX exports it as $encrypted$.
		if field.Secret() {
			field.Default, field.Choices = nil, nil
			p.secretSurveyDefault(fmt.Sprintf("template %q", jt.Name), f.Variable, f.Default)
		}
		fields = append(fields, field)
	}
	return fields
}

// addSchedules maps the schedules of one imported object into the plan. A rule cron says exactly
// becomes a cron expression, which is the easier form to read and edit; every other rule comes
// across as the RFC 5545 recurrence it is, evaluated the way AWX evaluates it.
//
// owner names the thing the schedules belong to, already quoted, such as template "patch" or
// workflow "rollout". Both kinds arrive here so the two paths cannot drift apart on which rules
// they accept or how they report a refusal, and the report says which object lost its cadence.
//
// Before recurrences existed, a rule cron could not say was skipped with a warning: a COUNT or an
// UNTIL, an EXDATE that takes a holiday out, the last Friday of the quarter, every third day. Those
// are the schedules an AWX estate keeps for exactly the cases a nightly cron does not cover, so
// leaving them behind left the part of the migration nobody would notice until it did not run.
func (p *Plan) addSchedules(owner string, schedules []awxSchedule, templateID string,
	inventoryIDs awxIDs, now time.Time) {
	for _, s := range schedules {
		if strings.TrimSpace(s.RRule) == "" {
			p.warn("schedule %q of %s has no recurrence rule, so it was not imported", s.Name, owner)
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
				p.warn("schedule %q of %s names the timezone %q, which this system cannot "+
					"resolve, so it imports in the server's local time: %v",
					s.Name, owner, oneLine(zone), err)
				zone = ""
			}
		}
		cron, rule := awxCadence(s.RRule, zone, now)
		target, held := templateID, ""
		if overridden, why := p.scheduleTemplate(owner, s, templateID, inventoryIDs, now); overridden != "" {
			target, held = overridden, why
		}
		added := len(p.Schedules)
		p.addSchedule(&schedule.Schedule{
			ID: schedule.NewID(), Name: s.Name, Cron: cron, RRule: rule, Timezone: zone,
			TemplateID: target, Enabled: enabled, CreatedAt: now,
			SpringForward: awxSpringForward(cron),
		}, "this AWX export", now)
		// Switched off once it is added rather than before, so the report gives the reason it was
		// held here instead of saying it was switched off at the source, which it was not. One
		// switched off on the way in, for a required secret question with no default, keeps that
		// line and gets this reason as a second thing to fix before it is switched on.
		if held == "" || !enabled || len(p.Schedules) == added {
			continue
		}
		if sc := p.Schedules[len(p.Schedules)-1]; sc.Enabled {
			sc.Enabled = false
			p.warn("schedule %q of %s arrives switched off. %s Then switch it on.", s.Name, owner, held)
		} else {
			p.warn("schedule %q of %s also needs this before it is switched on. %s", s.Name,
				owner, held)
		}
	}
}

// awxCadence picks how an AWX rule is carried: as the cron expression RRULEToCron gives when that
// expression fires at the same moments the rule does, and as the rule itself otherwise. It returns
// one of the two and leaves the other empty.
//
// The two are compared rather than trusted. RRULEToCron reads the rule's time of day and weekdays
// and nothing about when it starts, so a rule whose DTSTART is next month converted to a cron
// firing from tonight, and the comparison is what catches that and anything like it. A rule this
// system cannot evaluate keeps the cron conversion, which is what imported before recurrences
// existed.
func awxCadence(rrule, zone string, now time.Time) (string, string) {
	rule := strings.TrimSpace(rrule)
	cron, ok := RRULEToCron(rrule)
	if !ok {
		return "", rule
	}
	asCron := &schedule.Schedule{Cron: cron, Timezone: zone, SpringForward: awxSpringForward(cron)}
	asRule := &schedule.Schedule{RRule: rule, Timezone: zone}
	cronAt, ruleAt := now, now
	for range cadenceChecks {
		ruleNext, err := asRule.NextFire(ruleAt)
		if err != nil {
			if errors.Is(err, schedule.ErrExhausted) {
				return "", rule
			}
			return cron, ""
		}
		cronNext, err := asCron.NextFire(cronAt)
		if err != nil || !cronNext.Equal(ruleNext) {
			return "", rule
		}
		cronAt, ruleAt = cronNext, ruleNext
	}
	return cron, ""
}

// cadenceChecks is how many upcoming fires a cron conversion must agree with its rule on before the
// cron form is used. It spans more than a week of daily fires, so a weekday the conversion dropped
// shows up.
const cadenceChecks = 12

// scheduleTemplate gives a schedule that overrides its template a copy of the template with the
// overrides applied, and returns the copy's id and, when part of what it overrides did not come
// across, the sentences saying what and what to do about it, which keep the schedule switched off.
// It returns an empty id for a schedule that overrides nothing, which fires its template as it is.
//
// A schedule fires a stored template with that template's own settings. An AWX schedule carries more:
// its survey answers and extra variables, and any limit, tags, or check mode it was saved with.
// Dropping them ran the schedule wider or with different answers than the one AWX ran, a nightly
// check against one host became a real run against every host, and a copy is the one way to keep
// the difference without changing what the template does when somebody launches it by hand.
//
// A variable the template's survey asks is the schedule's answer to that question rather than a
// plain extra var, and it becomes the copy's default for the question. A schedule fires with each
// question's default, and refuses one that is required with none, so an answer left as a plain extra
// var would have refused every fire of a schedule AWX ran without complaint. A secret answer cannot
// come across, since an export never carries one readably, so it is not carried as whatever stood in
// for it, and the schedule waits switched off until the question has a default.
func (p *Plan) scheduleTemplate(owner string, s awxSchedule, templateID string, inventoryIDs awxIDs,
	now time.Time) (string, string) {
	idx := slices.IndexFunc(p.Templates, func(t *template.Template) bool { return t.ID == templateID })
	var survey []template.SurveyField
	if idx >= 0 {
		survey = p.Templates[idx].Survey
	}
	answers, vars := splitScheduleAnswers(decodeVars(s.ExtraData), survey)
	var changes []string
	if len(answers) > 0 {
		changes = append(changes, "its answers to the survey ("+
			strings.Join(slices.Sorted(maps.Keys(answers)), ", ")+")")
	}
	if len(vars) > 0 {
		changes = append(changes, "its own variables ("+strings.Join(slices.Sorted(maps.Keys(vars)), ", ")+")")
	}
	set := func(v *string) bool { return v != nil && strings.TrimSpace(*v) != "" }
	if set(s.Limit) {
		changes = append(changes, "limit "+*s.Limit)
	}
	if set(s.JobTags) {
		changes = append(changes, "tags "+*s.JobTags)
	}
	if set(s.SkipTags) {
		changes = append(changes, "skip tags "+*s.SkipTags)
	}
	if set(s.JobType) {
		changes = append(changes, "job type "+*s.JobType)
	}
	if s.DiffMode != nil {
		changes = append(changes, fmt.Sprintf("diff %t", *s.DiffMode))
	}
	if s.Verbosity != nil {
		changes = append(changes, fmt.Sprintf("verbosity %d", *s.Verbosity))
	}
	if s.Forks != nil && *s.Forks > 0 {
		changes = append(changes, fmt.Sprintf("forks %d", *s.Forks))
	}
	if s.Timeout != nil && *s.Timeout > 0 {
		changes = append(changes, fmt.Sprintf("timeout %d", *s.Timeout))
	}
	if s.Inventory.Name != "" {
		changes = append(changes, "inventory "+s.Inventory.Name)
	}
	if len(changes) == 0 || idx < 0 {
		return "", ""
	}
	orig := p.Templates[idx]
	copied := *orig
	copied.ID, copied.Name, copied.CreatedAt = template.NewID(), orig.Name+" ("+s.Name+")", now
	// Every slice and map is its own, so nothing done to one template can reach the other.
	copied.ExtraVars = maps.Clone(orig.ExtraVars)
	copied.CredentialIDs = slices.Clone(orig.CredentialIDs)
	copied.SelectableCredentialIDs = slices.Clone(orig.SelectableCredentialIDs)
	copied.Tags, copied.SkipTags = slices.Clone(orig.Tags), slices.Clone(orig.SkipTags)
	copied.Survey, copied.Steps = slices.Clone(orig.Survey), slices.Clone(orig.Steps)
	copied.Notifications = slices.Clone(orig.Notifications)
	if len(vars) > 0 && copied.ExtraVars == nil {
		copied.ExtraVars = map[string]any{}
	}
	maps.Copy(copied.ExtraVars, vars)
	var held []string
	if unreadable := answerSurvey(copied.Survey, answers); len(unreadable) > 0 {
		quoted := make([]string, len(unreadable))
		for i, v := range unreadable {
			quoted[i] = strconv.Quote(v)
		}
		them := "it"
		if len(unreadable) > 1 {
			them = "each"
		}
		held = append(held, fmt.Sprintf("Its answer to the secret survey question%s %s did not "+
			"come across, because an export never carries a secret answer readably, so give %s a "+
			"default on %q.", plural(len(unreadable)), strings.Join(quoted, ", "), them, copied.Name))
	}
	if set(s.Limit) {
		copied.Limit = *s.Limit
	}
	if set(s.JobTags) {
		copied.Tags = splitAWXTags(*s.JobTags)
	}
	if set(s.SkipTags) {
		copied.SkipTags = splitAWXTags(*s.SkipTags)
	}
	if set(s.JobType) {
		copied.DryRun = strings.EqualFold(*s.JobType, "check")
	}
	if s.DiffMode != nil {
		copied.DiffMode = *s.DiffMode
	}
	if s.Verbosity != nil {
		copied.Verbosity = int(*s.Verbosity)
	}
	if s.Forks != nil && *s.Forks > 0 {
		copied.Forks = int(*s.Forks)
	}
	if s.Timeout != nil && *s.Timeout > 0 {
		copied.Timeout = int(*s.Timeout)
	}
	if s.Inventory.Name != "" {
		if id, ok := inventoryIDs.get(s.Inventory); ok {
			copied.InventoryID = id
		} else {
			held = append(held, "It overrides an inventory that did not come across, so point its "+
				"template at the right inventory.")
			p.warn("schedule %q of %s references unknown inventory %s", s.Name, owner,
				inventoryIDs.unresolved(s.Inventory))
		}
	}
	p.Templates = append(p.Templates, &copied)
	p.awxNotify().copies[templateID] = append(p.awxNotify().copies[templateID], copied.ID)
	p.warn("schedule %q of %s runs with %s, so it imports firing %q, a copy of the template with "+
		"them applied. A later change to the original template does not reach the copy.",
		s.Name, owner, strings.Join(changes, ", "), copied.Name)
	return copied.ID, strings.Join(held, " ")
}

// splitScheduleAnswers separates a schedule's variables into its answers to the template's survey,
// keyed by question, and the plain variables left over.
func splitScheduleAnswers(vars map[string]any, survey []template.SurveyField) (map[string]any, map[string]any) {
	answers, rest := map[string]any{}, map[string]any{}
	for name, v := range vars {
		if slices.ContainsFunc(survey, func(f template.SurveyField) bool { return f.Var == name }) {
			answers[name] = v
			continue
		}
		rest[name] = v
	}
	return answers, rest
}

// answerSurvey makes each answer the default of its question in survey, which a schedule fires
// with, and returns the secret questions it could not answer, sorted. A secret answer is never
// carried as a default or a variable: what an export holds in its place is not the answer, and a
// play handed it would run with a placeholder for a password.
func answerSurvey(survey []template.SurveyField, answers map[string]any) []string {
	var unreadable []string
	for i := range survey {
		v, ok := answers[survey[i].Var]
		if !ok {
			continue
		}
		if survey[i].Secret() {
			unreadable = append(unreadable, survey[i].Var)
			continue
		}
		survey[i].Default = v
	}
	slices.Sort(unreadable)
	return unreadable
}

// convertHosts adapts AWX hosts to the shared import host shape, decoding host variables and leaving
// out the hosts AWX had switched off.
//
// A disabled host sits in an AWX inventory and takes part in nothing: AWX runs against the enabled
// members and passes it over. The field was not on the struct, so it could not be read, and the host
// came across indistinguishable from a live one. That is the rare import error that does something
// rather than failing to do something, because the next play targeting all reaches a machine the
// estate had deliberately held back, and the operator who disabled it has no reason to look.
//
// Left out rather than carried and marked, because what this writes is INI inventory content and INI
// has no off switch for a host. The count and the names are reported, since a host that disappears
// with nothing said about it is its own kind of wrong.
func (p *Plan) convertHosts(hosts []awxHost, where string) []importHost {
	out := make([]importHost, 0, len(hosts))
	var off []string
	for _, h := range hosts {
		if h.Enabled != nil && !*h.Enabled {
			off = append(off, h.Name)
			continue
		}
		out = append(out, importHost{Name: h.Name, Variables: decodeVars(h.Variables)})
	}
	if len(off) > 0 {
		p.warn("%s: %d host%s disabled in AWX %s left out, since AWX runs nothing against %s and "+
			"importing %s would put %s in reach of the next play that targets all: %s",
			where, len(off), plural(len(off)), wasWere(len(off)), itOrThem(len(off)),
			itOrThem(len(off)), itOrThem(len(off)), clipNames(off))
	}
	return out
}

// convertGroups adapts AWX groups to the shared import group shape.
func (p *Plan) convertGroups(groups []awxGroup, where string) []importGroup {
	out := make([]importGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, importGroup{
			Name: g.Name, Hosts: p.convertHosts(g.hosts(), where+", group "+quoteName(g.Name)),
			Variables: decodeVars(g.Variables), Children: g.childNames(),
		})
	}
	return out
}

// clipNames renders a list of names for a warning, capped so one line stays one line.
func clipNames(names []string) string {
	const show = 8
	if len(names) <= show {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:show], ", ") + ", and " + strconv.Itoa(len(names)-show) + " more"
}

// quoteName renders a name for a warning, on one line and in quotes.
func quoteName(name string) string {
	return strconv.Quote(oneLine(name))
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
		{plan.awxUnplacedOrganizations(export.Organizations), plan.awxUnplacedOrgNoun(),
			"create %s with POST /v1/orgs and add members, which carries the same ownership"},
		{len(export.Teams), "team",
			"create %s with POST /v1/teams and grant access per object"},
	} {
		if item.Count == 0 {
			continue
		}
		verb, why := "are", item.Why
		if item.Count == 1 {
			verb = "is"
		}
		if strings.Contains(why, "%s") {
			why = fmt.Sprintf(why, itOrThem(item.Count))
		}
		plan.warn("this export holds %d %s%s, which %s not imported: %s",
			item.Count, item.What, plural(item.Count), verb, why)
	}
}

// plural returns the "s" a count needs, so a report reads one schedule and two schedules.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
