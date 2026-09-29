package importer

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
)

// TestSemaphoreRefusesADocumentItCannotRead pins that malformed JSON is an error and a document that
// parses but holds nothing is a refusal rather than a plan of zeros. An operator who exported the
// wrong thing must not be told their migration succeeded.
func TestSemaphoreRefusesADocumentItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Doc         string
		WantNothing bool
	}{
		{Name: "empty", Doc: ""},                             // Test 0.
		{Name: "truncated", Doc: `{"projects": [`},           // Test 1.
		{Name: "wrong field type", Doc: `{"projects": 5}`},   // Test 2.
		{Name: "empty object", Doc: `{}`, WantNothing: true}, // Test 3.
		{Name: "json null", Doc: `null`, WantNothing: true},  // Test 4.
		{Name: "meta only", Doc: `{"meta": {"name": "ops"}}`,
			WantNothing: true}, // Test 5: a project name alone is not an asset.
		{Name: "empty project list", Doc: `{"projects": []}`, WantNothing: true}, // Test 6.
		{Name: "project with nothing in it", Doc: `{"projects": [{"name": "ops"}]}`,
			WantNothing: true}, // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, err := FromSemaphore([]byte(test.Doc), importNow)
			if err == nil {
				t.Fatalf("FromSemaphore(%q) error = nil, want a refusal", test.Doc)
			}
			if got := errors.Is(err, ErrNothingRecognized); got != test.WantNothing {
				t.Errorf("FromSemaphore(%q) errors.Is(ErrNothingRecognized) = %v, want %v (err %v)",
					test.Doc, got, test.WantNothing, err)
			}
		})
	}
}

// TestSemaphoreReadsBothBackupShapes pins that the flat single-project backup Semaphore itself
// writes imports, alongside the multi-project wrapper. Only the wrapper was read once, so a real
// backup matched nothing and the import told the operator their export held nothing recognizable.
func TestSemaphoreReadsBothBackupShapes(t *testing.T) {
	t.Parallel()
	const assets = `"repositories": [{"name": "infra", "git_url": "https://git.example/i.git",
        "git_branch": "main"}],
      "inventories": [{"name": "prod", "type": "static", "inventory": "web01"}],
      "keys": [{"name": "deploy", "type": "ssh"}],
      "templates": [{"name": "site", "playbook": "site.yml", "repository": "infra",
        "inventory": "prod"}],
      "schedules": [{"name": "nightly", "cron_format": "0 2 * * *", "template": "site"}]`
	tests := []struct {
		Name        string
		Doc         string
		WantProject string
	}{
		{Name: "flat backup", Doc: `{"meta": {"name": "ops"}, ` + assets + `}`,
			WantProject: "ops/infra"}, // Test 0.
		{Name: "flat without meta", Doc: `{` + assets + `}`,
			WantProject: "/infra"}, // Test 1: no project name to qualify with.
		{Name: "wrapper", Doc: `{"projects": [{"name": "ops", ` + assets + `}]}`,
			WantProject: "ops/infra"}, // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, err := FromSemaphore([]byte(test.Doc), importNow)
			if err != nil {
				t.Fatalf("FromSemaphore() error = %v", err)
			}
			if len(plan.Projects) != 1 || plan.Projects[0].Name != test.WantProject {
				t.Fatalf("projects = %+v, want one named %q", plan.Projects, test.WantProject)
			}
			if len(plan.Inventories) != 1 || len(plan.Credentials) != 1 ||
				len(plan.Templates) != 1 || len(plan.Schedules) != 1 {
				t.Fatalf("counts = %d inventories, %d credentials, %d templates, %d schedules; "+
					"want 1 of each", len(plan.Inventories), len(plan.Credentials),
					len(plan.Templates), len(plan.Schedules))
			}
			tpl := plan.Templates[0]
			if tpl.ProjectID != plan.Projects[0].ID {
				t.Errorf("template project = %q, want %q", tpl.ProjectID, plan.Projects[0].ID)
			}
			if tpl.InventoryID != plan.Inventories[0].ID {
				t.Errorf("template inventory = %q, want %q", tpl.InventoryID, plan.Inventories[0].ID)
			}
			sc := plan.Schedules[0]
			if sc.TemplateID != tpl.ID {
				t.Errorf("schedule template = %q, want %q", sc.TemplateID, tpl.ID)
			}
			if sc.NextRunAt == nil {
				t.Error("schedule has no next-run time, so it would never fire")
			}
		})
	}
}

