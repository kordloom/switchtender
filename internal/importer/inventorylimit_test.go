package importer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// oversizedAWXExport returns an AWX export with two inventories and a template on each. "Huge"
// renders past inventory.MaxContentBytes through its synced host variables, and its template has a
// schedule. "Small" is ordinary.
func oversizedAWXExport(t *testing.T) []byte {
	t.Helper()
	pad := strings.Repeat("v", 1000)
	hosts := make([]map[string]any, 0, 18000)
	for i := range 18000 {
		hosts = append(hosts, map[string]any{"name": fmt.Sprintf("h%05d.example", i),
			"variables": map[string]any{"ec2_tags": pad}})
	}
	export := map[string]any{
		"projects": []any{map[string]any{"name": "Web", "scm_type": "git",
			"scm_url": "https://github.com/acme/web.git", "scm_branch": "main"}},
		"inventory": []any{
			map[string]any{"name": "Huge", "hosts": hosts},
			map[string]any{"name": "Small", "hosts": []any{map[string]any{"name": "web1.example"}}},
		},
		"job_templates": []any{
			map[string]any{"name": "Deploy Huge", "playbook": "site.yml", "project": "Web", "inventory": "Huge",
				"related": map[string]any{"schedules": []any{map[string]any{"name": "Nightly huge",
					"rrule": "DTSTART:20260101T090000Z\nRRULE:FREQ=DAILY;INTERVAL=1", "enabled": true}}}},
			map[string]any{"name": "Deploy Small", "playbook": "site.yml", "project": "Web", "inventory": "Small"},
		},
	}
	data, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestInventoryLimitSkipsOversizedImport pins that an import leaves out an inventory that renders
// past the limit a stored inventory may hold, with every template and schedule that names it, says
// so in one warning naming the size and what was skipped, and applies the rest.
func TestInventoryLimitSkipsOversizedImport(t *testing.T) {
	t.Parallel()
	plan, err := importer.FromAWX(oversizedAWXExport(t), time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	plan.SkipOversizedInventories()
	var warning string
	for _, w := range plan.Warnings {
		if strings.Contains(w, `Inventory "Huge"`) {
			warning = w
		}
	}
	for _, want := range []string{"renders to 1", "MiB, past the 16 MiB limit", `template "Deploy Huge"`,
		`schedule "Nightly huge"`, "dynamic inventory file in a project"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning %q does not hold %q", warning, want)
		}
	}
	for _, inv := range plan.Inventories {
		if inv.Name == "Huge" {
			t.Error("the preview still plans the oversized inventory")
		}
	}

	stores := importer.ApplyStores{
		Projects: project.NewMemStore(), Inventories: inventory.NewMemStore(), Sources: invsource.NewMemStore(),
		Credentials: credential.NewMemStore(), Templates: template.NewMemStore(), Schedules: schedule.NewMemStore(),
	}
	if _, err := plan.Apply(context.Background(), stores); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	invs, _ := stores.Inventories.List(context.Background())
	tpls, _ := stores.Templates.List(context.Background())
	scheds, _ := stores.Schedules.List(context.Background())
	if len(invs) != 1 || invs[0].Name != "Small" {
		t.Errorf("stored inventories = %v, want Small alone", names(invs, func(i *inventory.Inventory) string { return i.Name }))
	}
	if len(tpls) != 1 || tpls[0].Name != "Deploy Small" || tpls[0].InventoryID != invs[0].ID {
		t.Errorf("stored templates = %v, want Deploy Small on Small", names(tpls, func(t *template.Template) string { return t.Name }))
	}
	if len(scheds) != 0 {
		t.Errorf("stored schedules = %d, want none", len(scheds))
	}
}

// names maps each element of items to its name.
func names[T any](items []T, name func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, name(it))
	}
	return out
}
