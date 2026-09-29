package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
)

// importNow is the fixed clock every mapping test stamps its objects with, so a plan is comparable
// between runs.
var importNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// warningContaining returns the first warning holding every fragment, and whether one was found. It
// keeps the refusal assertions readable without matching an entire sentence that may be reworded.
func warningContaining(t *testing.T, warnings []string, fragments ...string) (string, bool) {
	t.Helper()
	for _, w := range warnings {
		all := true
		for _, f := range fragments {
			if !strings.Contains(w, f) {
				all = false
				break
			}
		}
		if all {
			return w, true
		}
	}
	return "", false
}

// TestAWXRefusesADocumentItCannotParse pins that malformed JSON is an error rather than an empty
// plan. An import that answers "nothing recognized" to a truncated file would send the operator
// hunting for the wrong problem.
func TestAWXRefusesADocumentItCannotParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
	}{
		{Name: "empty", In: ""},                                      // Test 0.
		{Name: "truncated", In: `{"projects": [`},                    // Test 1.
		{Name: "not json", In: "projects: []"},                       // Test 2.
		{Name: "array root", In: `[{"name":"x"}]`},                   // Test 3.
		{Name: "wrong field type", In: `{"projects": "not a list"}`}, // Test 4.
		{Name: "nul bytes", In: "\x00\x00\x00"},                      // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte(test.In), importNow)
			if err == nil {
				t.Fatalf("FromAWX(%q) error = nil, want a parse failure; plan = %+v", test.In, plan)
			}
			if plan != nil {
				t.Errorf("FromAWX(%q) returned a plan alongside an error", test.In)
			}
		})
	}
}

// TestAWXRefusesADocumentThatRecognizesNothing pins the refusal that keeps an import from looking
// complete when it read nothing. A summary of zeros and exit status zero told an operator who
// exported from the wrong endpoint that their migration had succeeded.
func TestAWXRefusesADocumentThatRecognizesNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
	}{
		{Name: "empty object", In: `{}`},                                             // Test 0.
		{Name: "json null", In: `null`},                                              // Test 1.
		{Name: "empty lists", In: `{"projects": [], "inventory": []}`},               // Test 2.
		{Name: "only unknown keys", In: `{"unrelated": [{"a": 1}]}`},                 // Test 3.
		{Name: "only organizations", In: `{"organizations": [{"name": "Default"}]}`}, // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, err := FromAWX([]byte(test.In), importNow)
			if !errors.Is(err, ErrNothingRecognized) {
				t.Fatalf("FromAWX(%q) error = %v, want ErrNothingRecognized", test.In, err)
			}
		})
	}
}

// TestAWXRefusesAProjectTheAPIWouldRefuse pins that the repository URL check the API applies on
// create is applied here too. Skipping it let an export create stored projects the API itself would
// have refused, which then failed at clone time with an error about the repository rather than the
// import that made them.
func TestAWXRefusesAProjectTheAPIWouldRefuse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		ScmType      string
		ScmURL       string
		WantImported bool
	}{
		{Name: "git https", ScmType: "git", ScmURL: "https://git.example/i.git",
			WantImported: true}, // Test 0.
		{Name: "not git", ScmType: "svn", ScmURL: "https://svn.example/i"},   // Test 1.
		{Name: "empty scm type", ScmType: "", ScmURL: "https://g.example/i"}, // Test 2.
		{Name: "manual project", ScmType: "git", ScmURL: ""},                 // Test 3.
		{Name: "embedded credentials", ScmType: "git",
			ScmURL: "https://user:token@git.example/i.git"}, // Test 4.
		{Name: "cleartext http", ScmType: "git", ScmURL: "http://git.example/i.git"}, // Test 5.
		{Name: "metadata host", ScmType: "git",
			ScmURL: "https://metadata.google.internal/i.git"}, // Test 6: a clone would be an SSRF.
		{Name: "loopback host", ScmType: "git", ScmURL: "https://localhost/i.git"}, // Test 7.
		{Name: "whitespace url", ScmType: "git", ScmURL: "   "},                    // Test 8.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"projects": [{"name": "infra", "scm_type": %q, "scm_url": %q}],
				"inventory": [{"name": "prod", "hosts": [{"name": "web01"}]}]}`,
				test.ScmType, test.ScmURL)
			plan, err := FromAWX([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if got := len(plan.Projects); (got == 1) != test.WantImported {
				t.Fatalf("projects = %d, want imported = %v", got, test.WantImported)
			}
			if test.WantImported {
				return
			}
			if _, ok := warningContaining(t, plan.Warnings, `project "infra"`, "skipped"); !ok {
				t.Errorf("a skipped project was not reported.\nwarnings: %v", plan.Warnings)
			}
		})
	}
}

// TestAWXDuplicateNamesWarnAndTheLaterOneWins pins both halves of what the warning promises. An
// export may repeat a name, and a template naming it has to resolve to something; the report says
// which one it will be, so the assertion covers the wiring as well as the message.
func TestAWXDuplicateNamesWarnAndTheLaterOneWins(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [
        {"name": "infra", "scm_type": "git", "scm_url": "https://git.example/one.git"},
        {"name": "infra", "scm_type": "git", "scm_url": "https://git.example/two.git"}
      ],
      "inventory": [
        {"name": "prod", "hosts": [{"name": "web01"}]},
        {"name": "prod", "hosts": [{"name": "web02"}]}
      ],
      "credentials": [
        {"name": "deploy", "credential_type": {"name": "Machine"},
          "inputs": {"ssh_key_data": "$encrypted$"}},
        {"name": "deploy", "credential_type": {"name": "Vault"}, "inputs": {}}
      ],
      "job_templates": [
        {"name": "deploy-app", "playbook": "site.yml", "project": "infra",
          "inventory": "prod", "credentials": ["deploy"]}
      ]
    }`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	for _, what := range []string{`project "infra"`, `inventory "prod"`, `credential "deploy"`} {
		if _, ok := warningContaining(t, plan.Warnings, what, "appears more than once"); !ok {
			t.Errorf("a repeated %s was not reported.\nwarnings: %v", what, plan.Warnings)
		}
	}
	if len(plan.Projects) != 2 || len(plan.Inventories) != 2 || len(plan.Credentials) != 2 {
		t.Fatalf("objects = %d projects, %d inventories, %d credentials; want 2 of each",
			len(plan.Projects), len(plan.Inventories), len(plan.Credentials))
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	tpl := plan.Templates[0]
	if tpl.ProjectID != plan.Projects[1].ID {
		t.Errorf("template project = %q, want the later project %q", tpl.ProjectID,
			plan.Projects[1].ID)
	}
	if tpl.InventoryID != plan.Inventories[1].ID {
		t.Errorf("template inventory = %q, want the later inventory %q", tpl.InventoryID,
			plan.Inventories[1].ID)
	}
	wantCreds := []string{plan.Credentials[1].ID}
	if diff := cmp.Diff(wantCreds, tpl.CredentialIDs, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("template credentials mismatch (-want +got):\n%s", diff)
	}
}

