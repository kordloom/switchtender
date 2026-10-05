package importer

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/util"
)

// rundeckJob is one job definition from a Rundeck job export. Rundeck writes YAML or JSON, and JSON
// is valid YAML, so one decoder reads both.
type rundeckJob struct {
	// Name is the job name.
	Name string `yaml:"name" json:"name"`
	// Group is the job's folder path within its project, used to qualify the imported name.
	Group string `yaml:"group" json:"group"`
	// Project is the Rundeck project the job belongs to.
	Project string `yaml:"project" json:"project"`
	// Description is the job description.
	Description string `yaml:"description" json:"description"`
	// Timeout caps a job execution, written as a duration such as 1h or 30m, or plain seconds.
	Timeout string `yaml:"timeout" json:"timeout"`
	// ScheduleEnabled reports whether the job's schedule is active. Rundeck omits it when true.
	ScheduleEnabled *bool `yaml:"scheduleEnabled" json:"scheduleEnabled"`
	// ExecutionEnabled reports whether the job may run at all. Rundeck omits it when true.
	ExecutionEnabled *bool `yaml:"executionEnabled" json:"executionEnabled"`
	// Options are the job's prompted inputs, which map to survey fields.
	Options []rundeckOption `yaml:"options" json:"options"`
	// Sequence holds the ordered steps the job runs.
	Sequence rundeckSequence `yaml:"sequence" json:"sequence"`
	// Schedule is the job's cadence, either a Quartz crontab or a structured form.
	Schedule *rundeckSchedule `yaml:"schedule" json:"schedule"`
	// NodeFilters selects which nodes the job dispatches to.
	NodeFilters rundeckNodeFilters `yaml:"nodefilters" json:"nodefilters"`
	// Retry is how many times Rundeck retries the job after a failure, a count or a count with a delay.
	Retry any `yaml:"retry" json:"retry"`
	// Notification is where Rundeck reports the job's outcome. Only its presence is read.
	Notification any `yaml:"notification" json:"notification"`
}

// rundeckOption is one prompted job input.
type rundeckOption struct {
	// Name is the option name, which becomes the survey field's variable.
	Name string `yaml:"name" json:"name"`
	// Description is shown to the person launching the job.
	Description string `yaml:"description" json:"description"`
	// Value is the default value.
	Value string `yaml:"value" json:"value"`
	// Values is the allowed set, which makes the option a choice.
	Values []string `yaml:"values" json:"values"`
	// Required rejects a launch that leaves the option empty.
	Required bool `yaml:"required" json:"required"`
	// Enforced restricts the answer to Values rather than merely suggesting them.
	Enforced bool `yaml:"enforced" json:"enforced"`
	// Secure marks the option as a password, whose value Rundeck stores obscured.
	Secure bool `yaml:"secure" json:"secure"`
	// Multivalued lets the option carry several values at once.
	Multivalued bool `yaml:"multivalued" json:"multivalued"`
	// Regex is a regular expression the whole value must match.
	Regex string `yaml:"regex" json:"regex"`
}

// rundeckSequence is a job's ordered step list.
type rundeckSequence struct {
	// KeepGoing continues the sequence after a step fails.
	KeepGoing bool `yaml:"keepgoing" json:"keepgoing"`
	// Commands are the steps in order.
	Commands []rundeckCommand `yaml:"commands" json:"commands"`
}

// rundeckCommand is one step of a job sequence.
type rundeckCommand struct {
	// Description labels the step.
	Description string `yaml:"description" json:"description"`
	// Exec is a single shell command to run.
	Exec string `yaml:"exec" json:"exec"`
	// Script is an inline script body.
	Script string `yaml:"script" json:"script"`
	// ScriptInterpreter names the program an inline script is fed to. Empty means a shell, which is
	// what a Bash template runs.
	ScriptInterpreter string `yaml:"scriptinterpreter" json:"scriptinterpreter"`
	// ScriptFile names a script on the node rather than inline content.
	ScriptFile string `yaml:"scriptfile" json:"scriptfile"`
	// ScriptURL names a script fetched from a URL.
	ScriptURL string `yaml:"scripturl" json:"scripturl"`
	// JobRef calls another job, which has no direct single-template equivalent.
	JobRef *rundeckJobRef `yaml:"jobref" json:"jobref"`
	// Type names a plugin step, which does not map.
	Type string `yaml:"type" json:"type"`
	// Args are the arguments a script step passes to its script.
	Args string `yaml:"args" json:"args"`
	// ErrorHandler is the step Rundeck runs when this one fails. Only its presence is read.
	ErrorHandler any `yaml:"errorhandler" json:"errorhandler"`
}

