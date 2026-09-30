package importer

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/util"
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
	// SSHKey names the access key the repository is cloned with.
	SSHKey string `json:"ssh_key"`
}

// semaphoreInventory is a Semaphore inventory.
type semaphoreInventory struct {
	// Name identifies the inventory.
	Name string `json:"name"`
	// Type is static or file; only static carries inline content.
	Type string `json:"type"`
	// Inventory is the inline inventory text for a static inventory.
	Inventory string `json:"inventory"`
	// SSHKey names the access key the inventory's hosts are reached with.
	SSHKey string `json:"ssh_key"`
	// BecomeKey names the access key that holds the become password for its hosts.
	BecomeKey string `json:"become_key"`
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
	// Arguments are the extra ansible-playbook arguments passed on every run: a JSON array, which
	// Semaphore stores and exports as a string holding one.
	Arguments json.RawMessage `json:"arguments"`
	// App is the tool the template runs: ansible, bash, terraform, tofu, python, powershell, or
	// another Semaphore supports. Empty is ansible, which is what an older export means.
	App string `json:"app"`
	// Vaults are the Ansible Vault passwords the template unlocks, each naming an access key.
	Vaults []semaphoreVault `json:"vaults"`
}

// semaphoreVault is one vault password a Semaphore template unlocks.
type semaphoreVault struct {
	// VaultKey names the access key that holds the password.
	VaultKey string `json:"vault_key"`
	// Name is the vault id the password is for, empty for the default vault.
	Name string `json:"name"`
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
	Values []semaphoreEnumValue `json:"values"`
}

// semaphoreEnumValue is one allowed value of an enum survey variable. Semaphore writes each as an
// object holding a label and the value the variable takes, and older exports as the value alone.
// Reading only the second shape failed every current export that had an enum survey, whole.
type semaphoreEnumValue struct {
	// Name is the label shown when choosing, when the export gives one.
	Name string `json:"name"`
	// Value is what the variable is set to.
	Value string `json:"value"`
}

// UnmarshalJSON reads a value given alone or as an object, and a number or boolean value as the text
// Ansible would receive.
func (v *semaphoreEnumValue) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &v.Value); err == nil {
		return nil
	}
	var obj struct {
		// Name is the label shown when choosing.
		Name string `json:"name"`
		// Value is what the variable is set to, in whatever JSON type the export used.
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	v.Name = obj.Name
	if err := json.Unmarshal(obj.Value, &v.Value); err != nil {
		v.Value = strings.TrimSpace(string(obj.Value))
	}
	return nil
}

// semaphoreSchedule is a Semaphore schedule: a cron cadence, or a single run at a set time.
type semaphoreSchedule struct {
	// Name identifies the schedule.
	Name string `json:"name"`
	// CronFormat is the standard cron expression, empty for a single run.
	CronFormat string `json:"cron_format"`
	// Type is empty for a cron cadence and semaphoreRunOnce for a single run at RunAt.
	Type string `json:"type"`
	// RunAt is when a single run fires.
	RunAt string `json:"run_at"`
	// Template names the template the schedule runs.
	Template string `json:"template"`
	// Active reports whether Semaphore fires this schedule; absent means active, which is what an
	// export written before the field existed means.
	Active *bool `json:"active"`
}

// semaphoreRunOnce is the schedule type Semaphore gives a single run at a set time.
const semaphoreRunOnce = "run_at"

// FromSemaphore maps a Semaphore export into a Plan of SwitchTender objects with cross-references
// wired by generated id. Like the AWX mapping it records warnings rather than failing on an asset
// it cannot map cleanly.
func FromSemaphore(data []byte, now time.Time) (*Plan, error) {
	data, err := textOf(data)
	if err != nil {
		return nil, err
	}
	if err := refuseJSONTail(data); err != nil {
		return nil, err
	}
	var export semaphoreExport
	skipped, err := decodeLenient(data, &export, reflect.TypeFor[semaphoreProject]())
	if err != nil {
		return nil, fmt.Errorf("parse semaphore export: %w", err)
	}
	plan := &Plan{}
	// A skipped entry counts as refused, so an export whose every entry was malformed reports each
	// one and why, rather than claiming nothing in it was recognized.
	for _, s := range skipped {
		plan.warn("%s", s)
		plan.refused++
	}
	for _, proj := range export.projects() {
		plan.addSemaphoreProject(proj, now)
	}
	// A field this importer has no struct member for was dropped without a decision and without a
	// warning, which is the one import result an operator acts on without reading.
	reportUnread(plan, data, export)
	if err := plan.requireObjects("repositories, inventories, keys, or templates"); err != nil {
		return nil, err
	}
	return plan, nil
}

