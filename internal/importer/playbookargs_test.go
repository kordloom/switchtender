package importer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/template"
)

// TestCarryPlaybookArgs pins how each ansible-playbook argument lands on a template. The ones that
// narrow a run, limit, tags, skip tags, and check, are the ones whose loss widened a migrated deploy,
// so each is carried in every spelling ansible-playbook accepts and anything else is handed back.
func TestCarryPlaybookArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantTemplate template.Template
		WantLeft     []string
		Args         []string
	}{{ // Test 0: A limit as two arguments.
		Args: []string{"--limit", "web"}, WantTemplate: template.Template{Limit: "web"},
	}, { // Test 1: A limit joined with an equals sign keeps its pattern whole.
		Args: []string{"--limit=web:&prod"}, WantTemplate: template.Template{Limit: "web:&prod"},
	}, { // Test 2: The short limit, joined.
		Args: []string{"-lweb"}, WantTemplate: template.Template{Limit: "web"},
	}, { // Test 3: Tags accumulate across flags and commas, as ansible-playbook does.
		Args:         []string{"--tags", "deploy, config", "--tags=restart", "-t", "verify"},
		WantTemplate: template.Template{Tags: []string{"deploy", "config", "restart", "verify"}},
	}, { // Test 4: Skip tags.
		Args: []string{"--skip-tags", "slow"}, WantTemplate: template.Template{SkipTags: []string{"slow"}},
	}, { // Test 5: Check, diff, and verbosity, long and short.
		Args:         []string{"--check", "-D", "-vvv", "--verbose"},
		WantTemplate: template.Template{DryRun: true, DiffMode: true, Verbosity: 4},
	}, { // Test 6: Forks.
		Args: []string{"--forks", "20"}, WantTemplate: template.Template{Forks: 20},
	}, { // Test 7: Extra vars as key=value pairs arrive as strings, the way Ansible reads them.
		Args:         []string{"-e", "version=1.2 env=prod"},
		WantTemplate: template.Template{ExtraVars: map[string]any{"version": "1.2", "env": "prod"}},
	}, { // Test 8: Extra vars as JSON keep their types, and a later flag wins.
		Args: []string{"--extra-vars", `{"replicas": 3, "env": "stage"}`, "-eenv=prod"},
		WantTemplate: template.Template{
			ExtraVars: map[string]any{"replicas": float64(3), "env": "prod"},
		},
	}, { // Test 9: A vars file is not in the export, so it is handed back whole.
		Args: []string{"-e", "@vars.yml"}, WantLeft: []string{"-e", "@vars.yml"},
	}, { // Test 10: Flags with no template field are handed back in order, values included.
		Args:     []string{"--become", "--limit", "db", "--user", "deploy"},
		WantLeft: []string{"--become", "--user", "deploy"}, WantTemplate: template.Template{Limit: "db"},
	}, { // Test 11: A limit with no value is handed back rather than read as an empty limit.
		Args: []string{"--limit"}, WantLeft: []string{"--limit"},
	}, { // Test 12: Forks that are not a count are handed back.
		Args: []string{"--forks", "many"}, WantLeft: []string{"--forks", "many"},
	}, { // Test 13: A second limit is handed back, since which one wins should not be guessed.
		Args:         []string{"--limit", "web", "--limit", "db"},
		WantLeft:     []string{"--limit", "db"},
		WantTemplate: template.Template{Limit: "web"},
	}, { // Test 14: A quoted extra var is handed back rather than split by a rule that is wrong.
		Args: []string{"-e", `msg="two words"`}, WantLeft: []string{"-e", `msg="two words"`},
	}, { // Test 15: Nothing to carry.
		Args: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got template.Template
			left := carryPlaybookArgs(&got, test.Args)
			if diff := cmp.Diff(test.WantTemplate, got, cmpopts.EquateEmpty(),
				cmpopts.IgnoreTypes(time.Time{})); diff != "" {
				t.Errorf("template mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantLeft, left, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("arguments handed back mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSemaphoreCarriesTemplateArguments pins the import that ran a deploy on the wrong hosts.
//
// A Semaphore template passed --limit web on every run, and the import dropped its arguments, so the
// migrated template ran the same playbook against every host in the inventory. What cannot be
// carried is named, and the template's schedules arrive switched off, since firing a template that
// no longer does what it did is the failure this exists to prevent.
func TestSemaphoreCarriesTemplateArguments(t *testing.T) {
	t.Parallel()
	const doc = `{"meta": {"name": "limit-test"},
 "inventories": [{"name": "two-tier", "type": "static", "inventory": "[web]\nweb-a\n[db]\ndb-a\n"}],
 "templates": [
  {"name": "Deploy web only", "playbook": "site.yml", "inventory": "two-tier",
   "arguments": "[\"--limit\", \"web\", \"--tags\", \"deploy\", \"--check\"]"},
  {"name": "Array form", "playbook": "site.yml", "inventory": "two-tier",
   "arguments": ["--skip-tags", "slow"]},
  {"name": "Needs root", "playbook": "site.yml", "inventory": "two-tier",
   "arguments": "[\"--become\", \"--limit\", \"db\"]"},
  {"name": "Unreadable", "playbook": "site.yml", "inventory": "two-tier",
   "arguments": "--limit web"},
  {"name": "None", "playbook": "site.yml", "inventory": "two-tier", "arguments": null}
 ],
 "schedules": [
  {"name": "nightly deploy", "template": "Deploy web only", "cron_format": "0 2 * * *"},
  {"name": "nightly root", "template": "Needs root", "cron_format": "0 3 * * *"},
  {"name": "nightly unreadable", "template": "Unreadable", "cron_format": "0 4 * * *"}
 ]}`
	plan, err := FromSemaphore([]byte(doc), time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	byName := map[string]*template.Template{}
	for _, tpl := range plan.Templates {
		byName[tpl.Name] = tpl
	}
	web := byName["Deploy web only"]
	if web == nil || web.Limit != "web" || !web.DryRun ||
		cmp.Diff([]string{"deploy"}, web.Tags) != "" {
		t.Fatalf("the limited template came across as %+v, want limit web, tag deploy, and check", web)
	}
	if got := byName["Array form"]; got == nil || cmp.Diff([]string{"slow"}, got.SkipTags) != "" {
		t.Errorf("arguments given as a plain array were not read: %+v", got)
	}
	if got := byName["Needs root"]; got == nil || got.Limit != "db" {
		t.Errorf("the carried part of a partly carried template was lost: %+v", got)
	}
	if _, ok := warningContaining(t, plan.Warnings, `template "Needs root" passes "--become"`); !ok {
		t.Errorf("the argument that was not carried was not named.\nwarnings: %v", plan.Warnings)
	}
	if _, ok := warningContaining(t, plan.Warnings, `template "Unreadable" has arguments`); !ok {
		t.Errorf("arguments that could not be read were not reported.\nwarnings: %v", plan.Warnings)
	}
	armed := map[string]bool{}
	for _, sc := range plan.Schedules {
		armed[sc.Name] = sc.Enabled
	}
	want := map[string]bool{"nightly deploy": true, "nightly root": false, "nightly unreadable": false}
	if diff := cmp.Diff(want, armed); diff != "" {
		t.Errorf("schedule armed state mismatch (-want +got):\n%s\nwarnings: %v", diff, plan.Warnings)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "does not read") && strings.Contains(w, "arguments") {
			t.Errorf("arguments are still reported as a field this importer does not read: %s", w)
		}
	}
}
