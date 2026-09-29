// Package importer maps an AWX or Semaphore export into equivalent SwitchTender objects so a team can
// migrate in one command instead of a quarter. The mapping is pure: it reads export JSON and
// returns typed projects, inventories, templates, schedules, and credential shells plus warnings,
// with cross-references already wired by generated id. The command layer persists the result.
package importer

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
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
}

// warn appends a formatted warning to the plan.
func (p *Plan) warn(format string, args ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
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

// buildInventoryINI renders hosts and groups into an INI inventory the file plugins accept. Every
// host lands in the implicit all group; named groups list their own members below.
func buildInventoryINI(hosts []importHost, groups []importGroup) string {
	var b strings.Builder
	if len(hosts) > 0 {
		for _, h := range hosts {
			b.WriteString(hostLine(h))
		}
		b.WriteString("\n")
	}
	for _, g := range groups {
		fmt.Fprintf(&b, "[%s]\n", g.Name)
		for _, h := range g.Hosts {
			b.WriteString(hostLine(h))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// hostLine renders one inventory host with any host variables as inline key=value pairs.
func hostLine(h importHost) string {
	var line strings.Builder
	line.WriteString(h.Name)
	keys := make([]string, 0, len(h.Variables))
	for k := range h.Variables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&line, " %s=%v", k, h.Variables[k])
	}
	return line.String() + "\n"
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
// does not mask would otherwise leak into a warning that is displayed and stored.
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
