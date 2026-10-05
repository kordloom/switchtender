package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// credTypesPlan maps the custom credential type fixture.
func credTypesPlan(t *testing.T) *Plan {
	t.Helper()
	data, err := os.ReadFile("testdata/awx-credential-types.json")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := FromAWX(data, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	return plan
}

// TestAWXCustomTypesWithFilesImport proves an AWX custom credential type that writes files comes
// across whole: its fields, its file templates under the same keys, and the env and extra-var
// injectors that hand each file's path over. A credential of the type becomes a credential of the
// imported type rather than an env credential the operator has to rebuild by hand.
func TestAWXCustomTypesWithFilesImport(t *testing.T) {
	t.Parallel()
	plan := credTypesPlan(t)
	byName := map[string]*credential.CredentialType{}
	for _, ct := range plan.CredentialTypes {
		byName[ct.Name] = ct
	}
	tests := []struct {
		Name          string
		WantFields    []credential.Field
		WantFiles     map[string]string
		WantEnv       map[string]string
		WantExtraVars map[string]string
	}{{ // Test 0: A single kubeconfig file and the variable naming its path.
		Name: "Kubeconfig",
		WantFields: []credential.Field{
			{Name: "kube_config", Label: "Kubeconfig", Secret: true, Multiline: true},
		},
		WantFiles: map[string]string{"template": "{{ kube_config }}"},
		WantEnv:   map[string]string{"K8S_AUTH_KUBECONFIG": "{{ tower.filename }}"},
	}, { // Test 1: Two named files, one path through env and one through an awx-spelled extra var.
		Name: "Client Certificate",
		WantFields: []credential.Field{
			{Name: "cert", Label: "Certificate", Multiline: true},
			{Name: "key", Label: "Key", Secret: true, Multiline: true},
			{Name: "verify", Label: "Verify"},
		},
		WantFiles: map[string]string{"template.cert": "{{ cert }}", "template.key": "{{ key }}"},
		WantEnv: map[string]string{
			"TLS_CERT_FILE": "{{ tower.filename.cert }}", "TLS_VERIFY": "{{ verify }}",
		},
		WantExtraVars: map[string]string{"tls_key_file": "{{ awx.filename.key }}"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ct, ok := byName[test.Name]
			if !ok {
				t.Fatalf("type %q was not imported; imported: %v", test.Name, keys(byName))
			}
			if err := ct.Validate(); err != nil {
				t.Errorf("imported type does not validate: %v", err)
			}
			if diff := cmp.Diff(test.WantFields, ct.Fields); diff != "" {
				t.Errorf("fields mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantFiles, ct.FileInjectors, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("files mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantEnv, ct.EnvInjectors, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("env mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantExtraVars, ct.ExtraVarInjectors, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("extra vars mismatch (-want +got):\n%s", diff)
			}
		})
	}
	if len(plan.CredentialTypes) != 2 {
		t.Errorf("imported %d types, want 2: the Jinja one refused and AWX's managed one skipped",
			len(plan.CredentialTypes))
	}
}

// keys lists a map's keys for a failure message.
func keys(m map[string]*credential.CredentialType) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAWXCustomTypeCredentialsAndReport proves the credentials and the report: a credential of an
// imported type names it, a credential of a refused type falls back to the kind mapping, the report
// counts the types it creates and itemizes the refused one as not coming across, and nothing about
// credential types is reported as an unread field any more.
func TestAWXCustomTypeCredentialsAndReport(t *testing.T) {
	t.Parallel()
	plan := credTypesPlan(t)
	typeIDs := map[string]string{}
	for _, ct := range plan.CredentialTypes {
		typeIDs[ct.ID] = ct.Name
	}
	got := map[string]string{}
	for _, c := range plan.Credentials {
		if c.TypeID != "" {
			got[c.Name] = "type " + typeIDs[c.TypeID]
			continue
		}
		got[c.Name] = "kind " + string(c.Kind)
	}
	want := map[string]string{
		"prod-kube": "type Kubeconfig", "edge-cert": "type Client Certificate", "api-token": "kind token",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("credentials mismatch (-want +got):\n%s", diff)
	}

	report := plan.Report()
	counts := map[string]int{}
	for _, c := range report.Created {
		counts[c.Kind] = c.N
	}
	if counts["credential types"] != 2 || counts["credentials"] != 3 {
		t.Errorf("created counts = %v, want 2 credential types and 3 credentials", counts)
	}
	if report.CreatedTotal != 5 {
		t.Errorf("created total = %d, want 5", report.CreatedTotal)
	}
	var refused, unread int
	for _, w := range report.LeftOut {
		if strings.Contains(w, `credential type "Templated Token" was not imported`) &&
			strings.Contains(w, "Jinja") {
			refused++
		}
		if strings.Contains(w, "credential_types") || strings.Contains(w, "injectors") {
			unread++
		}
	}
	if refused != 1 {
		t.Errorf("the Jinja type is not itemized as not coming across: %v", report.LeftOut)
	}
	if unread != 0 {
		t.Errorf("credential type fields are still reported as unread: %v", report.LeftOut)
	}
	var review string
	for _, w := range report.NeedsReview {
		review += w + "\n"
	}
	for _, phrase := range []string{
		`credential "prod-kube" is of the custom type "Kubeconfig" and needs its field values`,
		`field "verify" is a boolean in AWX`,
		`field "verify" defaults to "true" in AWX`,
	} {
		if !strings.Contains(review, phrase) {
			t.Errorf("review items do not say %q:\n%s", phrase, review)
		}
	}
	var out bytes.Buffer
	Render(&out, "awx", "export.json", plan.Assess())
	if !strings.Contains(out.String(), "credential types") {
		t.Errorf("the assessment does not count the credential types:\n%s", out.String())
	}
}

// TestConvertAWXCredTypeRefusals pins each shape of AWX type that cannot come across as it was, and
// that each is refused with a reason rather than imported wrong.
func TestConvertAWXCredTypeRefusals(t *testing.T) {
	t.Parallel()
	field := []awxCredTypeField{{ID: "a"}}
	tests := []struct {
		Type     awxCredentialType
		WantText string
	}{{ // Test 0: A Jinja conditional in a file template.
		Type: awxCredentialType{Name: "T", Inputs: awxCredTypeInputs{Fields: field},
			Injectors: awxCredTypeInjectors{
				File: map[string]any{"template": "{% if a %}{{ a }}{% endif %}"},
				Env:  map[string]any{"F": "{{ tower.filename }}"},
			}},
		WantText: "Jinja",
	}, { // Test 1: An injector value that is not a string.
		Type: awxCredentialType{Name: "T", Inputs: awxCredTypeInputs{Fields: field},
			Injectors: awxCredTypeInjectors{ExtraVars: map[string]any{"x": map[string]any{"y": 1}}}},
		WantText: "not a string",
	}, { // Test 2: A field type with no equivalent.
		Type: awxCredentialType{Name: "T",
			Inputs:    awxCredTypeInputs{Fields: []awxCredTypeField{{ID: "a", Type: "integer"}}},
			Injectors: awxCredTypeInjectors{Env: map[string]any{"A": "{{ a }}"}}},
		WantText: "integer",
	}, { // Test 3: A field id that is not lowercase.
		Type: awxCredentialType{Name: "T",
			Inputs:    awxCredTypeInputs{Fields: []awxCredTypeField{{ID: "ApiKey"}}},
			Injectors: awxCredTypeInjectors{Env: map[string]any{"A": "{{ ApiKey }}"}}},
		WantText: "lowercase",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, _, err := convertAWXCredType(test.Type, time.Now())
			if err == nil || !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("convertAWXCredType() error = %v, want one naming %q", err, test.WantText)
			}
		})
	}
}

// TestApplyWritesCredentialTypesFirst proves the apply order and its guards: types are written
// before the credentials that name them, a plan with types and no type store writes nothing, and a
// type whose name the install already holds is refused before anything is written.
func TestApplyWritesCredentialTypesFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := func(types credential.TypeStore) ApplyStores {
		return ApplyStores{
			Projects: project.NewMemStore(), Inventories: inventory.NewMemStore(),
			Credentials: credential.NewMemStore(), CredentialTypes: types,
			Templates: template.NewMemStore(), Schedules: schedule.NewMemStore(),
		}
	}

	plan := credTypesPlan(t)
	s := stores(credential.NewMemTypeStore())
	created, err := plan.Apply(ctx, s)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if created != 5 {
		t.Errorf("Apply() created %d, want 5", created)
	}
	creds, err := s.Credentials.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range creds {
		if c.TypeID == "" {
			continue
		}
		if _, err := s.CredentialTypes.Get(ctx, c.TypeID); err != nil {
			t.Errorf("credential %q names type %s, which was not written: %v", c.Name, c.TypeID, err)
		}
	}

	none := stores(nil)
	if n, err := credTypesPlan(t).Apply(ctx, none); err == nil || n != 0 {
		t.Errorf("Apply() without a type store = %d, %v, want a refusal writing nothing", n, err)
	}
	if list, _ := none.Credentials.List(ctx); len(list) != 0 {
		t.Errorf("credentials written without their types: %d", len(list))
	}

	held := credential.NewMemTypeStore()
	if err := held.Save(ctx, &credential.CredentialType{ID: "ctype_other", Name: "Kubeconfig"}); err != nil {
		t.Fatal(err)
	}
	if _, err := credTypesPlan(t).Apply(ctx, stores(held)); !errors.Is(err, ErrAlreadyImported) {
		t.Errorf("Apply() over an existing type of the same name error = %v, want ErrAlreadyImported", err)
	}
}

