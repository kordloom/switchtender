// Package importer maps an AWX or Semaphore export into equivalent SwitchTender objects so a team can
// migrate in one command instead of a quarter. The mapping is pure: it reads export JSON and
// returns typed projects, inventories, templates, schedules, and credential shells plus warnings,
// with cross-references already wired by generated id. The command layer persists the result.
package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/util"
)

// Plan is the set of objects an import will create, cross-referenced by generated id, along with
// warnings a human must act on, chiefly the credential secrets an export omits by design.
type Plan struct {
	// Projects are the git projects to create.
	Projects []*project.Project
	// Inventories are the stored inventories to create, including the backing inventory each dynamic
	// source maintains.
	Inventories []*inventory.Inventory
	// Sources are the dynamic inventory sources to create; each maintains one backing inventory.
	Sources []*invsource.Source
	// Templates are the job templates to create.
	Templates []*template.Template
	// Schedules are the schedules to create.
	Schedules []*schedule.Schedule
	// Credentials are credential shells to create; their secrets must be re-entered.
	Credentials []*credential.Credential
	// Warnings names what could not be mapped cleanly or needs human follow up.
	Warnings []string
	// suppressed counts the warnings past the cap, so the total is still knowable.
	suppressed int
}

// objects counts everything the plan would create, which is what makes an import a success or a
// document nothing recognized.
func (p *Plan) objects() int {
	return len(p.Projects) + len(p.Inventories) + len(p.Sources) + len(p.Templates) +
		len(p.Schedules) + len(p.Credentials)
}

// ErrNothingRecognized is returned when a document parses but yields no objects at all.
//
// An empty plan used to be reported as a plan: a summary of zeros, exit status zero, and with --apply
// the words "Created 0 objects". An operator who exported from the wrong endpoint, the wrong API
// version, or the wrong project was told their migration had succeeded and had nothing to show for it.
// The one thing an import must never do is look complete when it read nothing.
var ErrNothingRecognized = errors.New("nothing in this document was recognized")

// requireObjects turns an empty plan into a refusal, naming what the importer was looking for so the
// operator can tell a wrong export from an empty one.
func (p *Plan) requireObjects(expected string) error {
	if p.objects() > 0 {
		return nil
	}
	return fmt.Errorf("%w: no %s were found. Check that this is the right export and the right "+
		"format, then try again", ErrNothingRecognized, expected)
}

// maxWarnings bounds how many warnings one plan reports.
//
// Two warnings are emitted per unmapped credential, and an export is a file somebody else wrote. A
// document at the upload limit produced millions of them: one preview request cost gigabytes of
// allocation and answered with a response many times the size of the request, on a path that needs
// no stores configured at all. A report nobody could read was the least of it.
const maxWarnings = 1000

// warn appends a formatted warning to the plan, up to maxWarnings.
//
// The cap is announced rather than silent. A truncated report that looks complete is how somebody
// concludes an import was clean when it was only long.
func (p *Plan) warn(format string, args ...any) {
	switch {
	case len(p.Warnings) < maxWarnings:
		p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
	case len(p.Warnings) == maxWarnings:
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"more than %d warnings, so the rest are not listed: this export needs review before "+
				"it is applied", maxWarnings))
		p.suppressed++
	default:
		p.suppressed++
	}
}

// Suppressed returns how many warnings were not listed because the plan hit its cap.
func (p *Plan) Suppressed() int { return p.suppressed }

// addSchedule validates a schedule before it joins the plan and stamps its first fire time.
//
// Two things were wrong with appending one directly. The cron string never went through
// Schedule.Validate, which the API applies on every other path, so an unparseable expression from an
// export became a stored row the scheduler logged an error over on every tick, forever. And
// NextRunAt was left nil, which the scheduler reads as "not due yet" and skips, so every imported
// schedule was reported as created and then never fired. A migrated nightly job that silently does
// not run is the worst shape this could take: nothing looks broken until the thing it was supposed
// to do has not happened for a month.
func (p *Plan) addSchedule(sc *schedule.Schedule, source string, now time.Time) {
	if err := sc.Validate(); err != nil {
		p.warn("schedule %q from %s was not imported: %v", sc.Name, source, err)
		return
	}
	next, err := sc.NextFire(now)
	if err != nil {
		p.warn("schedule %q from %s was not imported: its schedule never comes due: %v",
			sc.Name, source, err)
		return
	}
	sc.NextRunAt = &next
	p.Schedules = append(p.Schedules, sc)
}