// TestAWXTemplateWithAnUnresolvableProjectIsNotImported pins the fail-closed refusal the doc comment
// on addTemplate describes. Dispatch skips the playbook containment check when a template has no
// project, so importing one turns a path that was contained inside a checkout into one resolved
// against the server's own directory.
func TestAWXTemplateWithAnUnresolvableProjectIsNotImported(t *testing.T) {
	t.Parallel()
	const doc = `{
      "inventory": [{"name": "prod", "hosts": [{"name": "web01"}]}],
      "job_templates": [
        {"name": "escapes", "playbook": "../../etc/site.yml", "project": "missing"},
        {"name": "fine", "playbook": "site.yml", "inventory": "prod"}
      ]
    }`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	var names []string
	for _, tpl := range plan.Templates {
		names = append(names, tpl.Name)
	}
	if diff := cmp.Diff([]string{"fine"}, names, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("imported templates mismatch (-want +got):\n%s", diff)
	}
	if _, ok := warningContaining(t, plan.Warnings, `template "escapes"`, "was not imported",
		`project "missing"`); !ok {
		t.Errorf("the refused template was not named.\nwarnings: %v", plan.Warnings)
	}
}

// TestAWXTemplateKeepsItsExecutionSettings pins the fields that decide what a run does. Dropping
// them silently changed the template's behavior: a check-mode template imported as a live one, and
// one limited to a canary host imported targeting the whole inventory, both without a word.
func TestAWXTemplateKeepsItsExecutionSettings(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://git.example/i.git"}],
      "job_templates": [{
        "name": "patch", "playbook": "patch.yml", "project": "infra",
        "job_type": "check", "limit": "canary", "job_tags": "os, security,",
        "skip_tags": ",slow", "verbosity": 3, "forks": 12, "timeout": 900,
        "diff_mode": true, "job_slice_count": 4,
        "extra_vars": "env: prod\nwindow: night\n"
      }]
    }`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	tpl := plan.Templates[0]
	if !tpl.DryRun {
		t.Error("DryRun = false, want true for an AWX check job")
	}
	if tpl.Limit != "canary" {
		t.Errorf("Limit = %q, want %q", tpl.Limit, "canary")
	}
	if diff := cmp.Diff([]string{"os", "security"}, tpl.Tags, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Tags mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"slow"}, tpl.SkipTags, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("SkipTags mismatch (-want +got):\n%s", diff)
	}
	if tpl.Verbosity != 3 || tpl.Forks != 12 || tpl.Timeout != 900 || !tpl.DiffMode {
		t.Errorf("verbosity/forks/timeout/diff = %d/%d/%d/%v, want 3/12/900/true",
			tpl.Verbosity, tpl.Forks, tpl.Timeout, tpl.DiffMode)
	}
	if tpl.Shards != 4 {
		t.Errorf("Shards = %d, want 4", tpl.Shards)
	}
	wantVars := map[string]any{"env": "prod", "window": "night"}
	if diff := cmp.Diff(wantVars, tpl.ExtraVars, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("ExtraVars mismatch (-want +got):\n%s", diff)
	}
}

// TestAWXJobSliceCountBelowTwoIsNotAShardCount pins the boundary. AWX writes 0 or 1 for a template
// that is not sliced, and storing that as a shard count would turn an ordinary run into a sharded
// one whose bookkeeping the operator never asked for.
func TestAWXJobSliceCountBelowTwoIsNotAShardCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		SliceCount int
		WantShards int
	}{
		{SliceCount: 0, WantShards: 0},   // Test 0.
		{SliceCount: 1, WantShards: 0},   // Test 1.
		{SliceCount: 2, WantShards: 2},   // Test 2: the first value that means sliced.
		{SliceCount: 50, WantShards: 50}, // Test 3.
		{SliceCount: -3, WantShards: 0},  // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"job_templates": [{"name": "j", "playbook": "p.yml",
				"job_slice_count": %d}]}`, test.SliceCount)
			plan, err := FromAWX([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if got := plan.Templates[0].Shards; got != test.WantShards {
				t.Errorf("job_slice_count %d gave Shards = %d, want %d",
					test.SliceCount, got, test.WantShards)
			}
		})
	}
}