// addSemaphoreProject maps one Semaphore project's repositories, inventories, keys, templates, and
// schedules into the plan.
func (p *Plan) addSemaphoreProject(proj semaphoreProject, now time.Time) {
	keys := p.addSemaphoreKeys(proj, now)
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
		if key, ok := keys[repo.SSHKey]; ok {
			if key.Kind == credential.KindSSHKey {
				obj.CredentialID = key.ID
			} else {
				p.warn("repository %q in project %q is cloned with key %q, a login and password. A "+
					"project here clones a private repository over SSH with a key, so it imports "+
					"without one: point it at an SSH remote and attach a key credential",
					repo.Name, proj.Name, repo.SSHKey)
			}
		}
		p.Projects = append(p.Projects, obj)
		repoIDs[repo.Name] = obj.ID
	}

	inventoryIDs := map[string]string{}
	for _, inv := range proj.Inventories {
		if inv.Type != "" && inv.Type != "static" {
			p.warn("inventory %q in project %q is type %q, so it imports holding what the "+
				"export's inventory field carried, which for a file inventory is a path rather "+
				"than hosts. Read it before relying on it", inv.Name, proj.Name, inv.Type)
		}
		// An inventory that arrives with nothing in it is reported, on the same terms as the AWX
		// importer: this format has more than one shape, and one that carries its content somewhere
		// this reader does not look produces an inventory targeting no hosts while the import still
		// reports success. Only for the static type, since the others are already reported above and
		// are expected to carry no inline content.
		if (inv.Type == "" || inv.Type == "static") && strings.TrimSpace(inv.Inventory) == "" {
			p.warn("inventory %q in project %q imported with no content. If it is not empty in "+
				"Semaphore, this export carries it somewhere this importer does not read, and the "+
				"inventory will target nothing", inv.Name, proj.Name)
		}
		obj := &inventory.Inventory{
			ID: inventory.NewID(), Name: inv.Name, Content: inv.Inventory, CreatedAt: now,
		}
		// The keys an inventory reaches its hosts with ride on the inventory, so every run against it
		// carries them, which is how Semaphore applies them. They used to be attached to nothing.
		for _, name := range []string{inv.SSHKey, inv.BecomeKey} {
			if key, ok := keys[name]; ok && !slices.Contains(obj.CredentialIDs, key.ID) {
				obj.CredentialIDs = append(obj.CredentialIDs, key.ID)
			}
		}
		p.Inventories = append(p.Inventories, obj)
		inventoryIDs[inv.Name] = obj.ID
	}

	templateIDs := map[string]string{}
	// uncarried names the templates whose arguments did not all come across, so their schedules can
	// arrive switched off rather than firing a template that no longer does what it did.
	uncarried := map[string]bool{}
	for _, tmpl := range proj.Templates {
		obj, complete := p.semaphoreTemplate(tmpl, repoIDs, inventoryIDs, keys, now)
		if obj == nil {
			continue
		}
		p.Templates = append(p.Templates, obj)
		templateIDs[tmpl.Name] = obj.ID
		if !complete {
			uncarried[tmpl.Name] = true
		}
	}

	for _, s := range proj.Schedules {
		id, ok := templateIDs[s.Template]
		if !ok {
			p.warn("schedule %q in project %q references unknown template %q",
				s.Name, proj.Name, s.Template)
			continue
		}
		// A single run carries no cron expression, and handing its empty one to the cron check
		// was reported as "bad cron: empty spec string", which reads as a broken export rather
		// than as a one-off. A schedule here repeats, so it does not come across, and the report
		// says why.
		if s.Type == semaphoreRunOnce {
			when := ""
			if at := strings.TrimSpace(s.RunAt); at != "" {
				when = ", at " + oneLine(at)
			}
			p.warn("schedule %q in project %q was not imported: it runs once%s, and a "+
				"schedule here repeats on a cron expression. Launch template %q by hand when it "+
				"is due, if it is still needed", s.Name, proj.Name, when, s.Template)
			continue
		}
		// Semaphore's cron format is taken verbatim, so it is validated like any other before it
		// becomes a stored row.
		// Armed only if Semaphore had it armed. This was hardcoded true, so a schedule somebody had
		// deactivated in Semaphore came across live and started firing a template on a cadence its own
		// estate had turned off, which the migration report counted as one more schedule carried
		// faithfully.
		enabled := s.Active == nil || *s.Active
		if enabled && uncarried[s.Template] {
			enabled = false
			p.warn("schedule %q in project %q arrives switched off, because template %q passes "+
				"arguments that did not come across. Switch it on once the template does what it "+
				"did in Semaphore.", s.Name, proj.Name, s.Template)
		}
		p.addSchedule(&schedule.Schedule{
			ID: schedule.NewID(), Name: s.Name, Cron: s.CronFormat, TemplateID: id,
			Enabled: enabled, CreatedAt: now,
		}, "this Semaphore export", now)
	}
}