// rundeckJobRef is a reference from one job to another.
type rundeckJobRef struct {
	// Name is the referenced job's name.
	Name string `yaml:"name" json:"name"`
	// Group is the referenced job's folder path.
	Group string `yaml:"group" json:"group"`
}

// rundeckSchedule is a job's cadence, given either as a Quartz crontab or field by field.
type rundeckSchedule struct {
	// Crontab is a Quartz expression of six or seven fields, when the job uses one.
	Crontab string `yaml:"crontab" json:"crontab"`
	// Time holds the hour, minute, and seconds of a structured schedule.
	Time rundeckTime `yaml:"time" json:"time"`
	// Month is the month field of a structured schedule.
	Month string `yaml:"month" json:"month"`
	// Year is the year field, which a standard cron expression cannot represent.
	Year string `yaml:"year" json:"year"`
	// DayOfMonth holds the day field of a structured schedule.
	DayOfMonth rundeckDay `yaml:"dayofmonth" json:"dayofmonth"`
	// WeekDay holds the weekday field of a structured schedule.
	WeekDay rundeckDay `yaml:"weekday" json:"weekday"`
}

// rundeckTime is the clock portion of a structured schedule.
type rundeckTime struct {
	// Hour is the hour field.
	Hour string `yaml:"hour" json:"hour"`
	// Minute is the minute field.
	Minute string `yaml:"minute" json:"minute"`
	// Seconds is the seconds field, which a standard cron expression cannot represent.
	Seconds string `yaml:"seconds" json:"seconds"`
}

// rundeckDay is a day field of a structured schedule.
type rundeckDay struct {
	// Day is the day expression.
	Day string `yaml:"day" json:"day"`
}

// rundeckNodeFilters selects and paces the nodes a job dispatches to.
type rundeckNodeFilters struct {
	// Filter is the node filter expression, which selects hosts by attribute rather than by name.
	Filter string `yaml:"filter" json:"filter"`
	// Dispatch paces the fan out across nodes.
	Dispatch rundeckDispatch `yaml:"dispatch" json:"dispatch"`
}

// rundeckDispatch paces a job's fan out.
type rundeckDispatch struct {
	// ThreadCount is how many nodes run at once, the equivalent of Ansible forks.
	ThreadCount rundeckInt `yaml:"threadcount" json:"threadcount"`
	// KeepGoing continues across nodes after one fails.
	KeepGoing bool `yaml:"keepgoing" json:"keepgoing"`
}

// rundeckInt is a whole number Rundeck may write either as a number or as a quoted string.
//
// Rundeck's own published job exports quote threadcount. Decoding straight into an int failed the
// whole document on that one field, and because the bare-list attempt was discarded the operator was
// told the top level was the wrong shape. The file was fine; one scalar was quoted.
type rundeckInt int

// UnmarshalYAML decodes a number or a quoted number, and treats an empty value as zero.
func (n *rundeckInt) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		*n = 0
		return nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("%q is not a whole number", raw)
	}
	*n = rundeckInt(parsed)
	return nil
}