// TestAWXUnparseableExtraVarsAreReportedNotSwallowed pins that a template whose extra vars cannot be
// read imports with none and says so. Silently dropping them leaves a playbook running with the
// wrong environment and nothing in the report to explain it.
func TestAWXUnparseableExtraVarsAreReportedNotSwallowed(t *testing.T) {
	t.Parallel()
	const doc = `{"job_templates": [{"name": "broken", "playbook": "p.yml",
      "extra_vars": "a: [1, 2"}]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if got := plan.Templates[0].ExtraVars; got != nil {
		t.Errorf("ExtraVars = %v, want nil when they could not be parsed", got)
	}
	if _, ok := warningContaining(t, plan.Warnings, `template "broken"`,
		"extra_vars could not be parsed"); !ok {
		t.Errorf("unparseable extra vars were not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestAWXRefIsReadInEveryShapeAWXWrites pins the natural-key decoder. A reference typed as a plain
// string once failed the whole document on a real export that serialized it as an object, so an AWX
// with a live install imported nothing at all rather than importing partially.
func TestAWXRefIsReadInEveryShapeAWXWrites(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		JSON     string
		WantName string
	}{
		{Name: "string", JSON: `"infra"`, WantName: "infra"},                         // Test 0.
		{Name: "natural key array", JSON: `["Default", "infra"]`, WantName: "infra"}, // Test 1.
		{Name: "single element array", JSON: `["infra"]`, WantName: "infra"},         // Test 2.
		{Name: "empty array", JSON: `[]`, WantName: ""},                              // Test 3.
		{Name: "object", JSON: `{"name": "infra", "id": 5}`, WantName: "infra"},      // Test 4.
		{Name: "object without name", JSON: `{"id": 5}`, WantName: ""},               // Test 5.
		{Name: "null", JSON: `null`, WantName: ""},                                   // Test 6.
		{Name: "unicode", JSON: `"生产"`, WantName: "生产"},                              // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var ref awxRef
			if err := json.Unmarshal([]byte(test.JSON), &ref); err != nil {
				t.Fatalf("UnmarshalJSON(%s) error = %v, want none: a reference never fails the "+
					"document", test.JSON, err)
			}
			if string(ref) != test.WantName {
				t.Errorf("UnmarshalJSON(%s) = %q, want %q", test.JSON, string(ref), test.WantName)
			}
		})
	}
}

// TestAWXNumericReferencesSilentlyOrphanATemplate demonstrates a defect. The AWX REST API writes
// cross-object references as integer ids, and awxRef decodes only a string, a natural-key array, or
// an object, so a numeric reference becomes the empty name. The empty name then means "no reference
// was given" rather than "this reference could not be resolved", so the fail-closed refusal in
// addTemplate never runs: the template imports with no project and no inventory, no warning at all.
// addTemplate's own doc comment says a template with no project skips dispatch's containment check,
// which is the case this quietly creates.
func TestAWXNumericReferencesSilentlyOrphanATemplate(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [{"id": 5, "name": "infra", "scm_type": "git",
        "scm_url": "https://git.example/infra.git"}],
      "inventory": [{"id": 1, "name": "prod", "hosts": [{"name": "web01"}]}],
      "job_templates": [{"id": 10, "name": "deploy", "playbook": "../../etc/site.yml",
        "inventory": 1, "project": 5, "credentials": [7]}]
    }`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Templates) == 1 && plan.Templates[0].ProjectID == "" {
		if _, ok := warningContaining(t, plan.Warnings, `"deploy"`); !ok {
			t.Fatalf("a template imported with no project and no inventory, and nothing was "+
				"reported.\nwarnings: %v", plan.Warnings)
		}
	}
}

