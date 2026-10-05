package importer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// awxAliasExport renders an AWX export whose job templates are given by jobs, each a JSON object
// written without the project and inventory every one shares.
func awxAliasExport(jobs ...string) []byte {
	var rendered []string
	for _, j := range jobs {
		rendered = append(rendered, `{"organization": {"name": "Platform"}, "playbook": "boot.yml", `+
			`"project": {"name": "infra"}, "inventory": {"name": "Fleet"}, `+j+`}`)
	}
	return []byte(`{
  "projects": [{"name": "infra", "organization": {"name": "Platform"}, "scm_type": "git",
                "scm_url": "https://github.com/acme/infra.git"}],
  "inventory": [{"name": "Fleet", "organization": {"name": "Platform"},
                 "hosts": [{"name": "web01", "variables": {"ansible_host": "10.0.0.11"}}]}],
  "job_templates": [` + strings.Join(rendered, ",\n") + `]
}`)
}

// awxTemplateList is AWX's job template list as its API serves a page of it.
const awxTemplateList = `{"count": 4, "next": null, "previous": null, "results": [
  {"id": 42, "name": "provision", "organization": 3,
   "summary_fields": {"organization": {"id": 3, "name": "Platform"}}},
  {"id": "77", "name": "rebuild", "organization": 3,
   "summary_fields": {"organization": {"id": 3, "name": "Platform"}}},
  {"id": 9, "name": "provision", "organization": 4,
   "summary_fields": {"organization": {"id": 4, "name": "Edge"}}},
  {"id": 12, "name": "twice", "organization": 3,
   "summary_fields": {"organization": {"id": 3, "name": "Platform"}}}
]}`

// awxTemplateListPage2 is a second page of the same list, naming one template the first named.
const awxTemplateListPage2 = `[{"id": 13, "name": "twice",
  "summary_fields": {"organization": {"name": "Platform"}}}]`

// FuzzFromAWXWithTemplateIDs feeds arbitrary bytes to the AWX template list reader and the import
// it feeds, to prove a malformed or hostile list never panics.
func FuzzFromAWXWithTemplateIDs(f *testing.F) {
	f.Add([]byte(awxTemplateList), []byte(`{"job_templates":[]}`))
	f.Add([]byte(`[{"id": "x"}]`), []byte(`{`))
	f.Add([]byte(``), []byte(``))
	f.Fuzz(func(_ *testing.T, list, data []byte) {
		mapper, err := FromAWXWithTemplateIDs(list)
		if err != nil {
			return
		}
		_, _ = mapper(data, fuzzNow)
	})
}

// awxApplyStores returns empty stores an AWX plan applies to, sharing templates.
func awxApplyStores(templates template.Store) ApplyStores {
	return ApplyStores{Projects: project.NewMemStore(), Inventories: inventory.NewMemStore(),
		Sources: invsource.NewMemStore(), Credentials: credential.NewMemStore(),
		Templates: templates, Schedules: schedule.NewMemStore(),
		Sealer: credential.NewSealer("pass", "salt")}
}

