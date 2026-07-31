// Package importer maps an AWX or Semaphore export into equivalent SwitchTender objects so a team can
// migrate in one command instead of a quarter. The mapping is pure: it reads export JSON and
// returns typed projects, inventories, templates, schedules, and credential shells plus warnings,
// with cross-references already wired by generated id. The command layer persists the result.
package importer

import (
	"fmt"
	"sort"
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
	next, err := schedule.NextFire(sc.Cron, now)
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

// safeINIValue reports whether v can be written into an INI inventory as itself.
//
// A line break is the whole attack. An inventory is assembled from names and variable values that
// came out of somebody else's export, and a value carrying a newline does not land as a strange host
// name: it closes the line and starts a new directive. A host named "web1\n[all:vars]\nansible_python_interpreter=/tmp/x"
// produces a syntactically clean inventory that points Ansible at an arbitrary interpreter on the
// executor for every play, and "ansible_connection=local" redirects the whole run onto the executor
// itself. Neither has a command-line counterpart, so an inventory setting them wins.
//
// Values are refused rather than escaped. INI quoting rules differ between the parsers that read
// these files, so a value that is safe by one reading is not by another, and a migration that
// silently rewrote somebody's host names would be its own kind of wrong.
func safeINIValue(v string) bool {
	return !strings.ContainsAny(v, "\n\r")
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
		if !safeINIValue(g.Name) {
			plan.warn("inventory %q: group %q was dropped because its name spans more than one "+
				"line, which would have written new inventory directives", name, oneLine(g.Name))
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
	if !safeINIValue(h.Name) {
		plan.warn("inventory %q: host %q was dropped because its name spans more than one line, "+
			"which would have written new inventory directives", inv, oneLine(h.Name))
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
		value := fmt.Sprintf("%v", h.Variables[k])
		if !safeINIValue(k) || !safeINIValue(value) {
			plan.warn("inventory %q: variable %q on host %q was dropped because it spans more "+
				"than one line, which would have written new inventory directives",
				inv, oneLine(k), h.Name)
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
func mapSurveyType(awxType string) (template.FieldType, bool) {
	switch awxType {
	case "text", "textarea", "password":
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

// publicInputs returns an AWX credential's non-secret inputs as sorted key=value pairs, for telling
// an operator what the export recorded beyond the secret itself. Values that are empty or still
// carry AWX's encrypted marker are left out, as is anything not on the allowlist.
func publicInputs(inputs map[string]any) []string {
	if len(inputs) == 0 {
		return nil
	}
	var out []string
	for _, key := range awxPublicInputs {
		v, ok := inputs[key]
		if !ok {
			continue
		}
		value := strings.TrimSpace(fmt.Sprint(v))
		if value == "" || value == "$encrypted$" {
			continue
		}
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
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
			out = append(out, fmt.Sprintf("%v", item))
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
