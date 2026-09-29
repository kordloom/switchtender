package importer

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// semaphoreExport is a Semaphore export in either shape one arrives in.
//
// Semaphore's own project backup is a flat single-project document: its top level is the project,
// carrying meta, templates, repositories, keys, inventories, and schedules directly. Only the
// multi-project wrapper was read, so a real backup matched nothing and the import refused the file
// outright, telling the operator their export contained nothing recognizable. Both shapes are
// accepted here and normalized to the same list.
type semaphoreExport struct {
	// Projects carries the wrapper shape, a list of projects each holding its own assets.
	Projects []semaphoreProject `json:"projects"`
	// Meta names the project in a single-project backup.
	Meta *semaphoreMeta `json:"meta"`
	// The remaining fields are a single-project backup's own top-level assets.
	Repositories []semaphoreRepo      `json:"repositories"`
	Inventories  []semaphoreInventory `json:"inventories"`
	Keys         []semaphoreKey       `json:"keys"`
	Templates    []semaphoreTemplate  `json:"templates"`
	Schedules    []semaphoreSchedule  `json:"schedules"`
}

// semaphoreMeta is the project block of a single-project backup.
type semaphoreMeta struct {
	// Name is the project name.
	Name string `json:"name"`
}

// projects returns the projects to import from whichever shape the document used.
func (e semaphoreExport) projects() []semaphoreProject {
	if len(e.Projects) > 0 {
		return e.Projects
	}
	flat := semaphoreProject{
		Repositories: e.Repositories,
		Inventories:  e.Inventories,
		Keys:         e.Keys,
		Templates:    e.Templates,
		Schedules:    e.Schedules,
	}
	if e.Meta != nil {
		flat.Name = e.Meta.Name
	}
	if flat.empty() {
		return nil
	}
	return []semaphoreProject{flat}
}

// empty reports whether a project carries nothing worth importing, so a document that parsed but
// held no assets is refused rather than reported as a successful import of nothing.
func (p semaphoreProject) empty() bool {
	return len(p.Repositories) == 0 && len(p.Inventories) == 0 && len(p.Keys) == 0 &&
		len(p.Templates) == 0 && len(p.Schedules) == 0
}

// semaphoreProject is one Semaphore project with its nested assets.
type semaphoreProject struct {
	// Name is the project name.
	Name string `json:"name"`
	// Repositories are the git repositories, each mapping to a SwitchTender project.
	Repositories []semaphoreRepo `json:"repositories"`
	// Inventories are the project inventories.
	Inventories []semaphoreInventory `json:"inventories"`
	// Keys are the access keys, mapping to credentials.
	Keys []semaphoreKey `json:"keys"`
	// Templates are the task templates.
	Templates []semaphoreTemplate `json:"templates"`
	// Schedules are the cron schedules.
	Schedules []semaphoreSchedule `json:"schedules"`
}

// semaphoreRepo is a Semaphore git repository.
type semaphoreRepo struct {
	// Name identifies the repository within its project.
	Name string `json:"name"`
	// GitURL is the clone URL.
	GitURL string `json:"git_url"`
	// GitBranch is the branch to run.
	GitBranch string `json:"git_branch"`
}

// semaphoreInventory is a Semaphore inventory.
type semaphoreInventory struct {
	// Name identifies the inventory.
	Name string `json:"name"`
	// Type is static or file; only static carries inline content.
	Type string `json:"type"`
	// Inventory is the inline inventory text for a static inventory.
	Inventory string `json:"inventory"`
}

// semaphoreKey is a Semaphore access key, without secret material.
type semaphoreKey struct {
	// Name identifies the key.
	Name string `json:"name"`
	// Type is ssh, login_password, or none.
	Type string `json:"type"`
}

// semaphoreTemplate is a Semaphore task template.
type semaphoreTemplate struct {
	// Name identifies the template.
	Name string `json:"name"`
	// Playbook is the playbook path within the repository.
	Playbook string `json:"playbook"`
	// Repository names the repository the template runs from.
	Repository string `json:"repository"`
	// Inventory names the inventory the template targets.
	Inventory string `json:"inventory"`
	// SurveyVars are the template's survey variables.
	SurveyVars []semaphoreSurveyVar `json:"survey_vars"`
}

// semaphoreSurveyVar is one Semaphore survey variable.
type semaphoreSurveyVar struct {
	// Name is the variable name.
	Name string `json:"name"`
	// Title is the human prompt.
	Title string `json:"title"`
	// Type is string, int, enum, or secret.
	Type string `json:"type"`
	// Required rejects a launch that omits the variable.
	Required bool `json:"required"`
	// Values lists the allowed values for an enum variable.
	Values []string `json:"values"`
}

// semaphoreSchedule is a Semaphore cron schedule.
type semaphoreSchedule struct {
	// Name identifies the schedule.
	Name string `json:"name"`
	// CronFormat is the standard cron expression.
	CronFormat string `json:"cron_format"`
	// Template names the template the schedule runs.
	Template string `json:"template"`
}

// FromSemaphore maps a Semaphore export into a Plan of SwitchTender objects with cross-references
// wired by generated id. Like the AWX mapping it records warnings rather than failing on an asset
// it cannot map cleanly.
func FromSemaphore(data []byte, now time.Time) (*Plan, error) {
	var export semaphoreExport
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, fmt.Errorf("parse semaphore export: %w", err)
	}
	plan := &Plan{}
	for _, proj := range export.projects() {
		plan.addSemaphoreProject(proj, now)
	}
	if err := plan.requireObjects("repositories, inventories, keys, or templates"); err != nil {
		return nil, err
	}
	return plan, nil
}

