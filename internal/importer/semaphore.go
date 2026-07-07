package importer

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/schedule"
	"github.com/dcadolph/yardmaster/internal/template"
)

// semaphoreExport is a Semaphore export: a list of projects, each carrying its own repositories,
// inventories, keys, templates, and schedules.
type semaphoreExport struct {
	// Projects are the Semaphore projects.
	Projects []semaphoreProject `json:"projects"`
}

// semaphoreProject is one Semaphore project with its nested assets.
type semaphoreProject struct {
	// Name is the project name.
	Name string `json:"name"`
	// Repositories are the git repositories, each mapping to a Yardmaster project.
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

// FromSemaphore maps a Semaphore export into a Plan of Yardmaster objects with cross-references
// wired by generated id. Like the AWX mapping it records warnings rather than failing on an asset
// it cannot map cleanly.
func FromSemaphore(data []byte, now time.Time) (*Plan, error) {
	var export semaphoreExport
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, fmt.Errorf("parse semaphore export: %w", err)
	}
	plan := &Plan{}
	for _, proj := range export.Projects {
		plan.addSemaphoreProject(proj, now)
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
		p.Schedules = append(p.Schedules, &schedule.Schedule{
			ID: schedule.NewID(), Name: s.Name, Cron: s.CronFormat, TemplateID: id,
			Enabled: true, CreatedAt: now,
		})
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

// mapSemaphoreVarType converts a Semaphore survey variable type to a Yardmaster field type.
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