// TestSemaphoreWrapperWinsOverTheFlatFields pins the precedence in projects(). A document carrying
// both shapes must not import its assets twice, which would double every object an operator sees.
func TestSemaphoreWrapperWinsOverTheFlatFields(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [{"name": "wrapped",
        "repositories": [{"name": "a", "git_url": "https://git.example/a.git"}]}],
      "meta": {"name": "flat"},
      "repositories": [{"name": "b", "git_url": "https://git.example/b.git"}]
    }`
	plan, err := FromSemaphore([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	var names []string
	for _, p := range plan.Projects {
		names = append(names, p.Name)
	}
	if diff := cmp.Diff([]string{"wrapped/a"}, names, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("projects mismatch (-want +got):\n%s", diff)
	}
}

// TestSemaphoreRefusesARepositoryTheAPIWouldRefuse pins that the repository URL check runs here too,
// so an export cannot store a project the API itself would have turned down.
func TestSemaphoreRefusesARepositoryTheAPIWouldRefuse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		GitURL       string
		WantImported bool
	}{
		{Name: "https", GitURL: "https://git.example/i.git", WantImported: true}, // Test 0.
		{Name: "empty", GitURL: ""},                                               // Test 1.
		{Name: "cleartext http", GitURL: "http://git.example/i.git"},              // Test 2.
		{Name: "embedded credentials", GitURL: "https://u:p@git.example/i.git"},   // Test 3.
		{Name: "metadata host", GitURL: "https://metadata.google.internal/i.git"}, // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"meta": {"name": "ops"},
				"repositories": [{"name": "infra", "git_url": %q}],
				"inventories": [{"name": "prod", "inventory": "web01"}]}`, test.GitURL)
			plan, err := FromSemaphore([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromSemaphore() error = %v", err)
			}
			if got := len(plan.Projects); (got == 1) != test.WantImported {
				t.Fatalf("projects = %d, want imported = %v", got, test.WantImported)
			}
			if test.WantImported {
				return
			}
			if _, ok := warningContaining(t, plan.Warnings, `repository "infra"`, "skipped"); !ok {
				t.Errorf("a skipped repository was not reported.\nwarnings: %v", plan.Warnings)
			}
		})
	}
}

