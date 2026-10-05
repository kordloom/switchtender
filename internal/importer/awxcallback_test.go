package importer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// callbackExport is an AWX export with the fact cache and provisioning callback settings on job
// templates: one with a readable key, one whose key the exporting account could not read, and one
// with neither.
const callbackExport = `{
  "projects": [{"name": "infra", "scm_type": "git",
                "scm_url": "https://github.com/acme/infra.git"}],
  "inventory": [{"name": "Fleet", "hosts": [
    {"name": "web01", "variables": {"ansible_host": "10.0.0.11"}},
    {"name": "web02"}
  ]}],
  "job_templates": [
    {"name": "Boot", "playbook": "boot.yml", "project": {"name": "infra"},
     "inventory": {"name": "Fleet"}, "use_fact_cache": true, "allow_callbacks": true,
     "host_config_key": "awx-callback-key-1234"},
    {"name": "Boot hidden key", "playbook": "boot.yml", "project": {"name": "infra"},
     "inventory": {"name": "Fleet"}, "allow_callbacks": true, "host_config_key": "$encrypted$"},
    {"name": "Plain", "playbook": "site.yml", "project": {"name": "infra"},
     "inventory": {"name": "Fleet"}, "use_fact_cache": false, "allow_callbacks": false,
     "host_config_key": ""}
  ]
}`

// TestAWXImportCarriesFactCacheAndCallbacks pins that AWX's use_fact_cache, allow_callbacks, and
// host_config_key now come across instead of being reported as fields nobody reads. The key never
// appears in the plan's report, it is sealed with the install's key when the install has one, and
// without one the template arrives with callbacks on and no key, which the report says.
func TestAWXImportCarriesFactCacheAndCallbacks(t *testing.T) {
	t.Parallel()
	plan, err := FromAWX([]byte(callbackExport), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	type shape struct {
		UseFactCache   bool
		AllowCallbacks bool
	}
	got := map[string]shape{}
	for _, tpl := range plan.Templates {
		got[tpl.Name] = shape{UseFactCache: tpl.UseFactCache, AllowCallbacks: tpl.AllowCallbacks}
		if tpl.HostConfigKey != "" {
			t.Errorf("template %q carries a key before Apply: %q", tpl.Name, tpl.HostConfigKey)
		}
	}
	want := map[string]shape{
		"Boot":            {UseFactCache: true, AllowCallbacks: true},
		"Boot hidden key": {AllowCallbacks: true},
		"Plain":           {},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("imported settings mismatch (-want +got):\n%s", diff)
	}
	report := strings.Join(plan.Warnings, "\n")
	for _, field := range []string{"use_fact_cache", "allow_callbacks", "host_config_key"} {
		if strings.Contains(report, field) {
			t.Errorf("the report still names %s as a field nobody reads:\n%s", field, report)
		}
	}
	if strings.Contains(report, "awx-callback-key-1234") {
		t.Errorf("the report discloses the callback key:\n%s", report)
	}
	for _, want := range []string{`template "Boot" accepts provisioning callbacks`,
		`template "Boot hidden key" accepts provisioning callbacks in AWX, and the export ` +
			`carries no host config key`} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not say %q:\n%s", want, report)
		}
	}
	// The imported inventory has to read back as a host list, or no callback could ever match.
	hosts, err := inventory.Hosts(plan.Inventories[0].Content)
	if err != nil {
		t.Fatalf("Hosts() error = %v", err)
	}
	var addrs []string
	for _, h := range hosts {
		addrs = append(addrs, h.Name+"="+h.Address())
	}
	if diff := cmp.Diff([]string{"web01=10.0.0.11", "web02=web02"}, addrs); diff != "" {
		t.Errorf("imported inventory host list mismatch (-want +got):\n%s", diff)
	}

	tests := []struct {
		Sealer  *credential.Sealer
		WantKey bool
	}{
		{Sealer: credential.NewSealer("pass", "salt"), WantKey: true}, // Test 0: The key is sealed.
		{Sealer: credential.NewSealer("", "")},                        // Test 1: No key to seal with.
		{},                                                            // Test 2: No sealer at all.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte(callbackExport), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			templates := template.NewMemStore()
			if _, err := plan.Apply(context.Background(), ApplyStores{
				Projects: project.NewMemStore(), Inventories: inventory.NewMemStore(),
				Sources: invsource.NewMemStore(), Credentials: credential.NewMemStore(),
				Templates: templates, Schedules: schedule.NewMemStore(), Sealer: test.Sealer,
			}); err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			list, err := templates.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, tpl := range list {
				switch {
				case tpl.Name != "Boot":
					if tpl.HostConfigKey != "" {
						t.Errorf("template %q has a key it was never given", tpl.Name)
					}
				case !test.WantKey:
					if tpl.HostConfigKey != "" {
						t.Errorf("with no encryption key, %q stored a key: %q", tpl.Name, tpl.HostConfigKey)
					}
				default:
					if tpl.HostConfigKey == "awx-callback-key-1234" {
						t.Fatalf("the callback key was stored in the clear")
					}
					opened, err := test.Sealer.Open(tpl.HostConfigKey)
					if err != nil || opened != "awx-callback-key-1234" {
						t.Errorf("sealed key opens to %q, %v, want the AWX key", opened, err)
					}
				}
			}
		})
	}
}
