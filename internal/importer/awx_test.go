package importer_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
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
		t.Fatalf("templates = %d, want 1; warnings: %s", len(plan.Templates), strings.Join(plan.Warnings, " | "))
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
		// The export is decoded with UseNumber, so a numeric default arrives as json.Number and keeps
		// its exact digits rather than passing through float64. It marshals back to the same JSON.
		{Var: "count", Label: "How many", Type: template.FieldInt, Default: json.Number("1")},
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

	assertWarns(t, plan.Warnings, "Manual", "needs its secret re-entered", "cannot be expressed as cron",
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
		// AWX writes DTSTART and RRULE on one space-separated line, and carries the time of day in
		// DTSTART rather than BYHOUR, so a nightly job must not silently become a midnight job.
		{In: "DTSTART:20260101T020000Z RRULE:FREQ=DAILY;INTERVAL=1", Want: "0 2 * * *", OK: true},                           // Test 7: One-line daily at 2am.
		{In: "DTSTART:20260101T023000Z\nRRULE:FREQ=DAILY", Want: "30 2 * * *", OK: true},                                    // Test 8: Two-line daily at 2.30am.
		{In: "DTSTART;TZID=America/New_York:20260101T093000 RRULE:FREQ=WEEKLY;BYDAY=MO,FR", Want: "30 9 * * 1,5", OK: true}, // Test 9: Zoned weekly keeps its wall time.
		{In: "DTSTART:20260101T000000Z RRULE:FREQ=DAILY", Want: "0 0 * * *", OK: true},                                      // Test 10: Midnight stays midnight.
		{In: "DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYHOUR=5", Want: "0 5 * * *", OK: true},                             // Test 11: An explicit BYHOUR outranks DTSTART.
		{In: "DTSTART:20260101T183000Z RRULE:FREQ=MONTHLY;BYMONTHDAY=15", Want: "30 18 15 * *", OK: true},                   // Test 12: Monthly keeps the DTSTART time.
	}
	for i, test := range tests {
		got, ok := importer.RRULEToCron(test.In)
		if ok != test.OK || got != test.Want {
			t.Errorf("test %d: RRULEToCron(%q) = (%q,%v), want (%q,%v)",
				i, test.In, got, ok, test.Want, test.OK)
		}
	}
}

// TestFromAWXReadsNaturalKeyReferences pins that an export serializing its cross-object references
// as natural-key objects imports, which is how a live AWX actually writes them.
//
// Every reference field decodes through awxRef, which accepts a bare name, an array, or an object.
// The credential type was the one that did not: it was a plain string, so encoding/json failed on
// the whole document and an export taken from a real install imported nothing at all. The fixture
// this package tested against wrote the type as a bare name, which is a shape awxRef also accepts,
// so the gap was invisible until an export from an actual server was tried.
func TestFromAWXReadsNaturalKeyReferences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Type string
	}{{ // Test 0: A natural-key object, which is what a live export writes.
		Name: "object", Type: `{"name": "Machine", "kind": "ssh", "type": "credential_type"}`,
	}, { // Test 1: A bare name, the simplified shape.
		Name: "bare name", Type: `"Machine"`,
	}, { // Test 2: An array natural key, the third shape awxRef accepts.
		Name: "array", Type: `["Default", "Machine"]`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := `{
			  "credentials": [{
			    "name": "prod-ssh",
			    "credential_type": ` + test.Type + `,
			    "inputs": {"username": "deploy", "ssh_key_data": "$encrypted$"}
			  }],
			  "projects": [{
			    "name": "infra", "scm_type": "git",
			    "scm_url": "https://github.com/acme/infra.git", "scm_branch": "main",
			    "organization": {"name": "Default", "type": "organization"}
			  }],
			  "job_templates": [{
			    "name": "deploy", "playbook": "site.yml",
			    "project": {"organization": {"name": "Default"}, "name": "infra", "type": "project"},
			    "credentials": [{"name": "prod-ssh", "type": "credential"}]
			  }]
			}`
			plan, err := importer.FromAWX([]byte(doc), time.Now())
			if err != nil {
				t.Fatalf("FromAWX() error = %v, want the export to import", err)
			}
			if len(plan.Credentials) != 1 {
				t.Fatalf("imported %d credentials, want 1", len(plan.Credentials))
			}
			// The type is what decides the credential kind, so a reference that decodes to an empty
			// name would import the credential under the wrong kind rather than failing loudly.
			if got := plan.Credentials[0].Kind; got != credential.KindSSHKey {
				t.Errorf("credential kind = %q, want %q: the credential type did not survive the "+
					"reference decode", got, credential.KindSSHKey)
			}
			if len(plan.Projects) != 1 || len(plan.Templates) != 1 {
				t.Errorf("imported %d projects and %d templates, want 1 and 1",
					len(plan.Projects), len(plan.Templates))
			}
		})
	}
}

