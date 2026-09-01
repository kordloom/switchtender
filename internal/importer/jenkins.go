package importer

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// jenkinsBundle is the wrapper the CLI builds when it walks a Jenkins jobs directory.
//
// A Jenkins job's name is the name of the directory holding its config.xml and appears nowhere
// inside the file, so the walker records it here. The inner document is kept raw and parsed job by
// job, which lets one code path serve both a bundle and a single config.xml handed over on its own.
type jenkinsBundle struct {
	// Jobs are the collected job documents, each tagged with the name its directory carried.
	Jobs []jenkinsBundledJob `xml:"job"`
}

// jenkinsBundledJob is one job document inside a bundle, carrying the name its directory gave it.
type jenkinsBundledJob struct {
	// Name is the job's full Jenkins name, including any folder path.
	Name string `xml:"name,attr"`
	// Inner is the job's unparsed config.xml body.
	Inner []byte `xml:",innerxml"`
}

// jenkinsProject is a Jenkins freestyle job, the only job type with a faithful mapping here.
type jenkinsProject struct {
	// Description is the job description.
	Description string `xml:"description"`
	// DisplayName overrides the job's directory name in the Jenkins UI.
	DisplayName string `xml:"displayName"`
	// Disabled reports whether the job is switched off in Jenkins.
	Disabled bool `xml:"disabled"`
	// AssignedNode is the agent label the job is pinned to, when it does not roam.
	AssignedNode string `xml:"assignedNode"`
	// AuthToken is the job's remote build trigger token, a shared secret that is never imported.
	AuthToken string `xml:"authToken"`
	// SCM is the source control configuration, which becomes a project when it is git.
	SCM jenkinsSCM `xml:"scm"`
	// Properties holds the parameter definitions, which become survey fields.
	Properties jenkinsProperties `xml:"properties"`
	// Triggers holds the build triggers, of which only the timer becomes a schedule.
	Triggers jenkinsElements `xml:"triggers"`
	// Builders holds the ordered build steps.
	Builders jenkinsElements `xml:"builders"`
	// BuildWrappers holds wrappers such as the build timeout.
	BuildWrappers jenkinsElements `xml:"buildWrappers"`
}

// jenkinsElements collects a heterogeneous, ordered list of plugin elements.
//
// Jenkins names each build step, trigger, and wrapper after its implementing Java class, so the
// children of one list all differ and their order is the execution order. Decoding into a typed
// field per class would both miss every plugin not named here and lose that order, so the elements
// are collected as they come and identified by name afterward.
type jenkinsElements struct {
	// Items are the child elements in document order.
	Items []jenkinsElement `xml:",any"`
}

// jenkinsElement is one plugin element, identified by the Java class Jenkins named it after.
type jenkinsElement struct {
	// XMLName is the element name, which is the implementing class.
	XMLName xml.Name
	// Command is the script body of a shell or batch build step.
	Command string `xml:"command"`
	// Spec is the cron specification of a trigger.
	Spec string `xml:"spec"`
	// TimeoutMinutes is the cap set by a build timeout wrapper.
	TimeoutMinutes string `xml:"strategy>timeoutMinutes"`
	// Targets names the goals of an Ant or Maven step, used only to describe what was skipped.
	Targets string `xml:"targets"`
}

// jenkinsSCM is a job's source control configuration.
type jenkinsSCM struct {
	// Class is the SCM implementation, which is hudson.plugins.git.GitSCM for git.
	Class string `xml:"class,attr"`
	// URLs are the configured remote repository URLs.
	URLs []string `xml:"userRemoteConfigs>hudson.plugins.git.UserRemoteConfig>url"`
	// Branches are the configured branch specifications, such as */main.
	Branches []string `xml:"branches>hudson.plugins.git.BranchSpec>name"`
}

// jenkinsProperties holds the job properties that carry parameter definitions.
type jenkinsProperties struct {
	// Params are the job's parameter definitions, which become survey fields.
	Params jenkinsParamList `xml:"hudson.model.ParametersDefinitionProperty>parameterDefinitions"`
}