// addSemaphoreProject maps one Semaphore project's repositories, inventories, keys, templates, and
// schedules into the plan.
func (p *Plan) addSemaphoreProject(proj semaphoreProject, now time.Time) {
	repoIDs := map[string]string{}
	for _, repo := range proj.Repositories {
		if repo.GitURL == "" {
			p.warn("repository %q in project %q skipped: no git url", repo.Name, proj.Name)
			continue
		}
		// The same check the API applies when a person creates a project, so an export cannot store
		// one the API itself would refuse.
		if err := project.ValidateRepoURL(repo.GitURL); err != nil {
			p.warn("repository %q in project %q skipped: %v", repo.Name, proj.Name, err)
			continue
		}
		obj := &project.Project{
			ID: project.NewID(), Name: projectName(proj.Name, repo.Name),
			RepoURL: repo.GitURL, Branch: repo.GitBranch, InstallDeps: true, CreatedAt: now,
		}
		p.Projects = append(p.Projects, obj)
		repoIDs[repo.Name] = obj.ID
	}

	inventoryIDs := map[string]string{}
	for _, inv := range proj.Inventories {
		if inv.Type != "" && inv.Type != "static" {
			p.warn("inventory %q in project %q is type %q; only static content imports",
				inv.Name, proj.Name, inv.Type)
		}
		obj := &inventory.Inventory{
			ID: inventory.NewID(), Name: inv.Name, Content: inv.Inventory, CreatedAt: now,
		}
		p.Inventories = append(p.Inventories, obj)
		inventoryIDs[inv.Name] = obj.ID
	}

	for _, key := range proj.Keys {
		kind, exact := mapSemaphoreKey(key.Type)
		if !exact {
			p.warn("key %q in project %q type %q mapped to %q; verify it is correct",
				key.Name, proj.Name, key.Type, kind)
		}
		p.Credentials = append(p.Credentials,
			&credential.Credential{ID: credential.NewID(), Name: key.Name, Kind: kind, CreatedAt: now})
		p.warn("key %q needs its secret re-entered; exports omit secrets by design", key.Name)
	}

	templateIDs := map[string]string{}
	for _, tmpl := range proj.Templates {
		obj := p.semaphoreTemplate(tmpl, repoIDs, inventoryIDs, now)
		p.Templates = append(p.Templates, obj)
		templateIDs[tmpl.Name] = obj.ID
	}

	for _, s := range proj.Schedules {
		id, ok := templateIDs[s.Template]
		if !ok {
			p.warn("schedule %q in project %q references unknown template %q",
				s.Name, proj.Name, s.Template)
			continue
		}
		// Semaphore's cron format is taken verbatim, so it is validated like any other before it
		// becomes a stored row.
		p.addSchedule(&schedule.Schedule{
			ID: schedule.NewID(), Name: s.Name, Cron: s.CronFormat, TemplateID: id,
			Enabled: true, CreatedAt: now,
		}, "this Semaphore export", now)
	}
}

// semaphoreTemplate maps one Semaphore template, wiring its repository and inventory references by
// id and mapping its survey variables.
func (p *Plan) semaphoreTemplate(tmpl semaphoreTemplate,
	repoIDs, inventoryIDs map[string]string, now time.Time) *template.Template {
	obj := &template.Template{
		ID: template.NewID(), Name: tmpl.Name, Playbook: tmpl.Playbook, CreatedAt: now,
	}
	if tmpl.Repository != "" {
		if id, ok := repoIDs[tmpl.Repository]; ok {
			obj.ProjectID = id
		} else {
			p.warn("template %q references unknown repository %q", tmpl.Name, tmpl.Repository)
		}
	}
	if tmpl.Inventory != "" {
		if id, ok := inventoryIDs[tmpl.Inventory]; ok {
			obj.InventoryID = id
		} else {
			p.warn("template %q references unknown inventory %q", tmpl.Name, tmpl.Inventory)
		}
	}
	for _, v := range tmpl.SurveyVars {
		// Semaphore prompts for a secret variable and stores it obscured. A survey field here is plain
		// text whose answer is kept on the run and injected as an extra var, so importing one would
		// turn a secret prompt into a value stored in the clear on every run of this template, in its
		// record, its exports, and the evidence drawn from it. The AWX importer refuses its equivalent
		// for the same reason; this one silently did the downgrade.
		if strings.EqualFold(v.Type, "secret") {
			p.warn("survey variable %q of template %q prompts for a secret and was NOT imported. "+
				"Store its value as a credential instead: importing it as a survey field would keep "+
				"the answer in plain text on every run.", v.Name, tmpl.Name)
			continue
		}
		obj.Survey = append(obj.Survey, template.SurveyField{
			Var: v.Name, Label: v.Title, Type: mapSemaphoreVarType(v.Type),
			Required: v.Required, Choices: v.Values,
		})
	}
	return obj
}

// projectName qualifies a repository name with its Semaphore project so imported project names stay
// distinct when several projects share a repository name.
func projectName(project, repo string) string {
	if repo == "" || repo == project {
		return project
	}
	return project + "/" + repo
}

// mapSemaphoreKey converts a Semaphore key type to a credential kind, reporting whether the mapping
// is exact.
func mapSemaphoreKey(keyType string) (credential.Kind, bool) {
	switch keyType {
	case "ssh":
		return credential.KindSSHKey, true
	case "login_password":
		return credential.KindEnv, true
	default:
		return credential.KindEnv, false
	}
}

// mapSemaphoreVarType converts a Semaphore survey variable type to a SwitchTender field type.
func mapSemaphoreVarType(varType string) template.FieldType {
	switch varType {
	case "int":
		return template.FieldInt
	case "enum":
		return template.FieldChoice
	default:
		return template.FieldText
	}
}