// TestAWXCredentialKindFollowsTheInputsNotOnlyTheTypeName pins that a machine credential, which
// covers both key and password login in AWX, is told apart by what it configured. Getting it wrong
// imports a credential the injector will not materialize the way the operator expects.
func TestAWXCredentialKindFollowsTheInputsNotOnlyTheTypeName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Inputs   string
		WantKind credential.Kind
	}{
		{Name: "key wins", Inputs: `{"ssh_key_data": "$encrypted$", "password": "$encrypted$"}`,
			WantKind: credential.KindSSHKey}, // Test 0.
		{Name: "password only", Inputs: `{"password": "$encrypted$"}`,
			WantKind: credential.KindSSHPassword}, // Test 1.
		{Name: "neither", Inputs: `{"username": "deploy"}`,
			WantKind: credential.KindSSHKey}, // Test 2: the default for a machine credential.
		{Name: "empty inputs", Inputs: `{}`, WantKind: credential.KindSSHKey},    // Test 3.
		{Name: "absent inputs", Inputs: `null`, WantKind: credential.KindSSHKey}, // Test 4.
		{Name: "blank key falls through", Inputs: `{"ssh_key_data": "  ", "password": "$encrypted$"}`,
			WantKind: credential.KindSSHPassword}, // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"credentials": [{"name": "c",
				"credential_type": {"name": "Machine"}, "inputs": %s}]}`, test.Inputs)
			plan, err := FromAWX([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if got := plan.Credentials[0].Kind; got != test.WantKind {
				t.Errorf("kind = %q, want %q for inputs %s", got, test.WantKind, test.Inputs)
			}
		})
	}
}

// TestAWXNullCredentialInputBecomesTheLiteralNilString demonstrates a defect. A JSON null input is
// rendered with fmt.Sprint, which prints "<nil>", and neither publicInputPairs nor hasInput treats
// that as absent. The credential is imported with the setting user=<nil>, so a run connects as a
// user of that name, and a null password makes a machine credential map to the password kind rather
// than the key kind. The vault_id path guards against the same "<nil>" string explicitly, so the
// shape was known; the two readers here were not given the same guard.
func TestAWXNullCredentialInputBecomesTheLiteralNilString(t *testing.T) {
	t.Parallel()
	const doc = `{"credentials": [{"name": "machine",
      "credential_type": {"name": "Machine"},
      "inputs": {"username": null, "password": null, "ssh_key_data": "$encrypted$"}}]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	cred := plan.Credentials[0]
	if got, ok := cred.Settings["user"]; ok {
		t.Errorf("settings[user] = %q, want the null input to be omitted entirely", got)
	}
	if cred.Kind != credential.KindSSHKey {
		t.Errorf("kind = %q, want %q: a null password is not a configured password",
			cred.Kind, credential.KindSSHKey)
	}
}

// TestAWXVaultLabelIsCarriedOnlyWhenItIsUsable pins that a multi-vault setup keeps its --vault-id
// labels, and that a label the credential package would refuse is dropped rather than stored. A
// stored label that fails validation later is a credential that cannot be used at all.
func TestAWXVaultLabelIsCarriedOnlyWhenItIsUsable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		VaultID     string
		WantVaultID string
	}{
		{Name: "plain label", VaultID: `"prod"`, WantVaultID: "prod"},      // Test 0.
		{Name: "absent", VaultID: `null`, WantVaultID: ""},                 // Test 1.
		{Name: "empty", VaultID: `""`, WantVaultID: ""},                    // Test 2.
		{Name: "whitespace", VaultID: `"   "`, WantVaultID: ""},            // Test 3.
		{Name: "with a space", VaultID: `"prod vault"`, WantVaultID: ""},   // Test 4.
		{Name: "with an at sign", VaultID: `"prod@host"`, WantVaultID: ""}, // Test 5.
		{Name: "with a newline", VaultID: `"prod\nx"`, WantVaultID: ""},    // Test 6.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"credentials": [{"name": "v",
				"credential_type": {"name": "Vault"}, "inputs": {"vault_id": %s}}]}`, test.VaultID)
			plan, err := FromAWX([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			cred := plan.Credentials[0]
			if cred.Kind != credential.KindVaultPassword {
				t.Fatalf("kind = %q, want %q", cred.Kind, credential.KindVaultPassword)
			}
			if cred.VaultID != test.WantVaultID {
				t.Errorf("VaultID = %q, want %q", cred.VaultID, test.WantVaultID)
			}
		})
	}
}

// TestAWXEveryCredentialIsReportedAsNeedingItsSecret pins that no credential imports quietly. An
// export omits secrets by design, so a credential that arrives without a line in the report is one
// an operator will discover at the first failed run.
func TestAWXEveryCredentialIsReportedAsNeedingItsSecret(t *testing.T) {
	t.Parallel()
	doc := `{"credentials": [
      {"name": "bare", "credential_type": {"name": "Machine"}, "inputs": {}},
      {"name": "settings only", "credential_type": {"name": "Machine"},
        "inputs": {"username": "deploy", "become_method": "sudo"}},
      {"name": "refused only", "credential_type": {"name": "Machine"},
        "inputs": {"username": "` + strings.Repeat("u", 600) + `"}},
      {"name": "both", "credential_type": {"name": "Machine"},
        "inputs": {"username": "deploy", "region": "` + strings.Repeat("r", 600) + `"}}
    ]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	for _, name := range []string{"bare", "settings only", "refused only", "both"} {
		if _, ok := warningContaining(t, plan.Warnings, fmt.Sprintf("credential %q", name),
			"needs its secret re-entered"); !ok {
			t.Errorf("credential %q was not reported as needing its secret.\nwarnings: %v",
				name, plan.Warnings)
		}
	}
	if _, ok := warningContaining(t, plan.Warnings, `credential "refused only"`,
		"could not be stored and must be set by hand"); !ok {
		t.Errorf("a refused input was not named.\nwarnings: %v", plan.Warnings)
	}
	if _, ok := warningContaining(t, plan.Warnings, `credential "both"`,
		"were stored on the credential", "could not be stored"); !ok {
		t.Errorf("the mixed case did not report both halves.\nwarnings: %v", plan.Warnings)
	}
}