// jenkinsParamList collects a job's parameter definitions, which like build steps are each named
// after the Java class implementing them.
type jenkinsParamList struct {
	// Items are the parameter definitions in the order Jenkins prompts for them.
	Items []jenkinsParam `xml:",any"`
}

// jenkinsParam is one parameter definition.
type jenkinsParam struct {
	// XMLName is the element name, which names the parameter's Java class.
	XMLName xml.Name
	// Name is the parameter name, which becomes the survey field's variable.
	Name string `xml:"name"`
	// Description is shown to the person launching the job.
	Description string `xml:"description"`
	// DefaultValue is the parameter's default.
	DefaultValue string `xml:"defaultValue"`
	// NestedChoices are the allowed values in the wrapped form Jenkins writes today.
	NestedChoices []string `xml:"choices>a>string"`
	// FlatChoices are the allowed values in the older unwrapped form.
	FlatChoices []string `xml:"choices>string"`
}

// jenkinsJobTypes names each Jenkins root element that is not a freestyle project, so a job that
// cannot be imported is refused by name rather than reported as an unrecognized file.
var jenkinsJobTypes = map[string]string{
	"flow-definition": "a Pipeline job, whose Groovy script has no equivalent here. " +
		"Its stages would need rewriting as pipeline steps.",
	"org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject": "a multibranch " +
		"Pipeline, which builds a Jenkinsfile per branch and has no equivalent here.",
	"matrix-project": "a matrix job, whose axis combinations have no equivalent here. " +
		"A sharded template covers some of the same ground.",
	"maven2-moduleset":         "a Maven job, which is a build rather than an operational task.",
	"hudson.ivy.IvyModuleSet":  "an Ivy job, which is a build rather than an operational task.",
	"hudson.model.ExternalJob": "an external job, which only records runs that happened elsewhere.",
}

// jenkinsEnvVars are the variables Jenkins sets for every build. A script using one runs differently
// here, so each is reported rather than left to fail at run time.
var jenkinsEnvVars = []string{
	"BUILD_NUMBER", "BUILD_ID", "BUILD_DISPLAY_NAME", "BUILD_TAG", "BUILD_URL",
	"JOB_NAME", "JOB_BASE_NAME", "JOB_URL", "EXECUTOR_NUMBER", "NODE_NAME", "NODE_LABELS",
	"WORKSPACE", "WORKSPACE_TMP", "JENKINS_HOME", "JENKINS_URL",
	"GIT_COMMIT", "GIT_BRANCH", "GIT_URL", "GIT_PREVIOUS_COMMIT", "CHANGE_ID",
}

// FromJenkins maps Jenkins freestyle jobs into a plan of equivalent objects.
//
// Each freestyle job becomes a Bash template carrying its shell steps in order, its parameters
// become a survey, and each line of its timer trigger becomes a schedule. Jenkins targets an agent
// by label rather than an inventory file, so pass an inventory to say which hosts these jobs run
// against; without one every template launches with no target.
//
// Only freestyle jobs are imported. A Pipeline job is a Groovy program, and there is no honest
// mechanical translation from one to a template, so every other job type is refused by name instead
// of being half-imported into something that would not do what it used to.
func FromJenkins(inventory string) func([]byte, time.Time) (*Plan, error) {
	return func(data []byte, now time.Time) (*Plan, error) {
		jobs, err := decodeJenkins(data)
		if err != nil {
			return nil, err
		}
		plan := &Plan{}
		if inventory == "" {
			plan.warn("no inventory was named, so every imported template launches without one. " +
				"Re-run with --inventory to say which hosts these jobs run against, or set it on " +
				"each template afterward.")
		}
		for _, job := range jobs {
			plan.addJenkinsJob(job, inventory, now)
		}
		if err := plan.requireObjects("freestyle jobs"); err != nil {
			return nil, err
		}
		return plan, nil
	}
}

