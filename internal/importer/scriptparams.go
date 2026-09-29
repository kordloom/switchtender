package importer

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/util"
)

// shellVarName matches a name a shell variable can hold.
var shellVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// scriptParam is one parameter an imported script reads from its environment.
type scriptParam struct {
	// Var is the survey variable the answer is given under.
	Var string
	// Env is the variable the script reads, which the source tool set.
	Env string
	// Default is what the source tool set when nothing was given.
	Default string
}

// paramPreamble returns the lines that set each parameter under the name the script reads, from the
// entry its answer arrives in, or the job's default when nothing was given.
//
// Jenkins and Rundeck hand a job its parameters as environment variables, and a script reads them by
// name. They arrived here only inside the JSON of every answer, so an imported job ran with each one
// empty: a deploy of $VERSION deployed nothing in particular, and a cleanup under /data/$DIR reached
// /data. A parameter whose name no shell variable can hold is reported instead.
func (p *Plan) paramPreamble(job, source string, params []scriptParam) string {
	var b strings.Builder
	for _, param := range params {
		if !shellVarName.MatchString(param.Var) || !shellVarName.MatchString(param.Env) {
			p.warn("job %q parameter %q has a name no shell variable can hold, so the script does "+
				"not receive it by name. It is only in SWITCHTENDER_VARS.", job, param.Var)
			continue
		}
		entry := run.VarEnvPrefix + param.Var
		b.WriteString(param.Env + "=" + util.ShellQuote(param.Default) + "\n")
		b.WriteString(`if [ -n "${` + entry + `+set}" ]; then ` + param.Env + "=$" + entry + "; fi\n")
		b.WriteString("export " + param.Env + "\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "# " + source + " passed each parameter as an environment variable. Each is set here from " +
		"the answer\n# the run was given, or the job's default when none was.\n" + b.String()
}

// withPreamble inserts a preamble after a script's first line, which sets its error handling.
func withPreamble(script, preamble string) string {
	if preamble == "" {
		return script
	}
	first, rest, _ := strings.Cut(script, "\n")
	return first + "\n" + preamble + rest
}

// defaultText is a survey default as the text a parameter's environment variable carries.
func defaultText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	}
	return fmt.Sprint(v)
}

// surveyParams lists a survey's fields as parameters the script reads, each under the name env gives it.
func surveyParams(survey []template.SurveyField, env func(string) string) []scriptParam {
	params := make([]scriptParam, 0, len(survey))
	for _, f := range survey {
		params = append(params, scriptParam{Var: f.Var, Env: env(f.Var), Default: defaultText(f.Default)})
	}
	return params
}