// parseExtraVars decodes AWX or Semaphore extra vars, which arrive as a YAML or JSON string, into a
// map. An empty value yields nil; an unparseable one yields nil and is reported by the caller.
func parseExtraVars(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "---" {
		return nil, nil
	}
	var out map[string]any
	if err := yaml.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// safeININame reports whether s can be written as a host name, group name, or variable key in an INI
// inventory as itself.
//
// Tokenizing is the whole attack. Ansible's ini inventory plugin splits a host line on whitespace
// and reads every token after the host name as a key=value host variable, and it reads a bracketed
// word as a section header. So a name assembled from somebody else's export does not have to carry a
// newline to take the run over: a host named "web1 ansible_connection=local" lands as host web1 with
// ansible_connection=local, which redirects the whole play onto the executor, and one carrying
// ansible_python_interpreter=/tmp/x points Ansible at an interpreter of the export's choosing.
// Neither has a command-line counterpart, so an inventory setting them wins, and the content is
// written to a temp file and passed to ansible-playbook as -i verbatim.
//
// A name is refused, not rewritten, when it holds ASCII whitespace, '=', '#', '[', ']', or a control
// character. Each of those changes how the line parses, and a migration that silently renamed
// somebody's hosts would be its own kind of wrong; an inventory is the list of machines a play
// reaches, so a dropped-and-reported name beats an altered one.
func safeININame(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
		switch r {
		case ' ', '=', '#', '[', ']':
			return false
		}
	}
	return true
}

// hasControl reports whether s holds a control character, including any line break. Such a value
// cannot be written on a single INI line: Ansible reads the inventory line by line, so a break would
// start a fresh directive, and even quoting cannot fold it back onto one line.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// renderINIValue encodes v for the value side of an INI host variable and reports whether it can be
// written at all. A value carrying a control character is refused, because it cannot live on one
// line. Any other value is quoted when it holds whitespace, a comment mark, or a character shlex
// would act on, so Ansible reads it as a single value rather than as further host variables; a plain
// value is written as itself so the ordinary inventory stays readable.
func renderINIValue(v string) (string, bool) {
	if hasControl(v) {
		return "", false
	}
	if !strings.ContainsAny(v, " \t#\"'\\") {
		return v, true
	}
	return quoteINIValue(v), true
}