// decodeJenkins reads a zip of a jobs directory, a bundle of named job documents, or a single bare
// config.xml.
func decodeJenkins(data []byte) ([]jenkinsBundledJob, error) {
	if IsJenkinsZip(data) {
		bundle, err := JenkinsBundleFromZip(data)
		if err != nil {
			return nil, err
		}
		data = bundle
	}
	root, err := jenkinsRootElement(data)
	if err != nil {
		return nil, err
	}
	if root != "jobs" {
		// A lone config.xml carries no name of its own, so the job type stands in for one. The CLI
		// names jobs from their directories, which is where a real import gets them.
		return []jenkinsBundledJob{{Name: "", Inner: data}}, nil
	}
	var bundle jenkinsBundle
	if err := xml.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("parse jenkins jobs: %w", err)
	}
	if len(bundle.Jobs) == 0 {
		return nil, fmt.Errorf("parse jenkins jobs: no job documents found")
	}
	return bundle.Jobs, nil
}

// jenkinsRootElement returns the name of a document's first element, which is how a freestyle job,
// a Pipeline, and a bundle are told apart before any of them is decoded.
func jenkinsRootElement(data []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("parse jenkins xml: %w", err)
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, nil
		}
	}
}

// addJenkinsJob maps one job document into the plan, refusing any job type that has no faithful
// equivalent rather than importing part of it.
func (p *Plan) addJenkinsJob(job jenkinsBundledJob, inventoryName string, now time.Time) {
	// The bundle wrapper strips the job's own root element from view, so it is read back here.
	root, err := jenkinsRootElement(job.Inner)
	if err != nil {
		p.warn("job %q could not be read: %v", oneLine(job.Name), err)
		return
	}
	name := jenkinsCleanName(job.Name)
	if name == "" {
		name = root
	}
	if root == "com.cloudbees.hudson.plugins.folder.Folder" {
		// A folder holds jobs rather than doing anything itself, and the walker already flattened
		// its contents into the names of the jobs inside it.
		return
	}
	if why, refused := jenkinsJobTypes[root]; refused {
		p.warn("job %q is %s It was not imported.", name, why)
		return
	}
	if root != "project" {
		p.warn("job %q has the unrecognized type %q and was not imported", name, oneLine(root))
		return
	}

	var proj jenkinsProject
	if err := xml.Unmarshal(job.Inner, &proj); err != nil {
		p.warn("job %q could not be read: %v", name, err)
		return
	}
	p.addJenkinsFreestyle(name, proj, inventoryName, now)
}

// jenkinsCleanName strips control characters from a job name.
//
// A job's name comes from the name of the directory holding it, and a directory name on Unix may
// contain a newline. It is then interpolated into warning lines, so a job directory named with an
// embedded newline could otherwise write warning lines of its own into an import report the operator
// is reading to decide what is safe. Clipping is deliberately not applied here, unlike oneLine: a
// deeply nested job has a long legitimate name and this value becomes the template's real name.
func jenkinsCleanName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, name)
	return strings.TrimSpace(strings.Join(strings.Fields(cleaned), " "))
}

// addJenkinsFreestyle maps a decoded freestyle job into a template and its schedules.
func (p *Plan) addJenkinsFreestyle(name string, proj jenkinsProject, inventoryName string,
	now time.Time) {
	command, ok := p.jenkinsCommand(name, proj)
	if !ok {
		return
	}
	if proj.Disabled {
		p.warn("job %q is disabled in Jenkins; it is imported but you may want to leave it unused",
			name)
	}
	if proj.AuthToken != "" {
		p.warn("job %q has a remote trigger token, which was NOT imported. It is a shared secret "+
			"that would grant anybody holding it a launch. Use a webhook with its own secret "+
			"instead.", name)
	}
	if node := strings.TrimSpace(proj.AssignedNode); node != "" {
		p.warn("job %q was pinned to the Jenkins agent label %q. SwitchTender targets an "+
			"inventory, so check that the inventory you attached covers the same machines.",
			name, oneLine(node))
	}
	tmpl := &template.Template{
		ID: template.NewID(), Name: name, Tool: "bash", Command: command,
		Inventory: inventoryName, Survey: p.jenkinsSurvey(name, proj),
		Timeout: p.jenkinsTimeout(name, proj), CreatedAt: now,
	}
	p.jenkinsSCMWarning(name, proj)
	p.Templates = append(p.Templates, tmpl)

	for _, spec := range p.jenkinsSchedules(name, proj) {
		p.addSchedule(&schedule.Schedule{
			ID: schedule.NewID(), Name: name, Cron: spec, TemplateID: tmpl.ID,
			Enabled: !proj.Disabled, CreatedAt: now,
		}, "jenkins", now)
	}
}