func TestFromAWXCarriesVaultID(t *testing.T) {
	t.Parallel()
	export := `{
		"credentials": [
			{"name": "prod-vault", "credential_type": "Vault", "inputs": {"vault_id": "prod", "vault_password": "$encrypted$"}},
			{"name": "plain-vault", "credential_type": "Vault", "inputs": {"vault_password": "$encrypted$"}},
			{"name": "bad-vault", "credential_type": "Vault", "inputs": {"vault_id": "a b@c", "vault_password": "$encrypted$"}}
		]
	}`
	plan, err := importer.FromAWX([]byte(export), fixedTime)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	byName := map[string]*credential.Credential{}
	for _, c := range plan.Credentials {
		byName[c.Name] = c
	}
	if c := byName["prod-vault"]; c == nil || c.Kind != credential.KindVaultPassword || c.VaultID != "prod" {
		t.Errorf("prod-vault = %+v, want a vault password labeled prod", c)
	}
	// A vault with no label imports unlabeled, the classic single-vault case.
	if c := byName["plain-vault"]; c == nil || c.VaultID != "" {
		t.Errorf("plain-vault = %+v, want no label", c)
	}
	// A label AWX would never have written but a hand-edited export might is dropped rather than
	// carried into a --vault-id argument.
	if c := byName["bad-vault"]; c == nil || c.VaultID != "" {
		t.Errorf("bad-vault = %+v, want the malformed label dropped", c)
	}
}

// TestAWXPasswordSurveyRefused proves an AWX password survey field is not imported as a plaintext
// survey field.
//
// AWX prompts for such a value and stores it obscured. A survey field here is plain text whose
// answer is kept on the run and injected as an extra var, and AWX exports the field's default
// alongside it, so importing one would quietly downgrade a password prompt into a stored plaintext
// value. The Rundeck importer refuses the equivalent secure option, and this must agree with it.
func TestAWXPasswordSurveyRefused(t *testing.T) {
	t.Parallel()
	export := `{"job_templates":[{
		"name":"Rotate keys","playbook":"rotate.yml",
		"survey_spec":{"spec":[
			{"variable":"vault_pass","question_name":"Vault password","type":"password",
			 "required":true,"default":"hunter2"},
			{"variable":"region","question_name":"Region","type":"text"}
		]}
	}]}`
	plan, err := importer.FromAWX([]byte(export), time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1 (warnings %v)", len(plan.Templates), plan.Warnings)
	}
	for _, f := range plan.Templates[0].Survey {
		if f.Var == "vault_pass" {
			t.Fatal("a password survey field was imported, which stores the secret in plain text")
		}
	}
	if len(plan.Templates[0].Survey) != 1 {
		t.Errorf("survey fields = %d, want only the non-secret one", len(plan.Templates[0].Survey))
	}
	// The exported default must not ride along in the plan either.
	for _, f := range plan.Templates[0].Survey {
		if fmt.Sprint(f.Default) == "hunter2" {
			t.Error("the password field's default was imported")
		}
	}
	var told bool
	for _, w := range plan.Warnings {
		if strings.Contains(w, "vault_pass") && strings.Contains(w, "password") {
			told = true
		}
	}
	if !told {
		t.Errorf("refusing the password field was not reported; warnings = %v", plan.Warnings)
	}
}