// TestAWXInventorySourceEdges pins what a dynamic source does with each shape an export writes: a
// file path, a cloud plugin with a warning that a config is needed, and the two shapes that cannot
// be imported at all. A source that imports with nothing to refresh from is a source that will fail
// on its first sync with no clue why.
func TestAWXInventorySourceEdges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Source      string
		WantSources int
		WantWarning string
	}{
		{Name: "file path", Source: `{"name": "scm", "source": "scm",
			"source_path": "inventories/prod.ini"}`, WantSources: 1}, // Test 0.
		{Name: "cloud plugin", Source: `{"name": "ec2", "source": "ec2"}`,
			WantSources: 1, WantWarning: "point it at a plugin config file"}, // Test 1.
		{Name: "no name", Source: `{"source": "ec2"}`,
			WantSources: 0, WantWarning: "it has no name"}, // Test 2.
		{Name: "nothing to read", Source: `{"name": "empty"}`,
			WantSources: 0, WantWarning: "no source path or plugin type"}, // Test 3.
		{Name: "unknown project", Source: `{"name": "s", "source_path": "p.ini",
			"source_project": "missing"}`,
			WantSources: 1, WantWarning: `unknown project "missing"`}, // Test 4.
		{Name: "unknown credential", Source: `{"name": "s", "source_path": "p.ini",
			"credential": "missing"}`,
			WantSources: 1, WantWarning: `unknown credential "missing"`}, // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"inventory": [{"name": "prod", "hosts": [{"name": "w"}]}],
				"inventory_sources": [%s]}`, test.Source)
			plan, err := FromAWX([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if got := len(plan.Sources); got != test.WantSources {
				t.Fatalf("sources = %d, want %d", got, test.WantSources)
			}
			// A source that imports always brings its own backing inventory, and one that is
			// skipped must not leave an orphaned inventory behind.
			wantInventories := 1 + test.WantSources
			if got := len(plan.Inventories); got != wantInventories {
				t.Errorf("inventories = %d, want %d (one per imported source plus the named one)",
					got, wantInventories)
			}
			if test.WantWarning == "" {
				return
			}
			if _, ok := warningContaining(t, plan.Warnings, test.WantWarning); !ok {
				t.Errorf("missing warning %q.\nwarnings: %v", test.WantWarning, plan.Warnings)
			}
		})
	}
}

// TestAWXSourceWiresItsBackingInventoryToItself pins the cross-reference an operator cannot see. A
// source whose backing inventory id points at the wrong object refreshes into an inventory nothing
// targets, and every run against the source's name reads stale hosts forever.
func TestAWXSourceWiresItsBackingInventoryToItself(t *testing.T) {
	t.Parallel()
	const doc = `{"inventory_sources": [
      {"name": "ec2 prod", "source": "ec2"},
      {"name": "ec2 stage", "source": "ec2"}
    ]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Sources) != 2 || len(plan.Inventories) != 2 {
		t.Fatalf("sources = %d, inventories = %d, want 2 and 2",
			len(plan.Sources), len(plan.Inventories))
	}
	byID := map[string]string{}
	for _, inv := range plan.Inventories {
		byID[inv.ID] = inv.Name
	}
	for _, src := range plan.Sources {
		name, ok := byID[src.InventoryID]
		if !ok {
			t.Fatalf("source %q points at inventory %q, which is not in the plan",
				src.Name, src.InventoryID)
		}
		if name != src.Name+" (dynamic)" {
			t.Errorf("source %q backs inventory %q, want %q", src.Name, name,
				src.Name+" (dynamic)")
		}
	}
}

// TestAWXInventoryRendersEveryPartOfWhatItMeans pins the full INI a rich inventory produces. Group
// variables and nested groups were dropped once, which left a playbook reading a group variable
// running with the wrong value and a play targeting a parent group reaching none of its children.
func TestAWXInventoryRendersEveryPartOfWhatItMeans(t *testing.T) {
	t.Parallel()
	const doc = `{"inventory": [{
      "name": "prod",
      "hosts": [{"name": "bastion", "variables": {"ansible_port": 2222}}],
      "groups": [{
        "name": "web",
        "hosts": [{"name": "web01"}, {"name": "web02", "variables": {"role": "canary"}}],
        "variables": {"http_port": 80, "note": "with space"},
        "children": ["web-canary"]
      }],
      "variables": {"ansible_user": "deploy"}
    }]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	want := "bastion ansible_port=2222\n" +
		"\n" +
		"[web]\n" +
		"web01\n" +
		"web02 role=canary\n" +
		"\n" +
		"[web:vars]\n" +
		"http_port=80\n" +
		"note=\"with space\"\n" +
		"\n" +
		"[web:children]\n" +
		"web-canary\n" +
		"\n" +
		"[all:vars]\n" +
		"ansible_user=deploy\n"
	if diff := cmp.Diff(want, plan.Inventories[0].Content); diff != "" {
		t.Errorf("inventory content mismatch (-want +got):\n%s", diff)
	}
}

// TestAWXInventoryKeepsUnicodeNamesAsThemselves pins that a fleet named in another script imports
// unchanged. Nothing in a host name outside the tokenizing set needs rewriting, and an importer that
// mangled non-ASCII names would silently produce an inventory naming machines that do not exist.
func TestAWXInventoryKeepsUnicodeNamesAsThemselves(t *testing.T) {
	t.Parallel()
	const doc = `{"inventory": [{
      "name": "生产",
      "hosts": [{"name": "wéb01.münchen.example", "variables": {"ロール": "ウェブ"}}],
      "groups": [{"name": "grüppe", "hosts": [{"name": "db-δ"}], "children": ["παιδί"]}]
    }]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	inv := plan.Inventories[0]
	if inv.Name != "生产" {
		t.Errorf("inventory name = %q, want %q", inv.Name, "生产")
	}
	for _, want := range []string{"wéb01.münchen.example", "ロール=ウェブ", "[grüppe]", "db-δ",
		"[grüppe:children]", "παιδί"} {
		if !strings.Contains(inv.Content, want) {
			t.Errorf("inventory content is missing %q:\n%s", want, inv.Content)
		}
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "was dropped") {
			t.Errorf("a unicode name was dropped: %s", w)
		}
	}
}