// jenkinsCommand renders a job's build steps as one Bash script, reporting whether anything runnable
// was found.
//
// Jenkins stops a build at the first failing step, so the script opens with set -e to match. A step
// with no shell equivalent is reported and left out rather than guessed at, and a job made only of
// those is skipped rather than imported as a template that would report success without doing
// anything.
func (p *Plan) jenkinsCommand(name string, proj jenkinsProject) (string, bool) {
	var b strings.Builder
	b.WriteString("set -e\n")
	steps := 0
	for i, item := range proj.Builders.Items {
		switch item.XMLName.Local {
		case "hudson.tasks.Shell":
			body := p.jenkinsShellBody(name, i+1, item.Command)
			if body == "" {
				p.warn("job %q step %d is an empty shell step, which was left out", name, i+1)
				continue
			}
			b.WriteString("\n")
			b.WriteString(body)
			b.WriteString("\n")
			steps++
		case "hudson.tasks.BatchFile":
			p.warn("job %q step %d is a Windows batch step, which was left out because the rest of "+
				"the job imports as Bash. Recreate it as a PowerShell template if the job ran on "+
				"Windows.", name, i+1)
		default:
			p.warn("job %q step %d is a %s step%s, which has no equivalent and was left out",
				name, i+1, jenkinsStepLabel(item.XMLName.Local), jenkinsTargets(item.Targets))
		}
	}
	if steps == 0 {
		p.warn("job %q was skipped: none of its build steps could be imported, so the template "+
			"would have reported success without running anything", name)
		return "", false
	}
	return b.String(), true
}

// jenkinsShellBody prepares one shell step's script, reporting what will behave differently here.
func (p *Plan) jenkinsShellBody(name string, step int, command string) string {
	body := strings.TrimRight(strings.ReplaceAll(command, "\r\n", "\n"), "\n")
	if strings.TrimSpace(body) == "" {
		return ""
	}
	// Jenkins honors a shebang on the first line and runs the step under that interpreter. Templates
	// run their script with bash -c, where a shebang is only a comment, so a step written in another
	// language would silently be fed to the wrong interpreter.
	if first, _, _ := strings.Cut(body, "\n"); strings.HasPrefix(first, "#!") {
		if interp := strings.TrimSpace(strings.TrimPrefix(first, "#!")); !jenkinsShellShebang(interp) {
			p.warn("job %q step %d starts with %q, so Jenkins ran it under that interpreter. This "+
				"template runs its script with bash, so rewrite the step or move it to the matching "+
				"tool.", name, step, oneLine(first))
		}
	}
	for _, v := range jenkinsEnvVars {
		if jenkinsUsesVar(body, v) {
			p.warn("job %q step %d uses the Jenkins variable $%s, which is not set here. Supply it "+
				"as a survey field or an extra var, or the step will read it as empty.",
				name, step, v)
		}
	}
	return body
}

// jenkinsShellShebang reports whether a shebang names a shell, whose script bash will run correctly.
//
// The interpreter is the first word, not the last: reading the last word called "#!/bin/bash -xe" a
// non-shell script named "-xe" and reported every ordinary shell step in the export.
func jenkinsShellShebang(interp string) bool {
	fields := strings.Fields(interp)
	if len(fields) == 0 {
		return false
	}
	name := jenkinsBaseName(fields[0])
	// "env python3" names its interpreter in the next word, past any options env itself takes.
	if name == "env" {
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "-") || strings.Contains(f, "=") {
				continue
			}
			name = jenkinsBaseName(f)
			break
		}
		if name == "env" {
			return false
		}
	}
	switch name {
	case "sh", "bash", "dash", "ksh", "zsh", "ash":
		return true
	}
	return false
}

