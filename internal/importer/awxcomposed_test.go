package importer_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// composedExport is an awxkit-shaped export holding two ordinary inventories, a smart inventory,
// and a constructed inventory whose inputs and options arrive under related the way awxkit nests
// them, plus a job template that targets the constructed one.
const composedExport = `{
  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://example.com/infra.git"}],
  "inventory": [
    {"name": "web fleet", "related": {"hosts": [{"name": "web1"}, {"name": "web2"}]}},
    {"name": "db fleet", "related": {"hosts": [{"name": "db1", "variables": "state: shutdown"}]}},
    {"name": "all web", "kind": "smart", "organization": {"name": "Ops"},
     "host_filter": "groups__name=web and not name__icontains=canary"},
    {"name": "shut down", "kind": "constructed",
     "related": {
       "input_inventories": [{"name": "web fleet"}, {"name": "db fleet"}],
       "inventory_sources": [{"name": "shut down", "source": "constructed",
         "source_vars": "groups:\n  off: state | default('running') == 'shutdown'\n",
         "limit": "off",
         "update_cache_timeout": 0}]
     }}
  ],
  "job_templates": [
    {"name": "power on", "playbook": "on.yml", "project": {"name": "infra"},
     "inventory": {"name": "shut down"}}
  ]
}`

// TestAWXComposedInventoriesImport is the round trip for AWX smart and constructed inventories:
// they arrive as composed inventories with their filter, inputs wired by id, options, and limit,
// they are stored as such, the template that targeted one targets it here, and the assessment names
// them. Before, each came across as an empty static inventory reported as having no hosts, and
// every template on one ran against nothing.
func TestAWXComposedInventoriesImport(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	plan, err := importer.FromAWX([]byte(composedExport), at)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	byName := map[string]*inventory.Inventory{}
	for _, inv := range plan.Inventories {
		byName[inv.Name] = inv
	}
	smart, built := byName["all web"], byName["shut down"]
	if smart == nil || built == nil {
		t.Fatalf("composed inventories missing from the plan: %v", byName)
	}
	if smart.Kind != inventory.KindSmart ||
		smart.HostFilter != "groups__name=web and not name__icontains=canary" {
		t.Errorf("smart inventory = %+v, want its kind and filter", smart)
	}
	wantInputs := []string{byName["web fleet"].ID, byName["db fleet"].ID}
	if diff := cmp.Diff(wantInputs, built.InputIDs); diff != "" {
		t.Errorf("constructed inputs mismatch (-want +got):\n%s", diff)
	}
	if built.Kind != inventory.KindConstructed || built.Limit != "off" ||
		!strings.Contains(built.SourceVars, "off: state | default('running') == 'shutdown'") {
		t.Errorf("constructed inventory = %+v, want its kind, options, and limit", built)
	}
	if len(plan.Sources) != 0 {
		t.Errorf("the constructed source became a dynamic source too: %+v", plan.Sources)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "no hosts and no groups") && (strings.Contains(w, "all web") ||
			strings.Contains(w, "shut down")) {
			t.Errorf("a composed inventory was reported as empty: %s", w)
		}
		if strings.Contains(w, "does not read") && (strings.Contains(w, "host_filter") ||
			strings.Contains(w, "inventory[].kind") || strings.Contains(w, "input_inventories")) {
			t.Errorf("a composed inventory's definition was reported as unread: %s", w)
		}
	}
	if len(plan.Templates) != 1 || plan.Templates[0].InventoryID != built.ID {
		t.Errorf("the template does not target the constructed inventory: %+v", plan.Templates)
	}

	stores := importer.ApplyStores{
		Projects: project.NewMemStore(), Inventories: inventory.NewMemStore(),
		Sources: invsource.NewMemStore(), Credentials: credential.NewMemStore(),
		Templates: template.NewMemStore(), Schedules: schedule.NewMemStore(),
	}
	if _, err := plan.Apply(context.Background(), stores); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	stored, err := stores.Inventories.Get(context.Background(), built.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if err := inventory.Validate(stored); err != nil {
		t.Errorf("the stored constructed inventory does not validate: %v", err)
	}
	if diff := cmp.Diff(wantInputs, stored.InputIDs); diff != "" {
		t.Errorf("stored inputs mismatch (-want +got):\n%s", diff)
	}

	var doc bytes.Buffer
	importer.Render(&doc, "awx", "export.json", plan.Assess())
	for _, want := range []string{"composed inventories", "all web (smart)",
		"shut down (constructed)"} {
		if !strings.Contains(doc.String(), want) {
			t.Errorf("the assessment does not name %q:\n%s", want, doc.String())
		}
	}
}

// TestAWXComposedInventoriesRefused pins that a composed inventory that would never resolve is
// refused with its reason instead of created broken, and is counted as left out.
func TestAWXComposedInventoriesRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Inventory string
		WantWarn  string
	}{{ // Test 0: A filter on a field this cannot read.
		Inventory: `{"name": "odd", "kind": "smart", "host_filter": "instance_id=i-123"}`,
		WantWarn:  `smart inventory "odd" was not imported`,
	}, { // Test 1: A constructed inventory none of whose inputs came across.
		Inventory: `{"name": "lonely", "kind": "constructed", "input_inventories": ["missing"]}`,
		WantWarn:  `constructed inventory "lonely" was not imported`,
	}, { // Test 2: Options outside the constructed plugin.
		Inventory: `{"name": "plug", "kind": "constructed", "input_inventories": ["web"],
		  "source_vars": "plugin: amazon.aws.aws_ec2\n"}`,
		WantWarn: `constructed inventory "plug" was not imported`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			export := `{"inventory": [{"name": "web", "hosts": [{"name": "web1"}]}, ` +
				test.Inventory + `]}`
			plan, err := importer.FromAWX([]byte(export), time.Date(2026, 9, 1, 0, 0, 0, 0,
				time.UTC))
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			var names []string
			for _, inv := range plan.Inventories {
				names = append(names, inv.Name)
			}
			if diff := cmp.Diff([]string{"web"}, names, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("inventories mismatch (-want +got):\n%s", diff)
			}
			report := plan.Report()
			found := false
			for _, w := range report.LeftOut {
				if strings.Contains(w, test.WantWarn) {
					found = true
				}
			}
			if !found {
				t.Errorf("the refusal is not reported as left out: %v", report.LeftOut)
			}
		})
	}
}