// TestAWXTypeWithAnUnreferencedFileComesAcross proves an AWX type with a file no injector
// references imports as it is: marked as from AWX, its credentials typed by it, and the file named
// in the report as something to review, never as something left out. The assessment counts it among
// what the import creates.
func TestAWXTypeWithAnUnreferencedFileComesAcross(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Files       string
		Env         string
		WantUnrefed []string
		WantNote    string
	}{{ // Test 0: The single file, its path handed to nothing.
		Files:       `{"template": "{{ bundle }}"}`,
		Env:         `{"BUNDLE_HOST": "{{ host }}"}`,
		WantUnrefed: []string{credential.SingleFile},
		WantNote: `credential type "Legacy Bundle": file template is written for each run, but no ` +
			`env or extra-var injector references its path, so nothing tells the tool where it is. ` +
			`The type came across as it was defined in AWX, and each run of a credential of it ` +
			`carries a warning naming the file. Hand the path over with {{ tower.filename }} in an ` +
			`env or extra-var injector, or delete the file injector.`,
	}, { // Test 1: Two named files, one handed over and one not.
		Files:       `{"template.ca": "{{ bundle }}", "template.key": "{{ bundle }}"}`,
		Env:         `{"BUNDLE_CA": "{{ tower.filename.ca }}"}`,
		WantUnrefed: []string{"key"},
		WantNote:    `with {{ tower.filename.key }} in an env or extra-var injector`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			export := `{"credential_types": [{"name": "Legacy Bundle", "kind": "cloud",
				"inputs": {"fields": [{"id": "host", "label": "Host"},
					{"id": "bundle", "label": "Bundle", "secret": true, "multiline": true}]},
				"injectors": {"file": ` + test.Files + `, "env": ` + test.Env + `}}],
				"credentials": [{"name": "edge-bundle",
					"credential_type": {"name": "Legacy Bundle", "kind": "cloud"},
					"inputs": {"host": "edge.example.com", "bundle": "$encrypted$"}}]}`
			plan, err := FromAWX([]byte(export), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if len(plan.CredentialTypes) != 1 {
				t.Fatalf("imported %d types, want the one in the export: %v", len(plan.CredentialTypes),
					plan.Warnings)
			}
			typ := plan.CredentialTypes[0]
			if typ.Origin != credential.OriginAWX {
				t.Errorf("origin = %q, want %q", typ.Origin, credential.OriginAWX)
			}
			if diff := cmp.Diff(test.WantUnrefed, typ.UnreferencedFiles()); diff != "" {
				t.Errorf("unreferenced files (-want +got):\n%s", diff)
			}
			if len(plan.Credentials) != 1 || plan.Credentials[0].TypeID != typ.ID {
				t.Errorf("the credential does not name the imported type: %+v", plan.Credentials)
			}
			report := plan.Report()
			for _, w := range report.LeftOut {
				if strings.Contains(w, "Legacy Bundle") {
					t.Errorf("the type is reported as not coming across: %s", w)
				}
			}
			var found bool
			for _, w := range report.NeedsReview {
				found = found || strings.Contains(w, test.WantNote)
			}
			if !found {
				t.Errorf("no review item says %q:\n%s", test.WantNote,
					strings.Join(report.NeedsReview, "\n"))
			}
			var out bytes.Buffer
			Render(&out, "awx", "export.json", plan.Assess())
			for _, line := range []string{"credential types", "does not come across"} {
				if !strings.Contains(out.String(), line) {
					t.Fatalf("the assessment has no %q line:\n%s", line, out.String())
				}
			}
			if !regexp.MustCompile(`credential types\s+1\b`).MatchString(out.String()) ||
				!regexp.MustCompile(`does not come across\s+0\b`).MatchString(out.String()) {
				t.Errorf("the assessment does not count the type as coming across:\n%s", out.String())
			}
		})
	}
}