// jenkinsBaseName strips the directory from an interpreter path.
func jenkinsBaseName(p string) string {
	if slash := strings.LastIndex(p, "/"); slash >= 0 {
		return p[slash+1:]
	}
	return p
}

// jenkinsUsesVar reports whether a script reads a variable as $NAME or ${NAME}.
//
// The match is bounded on both sides. Testing for the bare name alone reported $WORKSPACE for every
// script mentioning $WORKSPACE_TMP, and testing for "$NAME" alone did the same, so the character
// after the name must not be one that would continue it.
func jenkinsUsesVar(body, name string) bool {
	if strings.Contains(body, "${"+name+"}") {
		return true
	}
	for i := 0; ; {
		idx := strings.Index(body[i:], "$"+name)
		if idx < 0 {
			return false
		}
		end := i + idx + len(name) + 1
		if end >= len(body) || !jenkinsNameByte(body[end]) {
			return true
		}
		i = end
	}
}

// jenkinsNameByte reports whether a byte may continue a shell variable name.
func jenkinsNameByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// jenkinsStepLabel renders a build step's class as something readable for a warning.
func jenkinsStepLabel(class string) string {
	switch class {
	case "hudson.tasks.Ant":
		return "an Ant"
	case "hudson.tasks.Maven":
		return "a Maven"
	case "hudson.plugins.gradle.Gradle":
		return "a Gradle"
	case "hudson.plugins.copyartifact.CopyArtifact":
		return "a copy artifact"
	case "hudson.tasks.junit.JUnitResultArchiver":
		return "a JUnit"
	}
	// XStream writes a literal underscore in a class name as two, so one reads oddly in a message.
	return strconv.Quote(oneLine(strings.ReplaceAll(class, "__", "_")))
}

// jenkinsTargets renders an Ant or Maven step's goals for a warning, when the export named any.
func jenkinsTargets(targets string) string {
	if t := strings.TrimSpace(targets); t != "" {
		return " running " + strconv.Quote(oneLine(t))
	}
	return ""
}

// jenkinsSurvey maps a job's parameters to survey fields.
//
// A password parameter is refused rather than imported. Jenkins stores its value encrypted, while a
// survey answer is kept in plain text on the run and injected as an extra var, so importing one
// would quietly downgrade a secret. The parameter is dropped and named.
func (p *Plan) jenkinsSurvey(name string, proj jenkinsProject) []template.SurveyField {
	var fields []template.SurveyField
	for _, param := range proj.Properties.Params.Items {
		if param.Name == "" {
			p.warn("job %q has a parameter with no name, which was skipped", name)
			continue
		}
		field, ok := p.jenkinsField(name, param)
		if !ok {
			continue
		}
		fields = append(fields, field)
	}
	return fields
}

