package importer

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// sharedOrgExport is an export whose organization Ops holds a smart inventory, the inventory it
// filters, two job templates, and notification templates attached to the organization itself, so
// both the smart inventory placement and the organization notification import place things in it.
const sharedOrgExport = `{
 "organizations": [
  {"name": "Ops", "related": {
    "notification_templates_error": [{"name": "ops chat", "type": "notification_template"}]}}
 ],
 "notification_templates": [
  {"name": "ops chat", "organization": {"name": "Ops", "type": "organization"},
   "notification_type": "mattermost",
   "notification_configuration": {"mattermost_url": "https://mm.example.com/hooks/ops"}}
 ],
 "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://example.com/infra.git"}],
 "inventory": [
  {"name": "web fleet", "organization": {"name": "Ops"},
   "related": {"hosts": [{"name": "web1"}]}},
  {"name": "all web", "kind": "smart", "organization": {"name": "Ops"},
   "host_filter": "name__startswith=web"}
 ],
 "job_templates": [
  {"name": "deploy", "playbook": "deploy.yml", "project": "infra",
   "organization": {"name": "Ops", "type": "organization"}},
  {"name": "patch", "playbook": "patch.yml", "project": "infra",
   "organization": {"name": "Ops", "type": "organization"}}
 ]
}`

// TestOneOrganizationForEverythingTheImportPlacesThere pins that the smart inventory placement and
// the organization notification import share one organization for one AWX organization, resolved
// by one set of rules: created when none of the name is held, the one held when exactly one is, and
// given up, with everything placed in it taken back out and its attachments falling back to its
// templates, when the name is ambiguous. Before, each made an organization of its own, so an
// organization holding both came across twice, and the second match then found two of the name and
// took the inventories back out.
func TestOneOrganizationForEverythingTheImportPlacesThere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Held names the organizations stored before the import is applied.
		Held []string
		// WantPlanOrgs are the organizations the plan makes before the apply.
		WantPlanOrgs []string
		// WantStored are the organizations stored after the apply, sorted.
		WantStored []string
		// WantPlaced maps each placed template and inventory to its organization's name.
		WantPlaced map[string]string
		// WantOutcome is what became of the organization's own attachments.
		WantOutcome string
	}{{ // Test 0: Nothing of the name is held, so one organization is created for everything.
		WantPlanOrgs: []string{"Ops"}, WantStored: []string{"Ops"},
		WantPlaced: map[string]string{"template deploy": "Ops", "template patch": "Ops",
			"inventory web fleet": "Ops", "inventory all web": "Ops"},
		WantOutcome: OrgNotifyImported,
	}, { // Test 1: One of the name is held, so everything goes on it and nothing is created.
		Held: []string{"Ops"}, WantPlanOrgs: []string{"Ops"}, WantStored: []string{"Ops"},
		WantPlaced: map[string]string{"template deploy": "Ops", "template patch": "Ops",
			"inventory web fleet": "Ops", "inventory all web": "Ops"},
		WantOutcome: OrgNotifyMatched,
	}, { // Test 2: Two of the name are held, so nothing is placed and the attachments fall back.
		Held: []string{"Ops", "Ops"}, WantPlanOrgs: []string{"Ops"},
		WantStored: []string{"Ops", "Ops"}, WantPlaced: map[string]string{},
		WantOutcome: OrgNotifyFellBack,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte(sharedOrgExport), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			var planOrgs []string
			for _, o := range plan.Orgs {
				planOrgs = append(planOrgs, o.Name)
			}
			if diff := cmp.Diff(test.WantPlanOrgs, planOrgs); diff != "" {
				t.Errorf("planned organizations mismatch (-want +got):\n%s", diff)
			}
			statement := `organization "Ops": the import places 2 templates (deploy, patch) and 2 ` +
				`inventories (all web, web fleet) in it`
			if !slices.ContainsFunc(plan.Warnings, func(w string) bool {
				return strings.Contains(w, statement) && strings.Contains(w, "--strict-grants")
			}) {
				t.Errorf("no warning states the placement %q:\n%s", statement,
					strings.Join(plan.Warnings, "\n"))
			}
			stores := orgApplyStores(t, append([]string{}, test.Held...))
			if _, err := plan.Apply(ctx, stores); err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			held, err := stores.Orgs.List(ctx)
			if err != nil {
				t.Fatalf("List(orgs) error = %v", err)
			}
			names := map[string]string{}
			var stored []string
			for _, o := range held {
				names[o.ID] = o.Name
				stored = append(stored, o.Name)
			}
			sort.Strings(stored)
			if diff := cmp.Diff(test.WantStored, stored); diff != "" {
				t.Errorf("stored organizations mismatch (-want +got):\n%s", diff)
			}
			placed := map[string]string{}
			templates, err := stores.Templates.List(ctx)
			if err != nil {
				t.Fatalf("List(templates) error = %v", err)
			}
			for _, tpl := range templates {
				if tpl.OrgID != "" {
					placed["template "+tpl.Name] = names[tpl.OrgID]
				}
			}
			invs, err := stores.Inventories.List(ctx)
			if err != nil {
				t.Fatalf("List(inventories) error = %v", err)
			}
			for _, inv := range invs {
				if inv.OrgID != "" {
					placed["inventory "+inv.Name] = names[inv.OrgID]
				}
			}
			if diff := cmp.Diff(test.WantPlaced, placed, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("stored placement mismatch (-want +got):\n%s", diff)
			}
			fromPlan := map[string]string{}
			for name, o := range plan.TemplateOrganizations() {
				fromPlan["template "+name] = o
			}
			for name, o := range plan.InventoryOrganizations() {
				fromPlan["inventory "+name] = o
			}
			if diff := cmp.Diff(test.WantPlaced, fromPlan, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("the plan's account of the placement mismatch (-want +got):\n%s", diff)
			}
			outcomes := plan.Report().Organizations
			if len(outcomes) != 1 || outcomes[0].Outcome != test.WantOutcome {
				t.Errorf("organization notification outcomes = %+v, want %s", outcomes,
					test.WantOutcome)
			}
		})
	}
}