// quoteINIValue wraps v in double quotes so Ansible's ini inventory plugin reads it as one value.
// The plugin tokenizes a host line with shlex, which treats a double-quoted run as a single token and
// honors a backslash before a backslash or a double quote inside it. Both are escaped so the value
// cannot close its own quotes early and inject further host variables.
func quoteINIValue(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		if c := v[i]; c == '\\' || c == '"' {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	b.WriteByte('"')
	return b.String()
}

// buildInventoryINI renders hosts and groups into an INI inventory the file plugins accept. Every
// host lands in the implicit all group; named groups list their own members below.
//
// Anything that cannot be written as itself is dropped and reported, because an inventory is the
// list of machines a play will reach and a silently altered one is worse than a refused import.
func buildInventoryINI(plan *Plan, name string, hosts []importHost, groups []importGroup) string {
	var b strings.Builder
	if len(hosts) > 0 {
		for _, h := range hosts {
			b.WriteString(hostLine(plan, name, h))
		}
		b.WriteString("\n")
	}
	for _, g := range groups {
		if !safeININame(g.Name) {
			plan.warn("inventory %q: group %q was dropped because its name holds whitespace or an "+
				"inventory metacharacter, which Ansible would read as a new section or extra "+
				"host variables", name, oneLine(g.Name))
			continue
		}
		fmt.Fprintf(&b, "[%s]\n", g.Name)
		for _, h := range g.Hosts {
			b.WriteString(hostLine(plan, name, h))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// hostLine renders one inventory host with any host variables as inline key=value pairs.
func hostLine(plan *Plan, inv string, h importHost) string {
	if !safeININame(h.Name) {
		plan.warn("inventory %q: host %q was dropped because its name holds whitespace or an "+
			"inventory metacharacter, which Ansible would read as extra host variables or a new "+
			"section", inv, oneLine(h.Name))
		return ""
	}
	var line strings.Builder
	line.WriteString(h.Name)
	keys := make([]string, 0, len(h.Variables))
	for k := range h.Variables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !safeININame(k) {
			plan.warn("inventory %q: variable %q on host %q was dropped because its name holds "+
				"whitespace or an inventory metacharacter, which Ansible would read as extra host "+
				"variables", inv, oneLine(k), h.Name)
			continue
		}
		value, ok := renderINIValue(jsonScalarString(h.Variables[k]))
		if !ok {
			plan.warn("inventory %q: variable %q on host %q was dropped because its value carries a "+
				"control character, which cannot be written on a single inventory line", inv, k, h.Name)
			continue
		}
		fmt.Fprintf(&line, " %s=%s", k, value)
	}
	return line.String() + "\n"
}

// oneLine collapses a value to a single line for a warning, so a report cannot be made to look like
// several messages by the thing it is reporting on.
func oneLine(v string) string {
	r := strings.NewReplacer("\n", "\\n", "\r", "\\r")
	return util.Clip(r.Replace(v), 80)
}

// importHost is a host with optional variables, shared by the export parsers.
type importHost struct {
	// Name is the host name.
	Name string
	// Variables are per host variables rendered inline.
	Variables map[string]any
}

// importGroup is a named group of hosts, shared by the export parsers.
type importGroup struct {
	// Name is the group name.
	Name string
	// Hosts are the group's members.
	Hosts []importHost
}

// mapSurveyType converts an AWX survey field type to a SwitchTender field type, reporting whether the
// mapping is exact. Unknown types fall back to text.
//
// The password type is absent deliberately. Its caller refuses such a field rather than mapping it,
// because there is no field kind here that keeps an answer secret, and mapping it to text would
// downgrade a password prompt to a stored plaintext value without saying so.
func mapSurveyType(awxType string) (template.FieldType, bool) {
	switch awxType {
	case "text", "textarea":
		return template.FieldText, true
	case "integer":
		return template.FieldInt, true
	case "multiplechoice", "multiselect":
		return template.FieldChoice, true
	case "float":
		return template.FieldText, false
	default:
		return template.FieldText, false
	}
}

// awxPublicInputs lists the AWX credential inputs that are never secret, so their values can be
// reported back to the operator. AWX replaces secrets with "$encrypted$" on export, but this
// allowlist decides rather than that marker alone: a custom credential type whose secret field AWX
// does not mask would otherwise leak into a warning that is displayed.
var awxPublicInputs = []string{
	"authorize", "become_method", "become_username", "client", "cloud_environment", "domain",
	"host", "organization", "project", "region", "resource_group", "subscription", "tenant",
	"username", "validate_certs",
}

// publicInputPairs returns an AWX credential's non-secret inputs as ordered key/value pairs, drawn
// from the allowlist so a custom type's secret field can never appear. Values that are empty or still
// carry AWX's encrypted marker are omitted. Both the operator-facing formatter and the settings
// importer read from here, so they cannot disagree on what counts as non-secret.
func publicInputPairs(inputs map[string]any) [][2]string {
	if len(inputs) == 0 {
		return nil
	}
	var out [][2]string
	for _, key := range awxPublicInputs {
		v, ok := inputs[key]
		if !ok {
			continue
		}
		value := strings.TrimSpace(fmt.Sprint(v))
		if value == "" || value == "$encrypted$" {
			continue
		}
		out = append(out, [2]string{key, value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// publicInputs returns the non-secret inputs as sorted key=value strings, for telling an operator
// what the export recorded beyond the secret itself.
func publicInputs(inputs map[string]any) []string {
	pairs := publicInputPairs(inputs)
	if len(pairs) == 0 {
		return nil
	}
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p[0] + "=" + p[1]
	}
	return out
}

// jsonScalarString renders a JSON-decoded value as the text an inventory line or a survey choice
// should carry, faithfully rather than through Go's default formatting. A string is itself; a bool
// is true or false; a number is its source digits (json.Number when the export was decoded with
// UseNumber, otherwise a float printed without scientific notation); JSON null is empty; and an
// object or array is compact JSON rather than Go's map[k:v] or [a b] form, which an inventory could
// not read back.
func jsonScalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", t)
	}
}

// settingsKey translates an AWX input name to the settings key the kind's injection reads. The
// connection kinds rename username and the become inputs to the field names their injectors consume;
// every other input keeps its AWX name and rides along as reference metadata.
func settingsKey(kind credential.Kind, awxKey string) string {
	switch kind {
	case credential.KindSSHKey, credential.KindSSHPassword:
		switch awxKey {
		case "username":
			return "user"
		case "become_username":
			return "become_user"
		}
	case credential.KindNetwork:
		if awxKey == "username" {
			return "user"
		}
	case credential.KindBecome:
		switch awxKey {
		case "become_method":
			return "method"
		case "become_username":
			return "user"
		}
	}
	return awxKey
}

// credentialSettings converts an AWX credential's non-secret inputs into the settings stored on the
// imported credential, keys translated to what injection reads, and returns the AWX input names that
// were present but refused by the settings rules so the caller can report rather than silently drop
// them. Refusing one input never fails the import, keeping the partial-import promise. Each value is
// checked on its own so one bad input does not discard the rest; the aggregate count cap
// ValidateSettings also enforces is satisfied by construction, since awxPublicInputs is far shorter
// than the limit. The refused names come back in the sorted order of publicInputPairs.
func credentialSettings(kind credential.Kind, inputs map[string]any) (map[string]string, []string) {
	pairs := publicInputPairs(inputs)
	if len(pairs) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	var refused []string
	for _, p := range pairs {
		key := settingsKey(kind, p[0])
		if credential.ValidateSettings(map[string]string{key: p[1]}) != nil {
			refused = append(refused, p[0])
			continue
		}
		out[key] = p[1]
	}
	if len(out) == 0 {
		out = nil
	}
	return out, refused
}

// settingsList renders stored settings as sorted key=value pairs for a plan warning.
func settingsList(settings map[string]string) string {
	out := make([]string, 0, len(settings))
	for k, v := range settings {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// mapCredentialKind converts an AWX credential type name to a SwitchTender credential kind, reporting
// whether the mapping is exact. Unknown types fall back to the env kind. The credential's inputs
// refine the machine type, which covers both key and password login in AWX and so cannot be told
// apart by its name alone.
func mapCredentialKind(awxType string, inputs map[string]any) (credential.Kind, bool) {
	lower := strings.ToLower(awxType)
	switch lower {
	case "machine", "source control", "scm":
		if hasInput(inputs, "ssh_key_data") {
			return credential.KindSSHKey, true
		}
		if hasInput(inputs, "password") {
			return credential.KindSSHPassword, true
		}
		return credential.KindSSHKey, true
	case "network":
		return credential.KindNetwork, true
	case "vault":
		return credential.KindVaultPassword, true
	case "registry", "container registry":
		return credential.KindRegistry, true
	}
	switch {
	case strings.Contains(lower, "amazon"), strings.Contains(lower, "aws"):
		return credential.KindAWS, true
	case strings.Contains(lower, "azure"):
		return credential.KindAzure, true
	case strings.Contains(lower, "google"), strings.Contains(lower, "gce"), strings.Contains(lower, "gcp"):
		return credential.KindGCP, true
	case strings.Contains(lower, "vmware"), strings.Contains(lower, "vcenter"):
		return credential.KindVMware, true
	case strings.Contains(lower, "token"), strings.Contains(lower, "bearer"):
		return credential.KindToken, true
	}
	return credential.KindEnv, false
}

// hasInput reports whether an AWX credential configured the named input. AWX exports a secret as the
// literal "$encrypted$", so a set secret is present but unreadable, which is all this needs to know.
func hasInput(inputs map[string]any, key string) bool {
	v, ok := inputs[key]
	if !ok {
		return false
	}
	return strings.TrimSpace(fmt.Sprint(v)) != ""
}

// choicesFrom normalizes a survey field's choices, which AWX encodes as either a list or a newline
// separated string.
func choicesFrom(v any) []string {
	switch c := v.(type) {
	case []any:
		out := make([]string, 0, len(c))
		for _, item := range c {
			out = append(out, jsonScalarString(item))
		}
		return out
	case string:
		var out []string
		for line := range strings.SplitSeq(c, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, line)
			}
		}
		return out
	default:
		return nil
	}
}