// TestSemaphoreNonStaticInventoryIsReportedAndKept pins that only a static inventory carries inline
// content, and that a type this cannot import is named rather than dropped. An inventory that
// arrives empty is one that targets nothing, which the report has to say once.
func TestSemaphoreNonStaticInventoryIsReportedAndKept(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name          string
		Type          string
		Content       string
		WantTypeWarn  bool
		WantEmptyWarn bool
	}{
		{Name: "static with content", Type: "static", Content: "web01"}, // Test 0.
		{Name: "unset type with content", Type: "", Content: "web01"},   // Test 1.
		{Name: "static but empty", Type: "static", Content: "",
			WantEmptyWarn: true}, // Test 2.
		{Name: "static but blank", Type: "static", Content: "   \n  ",
			WantEmptyWarn: true}, // Test 3.
		{Name: "file type", Type: "file", Content: "",
			WantTypeWarn: true}, // Test 4: reported once as the wrong type, not twice.
		{Name: "unknown type", Type: "terraform_workspace", Content: "",
			WantTypeWarn: true}, // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"meta": {"name": "ops"},
				"inventories": [{"name": "prod", "type": %q, "inventory": %q}]}`,
				test.Type, test.Content)
			plan, err := FromSemaphore([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromSemaphore() error = %v", err)
			}
			if len(plan.Inventories) != 1 {
				t.Fatalf("inventories = %d, want 1: an inventory always imports",
					len(plan.Inventories))
			}
			if got := plan.Inventories[0].Content; got != test.Content {
				t.Errorf("content = %q, want it carried verbatim %q", got, test.Content)
			}
			_, typeWarn := warningContaining(t, plan.Warnings, "only static content imports")
			if typeWarn != test.WantTypeWarn {
				t.Errorf("type warning = %v, want %v.\nwarnings: %v",
					typeWarn, test.WantTypeWarn, plan.Warnings)
			}
			_, emptyWarn := warningContaining(t, plan.Warnings, "imported with no content")
			if emptyWarn != test.WantEmptyWarn {
				t.Errorf("empty warning = %v, want %v.\nwarnings: %v",
					emptyWarn, test.WantEmptyWarn, plan.Warnings)
			}
		})
	}
}

// TestSemaphoreKeyKindsAndTheirWarnings pins that every key imports as a credential shell whose
// secret must be re-entered, and that a type with no exact equivalent says so. A key that maps to
// the wrong kind is one the injector will not materialize as the operator expects.
func TestSemaphoreKeyKindsAndTheirWarnings(t *testing.T) {
	t.Parallel()
	const doc = `{"meta": {"name": "ops"}, "keys": [
      {"name": "ssh key", "type": "ssh"},
      {"name": "login", "type": "login_password"},
      {"name": "nothing", "type": "none"},
      {"name": "untyped", "type": ""}
    ]}`
	plan, err := FromSemaphore([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	wantKinds := []credential.Kind{credential.KindSSHKey, credential.KindEnv,
		credential.KindEnv, credential.KindEnv}
	for i, cred := range plan.Credentials {
		if cred.Kind != wantKinds[i] {
			t.Errorf("key %q kind = %q, want %q", cred.Name, cred.Kind, wantKinds[i])
		}
		if _, ok := warningContaining(t, plan.Warnings, fmt.Sprintf("key %q", cred.Name),
			"needs its secret re-entered"); !ok {
			t.Errorf("key %q was not reported as needing its secret.\nwarnings: %v",
				cred.Name, plan.Warnings)
		}
	}
	for _, name := range []string{"nothing", "untyped"} {
		if _, ok := warningContaining(t, plan.Warnings, fmt.Sprintf("key %q", name),
			"verify it is correct"); !ok {
			t.Errorf("the inexact mapping for %q was not reported.\nwarnings: %v",
				name, plan.Warnings)
		}
	}
}

// TestSemaphoreUnresolvedReferencesAreReported pins that a template naming a repository or inventory
// this export does not hold still imports, with the missing reference named. A template silently
// wired to nothing is one that fails at launch with no clue why.
func TestSemaphoreUnresolvedReferencesAreReported(t *testing.T) {
	t.Parallel()
	const doc = `{"meta": {"name": "ops"},
      "templates": [{"name": "site", "playbook": "site.yml", "repository": "gone",
        "inventory": "also gone"}],
      "schedules": [{"name": "nightly", "cron_format": "0 2 * * *", "template": "missing"}]
    }`
	plan, err := FromSemaphore([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	tpl := plan.Templates[0]
	if tpl.ProjectID != "" || tpl.InventoryID != "" {
		t.Errorf("template wired to %q/%q, want neither resolved", tpl.ProjectID, tpl.InventoryID)
	}
	for _, want := range []string{`unknown repository "gone"`, `unknown inventory "also gone"`,
		`unknown template "missing"`} {
		if _, ok := warningContaining(t, plan.Warnings, want); !ok {
			t.Errorf("missing %q.\nwarnings: %v", want, plan.Warnings)
		}
	}
	if len(plan.Schedules) != 0 {
		t.Errorf("schedules = %d, want 0: a schedule with no template must not be created",
			len(plan.Schedules))
	}
}

// TestSemaphoreCronIsValidatedBeforeItBecomesARow pins that Semaphore's cron format is held to the
// same check as any other. An unparseable expression stored as a row makes the scheduler log an
// error on every tick forever, and a valid one that never comes due is read as due every tick.
func TestSemaphoreCronIsValidatedBeforeItBecomesARow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Cron         string
		WantImported bool
	}{
		{Name: "ordinary", Cron: "0 2 * * *", WantImported: true}, // Test 0.
		{Name: "step", Cron: "*/15 * * * *", WantImported: true},  // Test 1.
		{Name: "descriptor", Cron: "@daily", WantImported: true},  // Test 2.
		{Name: "empty", Cron: ""},                                 // Test 3.
		{Name: "too few fields", Cron: "0 2 * *"},                 // Test 4.
		{Name: "out of range", Cron: "99 2 * * *"},                // Test 5.
		{Name: "never comes due", Cron: "0 0 30 2 *"},             // Test 6: February the
		// thirtieth parses and never fires, which the scheduler reads as due on every tick.
		{Name: "nonsense", Cron: "not a cron"}, // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"meta": {"name": "ops"},
				"templates": [{"name": "site", "playbook": "site.yml"}],
				"schedules": [{"name": "nightly", "cron_format": %q, "template": "site"}]}`,
				test.Cron)
			plan, err := FromSemaphore([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromSemaphore() error = %v", err)
			}
			if got := len(plan.Schedules) == 1; got != test.WantImported {
				t.Fatalf("cron %q imported = %v, want %v.\nwarnings: %v",
					test.Cron, got, test.WantImported, plan.Warnings)
			}
			if test.WantImported {
				if plan.Schedules[0].NextRunAt == nil {
					t.Error("an imported schedule has no next-run time, so it would never fire")
				}
				return
			}
			if _, ok := warningContaining(t, plan.Warnings, `schedule "nightly"`,
				"was not imported"); !ok {
				t.Errorf("a refused cron was not reported.\nwarnings: %v", plan.Warnings)
			}
		})
	}
}