// jenkinsField maps one parameter to a survey field, refusing the kinds that must not be imported.
func (p *Plan) jenkinsField(name string, param jenkinsParam) (template.SurveyField, bool) {
	switch param.XMLName.Local {
	case "hudson.model.PasswordParameterDefinition",
		"com.michelin.cio.hudson.plugins.maskedpassword.MaskedPasswordParameterDefinition":
		p.warn("job %q parameter %q is a password parameter and was NOT imported. Store its value "+
			"as a credential instead: importing it as a survey field would keep the answer in "+
			"plain text on every run.", name, param.Name)
		return template.SurveyField{}, false
	case "hudson.model.FileParameterDefinition":
		p.warn("job %q parameter %q uploads a file at launch, which a survey cannot do, so it was "+
			"not imported", name, param.Name)
		return template.SurveyField{}, false
	case "hudson.model.RunParameterDefinition":
		p.warn("job %q parameter %q picks a previous build of another job, which has no "+
			"equivalent, so it was not imported", name, param.Name)
		return template.SurveyField{}, false
	}

	field := template.SurveyField{
		Var: param.Name, Label: param.Name, Type: template.FieldText,
		Help: strings.TrimSpace(param.Description),
	}
	// A parameter Jenkins left blank has no default rather than a default of the empty string, which
	// is the difference between a survey field that opens empty and one that offers "" as an answer.
	if param.DefaultValue != "" {
		field.Default = param.DefaultValue
	}
	switch param.XMLName.Local {
	case "hudson.model.BooleanParameterDefinition":
		field.Type = template.FieldBool
	case "hudson.model.ChoiceParameterDefinition":
		choices := param.NestedChoices
		if len(choices) == 0 {
			choices = param.FlatChoices
		}
		if len(choices) == 0 {
			p.warn("job %q parameter %q is a choice with no values, so it imports as free text",
				name, param.Name)
			break
		}
		field.Type = template.FieldChoice
		field.Choices = choices
		// A Jenkins choice has no separate default; the first entry is what it offers.
		field.Default = choices[0]
	case "hudson.model.TextParameterDefinition":
		field.Type = template.FieldMultiline
	case "hudson.model.StringParameterDefinition":
	default:
		p.warn("job %q parameter %q is a %s parameter, which imports as free text",
			name, param.Name, jenkinsStepLabel(param.XMLName.Local))
	}
	return field, true
}

// jenkinsTimeout reads a build timeout wrapper's cap and converts it to whole seconds.
func (p *Plan) jenkinsTimeout(name string, proj jenkinsProject) int {
	for _, item := range proj.BuildWrappers.Items {
		raw := strings.TrimSpace(item.TimeoutMinutes)
		if raw == "" {
			continue
		}
		minutes, err := strconv.Atoi(raw)
		if err != nil || minutes <= 0 {
			p.warn("job %q has a build timeout %q that could not be read, so it imports with no "+
				"timeout", name, oneLine(raw))
			return 0
		}
		return minutes * 60
	}
	return 0
}

// jenkinsSCMWarning reports a job's source control setup, which is not imported as a project.
//
// A freestyle job checks a repository out into its workspace and runs its steps there. A template's
// script runs wherever the worker puts it, so silently attaching a project would change where every
// path in the script resolves. The repository is named for the operator to attach deliberately.
func (p *Plan) jenkinsSCMWarning(name string, proj jenkinsProject) {
	if len(proj.SCM.URLs) == 0 {
		return
	}
	branch := ""
	if len(proj.SCM.Branches) > 0 {
		// Jenkins writes a branch as a remote-qualified spec such as */main.
		branch = strings.TrimPrefix(strings.TrimSpace(proj.SCM.Branches[0]), "*/")
	}
	at := ""
	if branch != "" {
		at = " at " + strconv.Quote(oneLine(branch))
	}
	p.warn("job %q checked out %s%s before running. Attach it as a project on the template if the "+
		"script needs those files; it was not attached for you, because that changes which "+
		"directory every relative path in the script resolves against.",
		name, oneLine(proj.SCM.URLs[0]), at)
}

// jenkinsCronAliases expands the shorthand Jenkins accepts in a timer spec. Jenkins spreads these
// across the period with H rather than firing every job at the same instant, and the expansions here
// are the ones Jenkins itself uses.
var jenkinsCronAliases = map[string]string{
	"@yearly":   "H H H H *",
	"@annually": "H H H H *",
	"@monthly":  "H H H * *",
	"@weekly":   "H H * * H",
	"@daily":    "H H * * *",
	"@midnight": "H H(0-2) * * *",
	"@hourly":   "H * * * *",
}

// jenkinsFieldBounds are the low and high values H may take in each cron field.
//
// The day of month stops at 28 rather than 31 deliberately, which is what Jenkins does: a monthly
// job hashed onto the 30th would skip February entirely.
var jenkinsFieldBounds = [5][2]int{{0, 59}, {0, 23}, {1, 28}, {1, 12}, {0, 6}}

