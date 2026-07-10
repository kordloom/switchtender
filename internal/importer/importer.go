// Package importer maps an AWX or Semaphore export into equivalent Yardmaster objects so a team can
// migrate in one command instead of a quarter. The mapping is pure: it reads export JSON and
// returns typed projects, inventories, templates, schedules, and credential shells plus warnings,
// with cross-references already wired by generated id. The command layer persists the result.
package importer

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/invsource"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/schedule"
	"github.com/dcadolph/yardmaster/internal/template"
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

// mapSurveyType converts an AWX survey field type to a Yardmaster field type, reporting whether the
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

// mapCredentialKind converts an AWX credential type name to a Yardmaster credential kind, reporting
// whether the mapping is exact. Unknown types fall back to the env kind.
func mapCredentialKind(awxType string) (credential.Kind, bool) {
	switch strings.ToLower(awxType) {
	case "machine", "source control", "scm":
		return credential.KindSSHKey, true
	case "vault":
		return credential.KindVaultPassword, true
	case "registry", "container registry":
		return credential.KindRegistry, true
	default:
		return credential.KindEnv, false
	}
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
