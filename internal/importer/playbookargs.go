package importer

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/template"
)

// verbosityArg matches a run of -v flags, which ansible-playbook counts.
var verbosityArg = regexp.MustCompile(`^-v+$`)

// valueShortFlags are the short flags that take a value, which ansible-playbook also accepts joined
// to the flag, as in -lweb.
var valueShortFlags = map[string]bool{"-l": true, "-t": true, "-e": true, "-f": true}

// carryPlaybookArgs moves the ansible-playbook arguments a template passes on every run onto the
// template fields that mean the same thing, and returns the arguments it could not carry, in order.
//
// These are not decoration. A limit narrows a deploy to the hosts it was written for, tags and skip
// tags choose which tasks run at all, and check makes the run a rehearsal. Dropping any of them
// widens what the imported template does, which is how a deploy meant for one tier ran on all of
// them after a migration.
func carryPlaybookArgs(t *template.Template, args []string) []string {
	var left []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		flag, value, joined := splitPlaybookArg(arg)
		// take returns the flag's value, joined to it or the next argument, and the arguments it
		// consumed, so an argument that cannot be carried is reported whole.
		take := func() (string, []string, bool) {
			if joined {
				return value, []string{arg}, true
			}
			if i+1 < len(args) {
				i++
				return args[i], []string{arg, args[i]}, true
			}
			return "", []string{arg}, false
		}
		switch {
		case flag == "--limit" || flag == "-l":
			v, used, ok := take()
			// A second limit is reported rather than guessed at: which one wins is the kind of
			// detail a deploy's scope should not hang on.
			if !ok || strings.TrimSpace(v) == "" || t.Limit != "" {
				left = append(left, used...)
				continue
			}
			t.Limit = v
		case flag == "--tags" || flag == "-t":
			v, used, ok := take()
			if !ok || len(splitAWXTags(v)) == 0 {
				left = append(left, used...)
				continue
			}
			t.Tags = append(t.Tags, splitAWXTags(v)...)
		case flag == "--skip-tags":
			v, used, ok := take()
			if !ok || len(splitAWXTags(v)) == 0 {
				left = append(left, used...)
				continue
			}
			t.SkipTags = append(t.SkipTags, splitAWXTags(v)...)
		case flag == "--forks" || flag == "-f":
			v, used, ok := take()
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if !ok || err != nil || n < 1 {
				left = append(left, used...)
				continue
			}
			t.Forks = n
		case flag == "--extra-vars" || flag == "-e":
			v, used, ok := take()
			vars, readable := extraVarsArg(v)
			if !ok || !readable {
				left = append(left, used...)
				continue
			}
			if t.ExtraVars == nil {
				t.ExtraVars = map[string]any{}
			}
			for k, val := range vars {
				t.ExtraVars[k] = val
			}
		case arg == "--check" || arg == "-C":
			t.DryRun = true
		case arg == "--diff" || arg == "-D":
			t.DiffMode = true
		case arg == "--verbose":
			t.Verbosity++
		case verbosityArg.MatchString(arg):
			t.Verbosity += len(arg) - 1
		default:
			left = append(left, arg)
		}
	}
	return left
}

// splitPlaybookArg splits one argument into its flag and a value joined to it, as in --limit=web or
// -lweb, reporting whether a value was joined.
func splitPlaybookArg(arg string) (flag, value string, joined bool) {
	if strings.HasPrefix(arg, "--") {
		if i := strings.IndexByte(arg, '='); i >= 0 {
			return arg[:i], arg[i+1:], true
		}
		return arg, "", false
	}
	if len(arg) > 2 && valueShortFlags[arg[:2]] {
		return arg[:2], arg[2:], true
	}
	return arg, "", false
}

// extraVarsArg reads the value of one --extra-vars argument, a JSON object or space separated
// key=value pairs, reporting false for anything else. A file reference (@vars.yml) is not readable
// here, since the file is not in the export.
func extraVarsArg(v string) (map[string]any, bool) {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "@") {
		return nil, false
	}
	if strings.HasPrefix(v, "{") {
		var vars map[string]any
		if err := json.Unmarshal([]byte(v), &vars); err != nil {
			return nil, false
		}
		return vars, true
	}
	vars := map[string]any{}
	for _, pair := range strings.Fields(v) {
		key, val, ok := strings.Cut(pair, "=")
		// A quoted value with a space in it splits differently in Ansible's own parser, so anything
		// carrying a quote is reported rather than read by a simpler rule that would get it wrong.
		if !ok || key == "" || strings.ContainsAny(pair, `"'`) {
			return nil, false
		}
		vars[key] = val
	}
	return vars, true
}