// jenkinsSchedules converts a job's triggers into cron specifications.
//
// Only the timer trigger becomes a schedule. A poll trigger looks like a schedule but is not one: it
// asks the repository whether anything changed and builds only if something did. Importing it as a
// plain schedule would turn a job that usually does nothing into one that runs every few minutes
// unconditionally, so it is refused and reported.
func (p *Plan) jenkinsSchedules(name string, proj jenkinsProject) []string {
	var specs []string
	for _, item := range proj.Triggers.Items {
		switch item.XMLName.Local {
		case "hudson.triggers.TimerTrigger":
			specs = append(specs, p.jenkinsTimer(name, item.Spec)...)
		case "hudson.triggers.SCMTrigger":
			p.warn("job %q polled source control on %q and built only when the repository had "+
				"changed. That is not a schedule, so it was NOT imported: as one it would run the "+
				"job every interval whether anything changed or not. Trigger it from a webhook "+
				"instead.", name, oneLine(strings.Join(strings.Fields(item.Spec), " ")))
		case "hudson.triggers.ReverseBuildTrigger":
			p.warn("job %q ran after another job finished, which was left out. Chain them with a "+
				"pipeline once both templates exist.", name)
		default:
			p.warn("job %q has a %s trigger, which has no equivalent and was left out",
				name, jenkinsStepLabel(item.XMLName.Local))
		}
	}
	return specs
}

// jenkinsTimer converts one timer trigger's specification into cron expressions.
//
// A Jenkins timer spec holds one rule per line and allows blank lines and # comments, so a single
// trigger can describe several firing times and each becomes its own schedule.
func (p *Plan) jenkinsTimer(name, spec string) []string {
	var specs []string
	for _, line := range strings.Split(strings.ReplaceAll(spec, "\r\n", "\n"), "\n") {
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if converted, ok := p.jenkinsCron(name, line); ok {
			specs = append(specs, converted)
		}
	}
	return specs
}

// jenkinsCron converts one Jenkins cron line into a standard five field expression.
func (p *Plan) jenkinsCron(name, line string) (string, bool) {
	if expanded, ok := jenkinsCronAliases[strings.ToLower(line)]; ok {
		line = expanded
	} else if strings.HasPrefix(line, "@") {
		p.warn("job %q has the timer %q, which is not a schedule this can express, so it was not "+
			"imported", name, oneLine(line))
		return "", false
	}
	fields := strings.Fields(line)
	if len(fields) != 5 {
		p.warn("job %q has the timer %q, which is not a five field cron expression, so it was not "+
			"imported", name, oneLine(line))
		return "", false
	}
	hashed := false
	for i, field := range fields {
		converted, usedHash, ok := p.jenkinsCronField(name, line, field, i)
		if !ok {
			return "", false
		}
		hashed = hashed || usedHash
		fields[i] = converted
	}
	if hashed {
		// Jenkins picks its own value from a hash of the job's full name, and the hash here is not
		// the same function, so the job keeps its cadence and its window but may land on a different
		// minute inside that window than it did before.
		p.warn("job %q used Jenkins H notation in %q, which spreads load by hashing the job name. "+
			"It imported as %q, which keeps the same cadence but may fire at a different minute "+
			"within the window than Jenkins chose.",
			name, oneLine(line), strings.Join(fields, " "))
	}
	return strings.Join(fields, " "), true
}

// jenkinsCronField converts one field of a Jenkins cron line, resolving H notation and renumbering
// the weekday where Jenkins and standard cron disagree. It reports whether the field used H.
func (p *Plan) jenkinsCronField(name, line, field string, idx int) (string, bool, bool) {
	lo, hi := jenkinsFieldBounds[idx][0], jenkinsFieldBounds[idx][1]
	terms := strings.Split(field, ",")
	hashed := false
	for i, term := range terms {
		term = strings.TrimSpace(term)
		if !strings.HasPrefix(term, "H") {
			terms[i] = jenkinsWeekday(term, idx)
			continue
		}
		hashed = true
		converted, ok := p.jenkinsHashTerm(name, line, term, idx, lo, hi)
		if !ok {
			return "", false, false
		}
		terms[i] = jenkinsWeekday(converted, idx)
	}
	return strings.Join(terms, ","), hashed, true
}

