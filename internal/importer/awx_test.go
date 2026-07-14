package importer_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/railwarden/internal/credential"
	"github.com/dcadolph/railwarden/internal/importer"
	"github.com/dcadolph/railwarden/internal/invsource"
	"github.com/dcadolph/railwarden/internal/template"
)

// fixedTime is a deterministic timestamp for import mapping tests.
var fixedTime = time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

func TestFromAWX(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/awx-export.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	plan, err := importer.FromAWX(data, fixedTime)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}

	// One git project imports; the manual project is skipped with a warning.
	if len(plan.Projects) != 1 || plan.Projects[0].Name != "Web" {
		t.Fatalf("projects = %+v, want one named Web", plan.Projects)
	}
	if plan.Projects[0].RepoURL != "https://github.com/acme/web.git" || plan.Projects[0].Branch != "main" {
		t.Errorf("project fields = %+v, want the git remote and branch", plan.Projects[0])
	}
	if !plan.Projects[0].InstallDeps {
		t.Error("imported project should default to installing dependencies")
	}

	// The inventory becomes a stored inventory whose INI names every host and group. Two more
	// inventories back the two dynamic sources.
	if len(plan.Inventories) != 3 {
		t.Fatalf("inventories = %d, want 3 (one static, two dynamic backings)", len(plan.Inventories))
	}
	ini := plan.Inventories[0].Content
	for _, want := range []string{"web1.acme.internal ansible_user=deploy", "[web]", "web2.acme.internal", "[db]", "db1.acme.internal"} {
		if !strings.Contains(ini, want) {
			t.Errorf("inventory INI missing %q:\n%s", want, ini)
		}
	}

	// Four credential shells, each without a secret, each flagged for re-entry. The GitHub token type
	// maps to the token kind.
	if len(plan.Credentials) != 4 {
		t.Fatalf("credentials = %d, want 4", len(plan.Credentials))
	}
	tokenSeen := false
	for _, c := range plan.Credentials {
		if c.Secret != "" {
			t.Errorf("credential %q carried a secret, want a shell", c.Name)
		}
		if c.Name == "gh-token" {
			tokenSeen = true
			if c.Kind != credential.KindToken {
				t.Errorf("gh-token kind = %q, want %q", c.Kind, credential.KindToken)
			}
		}
	}
	if !tokenSeen {
		t.Error("expected the gh-token credential in the plan")
	}

	// Two dynamic sources import: a file source keeps its path, a cloud plugin keeps its plugin name
	// and is flagged. Each gets a backing inventory and wires its project and credential by id.
	if len(plan.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(plan.Sources))
	}
	byName := map[string]*invsource.Source{}
	for _, s := range plan.Sources {
		byName[s.Name] = s
	}
	awsCred := plan.Credentials[2].ID // aws-keys is the third credential.
	cloud := byName["Cloud hosts"]
	if cloud == nil || cloud.Source != "inventories/prod.aws_ec2.yml" {
		t.Fatalf("cloud source = %+v, want the source path kept", cloud)
	}
	if cloud.ProjectID != plan.Projects[0].ID {
		t.Errorf("cloud source project id = %q, want %q", cloud.ProjectID, plan.Projects[0].ID)
	}
	if cloud.CredentialID != awsCred {
		t.Errorf("cloud source credential id = %q, want %q", cloud.CredentialID, awsCred)
	}
	if !backingInventoryExists(plan, cloud.InventoryID, "Cloud hosts (dynamic)") {
		t.Errorf("cloud source backing inventory %q not found", cloud.InventoryID)
	}
	legacy := byName["Legacy EC2"]
	if legacy == nil || legacy.Source != "ec2" {
		t.Fatalf("legacy source = %+v, want the plugin name kept", legacy)
	}
	if legacy.CredentialID != awsCred {
		t.Errorf("legacy source credential id = %q, want %q", legacy.CredentialID, awsCred)
	}
	if legacy.ProjectID != "" {
		t.Errorf("legacy source project id = %q, want empty", legacy.ProjectID)
	}
	if !backingInventoryExists(plan, legacy.InventoryID, "Legacy EC2 (dynamic)") {
		t.Errorf("legacy source backing inventory %q not found", legacy.InventoryID)
	}

	// One template, wired to the project, inventory, and credentials by id.
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	tpl := plan.Templates[0]
	if tpl.Name != "Deploy Web" || tpl.Playbook != "site.yml" {
		t.Errorf("template basics = %+v", tpl)
	}
	if tpl.ProjectID != plan.Projects[0].ID {
		t.Errorf("template project id = %q, want %q", tpl.ProjectID, plan.Projects[0].ID)
	}
	if tpl.InventoryID != plan.Inventories[0].ID {
		t.Errorf("template inventory id = %q, want %q", tpl.InventoryID, plan.Inventories[0].ID)
	}
	if tpl.Shards != 2 {
		t.Errorf("template shards = %d, want 2 from job_slice_count", tpl.Shards)
	}
	if len(tpl.CredentialIDs) != 2 {
		t.Errorf("template credential ids = %v, want 2", tpl.CredentialIDs)
	}
	if got := tpl.ExtraVars["release"]; got != "stable" {
		t.Errorf("extra var release = %v, want stable", got)
	}

	// The survey maps field for field, including the type translations.
	wantSurvey := []template.SurveyField{
		{Var: "version", Label: "Release version", Type: template.FieldText, Required: true, Default: "1.0.0"},
		{Var: "count", Label: "How many", Type: template.FieldInt, Default: float64(1)},
		{Var: "region", Label: "Region", Type: template.FieldChoice, Required: true, Choices: []string{"us-east", "us-west", "eu"}},
	}
	if diff := cmp.Diff(wantSurvey, tpl.Survey); diff != "" {
		t.Errorf("survey mismatch (-want +got):\n%s", diff)
	}

	// The daily schedule converts to cron; the every-three-days one is refused with a warning.
	if len(plan.Schedules) != 1 {
		t.Fatalf("schedules = %d, want 1 (the convertible one)", len(plan.Schedules))
	}
	sch := plan.Schedules[0]
	if sch.Cron != "30 2 * * *" {
		t.Errorf("schedule cron = %q, want 30 2 * * *", sch.Cron)
	}
	if sch.TemplateID != tpl.ID {
		t.Errorf("schedule template id = %q, want %q", sch.TemplateID, tpl.ID)
	}

	assertWarns(t, plan.Warnings, "Manual", "needs its secret re-entered", "cannot express",
		"point it at a plugin config file")
}

