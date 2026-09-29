package importer

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
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
	// ScriptFile names a script on the node rather than inline content.
	ScriptFile string `yaml:"scriptfile" json:"scriptfile"`
	// ScriptURL names a script fetched from a URL.
	ScriptURL string `yaml:"scripturl" json:"scripturl"`
	// JobRef calls another job, which has no direct single-template equivalent.
	JobRef *rundeckJobRef `yaml:"jobref" json:"jobref"`
	// Type names a plugin step, which does not map.
	Type string `yaml:"type" json:"type"`
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
	ThreadCount int `yaml:"threadcount" json:"threadcount"`
	// KeepGoing continues across nodes after one fails.
	KeepGoing bool `yaml:"keepgoing" json:"keepgoing"`
}

// FromRundeck maps a Rundeck job export into a plan of equivalent objects.
//
// A Rundeck export is a list of job definitions in YAML or JSON. Each job becomes a Bash template
// carrying its steps, its options become a survey, and its schedule becomes a cron schedule. Rundeck
// dispatches by node filter rather than by inventory file and its projects are not git repositories,
// so neither an inventory nor a project is invented here; both are reported for the operator to
// attach, because guessing which hosts somebody's job targets is the one thing an importer must not
// do.
func FromRundeck(inventory string) func([]byte, time.Time) (*Plan, error) {
	return func(data []byte, now time.Time) (*Plan, error) {
		jobs, err := decodeRundeck(data)
		if err != nil {
			return nil, err
		}
		plan := &Plan{}
		if inventory == "" {
			plan.warn("no inventory was named, so every imported template launches without one. " +
				"Re-run with --inventory to say which hosts these jobs target, or set it on each " +
				"template afterward.")
		}
		for _, job := range jobs {
			plan.addRundeckJob(job, inventory, now)
		}
		if err := plan.requireObjects("jobs"); err != nil {
			return nil, err
		}
		return plan, nil
	}
}

// decodeRundeck reads a Rundeck export, which is a list of jobs. Some exports wrap the list in a
// mapping, so a bare list and a wrapped one are both accepted.
func decodeRundeck(data []byte) ([]rundeckJob, error) {
	var jobs []rundeckJob
	if err := yaml.Unmarshal(data, &jobs); err == nil {
		return jobs, nil
	}
	var wrapped struct {
		// Jobs is the job list when the export wraps it.
		Jobs []rundeckJob `yaml:"jobs" json:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wrapped); err != nil {
		return nil, fmt.Errorf("parse rundeck export: %w", err)
	}
	if wrapped.Jobs == nil {
		return nil, fmt.Errorf("parse rundeck export: no job list found")
	}
	return wrapped.Jobs, nil
}

// addRundeckJob maps one job into the plan as a template, plus a schedule when the job has one.
func (p *Plan) addRundeckJob(job rundeckJob, inventoryName string, now time.Time) {
	name := rundeckJobName(job)
	if name == "" {
		p.warn("a job without a name was skipped")
		return
	}
	if job.ExecutionEnabled != nil && !*job.ExecutionEnabled {
		p.warn("job %q is disabled in Rundeck; it is imported but you may want to leave it unused", name)
	}

	command, ok := p.rundeckCommand(job, name)
	if !ok {
		return
	}
	tmpl := &template.Template{
		ID: template.NewID(), Name: name, Tool: "bash", Command: command,
		Inventory: inventoryName, Survey: p.rundeckSurvey(job, name),
		Forks: job.NodeFilters.Dispatch.ThreadCount, Timeout: p.rundeckTimeout(job, name),
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
	if job.ScheduleEnabled != nil && !*job.ScheduleEnabled {
		p.warn("job %q has a schedule that is disabled in Rundeck, so it was not imported", name)
		return
	}
	spec, ok := p.rundeckCron(job, name)
	if !ok {
		return
	}
	p.addSchedule(&schedule.Schedule{
		ID: schedule.NewID(), Name: name, Cron: spec, TemplateID: tmpl.ID,
		Enabled: true, CreatedAt: now,
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
		switch {
		case cmd.Exec != "":
			p.writeRundeckStep(&b, cmd.Description, cmd.Exec)
			steps++
		case cmd.Script != "":
			p.writeRundeckStep(&b, cmd.Description, cmd.Script)
			steps++
		case cmd.ScriptFile != "":
			p.writeRundeckStep(&b, cmd.Description, shellQuote(cmd.ScriptFile))
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

// shellQuote wraps a path in single quotes so a script file name carrying a space or a shell
// metacharacter runs as one argument rather than being split or interpreted.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// rundeckSurvey maps a job's options to survey fields.
//
// A secure option is refused rather than imported. Rundeck stores such a value obscured, and a
// survey field here is plain text whose answer is kept on the run and injected as an extra var, so
// importing one would quietly turn a password prompt into a stored plaintext value. Downgrading a
// secret without saying so is worse than not importing it, so the option is dropped and named.
func (p *Plan) rundeckSurvey(job rundeckJob, name string) []template.SurveyField {
	var fields []template.SurveyField
	for _, opt := range job.Options {
		if opt.Name == "" {
			p.warn("job %q has an option with no name, which was skipped", name)
			continue
		}
		if opt.Secure {
			p.warn("job %q option %q is a secure option and was NOT imported. Store its value as a "+
				"credential instead: importing it as a survey field would keep the answer in plain "+
				"text on every run.", name, opt.Name)
			continue
		}
		field := template.SurveyField{
			Var: opt.Name, Label: opt.Name, Type: template.FieldText,
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
	flush := func() bool {
		if token.Len() == 0 {
			return true
		}
		t := token.String()
		token.Reset()
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
	}
	if !flush() {
		return "", false
	}
	return out.String(), true
}