// semaphoreTemplate maps one Semaphore template, wiring its repository and inventory references by
// id and mapping its survey variables and arguments. It reports false when some of the template's
// arguments could not be carried, which the caller holds its schedules to.
func (p *Plan) semaphoreTemplate(tmpl semaphoreTemplate, repoIDs, inventoryIDs map[string]string,
	keys map[string]*credential.Credential, now time.Time) (*template.Template, bool) {
	// Semaphore runs more than Ansible, and the tool is on the template. It was ignored, so a Bash
	// script imported as an Ansible template whose playbook was the script, and a Terraform directory
	// as one whose playbook was the directory: neither could run.
	tool, known := semaphoreApps[strings.ToLower(strings.TrimSpace(tmpl.App))]
	if !known {
		p.warn("template %q runs %s in Semaphore, which has no equivalent here, so it was not "+
			"imported", tmpl.Name, oneLine(tmpl.App))
		p.refused++
		return nil, false
	}
	obj := &template.Template{ID: template.NewID(), Name: tmpl.Name, CreatedAt: now}
	complete := true
	args, err := semaphoreArguments(tmpl.Arguments)
	if err != nil {
		complete = false
		p.warn("template %q has arguments that could not be read, so none of them were imported: "+
			"%v. Set the same limit, tags, and variables on the template before it runs.",
			tmpl.Name, err)
		args = nil
	}
	switch tool {
	case run.ToolAnsible:
		obj.Playbook = tmpl.Playbook
		if left := carryPlaybookArgs(obj, args); len(left) > 0 {
			complete = false
			p.warn("template %q passes %s to ansible-playbook on every run, and those arguments "+
				"were not imported. Set the same behavior on the template or in the playbook before "+
				"it runs.", tmpl.Name, quotedArgs(left))
		}
	case run.ToolTerraform, run.ToolOpenTofu:
		// A run here applies without asking, and a Semaphore Terraform template plans first, so it
		// imports as a plan. Launching it without dry run applies it.
		obj.Tool, obj.Command, obj.DryRun = tool, tmpl.Playbook, true
		p.warn("template %q runs %s in Semaphore and imported as a plan of %s, since a run here "+
			"applies without asking. Launch it without dry run once its plan reads right",
			tmpl.Name, tool, tmpl.Playbook)
		if len(args) > 0 {
			complete = false
			p.warn("template %q passes %s to %s on every run, and those arguments were not "+
				"imported", tmpl.Name, quotedArgs(args), tool)
		}
	default:
		obj.Tool, obj.Command = tool, semaphoreScript(tool, tmpl.Playbook, args)
	}
	if tmpl.Repository != "" {
		if id, ok := repoIDs[tmpl.Repository]; ok {
			obj.ProjectID = id
		} else {
			p.warn("template %q references unknown repository %q", tmpl.Name, tmpl.Repository)
		}
	}
	switch {
	case tmpl.Inventory == "":
	case tool == run.ToolTerraform || tool == run.ToolOpenTofu:
		// A Terraform template's inventory in Semaphore selects a workspace rather than hosts.
		p.warn("template %q picks its Terraform workspace through inventory %q in Semaphore, and a "+
			"run here selects no workspace, so it plans the default one. Check that before applying",
			tmpl.Name, tmpl.Inventory)
	default:
		if id, ok := inventoryIDs[tmpl.Inventory]; ok {
			obj.InventoryID = id
		} else {
			p.warn("template %q references unknown inventory %q", tmpl.Name, tmpl.Inventory)
		}
	}
	for _, v := range tmpl.Vaults {
		key, ok := keys[v.VaultKey]
		if !ok {
			if v.VaultKey != "" {
				p.warn("template %q unlocks a vault with key %q, which is not in this export",
					tmpl.Name, v.VaultKey)
			}
			continue
		}
		obj.CredentialIDs = append(obj.CredentialIDs, key.ID)
		if label := strings.TrimSpace(v.Name); label != "" && key.VaultID == "" &&
			credential.ValidVaultID(label) {
			key.VaultID = label
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
			Required: v.Required, Choices: enumChoices(v.Values),
		})
	}
	return obj, complete
}