// FromRundeck maps a Rundeck export into a plan of equivalent objects.
//
// Two artifacts are accepted and told apart by their content, since the one an operator reaches for
// depends on what they were leaving with. A job export is a list of job definitions in YAML or JSON.
// A project archive is the zip the web interface hands back for a whole project, which carries the
// same jobs as XML plus the project's own configuration.
//
// Each job becomes a Bash template carrying its steps, its options become a survey, and its schedule
// becomes a cron schedule. Rundeck dispatches by node filter rather than by inventory file, so no
// inventory is invented here from either artifact: guessing which hosts somebody's job targets is
// the one thing an importer must not do, and an archive carries no node definitions to read anyway.
// A project comes across only from an archive whose SCM configuration names a repository this can
// reach.
func FromRundeck(inventory string) func([]byte, time.Time) (*Plan, error) {
	return func(data []byte, now time.Time) (*Plan, error) {
		data, err := textOf(data)
		if err != nil {
			return nil, err
		}
		if IsRundeckArchive(data) {
			return fromRundeckArchive(data, inventory, now)
		}
		// yaml.Unmarshal decodes the first document of a stream and ignores the rest, so a file of
		// per-project exports separated by --- imported one project and reported that as the estate.
		if err := refuseYAMLTail(data); err != nil {
			return nil, err
		}
		jobs, skipped, err := decodeRundeck(data)
		if err != nil {
			return nil, err
		}
		plan := &Plan{}
		for _, s := range skipped {
			plan.warn("%s", s)
			plan.refused++
		}
		plan.warnRundeckInventory(inventory)
		for _, job := range jobs {
			plan.addRundeckJob(job, inventory, now)
		}
		if err := plan.requireObjects("jobs"); err != nil {
			return nil, err
		}
		return plan, nil
	}
}

// decodeRundeck reads the job list from either export shape, a bare list or one wrapped under jobs.
// Each job is decoded on its own, so a job with a field that will not read is left out with a
// sentence saying which and why, and the other jobs import. One bad threadcount used to refuse the
// whole export.
func decodeRundeck(data []byte) ([]rundeckJob, []string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("parse rundeck export: %w", err)
	}
	list := &root
	if list.Kind == yaml.DocumentNode && len(list.Content) > 0 {
		list = list.Content[0]
	}
	if list.Kind == yaml.MappingNode {
		var jobs *yaml.Node
		for i := 0; i+1 < len(list.Content); i += 2 {
			if list.Content[i].Value == "jobs" {
				jobs = list.Content[i+1]
			}
		}
		if jobs == nil {
			return nil, nil, fmt.Errorf("parse rundeck export: no job list found")
		}
		list = jobs
	}
	if list.Kind != yaml.SequenceNode {
		return nil, nil, fmt.Errorf("parse rundeck export: the job list is not a list")
	}
	var jobs []rundeckJob
	var skipped []string
	for i, item := range list.Content {
		var job rundeckJob
		if err := item.Decode(&job); err != nil {
			skipped = append(skipped, fmt.Sprintf("job %s was skipped because %s, and the rest of "+
				"the export imports without it", yamlJobLabel(item, i), yamlProblem(err)))
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, skipped, nil
}

// yamlJobLabel names a job entry for a warning: its name when it has one, its position otherwise.
func yamlJobLabel(item *yaml.Node, idx int) string {
	if item.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(item.Content); i += 2 {
			if item.Content[i].Value == "name" && strings.TrimSpace(item.Content[i+1].Value) != "" {
				return strconv.Quote(item.Content[i+1].Value)
			}
		}
	}
	return "#" + strconv.Itoa(idx+1)
}

// yamlProblem says why a job did not decode, without the decoder's own framing.
func yamlProblem(err error) string {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) && len(typeErr.Errors) > 0 {
		return oneLine(strings.Join(typeErr.Errors, "; "))
	}
	return oneLine(strings.TrimPrefix(err.Error(), "yaml: "))
}