// TestAWXScheduleKeepsItsTimezone proves an AWX schedule's zone survives the import.
//
// AWX records the zone on the recurrence rule. Dropping it moved every imported maintenance window
// by the offset between that zone and the server's, silently, and the cron expression looked correct
// either way.
func TestAWXScheduleKeepsItsTimezone(t *testing.T) {
	t.Parallel()
	export := `{"job_templates":[{
		"name":"Nightly","playbook":"site.yml",
		"related":{"schedules":[
			{"name":"ny window","enabled":true,
			 "rrule":"DTSTART;TZID=America/New_York:20260101T020000 RRULE:FREQ=DAILY;INTERVAL=1"},
			{"name":"utc window","enabled":true,
			 "rrule":"DTSTART:20260101T030000Z RRULE:FREQ=DAILY;INTERVAL=1"},
			{"name":"floating window","enabled":true,
			 "rrule":"DTSTART:20260101T030000 RRULE:FREQ=DAILY;INTERVAL=1"}
		]}
	}]}`
	plan, err := importer.FromAWX([]byte(export), time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	byName := map[string]string{}
	for _, sc := range plan.Schedules {
		byName[sc.Name] = sc.Timezone
	}
	if got := byName["ny window"]; got != "America/New_York" {
		t.Errorf("timezone = %q, want America/New_York; the window would fire at the wrong hour", got)
	}
	// The Zulu form names UTC. It carries no TZID, which is what AWX wrote before it recorded zones and
	// what it still writes for a UTC schedule, and reading no zone from it imported the window into the
	// server's local time with the UTC hour, firing it hours off.
	if got := byName["utc window"]; got != "UTC" {
		t.Errorf("a Zulu DTSTART got timezone %q, want UTC; the window would fire at the server's "+
			"local hour instead of the UTC one it was written in", got)
	}
	// A floating local time names no zone, which is what it means.
	if got := byName["floating window"]; got != "" {
		t.Errorf("a floating DTSTART got timezone %q, want none", got)
	}
}

// TestAWXReportsUnmappedObjects proves the report names what an export held and the importer does
// not create, rather than staying silent about it.
func TestAWXReportsUnmappedObjects(t *testing.T) {
	t.Parallel()
	export := `{"job_templates":[{"name":"a","playbook":"a.yml"}],
		"workflow_job_templates":[{"name":"deploy chain"},{"name":"nightly chain"}],
		"organizations":[{"name":"acme"}],
		"teams":[{"name":"ops"}],
		"notification_templates":[{"name":"slack"}]}`
	plan, err := importer.FromAWX([]byte(export), time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	joined := strings.Join(plan.Warnings, "\n")
	// Workflows are imported now rather than reported as unmapped, so only the object kinds this
	// importer still does not create are named here.
	for _, want := range []string{
		"1 organization", "1 team", "1 notification template",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not mention %q:\n%s", want, joined)
		}
	}
}

// TestAWXWorkflowImportsAsSteppedTemplate proves an AWX workflow job template becomes a saved
// workflow template whose graph matches the AWX node wiring.
func TestAWXWorkflowImportsAsSteppedTemplate(t *testing.T) {
	t.Parallel()
	export := `{
		"job_templates":[
			{"name":"Build","playbook":"build.yml"},
			{"name":"Test","playbook":"test.yml"},
			{"name":"Deploy","playbook":"deploy.yml"}
		],
		"workflow_job_templates":[{
			"name":"ship it",
			"workflow_nodes":[
				{"id":1,"identifier":"build","unified_job_template":"Build","success_nodes":[2,3]},
				{"id":2,"identifier":"test","unified_job_template":"Test","success_nodes":[]},
				{"id":3,"identifier":"deploy","unified_job_template":"Deploy","success_nodes":[]}
			]
		}]
	}`
	plan, err := importer.FromAWX([]byte(export), time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	var wf *template.Template
	for _, tpl := range plan.Templates {
		if tpl.Name == "ship it" {
			wf = tpl
		}
	}
	if wf == nil {
		t.Fatalf("the workflow was not imported; warnings = %v", plan.Warnings)
	}
	if len(wf.Steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(wf.Steps))
	}
	byName := map[string]run.PipelineStep{}
	for _, s := range wf.Steps {
		byName[s.Name] = s
	}
	if got := byName["build"]; len(got.DependsOn) != 0 || got.Playbook != "build.yml" {
		t.Errorf("build step = %+v, want no dependencies and build.yml", got)
	}
	for _, name := range []string{"test", "deploy"} {
		got := byName[name]
		if len(got.DependsOn) != 1 || got.DependsOn[0] != "build" {
			t.Errorf("%s depends on %v, want [build]", name, got.DependsOn)
		}
	}
	// The imported graph must be one the dispatcher will actually run.
	if err := run.ValidatePipeline(wf.Steps); err != nil {
		t.Errorf("the imported graph does not validate: %v", err)
	}
}

// TestAWXWorkflowRefusedRatherThanPartial proves a workflow this importer cannot express whole is
// skipped and reported, never reduced to a subset.
//
// A partial graph is the dangerous outcome: it carries the workflow's name and runs some of its
// steps, so an operator who migrated would believe their change process moved across when part of it
// silently did not.
func TestAWXWorkflowRefusedRatherThanPartial(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		Name    string
		Export  string
		Because string
	}{{ // A failure edge runs work because something failed, which a pipeline cannot express.
		Name: "failure edge",
		Export: `{"job_templates":[{"name":"A","playbook":"a.yml"},{"name":"B","playbook":"b.yml"}],
			"workflow_job_templates":[{"name":"wf","workflow_nodes":[
				{"id":1,"unified_job_template":"A","failure_nodes":[2]},
				{"id":2,"unified_job_template":"B"}]}]}`,
		Because: "failure",
	}, { // A node pointing at a job template the export does not carry has no work to do.
		Name: "unresolved node",
		Export: `{"job_templates":[{"name":"A","playbook":"a.yml"}],
			"workflow_job_templates":[{"name":"wf","workflow_nodes":[
				{"id":1,"unified_job_template":"A","success_nodes":[2]},
				{"id":2,"unified_job_template":"Ghost"}]}]}`,
		Because: "not a job template",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			plan, err := importer.FromAWX([]byte(test.Export), at)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			for _, tpl := range plan.Templates {
				if tpl.Name == "wf" {
					t.Fatalf("a workflow that cannot be expressed whole was imported anyway: %+v", tpl.Steps)
				}
			}
			joined := strings.Join(plan.Warnings, "\n")
			if !strings.Contains(joined, test.Because) {
				t.Errorf("the refusal does not say why (%q):\n%s", test.Because, joined)
			}
		})
	}
}

