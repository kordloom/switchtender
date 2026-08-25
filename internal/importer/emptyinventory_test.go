package importer

import (
	"os"
	"strings"
	"testing"
	"time"
)

// awxHostsInAnUnknownPlace is an export whose inventory carries its hosts under a key this importer
// does not read, which is what a format the importer has not seen looks like from the inside.
const awxHostsInAnUnknownPlace = `{
  "inventory": [
    {"id": 1, "name": "prod", "hosts_list": [{"name": "web01"}, {"name": "web02"}]}
  ],
  "projects": [
    {"id": 5, "name": "infra", "scm_type": "git", "scm_url": "https://git.example/infra.git"}
  ],
  "job_templates": [
    {"id": 10, "name": "deploy", "playbook": "site.yml", "inventory": 1, "project": 5}
  ]
}`

// TestAnAWXInventoryThatImportsEmptyIsReported pins that an inventory arriving with nothing in it is
// named to the operator.
//
// AWX writes hosts in more than one place and this importer reads the two it knows about. When a
// real export put them somewhere else, every inventory was created empty and the import still
// reported success, so a migration produced a set of inventories that targeted no hosts and said
// nothing about it. Reading one more shape does not close that, because the next unseen shape fails
// the same silent way. Reporting the outcome closes it, whatever the shape was.
func TestAnAWXInventoryThatImportsEmptyIsReported(t *testing.T) {
	t.Parallel()
	plan, err := FromAWX([]byte(awxHostsInAnUnknownPlace), time.Unix(1787000000, 0).UTC())
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Inventories) != 1 {
		t.Fatalf("inventories = %d, want 1", len(plan.Inventories))
	}
	var found bool
	for _, w := range plan.Warnings {
		if strings.Contains(w, `inventory "prod"`) && strings.Contains(w, "no hosts and no groups") {
			found = true
		}
	}
	if !found {
		t.Errorf("an inventory that imported with no hosts was not reported.\nwarnings: %v",
			plan.Warnings)
	}
}

// TestASemaphoreInventoryThatImportsEmptyIsReported pins the same for the other importer whose
// format has more than one shape.
func TestASemaphoreInventoryThatImportsEmptyIsReported(t *testing.T) {
	t.Parallel()
	const doc = `{
      "inventories": [{"id": 1, "name": "prod", "type": "static", "inventory_file": "web01"}],
      "repositories": [{"id": 7, "name": "infra", "git_url": "https://git.example/i.git",
        "git_branch": "main"}],
      "templates": [{"id": 3, "name": "deploy", "playbook": "site.yml", "inventory_id": 1,
        "repository_id": 7}]
    }`
	plan, err := FromSemaphore([]byte(doc), time.Unix(1787000000, 0).UTC())
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	var found bool
	for _, w := range plan.Warnings {
		if strings.Contains(w, `inventory "prod"`) && strings.Contains(w, "no content") {
			found = true
		}
	}
	if !found {
		t.Errorf("an inventory that imported with no content was not reported.\nwarnings: %v",
			plan.Warnings)
	}
}

// TestARealExportDoesNotTripTheEmptyWarning pins that the warning stays quiet on the exports that
// do carry their hosts, so it does not become noise an operator learns to ignore.
func TestARealExportDoesNotTripTheEmptyWarning(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"awx-export.json", "awx-awxkit-export.json"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			plan, err := FromAWX(data, time.Unix(1787000000, 0).UTC())
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			for _, w := range plan.Warnings {
				if strings.Contains(w, "no hosts and no groups") {
					t.Errorf("a healthy export tripped the empty-inventory warning: %s", w)
				}
			}
		})
	}
}