// addRundeckJob maps one job into the plan as a template, plus a schedule when the job has one.
func (p *Plan) addRundeckJob(job rundeckJob, inventoryName string, now time.Time) {
	name := rundeckJobName(job)
	if name == "" {
		p.warn("a job without a name was skipped")
		return
	}
	// A job disabled for execution never runs in Rundeck, whatever its schedule says, so its schedule
	// arrives switched off too. It used to arrive armed, which turned a job the estate had parked
	// into one that fires on its old cadence the night after the import.
	if retries := rundeckRetries(job.Retry); retries != "" {
		p.warn("job %q retries up to %s times after a failure in Rundeck, which was left out, so a "+
			"failed run here is not retried", name, oneLine(retries))
	}
	if job.Notification != nil {
		p.warn("job %q sends notifications in Rundeck, which were left out. Set the template's "+
			"notifications to match", name)
	}
	disabled := job.ExecutionEnabled != nil && !*job.ExecutionEnabled
	if disabled {
		p.warn("job %q is disabled in Rundeck; it is imported with any schedule it has switched "+
			"off, and you may want to leave it unused", name)
	}

	command, ok := p.rundeckCommand(job, name)
	if !ok {
		return
	}
	// Rundeck sets each option as RD_OPTION_ and its name in capitals.
	survey := p.rundeckSurvey(job, name)
	command = withPreamble(command, p.paramPreamble(name, "Rundeck", surveyParams(survey, rundeckOptionEnv)))
	tmpl := &template.Template{
		ID: template.NewID(), Name: name, Tool: "bash", Command: command,
		Inventory: inventoryName, Survey: survey,
		Forks: int(job.NodeFilters.Dispatch.ThreadCount), Timeout: p.rundeckTimeout(job, name),
		CreatedAt: now,
	}
	if job.NodeFilters.Filter != "" {
		p.warn("job %q dispatches by the node filter %q. SwitchTender targets an inventory, so "+
			"check that the inventory you attached covers the same hosts.",
			name, oneLine(job.NodeFilters.Filter))
	}
	p.Templates = append(p.Templates, tmpl)

	if job.Schedule == nil {
		return
	}
	// A schedule disabled in Rundeck comes across disabled rather than being left behind. Dropping it
	// was safe and lossy: the cadence somebody had written and parked was gone, so re-enabling it later
	// meant writing it again from memory, and the four formats disagreed about the same situation while
	// AWX, Jenkins and Semaphore all carry theirs switched off. addSchedule says which ones arrive off.
	enabled := !disabled && (job.ScheduleEnabled == nil || *job.ScheduleEnabled)
	// The Quartz forms cron has no reading for, the third Friday or the last day of the month, were
	// refused outright. A recurrence says each of them exactly, so they come across as one rather
	// than being left for somebody to rebuild by hand. Anything else keeps the cron conversion.
	if fields, quartz := rundeckQuartzFields(job.Schedule); quartz && quartzNeedsRecurrence(fields) {
		if rule, ok := quartzToRRule(fields, now); ok {
			p.addSchedule(&schedule.Schedule{
				ID: schedule.NewID(), Name: name, RRule: rule, TemplateID: tmpl.ID,
				Enabled: enabled, CreatedAt: now,
			}, "rundeck", now)
			return
		}
	}
	spec, ok := p.rundeckCron(job, name)
	if !ok {
		return
	}
	p.addSchedule(&schedule.Schedule{
		ID: schedule.NewID(), Name: name, Cron: spec, TemplateID: tmpl.ID,
		Enabled: enabled, CreatedAt: now,
	}, "rundeck", now)
}

// rundeckJobName qualifies a job's name with its group, so two jobs of the same name in different
// folders do not collide.
func rundeckJobName(job rundeckJob) string {
	name := strings.TrimSpace(job.Name)
	group := strings.Trim(strings.TrimSpace(job.Group), "/")
	if name == "" || group == "" {
		return name
	}
	return group + "/" + name
}