// TestAWXKubeconfigTypeIsNamedWithTheSwitch proves the report names a type shaped like a kubeconfig
// and gives each of its credentials the one request that moves it to the built-in kind, while the
// import itself changes nothing: the type comes across as a custom type and its credentials stay
// typed, so they keep masking every line of the document until somebody switches one.
func TestAWXKubeconfigTypeIsNamedWithTheSwitch(t *testing.T) {
	t.Parallel()
	plan := credTypesPlan(t)
	byName := map[string]*credential.CredentialType{}
	for _, ct := range plan.CredentialTypes {
		byName[ct.Name] = ct
	}
	if kube := byName["Kubeconfig"]; kube == nil || len(kube.FileInjectors) != 1 {
		t.Fatalf("the kubeconfig type did not come across as a custom type: %+v", kube)
	}
	for _, c := range plan.Credentials {
		if c.Name == "prod-kube" && (c.TypeID != byName["Kubeconfig"].ID || c.Kind != "") {
			t.Errorf("prod-kube was converted on import: type %q, kind %q", c.TypeID, c.Kind)
		}
	}
	report := plan.Report()
	review := strings.Join(report.NeedsReview, "\n")
	for _, phrase := range []string{
		`credential type "Kubeconfig": it writes the kubeconfig in field "kube_config" to a file`,
		"mask every line of that document wherever a tool prints it",
		"masks only the secrets inside the document",
		"points KUBECONFIG, K8S_AUTH_KUBECONFIG, and KUBE_CONFIG_PATH at the file",
		`PUT /v1/credentials/{id} with its name, "kind": "kubeconfig", and the document as "secret"`,
		"Nothing switches unless you do it",
		`credential "prod-kube" is of the custom type "Kubeconfig" and needs its field values ` +
			`entered (kube_config)`,
		`credential "prod-kube" can switch to the built-in kubeconfig kind in the same request ` +
			`that enters its value, which masks only the secrets inside the document: PUT ` +
			`/v1/credentials/{id} with {"name": "prod-kube", "kind": "kubeconfig", "secret": ` +
			`"<the kubeconfig document>"}`,
	} {
		if !strings.Contains(review, phrase) {
			t.Errorf("the review items do not say %q:\n%s", phrase, review)
		}
	}
	// The certificate type writes two files, so it is no kubeconfig and neither it nor its
	// credential is offered the switch.
	for _, w := range append(report.NeedsReview, report.LeftOut...) {
		if (strings.Contains(w, "Client Certificate") || strings.Contains(w, "edge-cert")) &&
			strings.Contains(w, "kubeconfig") {
			t.Errorf("a type that is not a kubeconfig was offered the switch: %s", w)
		}
	}
	for _, w := range report.LeftOut {
		if strings.Contains(w, "kubeconfig kind") {
			t.Errorf("the kubeconfig note is counted as something left out: %s", w)
		}
	}
}