// TestAWXHostNamesThatShlexRewritesAreNotRefused demonstrates a defect. safeININame refuses
// whitespace and the inventory metacharacters but allows a double quote and a backslash, both of
// which Ansible's ini plugin acts on: it tokenizes each host line with shlex, so a host named
// a"b"c is read as abc and one named a\b is read as ab. That is exactly the silent rename the
// function's own doc comment says must not happen, and an odd number of quotes makes the whole
// inventory file unparseable, taking every other host in it down with the one bad name.
func TestAWXHostNamesThatShlexRewritesAreNotRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Host string
	}{
		{Name: "balanced quotes", Host: `a"b"c`}, // Test 0: Ansible reads this host as abc.
		{Name: "odd quote", Host: `web1"`},       // Test 1: this breaks the whole inventory file.
		{Name: "backslash", Host: `a\b`},         // Test 2: Ansible reads this host as ab.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if safeININame(test.Host) {
				t.Errorf("safeININame(%q) = true, want false: shlex does not read it as itself",
					test.Host)
			}
		})
	}
}

// TestAWXReportsUnmappedCountsWithTheRightPlural pins the sentence an operator reads to learn what
// is not coming across. A count of one that reads "1 teams" is the kind of thing that makes a report
// look generated rather than checked, and the branch was untested.
func TestAWXReportsUnmappedCountsWithTheRightPlural(t *testing.T) {
	t.Parallel()
	const doc = `{
      "inventory": [{"name": "prod", "hosts": [{"name": "w"}]}],
      "organizations": [{"name": "a"}],
      "teams": [{"name": "a"}, {"name": "b"}],
      "notification_templates": [{"name": "slack"}]
    }`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	for _, want := range []string{"1 organization,", "2 teams,", "1 notification template,"} {
		if _, ok := warningContaining(t, plan.Warnings, want); !ok {
			t.Errorf("missing %q in the report.\nwarnings: %v", want, plan.Warnings)
		}
	}
	if _, ok := warningContaining(t, plan.Warnings, "1 organizations"); ok {
		t.Error("a count of one was pluralized")
	}
}

// TestAWXScheduleRefusalsNameTheRemedy pins that a schedule cron cannot express is skipped with a
// reason specific enough to act on. A rule that bounds itself needs a different fix from a cadence
// cron cannot say, and a cron entry created from a bounded rule would fire forever.
func TestAWXScheduleRefusalsNameTheRemedy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		RRule        string
		WantFragment string
	}{
		{Name: "count", RRule: "DTSTART:20260101T020000Z RRULE:FREQ=MINUTELY;COUNT=1",
			WantFragment: "runs a fixed number of times"}, // Test 0.
		{Name: "until", RRule: "DTSTART:20260101T020000Z RRULE:FREQ=DAILY;UNTIL=20270101T000000Z",
			WantFragment: "stops on a date"}, // Test 1.
		{Name: "uneven interval", RRule: "DTSTART:20260101T020000Z RRULE:FREQ=MINUTELY;INTERVAL=45",
			WantFragment: "cadence cannot be expressed as cron"}, // Test 2.
		{Name: "every three days", RRule: "DTSTART:20260101T020000Z RRULE:FREQ=DAILY;INTERVAL=3",
			WantFragment: "cadence cannot be expressed as cron"}, // Test 3.
		{Name: "yearly", RRule: "DTSTART:20260101T020000Z RRULE:FREQ=YEARLY",
			WantFragment: "cadence cannot be expressed as cron"}, // Test 4.
		{Name: "empty", RRule: "", WantFragment: "cadence cannot be expressed as cron"}, // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"job_templates": [{"name": "j", "playbook": "p.yml",
				"related": {"schedules": [{"name": "nightly", "rrule": %q}]}}]}`, test.RRule)
			plan, err := FromAWX([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if len(plan.Schedules) != 0 {
				t.Fatalf("schedules = %d, want 0: a rule cron cannot express must not become one",
					len(plan.Schedules))
			}
			if _, ok := warningContaining(t, plan.Warnings, `schedule "nightly"`,
				test.WantFragment); !ok {
				t.Errorf("missing %q.\nwarnings: %v", test.WantFragment, plan.Warnings)
			}
		})
	}
}

// TestAWXImportedScheduleIsStampedAndEnabled pins that a schedule joins the plan only after it
// validates and with its first fire time set. NextRunAt left nil reads to the scheduler as "not due
// yet" forever, so every imported schedule was reported as created and then never fired.
func TestAWXImportedScheduleIsStampedAndEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Enabled     string
		WantEnabled bool
	}{
		{Name: "absent means enabled", Enabled: `null`, WantEnabled: true}, // Test 0.
		{Name: "explicitly on", Enabled: `true`, WantEnabled: true},        // Test 1.
		{Name: "explicitly off", Enabled: `false`, WantEnabled: false},     // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"job_templates": [{"name": "j", "playbook": "p.yml",
				"related": {"schedules": [{"name": "nightly", "enabled": %s,
				"rrule": "DTSTART;TZID=America/New_York:20260101T020000 RRULE:FREQ=DAILY"}]}}]}`,
				test.Enabled)
			plan, err := FromAWX([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if len(plan.Schedules) != 1 {
				t.Fatalf("schedules = %d, want 1", len(plan.Schedules))
			}
			sc := plan.Schedules[0]
			if sc.Enabled != test.WantEnabled {
				t.Errorf("Enabled = %v, want %v", sc.Enabled, test.WantEnabled)
			}
			if sc.NextRunAt == nil || !sc.NextRunAt.After(importNow) {
				t.Errorf("NextRunAt = %v, want a time after %v", sc.NextRunAt, importNow)
			}
			if sc.Cron != "0 2 * * *" {
				t.Errorf("Cron = %q, want %q", sc.Cron, "0 2 * * *")
			}
			if sc.Timezone != "America/New_York" {
				t.Errorf("Timezone = %q, want %q", sc.Timezone, "America/New_York")
			}
			if sc.TemplateID != plan.Templates[0].ID {
				t.Errorf("TemplateID = %q, want the imported template %q",
					sc.TemplateID, plan.Templates[0].ID)
			}
		})
	}
}

