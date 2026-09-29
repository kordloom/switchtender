package importer

import (
	"strings"
	"testing"
	"time"
)

// TestAGroupNestedUnderAnotherGroupComesAcross covers the shape awxkit writes.
//
// A group's children arrive two ways. A hand-written export names them as strings at the top level,
// which was read; awxkit nests whole group objects under the group's related block, which was not. So a
// real export imported every group flat and with the nested ones missing: the [name:children] sections
// were empty, a play targeting a parent group reached nothing under it, and the hosts belonging only to
// a subgroup were absent from the inventory altogether. The group count and host count both looked
// right, because the hosts are also listed at the inventory level and the groups that were read existed.
func TestAGroupNestedUnderAnotherGroupComesAcross(t *testing.T) {
	t.Parallel()
	const export = `{
	  "inventories": [{
	    "name": "prod",
	    "related": {
	      "hosts": [{"name": "web1.prod"}, {"name": "web2.prod"}, {"name": "db1.prod"}],
	      "groups": [{
	        "name": "web",
	        "related": {
	          "hosts": [{"name": "web1.prod"}],
	          "children": [{
	            "name": "web-canary",
	            "related": {"hosts": [{"name": "web2.prod"}]},
	            "variables": {"canary": true}
	          }]
	        }
	      }]
	    }
	  }],
	  "projects": [{"name": "web", "scm_type": "git", "scm_url": "https://example.invalid/w.git"}],
	  "job_templates": [{"name": "Deploy", "playbook": "site.yml", "project": "web",
	    "inventory": "prod"}]
	}`
	plan, err := FromAWX([]byte(export), time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Inventories) != 1 {
		t.Fatalf("inventories = %d, want 1", len(plan.Inventories))
	}
	content := plan.Inventories[0].Content

	if !strings.Contains(content, "[web-canary]") {
		t.Errorf("the group nested under web is not in the inventory, so its hosts and variables "+
			"exist nowhere:\n%s", content)
	}
	if !strings.Contains(content, "[web:children]") {
		t.Errorf("web has no children section, so a play targeting web reaches nothing underneath "+
			"it:\n%s", content)
	}
	if section := iniSection(content, "web:children"); !strings.Contains(section, "web-canary") {
		t.Errorf("web's children section does not name web-canary:\n%s", content)
	}
	if section := iniSection(content, "web-canary"); !strings.Contains(section, "web2.prod") {
		t.Errorf("the nested group has no hosts, so the host that belongs only to it is unreachable "+
			"by group:\n%s", content)
	}
}

// TestGroupNestingDoesNotRunAwayOnAnUntrustedExport holds the walk to a bound and to the fuller record.
//
// An export is a file somebody hands over. A group that lists itself among its children is a cycle, and
// two records of the same group is ordinary, since awxkit writes the whole object in one place and a
// bare reference in another.
func TestGroupNestingDoesNotRunAwayOnAnUntrustedExport(t *testing.T) {
	t.Parallel()
	self := awxGroup{Name: "loop"}
	self.Related = &awxGroupRelated{Children: []awxGroup{self}}

	done := make(chan []awxGroup, 1)
	go func() { done <- flattenGroups([]awxGroup{self}) }()
	select {
	case got := <-done:
		if len(got) != 1 {
			t.Errorf("a group naming itself flattened to %d groups, want 1", len(got))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("flattenGroups did not return on a group that names itself as its own child, so an " +
			"export a stranger wrote hangs the import")
	}

	stub := awxGroup{Name: "web"}
	full := awxGroup{Name: "web", Related: &awxGroupRelated{Hosts: []awxHost{{Name: "web1"}}}}
	for _, order := range [][]awxGroup{{stub, full}, {full, stub}} {
		got := flattenGroups(order)
		if len(got) != 1 {
			t.Fatalf("two records of one group flattened to %d, want 1", len(got))
		}
		if len(got[0].hosts()) != 1 {
			t.Error("the record without the hosts won, so which order the export happens to list a " +
				"group in decides whether its hosts import")
		}
	}
}

// iniSection returns the body of one INI section, for asserting about what is under a heading rather
// than about whether a name appears anywhere in the file.
func iniSection(content, name string) string {
	head := "[" + name + "]"
	start := strings.Index(content, head)
	if start < 0 {
		return ""
	}
	rest := content[start+len(head):]
	if end := strings.Index(rest, "\n["); end >= 0 {
		return rest[:end]
	}
	return rest
}