// TestSemaphoreSecretVariableIsNeverDowngradedToPlainText pins the refusal. Semaphore stores a
// secret variable obscured, while a survey answer is kept in the clear on every run of the template,
// in its record, its exports, and the evidence drawn from it. This importer once did the downgrade
// silently.
func TestSemaphoreSecretVariableIsNeverDowngradedToPlainText(t *testing.T) {
	t.Parallel()
	const doc = `{"meta": {"name": "ops"}, "templates": [{"name": "site", "playbook": "site.yml",
      "survey_vars": [
        {"name": "token", "type": "secret", "title": "API token"},
        {"name": "shout", "type": "SECRET"},
        {"name": "env", "type": "enum", "title": "Environment", "required": true,
          "values": ["prod", "stage"]},
        {"name": "count", "type": "int"},
        {"name": "note", "type": "string"}
      ]}]}`
	plan, err := FromSemaphore([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	survey := plan.Templates[0].Survey
	var vars []string
	for _, f := range survey {
		vars = append(vars, f.Var)
	}
	if diff := cmp.Diff([]string{"env", "count", "note"}, vars, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("survey variables mismatch (-want +got):\n%s", diff)
	}
	for _, name := range []string{"token", "shout"} {
		if _, ok := warningContaining(t, plan.Warnings, fmt.Sprintf("variable %q", name),
			"NOT imported"); !ok {
			t.Errorf("the secret variable %q was not named.\nwarnings: %v", name, plan.Warnings)
		}
	}
	if survey[0].Type != "choice" || survey[0].Label != "Environment" || !survey[0].Required {
		t.Errorf("enum variable = %+v, want a required choice carrying its title", survey[0])
	}
	if diff := cmp.Diff([]string{"prod", "stage"}, survey[0].Choices,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("enum choices mismatch (-want +got):\n%s", diff)
	}
	if survey[1].Type != "int" || survey[2].Type != "text" {
		t.Errorf("types = %q and %q, want int and text", survey[1].Type, survey[2].Type)
	}
}

// TestSemaphoreImportsSeveralProjectsWithoutCollidingNames pins that the same repository name in two
// projects produces two distinct imported project names, and that each project's templates resolve
// against their own project's assets rather than the other's.
func TestSemaphoreImportsSeveralProjectsWithoutCollidingNames(t *testing.T) {
	t.Parallel()
	const doc = `{"projects": [
      {"name": "alpha",
        "repositories": [{"name": "infra", "git_url": "https://git.example/alpha.git"}],
        "templates": [{"name": "site", "playbook": "site.yml", "repository": "infra"}]},
      {"name": "beta",
        "repositories": [{"name": "infra", "git_url": "https://git.example/beta.git"}],
        "templates": [{"name": "site", "playbook": "site.yml", "repository": "infra"}]}
    ]}`
	plan, err := FromSemaphore([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	var names []string
	for _, p := range plan.Projects {
		names = append(names, p.Name)
	}
	if diff := cmp.Diff([]string{"alpha/infra", "beta/infra"}, names,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("project names mismatch (-want +got):\n%s", diff)
	}
	if len(plan.Templates) != 2 {
		t.Fatalf("templates = %d, want 2", len(plan.Templates))
	}
	if plan.Templates[0].ProjectID != plan.Projects[0].ID {
		t.Errorf("the first template resolved to %q, want its own project %q",
			plan.Templates[0].ProjectID, plan.Projects[0].ID)
	}
	if plan.Templates[1].ProjectID != plan.Projects[1].ID {
		t.Errorf("the second template resolved to %q, want its own project %q",
			plan.Templates[1].ProjectID, plan.Projects[1].ID)
	}
}

// TestSemaphoreKeepsUnicodeAndInventoryContentVerbatim pins that a Semaphore inventory's inline text
// is stored exactly as written. Semaphore already holds a real inventory file, so rewriting it would
// change which machines a play reaches for no reason.
func TestSemaphoreKeepsUnicodeAndInventoryContentVerbatim(t *testing.T) {
	t.Parallel()
	const content = "[生产]\nwéb01 ansible_user=deploy\n\n[生产:vars]\nnote=\"with space\"\n"
	doc := fmt.Sprintf(`{"meta": {"name": "運用"},
      "inventories": [{"name": "生产", "type": "static", "inventory": %q}]}`, content)
	plan, err := FromSemaphore([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	inv := plan.Inventories[0]
	if inv.Name != "生产" {
		t.Errorf("name = %q, want %q", inv.Name, "生产")
	}
	if diff := cmp.Diff(content, inv.Content); diff != "" {
		t.Errorf("content was not carried verbatim (-want +got):\n%s", diff)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "no content") {
			t.Errorf("an inventory with content tripped the empty warning: %s", w)
		}
	}
}