// TestAWXImportsARealAwxkitExport pins the shape awxkit actually writes, which is the shape the
// migration guide tells an operator to produce.
//
// awxkit nests an inventory's hosts and groups under its related block. The importer read only the
// top level, so every inventory from a real export arrived empty and the import still reported
// success: the inventory existed, it had no hosts, and nothing said so. The execution settings on a
// job template were dropped the same way, which silently converted a check-mode template limited to
// one canary host into a live template targeting the whole fleet.
func TestAWXImportsARealAwxkitExport(t *testing.T) {
	t.Parallel()
	const export = `{
	  "inventory": [{
	    "name": "Production",
	    "related": {
	      "hosts": [{"name": "web01"}, {"name": "web02"}],
	      "groups": [{"name": "web", "hosts": [{"name": "web01"}]}]
	    }
	  }],
	  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://example.com/infra.git", "scm_branch": "main"}],
	  "job_templates": [{
	    "name": "canary",
	    "playbook": "site.yml",
	    "project": "infra",
	    "inventory": "Production",
	    "limit": "canary-01",
	    "job_tags": "preflight, smoke",
	    "skip_tags": "slow",
	    "verbosity": 2,
	    "forks": 10,
	    "timeout": 600,
	    "job_type": "check",
	    "diff_mode": true
	  }]
	}`

	plan, err := importer.FromAWX([]byte(export), time.Now())
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if len(plan.Inventories) != 1 {
		t.Fatalf("inventories = %d, want 1", len(plan.Inventories))
	}
	content := plan.Inventories[0].Content
	for _, host := range []string{"web01", "web02"} {
		if !strings.Contains(content, host) {
			t.Errorf("inventory imported without %s, so a real export arrives empty:\n%s", host, content)
		}
	}
	if !strings.Contains(content, "[web]") {
		t.Errorf("inventory imported without its group:\n%s", content)
	}

	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	tpl := plan.Templates[0]
	checks := []struct {
		Name string
		Got  any
		Want any
	}{
		{"limit", tpl.Limit, "canary-01"},
		{"verbosity", tpl.Verbosity, 2},
		{"forks", tpl.Forks, 10},
		{"timeout", tpl.Timeout, 600},
		{"diff mode", tpl.DiffMode, true},
		{"check mode becomes dry run", tpl.DryRun, true},
		{"tags", strings.Join(tpl.Tags, ","), "preflight,smoke"},
		{"skip tags", strings.Join(tpl.SkipTags, ","), "slow"},
	}
	for _, c := range checks {
		if c.Got != c.Want {
			t.Errorf("%s = %v, want %v; the import silently changed what this template does",
				c.Name, c.Got, c.Want)
		}
	}
}