// semaphoreApps maps the tool a Semaphore template runs to the tool it runs as here. An app missing
// from it, such as pulumi, has no equivalent.
var semaphoreApps = map[string]string{
	"": run.ToolAnsible, "ansible": run.ToolAnsible, "bash": run.ToolBash,
	"terraform": run.ToolTerraform, "tofu": run.ToolOpenTofu, "opentofu": run.ToolOpenTofu,
	"python": run.ToolPython, "powershell": run.ToolPowerShell, "pwsh": run.ToolPowerShell,
}

// semaphoreScript returns the command that runs a Semaphore script template's file from the project
// checkout with its arguments. A run's command here is the script itself, and Semaphore's template
// names a file in the repository, so the command runs that file.
func semaphoreScript(tool, path string, args []string) string {
	switch tool {
	case run.ToolPython:
		argv := []string{util.PyQuote(path)}
		for _, a := range args {
			argv = append(argv, util.PyQuote(a))
		}
		return "import runpy\nimport sys\n\nsys.argv = [" + strings.Join(argv, ", ") + "]\n" +
			"runpy.run_path(" + util.PyQuote(path) + ", run_name=\"__main__\")\n"
	case run.ToolPowerShell:
		cmd := "& (Join-Path (Get-Location) " + psQuote(path) + ")"
		for _, a := range args {
			cmd += " " + psQuote(a)
		}
		return cmd
	default:
		cmd := "bash " + util.ShellArg(path)
		for _, a := range args {
			cmd += " " + util.ShellArg(a)
		}
		return cmd
	}
}

// psQuote writes s as a PowerShell single-quoted string, in which nothing is expanded.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// enumChoices returns the values an enum variable may take, in the export's order.
func enumChoices(values []semaphoreEnumValue) []string {
	var out []string
	for _, v := range values {
		out = append(out, v.Value)
	}
	return out
}

// semaphoreArguments reads a template's arguments. Semaphore stores them as a string holding a JSON
// array and exports them that way, so both that and a plain array are read, and an absent, null, or
// empty value means none.
func semaphoreArguments(raw json.RawMessage) ([]string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil, nil
	}
	if strings.HasPrefix(text, `"`) {
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return nil, err
		}
		text = strings.TrimSpace(inner)
		if text == "" {
			return nil, nil
		}
	}
	var args []string
	if err := json.Unmarshal([]byte(text), &args); err != nil {
		return nil, fmt.Errorf("%q is not a list of arguments", oneLine(text))
	}
	return args, nil
}

// quotedArgs renders arguments for a warning, each quoted so an empty or spaced one is visible.
func quotedArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = strconv.Quote(a)
	}
	return strings.Join(quoted, " ")
}

// projectName qualifies a repository name with its Semaphore project so imported project names stay
// distinct when several projects share a repository name.
func projectName(project, repo string) string {
	if repo == "" || repo == project {
		return project
	}
	return project + "/" + repo
}