// TestAWXCallbackTemplatesWithALimitAreNamed pins the import half of decision seventeen. A template
// that accepted callbacks and had a limit in AWX keeps its limit on callbacks here, the default,
// and the report names it, says which setting it got, and how to switch to AWX's behavior. Nothing
// is said for a template with only one of the two.
func TestAWXCallbackTemplatesWithALimitAreNamed(t *testing.T) {
	t.Parallel()
	plan, err := FromAWX(awxAliasExport(
		`"name": "provision", "allow_callbacks": true, "limit": "web:!canary"`,
		`"name": "open", "allow_callbacks": true`,
		`"name": "limited", "limit": "web"`,
	), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	modes := map[string]string{}
	for _, tpl := range plan.Templates {
		modes[tpl.Name] = template.NormalizeCallbackLimit(tpl.CallbackLimit)
	}
	if diff := cmp.Diff(map[string]string{"provision": "intersect", "open": "intersect",
		"limited": "intersect"}, modes); diff != "" {
		t.Errorf("callback limit modes mismatch (-want +got):\n%s", diff)
	}
	var flagged []string
	for _, w := range plan.Warnings {
		if strings.Contains(w, "callback_limit") {
			flagged = append(flagged, w)
		}
	}
	if len(flagged) != 1 {
		t.Fatalf("templates flagged for their callback limit = %q, want provision alone", flagged)
	}
	for _, want := range []string{`template "provision"`, `"web:!canary"`, "callback_limit intersect",
		"set callback_limit to replace"} {
		if !strings.Contains(flagged[0], want) {
			t.Errorf("the flag %q does not say %q", flagged[0], want)
		}
	}
	report := plan.Report()
	for _, left := range report.LeftOut {
		if strings.Contains(left, "callback_limit") {
			t.Errorf("the callback limit flag is counted as something left out: %s", left)
		}
	}
}

// TestAWXImportBindsTheAWXIDsOfCallbackTemplates pins where decision twenty-one's binding comes
// from. A template that accepted callbacks in AWX is bound to its AWX id, from the export when it
// carries one and otherwise from AWX's template list by exact organization and name, and its
// AWX-compatible address is on. A template that did not accept callbacks is never bound, and an id
// the import cannot trust binds nothing while the template itself still comes across.
func TestAWXImportBindsTheAWXIDsOfCallbackTemplates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantBindings map[string]int64
		WantWarning  string
		Lists        []string
		Job          string
	}{{ // Test 0: The export carries the id, as one taken from the API does.
		Job:          `"name": "provision", "allow_callbacks": true, "id": 42`,
		WantBindings: map[string]int64{"provision": 42},
		WantWarning:  "/api/v2/job_templates/42/callback/",
	}, { // Test 1: The id comes from AWX's template list, by organization and name.
		Job: `"name": "provision", "allow_callbacks": true`, Lists: []string{awxTemplateList},
		WantBindings: map[string]int64{"provision": 42},
	}, { // Test 2: A list that writes the id as a string still binds it.
		Job: `"name": "rebuild", "allow_callbacks": true`, Lists: []string{awxTemplateList},
		WantBindings: map[string]int64{"rebuild": 77},
	}, { // Test 3: Without callbacks there is nothing for the address to reach.
		Job: `"name": "provision", "id": 42`, Lists: []string{awxTemplateList},
	}, { // Test 4: No id anywhere, so only the template's own address answers, and the report
		// says how to bind it.
		Job:         `"name": "provision", "allow_callbacks": true`,
		WantWarning: "--awx-template-ids",
	}, { // Test 5: An id that is not a number binds nothing.
		Job:         `"name": "provision", "allow_callbacks": true, "id": "forty-two"`,
		WantWarning: "not a whole number above zero",
	}, { // Test 6: A list naming the same template twice across its pages binds neither id.
		Job:   `"name": "twice", "allow_callbacks": true`,
		Lists: []string{awxTemplateList, awxTemplateListPage2}, WantWarning: "more than once",
	}, { // Test 7: An export and a list that disagree bind nothing until they agree.
		Job:   `"name": "provision", "allow_callbacks": true, "id": 43`,
		Lists: []string{awxTemplateList}, WantWarning: "until the two agree",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			mapper := FromAWX
			if len(test.Lists) > 0 {
				lists := make([][]byte, 0, len(test.Lists))
				for _, l := range test.Lists {
					lists = append(lists, []byte(l))
				}
				var err error
				if mapper, err = FromAWXWithTemplateIDs(lists...); err != nil {
					t.Fatalf("FromAWXWithTemplateIDs() error = %v", err)
				}
			}
			plan, err := mapper(awxAliasExport(test.Job), importNow)
			if err != nil {
				t.Fatalf("map error = %v", err)
			}
			if len(plan.Templates) != 1 {
				t.Fatalf("imported %d templates, want the template whatever its id", len(plan.Templates))
			}
			tpl := plan.Templates[0]
			got := map[string]int64{}
			for _, b := range plan.awxBindings {
				if b.TemplateID != tpl.ID || b.Organization != "Platform" || b.Name != tpl.Name {
					t.Errorf("binding %+v does not name the template and its AWX object", b)
				}
				got[tpl.Name] = b.AWXID
			}
			if diff := cmp.Diff(test.WantBindings, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("bindings mismatch (-want +got):\n%s", diff)
			}
			if tpl.AWXCallback != (len(test.WantBindings) > 0) {
				t.Errorf("AWXCallback = %v, want it on exactly when the template is bound",
					tpl.AWXCallback)
			}
			if test.WantWarning != "" && !strings.Contains(strings.Join(plan.Warnings, "\n"),
				test.WantWarning) {
				t.Errorf("the report does not say %q:\n%s", test.WantWarning,
					strings.Join(plan.Warnings, "\n"))
			}
		})
	}
}

