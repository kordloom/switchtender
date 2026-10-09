package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestAMalformedAWXTemplateIsSkippedNotFatal pins the promise the migration page makes: a malformed
// asset is skipped with a warning and the rest of the export imports.
//
// One job template carrying its forks as a word refused the whole document, so an export of hundreds
// migrated nothing. A quoted number is read as the number it is, and only a value that is not one
// costs its own template.
func TestAMalformedAWXTemplateIsSkippedNotFatal(t *testing.T) {
	t.Parallel()
	const export = `{
	  "projects": [{"name": "web", "scm_type": "git", "scm_url": "https://example.com/web.git"}],
	  "job_templates": [
	    {"name": "Good", "playbook": "site.yml", "project": "web", "forks": 10},
	    {"name": "Quoted", "playbook": "site.yml", "project": "web", "forks": "5",
	     "job_slice_count": "2", "verbosity": "", "timeout": null},
	    {"name": "Broken", "playbook": "site.yml", "project": "web", "forks": "many"}
	  ]}`
	plan, err := FromAWX([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromAWX() error = %v, want the malformed template skipped and the rest imported", err)
	}
	forks := map[string]int{}
	shards := map[string]int{}
	for _, tpl := range plan.Templates {
		forks[tpl.Name] = tpl.Forks
		shards[tpl.Name] = tpl.Shards
	}
	if diff := cmp.Diff(map[string]int{"Good": 10, "Quoted": 5}, forks); diff != "" {
		t.Errorf("templates imported mismatch (-want +got):\n%s", diff)
	}
	if shards["Quoted"] != 2 {
		t.Errorf("a quoted job_slice_count read as %d shards, want 2", shards["Quoted"])
	}
	w, ok := warningContaining(t, plan.Warnings, `job_templates entry "Broken" was skipped`,
		"forks field is a string where a whole number belongs")
	if !ok {
		t.Fatalf("the skipped template was not named with its reason.\nwarnings: %v", plan.Warnings)
	}
	if !slices.Contains(plan.Report().LeftOut, w) {
		t.Errorf("the skipped template was not counted as left out, so the summary understates the "+
			"loss: %q", w)
	}
}

// TestSemaphoreEnumValuesInTheirCurrentShape pins the enum survey Semaphore writes today, each value
// an object holding a label and the value. The importer read only bare strings, so every current
// export with an enum survey failed whole.
func TestSemaphoreEnumValuesInTheirCurrentShape(t *testing.T) {
	t.Parallel()
	const export = `{"templates": [{"name": "Deploy", "playbook": "site.yml", "survey_vars": [
	  {"name": "env", "title": "Environment", "type": "enum", "required": true,
	   "values": [{"name": "Staging", "value": "stage"}, {"name": "Production", "value": "prod"}]},
	  {"name": "size", "title": "Size", "type": "enum",
	   "values": ["small", "large"]},
	  {"name": "replicas", "title": "Replicas", "type": "enum",
	   "values": [{"name": "Three", "value": 3}]}
	]}]}`
	plan, err := FromSemaphore([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	got := map[string][]string{}
	for _, f := range plan.Templates[0].Survey {
		got[f.Var] = f.Choices
	}
	want := map[string][]string{
		"env": {"stage", "prod"}, "size": {"small", "large"}, "replicas": {"3"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("choices mismatch (-want +got):\n%s", diff)
	}
}

// TestABadTemplateCostsOnlyItselfInsideASemaphoreProject pins the container rule. A project backup
// wraps every asset in a project, so dropping the entry that failed would have dropped the whole
// project for one template.
func TestABadTemplateCostsOnlyItselfInsideASemaphoreProject(t *testing.T) {
	t.Parallel()
	const export = `{"projects": [{"name": "ops",
	  "inventories": [{"name": "prod", "type": "static", "inventory": "[all]\nh1\n"}],
	  "templates": [
	    {"name": "Good", "playbook": "site.yml", "inventory": "prod"},
	    {"name": "Broken", "playbook": "site.yml", "survey_vars": "not a list"}
	  ]}]}`
	plan, err := FromSemaphore([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	if len(plan.Inventories) != 1 || len(plan.Templates) != 1 || plan.Templates[0].Name != "Good" {
		t.Fatalf("got %d inventories and templates %v, want the project's other assets imported",
			len(plan.Inventories), plan.Templates)
	}
	if _, ok := warningContaining(t, plan.Warnings, `projects entry "ops": templates entry "Broken"`,
		"was skipped"); !ok {
		t.Errorf("the skipped template was not named inside its project.\nwarnings: %v", plan.Warnings)
	}
}

// TestABadHostCostsOnlyItselfInsideAnAWXInventory pins the container rule for the shape awxkit
// writes. One malformed host costs that host alone, not the inventory it sits in with its other
// hosts, groups, and variables, and not every template on that inventory. A host skipped from the
// inventory is left out of every group that names it too, so no group brings it back.
func TestABadHostCostsOnlyItselfInsideAnAWXInventory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the subtest.
		Name string
		// Key is the top-level key the inventory list is under, inventory when empty.
		Key string
		// Inventory is the inventory entry, holding one malformed host somewhere inside it.
		Inventory string
		// WantSkipped is the warning naming the host that was skipped.
		WantSkipped string
		// WantGone is a line the inventory content must not hold, 123 when empty.
		WantGone string
		// WantKept are lines the inventory content must hold beyond the ones every case keeps.
		WantKept []string
	}{{ // Test 0: The bad host sits under the inventory's related block, where awxkit writes hosts.
		Name: "host under related",
		Inventory: `{"name": "Production", "variables": {"env": "prod"}, "related": {
		  "hosts": [{"name": "web01"}, {"name": 123}, {"name": "db01"}],
		  "groups": [{"name": "web", "related": {"hosts": [{"name": "web01"}]}}]}}`,
		WantSkipped: `inventory entry "Production": related.hosts entry #2 was skipped because its ` +
			"name field is a number where text belongs",
	}, { // Test 1: The bad host sits in a group's related block, two containers down.
		Name: "host under a group",
		Inventory: `{"name": "Production", "variables": {"env": "prod"}, "related": {
		  "hosts": [{"name": "web01"}, {"name": "db01"}],
		  "groups": [{"name": "web", "related": {"hosts": [{"name": "web01"}, {"name": 123}]}}]}}`,
		WantSkipped: `inventory entry "Production": related.groups entry "web": related.hosts ` +
			"entry #2 was skipped because its name field is a number where text belongs",
	}, { // Test 2: The bad host sits at the inventory's top level, the hand-written shape.
		Name: "host at the top level",
		Inventory: `{"name": "Production", "variables": {"env": "prod"},
		  "hosts": [{"name": "web01"}, {"name": 123}, {"name": "db01"}],
		  "groups": [{"name": "web", "hosts": [{"name": "web01"}]}]}`,
		WantSkipped: `inventory entry "Production": hosts entry #2 was skipped because its name ` +
			"field is a number where text belongs",
	}, { // Test 3: A named host with a bad field is left out of the group awxkit also lists it in.
		Name: "skipped host a group names",
		Inventory: `{"name": "Production", "variables": {"env": "prod"}, "related": {
		  "hosts": [{"name": "web01"}, {"name": "web02", "enabled": "no",
		    "variables": {"ansible_host": "10.0.0.2"}}, {"name": "db01"}],
		  "groups": [{"name": "web", "related": {"hosts": [{"name": "web01"}, {"name": "web02"}]}},
		    {"name": "edge", "related": {"hosts": [{"name": "web02"}]}}]}}`,
		WantSkipped: `inventory entry "Production": related.hosts entry "web02" was skipped ` +
			"because its enabled field is a string where true or false belongs, and the rest of " +
			`the export imports without it. It is left out of groups "edge", "web" too, which ` +
			"list it, so no play reaches it",
		WantGone: "web02",
	}, { // Test 4: The same under a nested child group and the inventories key.
		Name: "skipped host a nested group names",
		Key:  "inventories",
		Inventory: `{"name": "Production", "variables": {"env": "prod"}, "related": {
		  "hosts": [{"name": "web01"}, {"name": "web02", "enabled": "no"}, {"name": "db01"}],
		  "groups": [{"name": "web", "related": {"hosts": [{"name": "web01"}],
		    "children": [{"name": "canary", "related": {"hosts": [{"name": "web02"}]}}]}}]}}`,
		WantSkipped: `inventories entry "Production": related.hosts entry "web02" was skipped ` +
			"because its enabled field is a string where true or false belongs, and the rest of " +
			`the export imports without it. It is left out of group "canary" too, which lists ` +
			"it, so no play reaches it",
		WantGone: "web02",
		WantKept: []string{"[canary]"},
	}, { // Test 5: A host listed twice, once readably, stays in the group that names it.
		Name: "host listed twice",
		Inventory: `{"name": "Production", "variables": {"env": "prod"}, "related": {
		  "hosts": [{"name": "web01"}, {"name": "web02", "enabled": "no"}, {"name": "web02"},
		    {"name": "db01"}],
		  "groups": [{"name": "web", "related": {"hosts": [{"name": "web01"}, {"name": "web02"}]}}]}}`,
		WantSkipped: `inventory entry "Production": related.hosts entry "web02" was skipped ` +
			"because its enabled field is a string where true or false belongs, and the rest of " +
			"the export imports without it",
		WantKept: []string{"[web]\nweb01\nweb02"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			key := test.Key
			if key == "" {
				key = "inventory"
			}
			export := `{
			  "projects": [{"name": "web", "scm_type": "git", "scm_url": "https://example.com/web.git"}],
			  "` + key + `": [` + test.Inventory + `],
			  "job_templates": [{"name": "Deploy", "playbook": "site.yml", "project": "web",
			    "inventory": "Production"}]}`
			plan, err := FromAWX([]byte(export), time.Now())
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if len(plan.Inventories) != 1 {
				t.Fatalf("inventories = %d, want the inventory kept without its one bad host.\n"+
					"warnings: %v", len(plan.Inventories), plan.Warnings)
			}
			content := plan.Inventories[0].Content
			for _, want := range append([]string{"web01", "db01", "[web]", "env=prod"},
				test.WantKept...) {
				if !strings.Contains(content, want) {
					t.Errorf("the kept inventory lost %q:\n%s", want, content)
				}
			}
			gone := test.WantGone
			if gone == "" {
				gone = "123"
			}
			if strings.Contains(content, gone) {
				t.Errorf("the skipped host %q is in the inventory anyway:\n%s", gone, content)
			}
			if _, ok := warningContaining(t, plan.Warnings, test.WantSkipped); !ok {
				t.Errorf("the skipped host was not named inside its inventory.\nwarnings: %v",
					plan.Warnings)
			}
			if _, ok := warningContaining(t, plan.Warnings, `template "Deploy" references unknown`); ok {
				t.Errorf("the template lost its inventory to one bad host: %v", plan.Warnings)
			}
		})
	}
}

// TestADocumentThatIsNotJSONIsStillRefused pins the other side: leniency is for an asset that does
// not fit, not for a file that cannot be read at all, which is refused as it always was.
func TestADocumentThatIsNotJSONIsStillRefused(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{`{"job_templates": [`, `not json`} {
		if _, err := FromAWX([]byte(doc), time.Now()); err == nil {
			t.Errorf("FromAWX(%q) = nil error, want the unreadable document refused", doc)
		}
	}
}

// TestLooseInt pins the whole-number reader shared by the importers.
func TestLooseInt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantResult looseInt
		Want       bool
		In         string
	}{{ // Test 0: A number.
		In: `5`, WantResult: 5,
	}, { // Test 1: A quoted number.
		In: `"12"`, WantResult: 12,
	}, { // Test 2: A quoted number with spaces.
		In: `" 3 "`, WantResult: 3,
	}, { // Test 3: Null is zero.
		In: `null`,
	}, { // Test 4: An empty string is zero.
		In: `""`,
	}, { // Test 5: A word is a type error, which leaves its asset out.
		In: `"many"`, Want: true,
	}, { // Test 6: A fraction is not a whole number.
		In: `2.5`, Want: true,
	}, { // Test 7: A boolean is not a number.
		In: `true`, Want: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got looseInt
			err := json.Unmarshal([]byte(test.In), &got)
			var typeErr *json.UnmarshalTypeError
			if test.Want != errors.As(err, &typeErr) {
				t.Fatalf("Unmarshal(%s) error = %v, want a type error %t", test.In, err, test.Want)
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAnExportOfOnlyMalformedEntriesSaysWhy pins the case where the lenient decode leaves nothing. It
// was reported as a document nothing in which was recognized, which sends a reader to check the
// format when every entry was recognized and each one had a field of the wrong type.
func TestAnExportOfOnlyMalformedEntriesSaysWhy(t *testing.T) {
	t.Parallel()
	const export = `{"job_templates": [{"name": "Only", "playbook": "site.yml", "forks": "many"}]}`
	plan, err := FromAWX([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("FromAWX() error = %v, want a plan that names the skipped template", err)
	}
	if _, ok := warningContaining(t, plan.Warnings, `job_templates entry "Only" was skipped`); !ok {
		t.Errorf("the skipped template was not named.\nwarnings: %v", plan.Warnings)
	}
}