// TestAWXUnresolvableTimezoneStillImportsInServerTime pins the deliberate choice named in the doc
// comment: a zone this build cannot resolve is reported and the schedule still imports. A job that
// runs at the wrong hour is recoverable; one that was never created is easy to miss.
func TestAWXUnresolvableTimezoneStillImportsInServerTime(t *testing.T) {
	t.Parallel()
	const doc = `{"job_templates": [{"name": "j", "playbook": "p.yml",
      "related": {"schedules": [{"name": "nightly",
        "rrule": "DTSTART;TZID=Mars/Olympus:20260101T020000 RRULE:FREQ=DAILY"}]}}]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Schedules) != 1 {
		t.Fatalf("schedules = %d, want 1: an unknown zone must not drop the schedule",
			len(plan.Schedules))
	}
	if got := plan.Schedules[0].Timezone; got != "" {
		t.Errorf("Timezone = %q, want empty so the schedule runs in server time", got)
	}
	if _, ok := warningContaining(t, plan.Warnings, "Mars/Olympus",
		"imports in the server's local time"); !ok {
		t.Errorf("an unresolvable zone was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestDTSTARTZonePanicsOnANonASCIIField demonstrates a defect. dtstartZone finds "TZID=" in the
// uppercased copy of the field and then slices the original with that index. Uppercasing is not
// length preserving in UTF-8, so a DTSTART carrying enough runes that grow when uppercased shifts
// the index past the end of the original and the slice panics. The document is somebody else's
// export arriving over an import endpoint, so this is a crash a stranger can trigger.
func TestDTSTARTZonePanicsOnANonASCIIField(t *testing.T) {
	t.Parallel()
	rrule := "DTSTART" + strings.Repeat("ɐ", 40) +
		";TZID=America/New_York:20260101T020000 RRULE:FREQ=DAILY"
	doc := fmt.Sprintf(`{"job_templates": [{"name": "j", "playbook": "p.yml",
      "related": {"schedules": [{"name": "n", "rrule": %q}]}}]}`, rrule)
	if _, err := FromAWX([]byte(doc), importNow); err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
}

// TestAWXSurveyRefusesAPasswordFieldWhereverItIsCarried pins the refusal on both export shapes. A
// survey field here is plain text kept on the run and injected as an extra var, so importing an AWX
// password prompt would hand the operator a migration that looks complete and is less safe than what
// they left.
func TestAWXSurveyRefusesAPasswordFieldWhereverItIsCarried(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Doc  string
	}{
		{Name: "top level survey", Doc: `{"job_templates": [{"name": "j", "playbook": "p.yml",
			"survey_spec": {"spec": [
				{"variable": "secret", "type": "password", "default": "hunter2"},
				{"variable": "env", "type": "text"}]}}]}`}, // Test 0.
		{Name: "nested survey", Doc: `{"job_templates": [{"name": "j", "playbook": "p.yml",
			"related": {"survey_spec": {"spec": [
				{"variable": "secret", "type": "PASSWORD", "default": "hunter2"},
				{"variable": "env", "type": "text"}]}}}]}`}, // Test 1: the check is case folded.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte(test.Doc), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			survey := plan.Templates[0].Survey
			if len(survey) != 1 || survey[0].Var != "env" {
				t.Fatalf("survey = %+v, want only the non-secret field", survey)
			}
			if _, ok := warningContaining(t, plan.Warnings, `survey field "secret"`,
				"NOT imported"); !ok {
				t.Errorf("the password field was not named.\nwarnings: %v", plan.Warnings)
			}
			for _, w := range plan.Warnings {
				if strings.Contains(w, "hunter2") {
					t.Errorf("the refused field's default leaked into the report: %s", w)
				}
			}
		})
	}
}

// TestAWXSurveyCarriesItsShapeThrough pins that a survey's defaults and choices survive in both the
// list and newline encodings AWX uses, and that an inexact type mapping is named. A survey that
// arrives without its choices is one nobody can answer as they used to.
func TestAWXSurveyCarriesItsShapeThrough(t *testing.T) {
	t.Parallel()
	const doc = `{"job_templates": [{"name": "j", "playbook": "p.yml", "survey_spec": {"spec": [
      {"variable": "env", "question_name": "Which environment?", "type": "multiplechoice",
        "required": true, "default": "stage", "choices": ["prod", "stage"]},
      {"variable": "region", "type": "multiselect", "choices": "us-east-1\nus-west-2\n"},
      {"variable": "ratio", "type": "float", "default": 0.5},
      {"variable": "count", "type": "integer", "default": 3}
    ]}}]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	survey := plan.Templates[0].Survey
	if len(survey) != 4 {
		t.Fatalf("survey fields = %d, want 4", len(survey))
	}
	if survey[0].Label != "Which environment?" || !survey[0].Required {
		t.Errorf("field 0 = %+v, want its prompt and required flag carried", survey[0])
	}
	if diff := cmp.Diff([]string{"prod", "stage"}, survey[0].Choices,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("list choices mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"us-east-1", "us-west-2"}, survey[1].Choices,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("newline choices mismatch (-want +got):\n%s", diff)
	}
	if _, ok := warningContaining(t, plan.Warnings, `survey field "ratio"`, `type "float"`); !ok {
		t.Errorf("an inexact survey type was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestAWXWarningCapIsAnnouncedAndCounted pins that a report which hit its ceiling says so and still
// knows the total. A truncated report that looks complete is how somebody concludes an import was
// clean when it was only long, and the cap itself is what keeps one upload from answering with a
// response many times the size of the request.
func TestAWXWarningCapIsAnnouncedAndCounted(t *testing.T) {
	t.Parallel()
	var creds []string
	for i := range 900 {
		creds = append(creds, fmt.Sprintf(
			`{"name": "c%d", "credential_type": {"name": "Weird Type"}, "inputs": {}}`, i))
	}
	doc := `{"credentials": [` + strings.Join(creds, ",") + `]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Warnings) != maxWarnings+1 {
		t.Fatalf("warnings = %d, want %d listed plus the one announcing the cap",
			len(plan.Warnings), maxWarnings+1)
	}
	if got := plan.Warnings[maxWarnings]; !strings.Contains(got, "more than 1000 warnings") {
		t.Errorf("the cap warning is missing; last listed warning = %q", got)
	}
	if plan.Suppressed() == 0 {
		t.Error("Suppressed() = 0, want the count of what was not listed")
	}
	// Two warnings per unmapped credential, and 900 credentials, so the total is knowable from the
	// listed ones plus the suppressed count.
	if got := len(plan.Warnings) - 1 + plan.Suppressed(); got != 1800 {
		t.Errorf("listed plus suppressed = %d, want 1800", got)
	}
}

// TestAWXNestedRelatedAssetsAreReadWhereAwxkitPutsThem pins the accessors that read hosts, groups,
// and credentials from an export's related blocks. Reading only the top level meant every inventory
// from a real awxkit export arrived empty, and silently: the import reported success, the inventory
// existed, and it had no hosts in it.
func TestAWXNestedRelatedAssetsAreReadWhereAwxkitPutsThem(t *testing.T) {
	t.Parallel()
	const doc = `{
      "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://git.example/i.git"}],
      "credentials": [{"name": "deploy", "credential_type": {"name": "Machine"}, "inputs": {}}],
      "inventory": [{
        "name": "prod",
        "related": {
          "hosts": [{"name": "bastion"}],
          "groups": [{"name": "web", "related": {"hosts": [{"name": "web01"}]}}],
          "inventory_sources": [{"name": "ec2", "source": "ec2"}]
        }
      }],
      "job_templates": [{"name": "j", "playbook": "p.yml", "project": "infra",
        "related": {"credentials": ["deploy"]}}]
    }`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	content := plan.Inventories[0].Content
	for _, want := range []string{"bastion", "[web]", "web01"} {
		if !strings.Contains(content, want) {
			t.Errorf("nested inventory content is missing %q:\n%s", want, content)
		}
	}
	if len(plan.Sources) != 1 {
		t.Errorf("sources = %d, want the one nested under the inventory", len(plan.Sources))
	}
	if diff := cmp.Diff([]string{plan.Credentials[0].ID}, plan.Templates[0].CredentialIDs,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("nested credentials mismatch (-want +got):\n%s", diff)
	}
}

// TestAWXTopLevelBeatsNestedWhenBothArePresent pins the precedence the accessors document. An export
// carrying both shapes must not merge them into a doubled host list, which would show an operator a
// fleet twice the size of the one they have.
func TestAWXTopLevelBeatsNestedWhenBothArePresent(t *testing.T) {
	t.Parallel()
	const doc = `{"inventory": [{
      "name": "prod",
      "hosts": [{"name": "top01"}],
      "groups": [{"name": "web", "hosts": [{"name": "topweb"}],
        "related": {"hosts": [{"name": "nestedweb"}]}}],
      "related": {"hosts": [{"name": "nested01"}], "groups": [{"name": "nestedgroup"}]}
    }]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	content := plan.Inventories[0].Content
	for _, want := range []string{"top01", "[web]", "topweb"} {
		if !strings.Contains(content, want) {
			t.Errorf("content is missing %q:\n%s", want, content)
		}
	}
	for _, unwanted := range []string{"nested01", "nestedgroup", "nestedweb"} {
		if strings.Contains(content, unwanted) {
			t.Errorf("content merged in the nested shape %q:\n%s", unwanted, content)
		}
	}
}