// rundeckCommand renders a job's step sequence as one Bash script, reporting whether anything
// runnable was found.
//
// Rundeck runs steps in order and stops at the first failure unless the sequence sets keepgoing, so
// the script opens with set -e to match, and omits it when the job asked to keep going. A step that
// has no script equivalent, a job reference or a plugin, is reported and left out rather than
// guessed at, and a job made only of those is skipped entirely rather than imported as an empty
// template that would report success without doing anything.
func (p *Plan) rundeckCommand(job rundeckJob, name string) (string, bool) {
	var b strings.Builder
	if job.Sequence.KeepGoing {
		b.WriteString("# Rundeck kept going past a failed step, so this script does not set -e.\n")
	} else {
		b.WriteString("set -e\n")
	}
	steps := 0
	for i, cmd := range job.Sequence.Commands {
		if args := strings.TrimSpace(cmd.Args); args != "" {
			p.warn("job %q step %d passes %q to its script, which was left out, so the script runs "+
				"with no arguments", name, i+1, oneLine(args))
		}
		if cmd.ErrorHandler != nil {
			p.warn("job %q step %d has an error handler, which was left out. A failure in that step "+
				"now ends the job, or is passed over when the job keeps going", name, i+1)
		}
		switch {
		case cmd.Exec != "":
			p.writeRundeckStep(&b, cmd.Description, p.rundeckTokens(name, i+1, cmd.Exec))
			steps++
		case cmd.Script != "":
			if !rundeckShellScript(cmd.ScriptInterpreter) {
				p.warn("job %q step %d feeds its script to %q rather than to a shell, and a "+
					"template runs one Bash script, so the step was left out. Rewrite it as a "+
					"step that calls that interpreter itself.",
					name, i+1, oneLine(cmd.ScriptInterpreter))
				continue
			}
			// With no interpreter named, Rundeck runs the script as a file, so its first line decides
			// what reads it. A Python script inlined into Bash failed on its first import line.
			if cmd.ScriptInterpreter == "" {
				if shebang := rundeckShebang(cmd.Script); shebang != "" && !rundeckShellScript(shebang) {
					p.warn("job %q step %d is a script whose first line runs it with %q rather than a "+
						"shell, and a template runs one Bash script, so the step was left out. Rewrite "+
						"it as a step that calls that interpreter itself.", name, i+1, oneLine(shebang))
					continue
				}
			}
			p.writeRundeckStep(&b, cmd.Description, p.rundeckTokens(name, i+1, cmd.Script))
			steps++
		case cmd.ScriptFile != "":
			p.writeRundeckStep(&b, cmd.Description, util.ShellQuote(cmd.ScriptFile))
			p.warn("job %q step %d runs the script file %q, which must already exist on the target",
				name, i+1, oneLine(cmd.ScriptFile))
			steps++
		case cmd.JobRef != nil:
			p.warn("job %q step %d calls another job, %q, which was left out. Chain them with a "+
				"pipeline once both templates exist.", name, i+1, oneLine(cmd.JobRef.Name))
		case cmd.ScriptURL != "":
			p.warn("job %q step %d fetches a script from a URL, which was left out. Fetching and "+
				"running remote code is not imported for you.", name, i+1)
		default:
			p.warn("job %q step %d is a plugin step%s, which has no equivalent and was left out",
				name, i+1, rundeckStepType(cmd.Type))
		}
	}
	if steps == 0 {
		p.warn("job %q was skipped: none of its steps could be imported, so the template would "+
			"have reported success without running anything", name)
		return "", false
	}
	return b.String(), true
}

// rundeckShellScript reports whether an inline script's interpreter is a plain shell, which is what
// lets the step be inlined into the one Bash script a template runs.
//
// Rundeck writes the script body to a file and feeds it to whatever the step names, so a step naming
// python3 holds Python source and a step naming "sudo -u deploy /bin/bash" runs as somebody else.
// Inlining either into a Bash script keeps the job's name on something that does not do what the job
// did, so only a bare shell passes and every other interpreter is reported and left out.
func rundeckShellScript(interpreter string) bool {
	fields := strings.Fields(interpreter)
	if len(fields) == 0 {
		return true
	}
	if len(fields) > 1 {
		return false
	}
	switch path.Base(fields[0]) {
	case "sh", "bash", "dash", "ksh", "zsh", "ash":
		return true
	}
	return false
}

// rundeckShebang returns the program an inline script's first line names, or the empty string when it
// has no #! line. The program env stands in for, as in "#!/usr/bin/env python3", is what is returned.
func rundeckShebang(script string) string {
	line, _, _ := strings.Cut(strings.TrimLeft(script, " \t\r\n"), "\n")
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "#!")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	if path.Base(fields[0]) != "env" {
		return fields[0]
	}
	for _, f := range fields[1:] {
		if !strings.HasPrefix(f, "-") {
			return f
		}
	}
	return ""
}

