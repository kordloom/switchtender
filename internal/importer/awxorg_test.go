package importer

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestAWXResolvesReferencesWithinTheirOrganization pins the wiring that sent one team's deploy to
// another team's hosts.
//
// Two organizations each held a project named site and an inventory named Production. Keyed by name
// alone, the second of each replaced the first, and both job templates were wired to the second
// organization's repository and inventory. Each template now reaches its own organization's objects,
// and the objects that share a name arrive named with their organization so both can be told apart.
func TestAWXResolvesReferencesWithinTheirOrganization(t *testing.T) {
	t.Parallel()
	const export = `{
	  "projects": [
	    {"name": "site", "organization": {"name": "Team A", "type": "organization"},
	     "scm_type": "git", "scm_url": "https://git.example.com/a/site.git"},
	    {"name": "site", "organization": {"name": "Team B", "type": "organization"},
	     "scm_type": "git", "scm_url": "https://git.example.com/b/site.git"},
	    {"name": "tools", "organization": {"name": "Team A", "type": "organization"},
	     "scm_type": "git", "scm_url": "https://git.example.com/a/tools.git"}
	  ],
	  "inventory": [
	    {"name": "Production", "organization": {"name": "Team A", "type": "organization"},
	     "hosts": [{"name": "a1"}]},
	    {"name": "Production", "organization": {"name": "Team B", "type": "organization"},
	     "hosts": [{"name": "b1"}]}
	  ],
	  "job_templates": [
	    {"name": "Deploy", "organization": {"name": "Team A", "type": "organization"},
	     "playbook": "site.yml",
	     "project": {"organization": {"name": "Team A", "type": "organization"}, "name": "site"},
	     "inventory": {"organization": {"name": "Team A", "type": "organization"}, "name": "Production"}},
	    {"name": "Deploy", "organization": {"name": "Team B", "type": "organization"},
	     "playbook": "site.yml",
	     "project": {"organization": {"name": "Team B", "type": "organization"}, "name": "site"},
	     "inventory": {"organization": {"name": "Team B", "type": "organization"}, "name": "Production"}},
	    {"name": "Unscoped", "playbook": "site.yml", "project": "tools", "inventory": "Production"}
	  ]}`
	plan, err := FromAWX([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	projects := map[string]string{}
	for _, p := range plan.Projects {
		projects[p.ID] = p.Name + " " + p.RepoURL
	}
	inventories := map[string]string{}
	for _, inv := range plan.Inventories {
		inventories[inv.ID] = inv.Name
	}
	got := map[string][2]string{}
	for _, tpl := range plan.Templates {
		got[tpl.Name] = [2]string{projects[tpl.ProjectID], inventories[tpl.InventoryID]}
	}
	want := map[string][2]string{
		"Team A/Deploy": {"Team A/site https://git.example.com/a/site.git", "Team A/Production"},
		"Team B/Deploy": {"Team B/site https://git.example.com/b/site.git", "Team B/Production"},
		// A reference that names no organization resolves a name only one organization uses, and
		// is not guessed at for a name two of them use.
		"Unscoped": {"tools https://git.example.com/a/tools.git", ""},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("template wiring mismatch (-want +got):\n%s\nwarnings: %v", diff, plan.Warnings)
	}
	if _, ok := warningContaining(t, plan.Warnings, `template "Unscoped" references unknown inventory`,
		"more than one organization"); !ok {
		t.Errorf("the ambiguous reference was not reported as ambiguous.\nwarnings: %v", plan.Warnings)
	}
	if _, ok := warningContaining(t, plan.Warnings, "appears more than once"); ok {
		t.Errorf("objects in different organizations were reported as duplicates.\nwarnings: %v",
			plan.Warnings)
	}
}

// TestAWXIDs pins the resolution rules on their own.
func TestAWXIDs(t *testing.T) {
	t.Parallel()
	ids := awxIDs{}
	ids.set("A", "site", "a-site")
	ids.set("B", "site", "b-site")
	ids.set("", "plain", "plain-id")
	ids.set("A", "only", "a-only")
	tests := []struct {
		Ref    awxRef
		WantID string
		WantOK bool
	}{
		{Ref: awxRef{Name: "site", Org: "A"}, WantID: "a-site", WantOK: true},    // Test 0.
		{Ref: awxRef{Name: "site", Org: "B"}, WantID: "b-site", WantOK: true},    // Test 1.
		{Ref: awxRef{Name: "site", Org: "C"}},                                    // Test 2: never another org's.
		{Ref: awxRef{Name: "site"}},                                              // Test 3: two orgs, no guess.
		{Ref: awxRef{Name: "only"}, WantID: "a-only", WantOK: true},              // Test 4: the one there is.
		{Ref: awxRef{Name: "plain", Org: "A"}, WantID: "plain-id", WantOK: true}, // Test 5: an unscoped object.
		{Ref: awxRef{}}, // Test 6: no reference.
	}
	for i, test := range tests {
		id, ok := ids.get(test.Ref)
		if id != test.WantID || ok != test.WantOK {
			t.Errorf("test %d: get(%+v) = %q, %t, want %q, %t", i, test.Ref, id, ok, test.WantID,
				test.WantOK)
		}
	}
}