// TestKubeconfigSwitchNamesTheCredentialAsStored proves the request body in the switch carries the
// name the credential has here. Two organizations holding a credential of the same name import as
// org/name, and a body carrying the bare name would rename the credential it was meant to switch.
func TestKubeconfigSwitchNamesTheCredentialAsStored(t *testing.T) {
	t.Parallel()
	export := `{"credential_types": [{"name": "Kube", "kind": "cloud",
		"inputs": {"fields": [{"id": "kc", "label": "Kubeconfig", "secret": true, "multiline": true}]},
		"injectors": {"file": {"template": "{{ kc }}"}, "env": {"KUBECONFIG": "{{ tower.filename }}"}}}],
		"credentials": [
			{"name": "cluster", "organization": {"name": "Blue"},
				"credential_type": {"name": "Kube", "kind": "cloud"}, "inputs": {"kc": "$encrypted$"}},
			{"name": "cluster", "organization": {"name": "Green"},
				"credential_type": {"name": "Kube", "kind": "cloud"}, "inputs": {"kc": "$encrypted$"}}]}`
	plan, err := FromAWX([]byte(export), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	review := strings.Join(plan.Report().NeedsReview, "\n")
	for _, name := range []string{"Blue/cluster", "Green/cluster"} {
		body := `{"name": "` + name + `", "kind": "kubeconfig"`
		if !strings.Contains(review, body) {
			t.Errorf("no switch carries the stored name %q:\n%s", name, review)
		}
	}
}