// jenkinsHashTerm resolves one H term into concrete numbers within a field's bounds.
func (p *Plan) jenkinsHashTerm(name, line, term string, idx, lo, hi int) (string, bool) {
	rest := strings.TrimPrefix(term, "H")
	// H(a-b) narrows the window the hash may land in.
	if strings.HasPrefix(rest, "(") {
		end := strings.Index(rest, ")")
		if end < 0 {
			p.warn("job %q has the timer %q whose %q is missing a closing bracket, so it was not "+
				"imported", name, oneLine(line), oneLine(term))
			return "", false
		}
		a, b, ok := jenkinsRange(rest[1:end])
		if !ok || a > b || a < lo || b > hi {
			p.warn("job %q has the timer %q whose range %q is not valid for that field, so it was "+
				"not imported", name, oneLine(line), oneLine(term))
			return "", false
		}
		lo, hi = a, b
		rest = rest[end+1:]
	}
	// A bare H picks one value in the window.
	if rest == "" {
		return strconv.Itoa(lo + jenkinsHash(name, idx)%(hi-lo+1)), true
	}
	// H/n repeats every n, offset into the window so jobs do not stack on the same instant.
	if !strings.HasPrefix(rest, "/") {
		p.warn("job %q has the timer %q whose term %q could not be read, so it was not imported",
			name, oneLine(line), oneLine(term))
		return "", false
	}
	step, err := strconv.Atoi(strings.TrimPrefix(rest, "/"))
	if err != nil || step <= 0 {
		p.warn("job %q has the timer %q whose step in %q could not be read, so it was not imported",
			name, oneLine(line), oneLine(term))
		return "", false
	}
	if step > hi-lo {
		// A step wider than the window fires once, which a range with a step cannot express.
		return strconv.Itoa(lo + jenkinsHash(name, idx)%(hi-lo+1)), true
	}
	start := lo + jenkinsHash(name, idx)%step
	return fmt.Sprintf("%d-%d/%d", start, hi, step), true
}

// jenkinsRange parses the "a-b" inside an H window.
func jenkinsRange(s string) (int, int, bool) {
	lo, hi, found := strings.Cut(s, "-")
	if !found {
		return 0, 0, false
	}
	a, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return 0, 0, false
	}
	b, err := strconv.Atoi(strings.TrimSpace(hi))
	if err != nil {
		return 0, 0, false
	}
	return a, b, true
}

// jenkinsWeekday renumbers a weekday token where Jenkins and standard cron disagree.
//
// Jenkins accepts both 0 and 7 for Sunday. The parser here rejects 7 outright, so a weekly job
// written the second way would have been dropped as invalid rather than scheduled.
func jenkinsWeekday(term string, idx int) string {
	if idx != 4 {
		return term
	}
	var b strings.Builder
	for i := 0; i < len(term); i++ {
		c := term[i]
		// Only a 7 standing alone is Sunday; one inside 17 or 7-7 is a different number.
		if c == '7' && !jenkinsDigitAt(term, i-1) && !jenkinsDigitAt(term, i+1) {
			b.WriteByte('0')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// jenkinsDigitAt reports whether the byte at i is a digit, treating out of range as not one.
func jenkinsDigitAt(s string, i int) bool {
	return i >= 0 && i < len(s) && s[i] >= '0' && s[i] <= '9'
}

// jenkinsHash derives the value H stands for, from the job name and which field is being filled.
//
// Jenkins hashes the job's full name so that a given job always fires at the same moment while
// different jobs spread across the window. The same property holds here, which is what matters: a
// re-import produces the identical schedule rather than moving the job every time. The field index
// is mixed in so a spec whose fields are all H does not put the same number in each.
func jenkinsHash(name string, field int) int {
	var h int32
	for _, c := range name {
		h = 31*h + int32(c)
	}
	h += int32(field) * 7919
	if h < 0 {
		// Negating the smallest value overflows, so the fold adds one first.
		h = -(h + 1)
	}
	return int(h)
}