// semaphoreKeyUse records where a Semaphore access key is attached, which is what decides what it
// holds.
type semaphoreKeyUse struct {
	// repo is set when a repository is cloned with the key.
	repo bool
	// login is set when an inventory reaches its hosts with the key.
	login bool
	// become is set when an inventory's become password is the key.
	become bool
	// vault is set when a template unlocks an Ansible Vault with the key.
	vault bool
}

// semaphoreKeyUses records where each access key in a project is attached.
func semaphoreKeyUses(proj semaphoreProject) map[string]semaphoreKeyUse {
	uses := map[string]semaphoreKeyUse{}
	mark := func(name string, set func(*semaphoreKeyUse)) {
		if name == "" {
			return
		}
		u := uses[name]
		set(&u)
		uses[name] = u
	}
	for _, r := range proj.Repositories {
		mark(r.SSHKey, func(u *semaphoreKeyUse) { u.repo = true })
	}
	for _, inv := range proj.Inventories {
		mark(inv.SSHKey, func(u *semaphoreKeyUse) { u.login = true })
		mark(inv.BecomeKey, func(u *semaphoreKeyUse) { u.become = true })
	}
	for _, t := range proj.Templates {
		for _, v := range t.Vaults {
			mark(v.VaultKey, func(u *semaphoreKeyUse) { u.vault = true })
		}
	}
	return uses
}

// semaphoreKeyKind returns the credential kind an access key imports as, and a note for the report
// when that kind cannot be settled from the export. A login_password key means what its attachment
// makes it: a vault password, the login a host is reached with, or a become password. It imported as
// env wherever it was used, so a vault password or an SSH login arrived as an environment line that no
// run would read.
func semaphoreKeyKind(keyType string, use semaphoreKeyUse) (credential.Kind, string) {
	switch keyType {
	case "ssh":
		return credential.KindSSHKey, ""
	case "login_password":
		uses := 0
		for _, used := range []bool{use.vault, use.login, use.become} {
			if used {
				uses++
			}
		}
		also := ""
		if uses > 1 {
			also = "it serves more than one purpose in Semaphore, and a credential here holds one, so " +
				"attach a second credential for the others"
		}
		switch {
		case use.vault:
			return credential.KindVaultPassword, also
		case use.login:
			return credential.KindSSHPassword, also
		case use.become:
			return credential.KindBecomePassword, ""
		case use.repo:
			return credential.KindEnv, "it clones a git repository over HTTPS, which a project here " +
				"does not do"
		default:
			return credential.KindEnv, "nothing in this export uses it, so what it holds is a guess"
		}
	default:
		return credential.KindEnv, fmt.Sprintf("its type %q has no equivalent here", keyType)
	}
}

// addSemaphoreKeys imports a project's access keys as credential shells, each of the kind its use in
// the export makes it, and returns them by name so the objects that use them can attach them. A key of
// type none holds nothing, and Semaphore creates one as a placeholder, so none is imported.
func (p *Plan) addSemaphoreKeys(proj semaphoreProject, now time.Time) map[string]*credential.Credential {
	uses := semaphoreKeyUses(proj)
	keys := map[string]*credential.Credential{}
	var empty []string
	for _, key := range proj.Keys {
		if key.Type == "none" {
			empty = append(empty, strconv.Quote(key.Name))
			continue
		}
		kind, note := semaphoreKeyKind(key.Type, uses[key.Name])
		obj := &credential.Credential{ID: credential.NewID(), Name: key.Name, Kind: kind, CreatedAt: now}
		p.Credentials = append(p.Credentials, obj)
		keys[key.Name] = obj
		if note != "" {
			p.warn("key %q in project %q imported as %s, and %s. Check it before a run uses it",
				key.Name, proj.Name, kind, note)
		}
		p.warn("key %q needs its secret re-entered; an export never carries secret values", key.Name)
	}
	if len(empty) > 0 {
		verb := "holds"
		if len(empty) > 1 {
			verb = "hold"
		}
		p.warn("key%s %s in project %q %s no secret in Semaphore (type none), so no credential was "+
			"created for %s", plural(len(empty)), strings.Join(empty, ", "), proj.Name, verb,
			itOrThem(len(empty)))
	}
	return keys
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
