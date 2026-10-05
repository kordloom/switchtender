package migration

import (
	"os"
	"strings"
	"testing"
)

// TestImportedCredentialTypesKeepTheirShape is the scenario for two custom types an AWX estate
// brings that SwitchTender would not let anyone define the same way. One writes a kubeconfig to a
// file, the shape the built-in kubeconfig kind covers. The other writes a file and hands its path
// to nothing.
//
// Both import as they are. The report names the kubeconfig type with the one-step switch and the
// other with the file nothing references. A run of the kubeconfig credential masks every line of
// the document until an operator switches it, and masks only the token after. A run of the other
// carries a warning naming the file, never its contents, and the evidence still verifies offline.
func TestImportedCredentialTypesKeepTheirShape(t *testing.T) {
	t.Parallel()
	for _, store := range []storeKind{onSQLite, onPostgres} {
		t.Run(string(store), func(t *testing.T) {
			t.Parallel()
			testImportedCredentialTypes(t, store)
		})
	}
}

// testImportedCredentialTypes runs the scenario on one store.
func testImportedCredentialTypes(t *testing.T, store storeKind) {
	in := newInstall(t, installOptions{Store: store})

	// The preview an operator reads before applying names both types and what to do about each.
	preview := in.cli("import", "awx", renderFixture(t, t.TempDir(), "file:///unused", in.markers))
	report := strings.Join(strings.Fields(preview), " ")
	for _, phrase := range []string{
		`credential type "Cluster Kubeconfig": it writes the kubeconfig in field "kube_config" to a file`,
		`credential "cluster-kube" can switch to the built-in kubeconfig kind in the same request`,
		`{"name": "cluster-kube", "kind": "kubeconfig", "secret": "<the kubeconfig document>"}`,
		`credential type "Legacy Bundle": file template is written for each run, but no env or ` +
			`extra-var injector references its path`,
	} {
		if !strings.Contains(report, phrase) {
			t.Errorf("the import preview does not say %q:\n%s", phrase, preview)
		}
	}
	if strings.Contains(report, `credential type "Legacy Bundle" was not imported`) {
		t.Errorf("the type with a file nothing references was left out:\n%s", preview)
	}

	s := in.startServer("a")
	var ids []string

	// The bundle type: the run goes ahead, and its record and the server log name the file.
	bundle := "-----BEGIN BUNDLE-----\nbundle-" + randomHex(t, 12) + "\n-----END BUNDLE-----"
	in.addSecret(strings.Split(bundle, "\n")[1])
	in.fillCredential(s, "edge-bundle", map[string]string{"host": "edge.example.invalid",
		"bundle": bundle})
	rec := in.launched(s, "operator", "use bundle", nil)
	ids = append(ids, rec.ID)
	done := in.waitDone(s, rec.ID)
	if done.Status != "succeeded" {
		t.Fatalf("the bundle run = %s: %s", done.Status, describe(done.Raw))
	}
	if got := in.marked("bundle"); len(got) != 1 {
		t.Errorf("the bundle run marked %v, want web1", got)
	}
	warning := str(done.Raw["warning"])
	for _, phrase := range []string{
		`credential "edge-bundle" of type "Legacy Bundle" wrote file template`,
		"Hand the path over with {{ tower.filename }} on the type",
	} {
		if !strings.Contains(warning, phrase) {
			t.Errorf("the bundle run's warning %q does not say %q", warning, phrase)
		}
	}
	serverLog, err := os.ReadFile(s.logPath)
	if err != nil {
		t.Fatalf("read the server log: %v", err)
	}
	if !strings.Contains(string(serverLog), "credential type wrote a file no injector references") ||
		!strings.Contains(string(serverLog), rec.ID) {
		t.Errorf("the server log has no warning about the bundle run's file:\n%s", serverLog)
	}

	// The kubeconfig type, filled the way it was imported: every line of the document is masked.
	token := "kube-" + randomHex(t, 16)
	in.addSecret(token)
	kubeconfig := "apiVersion: v1\nkind: Config\nusers:\n- name: deployer\n  user:\n    token: " +
		token + "\n"
	in.fillCredential(s, "cluster-kube", map[string]string{"kube_config": kubeconfig})
	typed := in.kubeRunLog(s, &ids, "typed")
	if strings.Contains(typed, "kind: Config") {
		t.Errorf("the imported type showed the document's ordinary lines:\n%s", typed)
	}

	// The one-step switch from the report: the same request that enters the value, as the kind.
	credID := in.lookup(s, "credentials", "cluster-kube")
	in.must(s, "admin", "PUT", "/v1/credentials/"+credID, map[string]any{
		"name": "cluster-kube", "kind": "kubeconfig", "secret": kubeconfig,
	}, 200)
	var listing struct {
		// Credentials are every stored credential, secrets never included.
		Credentials []map[string]any `json:"credentials"`
	}
	in.must(s, "admin", "GET", "/v1/credentials", nil, 200).decode(t, &listing)
	for _, c := range listing.Credentials {
		if c["id"] == credID && (c["kind"] != "kubeconfig" || c["type_id"] != nil ||
			c["needs_secret"] != false) {
			t.Errorf("after the switch the credential is %s", describe(c))
		}
	}
	switched := in.kubeRunLog(s, &ids, "kind")
	if !strings.Contains(switched, "kind: Config") {
		t.Errorf("the kubeconfig kind still hides the document's ordinary lines:\n%s", switched)
	}

	in.checkEvidence(s, ids...)
}

// kubeRunLog launches the kubeconfig template, requires it to succeed, records its id in ids, and
// returns its log. The token must be masked in every case, which the leak scan checks as well.
func (in *install) kubeRunLog(s *server, ids *[]string, label string) string {
	in.t.Helper()
	rec := in.launched(s, "operator", "show kubeconfig", map[string]any{
		"extra_vars": map[string]any{"marker_name": "kube-" + label},
	})
	*ids = append(*ids, rec.ID)
	done := in.waitDone(s, rec.ID)
	if done.Status != "succeeded" {
		in.t.Fatalf("the %s kubeconfig run = %s: %s", label, done.Status, describe(done.Raw))
	}
	if got := in.marked("kube-" + label); len(got) != 1 {
		in.t.Errorf("the %s kubeconfig run marked %v, want web1", label, got)
	}
	body := string(in.must(s, "admin", "GET", "/v1/runs/"+rec.ID+"/logs", nil, 200).Body)
	if !strings.Contains(body, "Print the kubeconfig") {
		in.t.Fatalf("the %s kubeconfig run's log does not show the print task:\n%s", label, body)
	}
	return body
}