// backingInventoryExists reports whether the plan holds a backing inventory with the given id and
// name and the empty JSON content a fresh dynamic inventory starts with.
func backingInventoryExists(plan *importer.Plan, id, name string) bool {
	for _, inv := range plan.Inventories {
		if inv.ID == id && inv.Name == name && inv.Content == "{}" {
			return true
		}
	}
	return false
}

// assertWarns fails the test unless every needle appears in some warning.
func assertWarns(t *testing.T, warnings []string, needles ...string) {
	t.Helper()
	joined := strings.Join(warnings, "\n")
	for _, needle := range needles {
		if !strings.Contains(joined, needle) {
			t.Errorf("warnings missing %q; got:\n%s", needle, joined)
		}
	}
}

func TestRRULEConversions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want string
		OK   bool
	}{
		{In: "RRULE:FREQ=DAILY;BYHOUR=3;BYMINUTE=15", Want: "15 3 * * *", OK: true},        // Test 0: Daily at a time.
		{In: "RRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;BYHOUR=6", Want: "0 6 * * 1,3,5", OK: true}, // Test 1: Weekly on days.
		{In: "RRULE:FREQ=HOURLY;INTERVAL=4", Want: "0 */4 * * *", OK: true},                // Test 2: Every four hours.
		{In: "RRULE:FREQ=MINUTELY;INTERVAL=30", Want: "*/30 * * * *", OK: true},            // Test 3: Every 30 minutes.
		{In: "RRULE:FREQ=MONTHLY;BYMONTHDAY=1;BYHOUR=0", Want: "0 0 1 * *", OK: true},      // Test 4: Monthly on the first.
		{In: "RRULE:FREQ=DAILY;INTERVAL=3", Want: "", OK: false},                           // Test 5: Every three days is unmappable.
		{In: "RRULE:FREQ=YEARLY", Want: "", OK: false},                                     // Test 6: Yearly is unmappable.
	}
	for i, test := range tests {
		got, ok := importer.RRULEToCron(test.In)
		if ok != test.OK || got != test.Want {
			t.Errorf("test %d: RRULEToCron(%q) = (%q,%v), want (%q,%v)",
				i, test.In, got, ok, test.Want, test.OK)
		}
	}
}