// TestAWXBindingsAreImmutableAcrossImports pins the rest of decision twenty-one's binding. A first
// import binds the id. Importing the same AWX object again after its template was deleted points
// the id at the new template. A different AWX object claiming the id fails the import before
// anything is written, even when the template the id reached is gone, and so does an export giving
// one id to two templates.
func TestAWXBindingsAreImmutableAcrossImports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	templates := template.NewMemStore()
	apply := func(export []byte) (int, error) {
		t.Helper()
		plan, err := FromAWX(export, importNow)
		if err != nil {
			t.Fatalf("FromAWX() error = %v", err)
		}
		return plan.Apply(ctx, awxApplyStores(templates))
	}
	provision := awxAliasExport(`"name": "provision", "allow_callbacks": true, "id": 42`)
	if _, err := apply(provision); err != nil {
		t.Fatalf("first import error = %v", err)
	}
	first, err := templates.AWXBindingFor(ctx, 42)
	if err != nil {
		t.Fatalf("AWXBindingFor() error = %v", err)
	}
	if err := templates.Delete(ctx, first.TemplateID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := apply(provision); err != nil {
		t.Fatalf("re-import of the same AWX object error = %v", err)
	}
	moved, err := templates.AWXBindingFor(ctx, 42)
	if err != nil {
		t.Fatalf("AWXBindingFor() error = %v", err)
	}
	if moved.TemplateID == first.TemplateID {
		t.Errorf("a re-import of the same AWX object left the id on the deleted template %s",
			first.TemplateID)
	}
	if err := templates.Delete(ctx, moved.TemplateID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	tests := []struct {
		Export []byte
	}{{ // Test 0: A different AWX object claims the id of a deleted template.
		Export: awxAliasExport(`"name": "deploy", "allow_callbacks": true, "id": 42`),
	}, { // Test 1: The same name in another organization is a different AWX object.
		Export: []byte(strings.Replace(string(provision), `"organization": {"name": "Platform"}, `+
			`"playbook"`, `"organization": {"name": "Edge"}, "playbook"`, 1)),
	}, { // Test 2: An export giving one id to two templates.
		Export: awxAliasExport(`"name": "a", "allow_callbacks": true, "id": 50`,
			`"name": "b", "allow_callbacks": true, "id": 50`),
	}}
	for testNum, test := range tests {
		before, _ := templates.List(ctx)
		if _, err := apply(test.Export); !errors.Is(err, template.ErrAWXConflict) {
			t.Errorf("test %d: Apply() error = %v, want ErrAWXConflict", testNum, err)
		}
		after, _ := templates.List(ctx)
		if len(after) != len(before) {
			t.Errorf("test %d: a refused import wrote %d templates", testNum, len(after)-len(before))
		}
	}
	still, err := templates.AWXBindingFor(ctx, 42)
	if err != nil || still.TemplateID != moved.TemplateID ||
		!still.SameObject("Platform", "provision") {
		t.Errorf("after refused imports the binding is %+v %v, want it unchanged", still, err)
	}
	if _, err := templates.AWXBindingFor(ctx, 50); !errors.Is(err, template.ErrNotFound) {
		t.Errorf("an export giving one id to two templates bound it: %v", err)
	}
}