// rundeckRetries returns how many times a job retries after a failure, or the empty string when it
// does not. Rundeck writes a count, or a count with a delay.
func rundeckRetries(v any) string {
	switch r := v.(type) {
	case nil:
		return ""
	case map[string]any:
		return rundeckRetries(r["retry"])
	default:
		s := strings.TrimSpace(fmt.Sprint(r))
		if s == "" || s == "0" {
			return ""
		}
		return s
	}
}

// writeRundeckStep appends one step's body to the script, preceded by its description as a comment.
func (p *Plan) writeRundeckStep(b *strings.Builder, description, body string) {
	if d := strings.TrimSpace(description); d != "" {
		fmt.Fprintf(b, "\n# %s\n", oneLine(d))
	} else {
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n")
}

// rundeckStepType renders a plugin step's type for a warning, when the export named one.
func rundeckStepType(t string) string {
	if t == "" {
		return ""
	}
	return " of type " + strconv.Quote(oneLine(t))
}

// rundeckSurvey maps a job's options to survey fields.
//
// A secure option imports as a secret field. Rundeck stores such a value obscured, and a secret
// field's answer is sealed with the credential key and never kept on the run in plain text.
func (p *Plan) rundeckSurvey(job rundeckJob, name string) []template.SurveyField {
	var fields []template.SurveyField
	taken := map[string]string{}
	for _, opt := range job.Options {
		if opt.Name == "" {
			p.warn("job %q has an option with no name, which was skipped", name)
			continue
		}
		// The answer reaches the script under a variable name, so the survey variable is the
		// option's name with the characters a variable cannot hold made underscores, the same
		// folding Rundeck applies to RD_OPTION_. A dash in a name used to keep the answer from
		// reaching the script at all.
		variable := nonEnvChar.ReplaceAllString(opt.Name, "_")
		if first, dup := taken[variable]; dup {
			p.warn("job %q options %q and %q fold to the same variable, %s, so the second was "+
				"skipped", name, first, opt.Name, variable)
			continue
		}
		taken[variable] = opt.Name
		// A secure option imports as a secret field, whose answer is sealed rather than kept on the
		// run in plain text. It keeps no default, choices, or pattern: Rundeck reads a secure
		// default from its key storage, which an export does not carry.
		if opt.Secure {
			p.secretSurveyDefault(fmt.Sprintf("job %q", name), opt.Name, opt.Value)
			fields = append(fields, template.SurveyField{
				Var: variable, Label: opt.Name, Type: template.FieldSecret,
				Required: opt.Required, Help: opt.Description,
			})
			continue
		}
		field := template.SurveyField{
			Var: variable, Label: opt.Name, Type: template.FieldText,
			Required: opt.Required, Help: opt.Description,
		}
		if len(opt.Values) > 0 && opt.Enforced {
			field.Type = template.FieldChoice
			field.Choices = opt.Values
		} else if len(opt.Values) > 0 {
			p.warn("job %q option %q suggests values without enforcing them, so it imports as free "+
				"text with the first as its default", name, opt.Name)
		}
		if opt.Value != "" {
			field.Default = opt.Value
		}
		if opt.Multivalued {
			p.warn("job %q option %q accepted several values at once, which imports as a single "+
				"text answer", name, opt.Name)
		}
		// Rundeck checks the whole value against the option's regex, which is what a survey pattern
		// does here. The check was dropped, so a value Rundeck refused was accepted. A pattern this
		// engine cannot compile is reported rather than carried broken.
		if re := strings.TrimSpace(opt.Regex); re != "" && field.Type == template.FieldText {
			if _, err := regexp.Compile(re); err != nil {
				p.warn("job %q option %q must match %q in Rundeck, which was left out because it "+
					"does not compile here: %v. Set a pattern on the survey field by hand",
					name, opt.Name, oneLine(re), err)
			} else {
				field.Pattern = re
			}
		}
		fields = append(fields, field)
	}
	return fields
}

// rundeckTimeout converts a job timeout to whole seconds, reporting one it cannot read.
func (p *Plan) rundeckTimeout(job rundeckJob, name string) int {
	raw := strings.TrimSpace(job.Timeout)
	if raw == "" {
		return 0
	}
	// A bare number is seconds in Rundeck.
	if n, err := strconv.Atoi(raw); err == nil {
		if n < 0 {
			p.warn("job %q has a negative timeout %q, which was ignored", name, raw)
			return 0
		}
		return n
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		p.warn("job %q has a timeout %q that could not be read, so it imports with no timeout",
			name, oneLine(raw))
		return 0
	}
	return int(d.Seconds())
}

// rundeckCron converts a job schedule to a standard five field cron expression.
func (p *Plan) rundeckCron(job rundeckJob, name string) (string, bool) {
	sc := job.Schedule
	if spec := strings.TrimSpace(sc.Crontab); spec != "" {
		return p.quartzToCron(spec, name)
	}
	minute := rundeckField(sc.Time.Minute)
	hour := rundeckField(sc.Time.Hour)
	dom := rundeckField(sc.DayOfMonth.Day)
	month := rundeckField(sc.Month)
	dow := rundeckField(sc.WeekDay.Day)
	if seconds := strings.TrimSpace(sc.Time.Seconds); seconds != "" && seconds != "0" {
		p.warn("job %q fires at second %q, which a cron schedule cannot express, so it imports at "+
			"the top of the minute", name, oneLine(seconds))
	}
	if year := strings.TrimSpace(sc.Year); year != "" && year != "*" {
		p.warn("job %q is limited to the year %q, which a cron schedule cannot express, so it "+
			"imports without that limit", name, oneLine(year))
	}
	converted, ok := p.convertQuartzDOW(dow, name)
	if !ok {
		return "", false
	}
	return strings.Join([]string{minute, hour, dom, month, converted}, " "), true
}

// quartzToCron converts a Quartz expression of six or seven fields to the standard five.
//
// Quartz leads with a seconds field and may end with a year, neither of which standard cron has, and
// it numbers weekdays from one for Sunday where cron numbers from zero. Dropping the extra fields
// without renumbering the weekday would shift every weekly job by a day, so the weekday is converted
// rather than copied.
func (p *Plan) quartzToCron(spec, name string) (string, bool) {
	fields := strings.Fields(spec)
	if len(fields) == 5 {
		return spec, true
	}
	if len(fields) != 6 && len(fields) != 7 {
		p.warn("job %q has the schedule %q, which is neither a five field cron expression nor a "+
			"six or seven field Quartz one, so it was not imported", name, oneLine(spec))
		return "", false
	}
	if seconds := fields[0]; seconds != "0" {
		p.warn("job %q fires at second %q, which a cron schedule cannot express, so it imports at "+
			"the top of the minute", name, oneLine(seconds))
	}
	if len(fields) == 7 && fields[6] != "*" {
		p.warn("job %q is limited to the year %q, which a cron schedule cannot express, so it "+
			"imports without that limit", name, oneLine(fields[6]))
	}
	dow, ok := p.convertQuartzDOW(rundeckField(fields[5]), name)
	if !ok {
		return "", false
	}
	return strings.Join([]string{
		rundeckField(fields[1]), rundeckField(fields[2]),
		rundeckField(fields[3]), rundeckField(fields[4]), dow,
	}, " "), true
}

// rundeckField normalizes one schedule field. Quartz writes '?' where a day field is unset, which
// standard cron spells '*'; an empty field is also '*'.
func rundeckField(f string) string {
	f = strings.TrimSpace(f)
	if f == "" || f == "?" {
		return "*"
	}
	return f
}

// convertQuartzDOW renumbers a Quartz weekday field for standard cron.
//
// Quartz numbers Sunday as one through Saturday as seven; cron numbers Sunday as zero through
// Saturday as six. Every numeric token is shifted down by one, including inside ranges, lists, and
// steps. Day names such as SUN and MON are the same in both and pass through. A Quartz-only form
// that cron has no reading for, the nth-weekday '#' or 'L' for last, is reported rather than
// converted to something that would fire on the wrong day.
//
// Only '#' and 'L' are refused, not 'W'. Quartz's nearest-weekday 'W' belongs to the day-of-month
// field and never appears here, while WED is a weekday name that does carry one, so rejecting the
// letter outright would refuse every Wednesday schedule.
func (p *Plan) convertQuartzDOW(field, name string) (string, bool) {
	if field == "*" {
		return field, true
	}
	if strings.ContainsAny(field, "#Ll") {
		p.warn("job %q uses the Quartz weekday expression %q, which has no cron equivalent, so the "+
			"schedule was not imported", name, oneLine(field))
		return "", false
	}
	var out strings.Builder
	token := strings.Builder{}
	// The number after a slash is how many days to skip, not which day, so it is the one numeric
	// token in the field that must not be renumbered. Shifting it turned "every second day starting
	// Sunday" into "from Sunday, every day", and an alternate-day job imported as a daily one with
	// nothing to show it had changed. The Jenkins dialect already splits on the slash for the same
	// reason.
	isStep := false
	flush := func() bool {
		if token.Len() == 0 {
			return true
		}
		t := token.String()
		token.Reset()
		if isStep {
			if n, err := strconv.Atoi(t); err != nil || n < 1 {
				p.warn("job %q has the weekday step %q, which is not a positive interval, so the "+
					"schedule was not imported", name, oneLine(field))
				return false
			}
			out.WriteString(t)
			return true
		}
		n, err := strconv.Atoi(t)
		if err != nil {
			// A day name, which both spellings share.
			out.WriteString(t)
			return true
		}
		if n < 1 || n > 7 {
			p.warn("job %q has the weekday %q, which is outside the Quartz range of 1 to 7, so the "+
				"schedule was not imported", name, oneLine(field))
			return false
		}
		out.WriteString(strconv.Itoa(n - 1))
		return true
	}
	for _, r := range field {
		if r >= '0' && r <= '9' {
			token.WriteRune(r)
			continue
		}
		if !flush() {
			return "", false
		}
		out.WriteRune(r)
		switch r {
		case '/':
			isStep = true
		case ',':
			// A comma starts a fresh term, so the next number is a weekday again.
			isStep = false
		}
	}
	if !flush() {
		return "", false
	}
	return out.String(), true
}

// rundeckOptionToken matches a reference to an option's value that Rundeck substitutes into a step's
// text before it runs, in either of the two spellings it accepts.
var rundeckOptionToken = regexp.MustCompile(`@option\.([A-Za-z0-9_.\-]+)@|\$\{option\.([A-Za-z0-9_.\-]+)\}`)

// rundeckContextToken matches a reference to the rest of Rundeck's data context, which has no
// equivalent here.
var rundeckContextToken = regexp.MustCompile(`@(node|job|globals|execution)\.[A-Za-z0-9_.\-]+@|\$\{(node|job|globals|execution)\.[A-Za-z0-9_.\-]+\}`)

// rundeckTokens rewrites a step's references to option values as the variables the preamble sets.
// Rundeck replaces @option.name@ and ${option.name} in the text itself, so left alone the first ran as
// a literal word and the second stopped Bash with a bad substitution. A reference to the rest of the
// data context, a node's name or the job's, is reported, since nothing here fills it.
func (p *Plan) rundeckTokens(job string, step int, text string) string {
	text = rundeckOptionToken.ReplaceAllStringFunc(text, func(m string) string {
		parts := rundeckOptionToken.FindStringSubmatch(m)
		name := parts[1]
		if name == "" {
			name = parts[2]
		}
		return "${" + rundeckOptionEnv(name) + "}"
	})
	if ref := rundeckContextToken.FindString(text); ref != "" {
		p.warn("job %q step %d refers to Rundeck's own context, %s, which nothing here fills, so "+
			"it expands to nothing or stops the script", job, step, oneLine(ref))
	}
	return text
}

// rundeckOptionEnv is the environment variable Rundeck sets for an option.
func rundeckOptionEnv(name string) string {
	return "RD_OPTION_" + strings.ToUpper(nonEnvChar.ReplaceAllString(name, "_"))
}

// nonEnvChar matches a character an environment variable name cannot hold.
var nonEnvChar = regexp.MustCompile(`[^A-Za-z0-9_]`)
