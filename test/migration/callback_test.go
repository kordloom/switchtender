package migration

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// awxTemplateList is AWX's job template list for the fixture, which gives the provision template
// the AWX id its hosts' boot scripts call.
const awxTemplateList = "testdata/awx-job-templates.json"

// awxProvisionID is the AWX job template id the fixture's provision template had in AWX.
const awxProvisionID = "7"

// callbackPolicy holds every host-initiated run for a person, written in Rego so the gate a
// callback passes through is the one a migrated OPA rule would be.
const callbackPolicy = `rego:
  - name: callbacks-wait
    files: [rego/callbacks.rego]
`

// callbackRego is the module callbackPolicy loads.
const callbackRego = `package switchtender

hold contains "a host called back, and a person confirms what it provisions" if {
	input.run.source == "callback"
}
`

// TestImportedProvisioningCallbackIsGatedAndNamesTheHost is scenario seven. AWX exports a template
// that accepts provisioning callbacks, with its host config key. A host calling back with that key
// from its own address has to get a run that the approval policy holds, that after approval runs on
// that host alone, and whose record names the host and the address it called from. A caller the
// inventory does not hold, or one with the wrong key, gets nothing.
//
// The import is also given AWX's job template list, so a host whose image still posts to the
// template's old AWX address, the way AWX's boot script does, reaches the same template through the
// same gate, and the evidence says which address it used.
//
// It runs on SQLite and on PostgreSQL, the backend high availability runs on, where a second server
// shares the database and the repeated callback reaches that one, so the refusal holds across
// replicas rather than inside one process.
func TestImportedProvisioningCallbackIsGatedAndNamesTheHost(t *testing.T) {
	t.Parallel()
	for _, store := range []storeKind{onSQLite, onPostgres} {
		t.Run(string(store), func(t *testing.T) {
			t.Parallel()
			testImportedProvisioningCallback(t, store)
		})
	}
}

// testImportedProvisioningCallback is scenario seven on one backend.
func testImportedProvisioningCallback(t *testing.T, store storeKind) {
	list, err := filepath.Abs(awxTemplateList)
	if err != nil {
		t.Fatalf("locate the AWX template list: %v", err)
	}
	in := newInstall(t, installOptions{Store: store, Policy: callbackPolicy,
		Rego:       map[string]string{"rego/callbacks.rego": callbackRego},
		ImportArgs: []string{"--awx-template-ids", list}})
	s := in.startServer("a", "--trusted-proxy", "127.0.0.1/32")
	replica := s
	if store == onPostgres {
		replica = in.startServer("b", "--trusted-proxy", "127.0.0.1/32")
	}
	tpl := in.template(s, "provision")
	callbackOn := func(srv *server, addr, key string) response {
		return in.apiHeaders(srv, "", "POST", "/v1/templates/"+tpl+"/callback",
			map[string]any{"host_config_key": key}, map[string]string{"X-Forwarded-For": addr})
	}
	callback := func(addr, key string) response { return callbackOn(s, addr, key) }

	if r := callback("10.99.0.1", hostConfigKey); r.Status != 400 {
		t.Errorf("a callback from an address the inventory does not hold = %d, want 400: %s",
			r.Status, r.Body)
	}
	if r := callback("10.20.0.12", "hck-not-the-key-at-all-000000"); r.Status != 403 {
		t.Errorf("a callback with the wrong key = %d, want 403: %s", r.Status, r.Body)
	}
	if runs := in.runsFrom(s, tpl); len(runs) != 0 {
		t.Fatalf("refused callbacks created runs: %v", runs)
	}

	r := callback("10.20.0.12", hostConfigKey)
	if r.Status != 201 {
		t.Fatalf("the host's callback = %d, want 201: %s", r.Status, r.Body)
	}
	runs := in.runsFrom(s, tpl)
	if len(runs) != 1 {
		t.Fatalf("the callback created %d runs, want one", len(runs))
	}
	id := runs[0].ID
	if !strings.HasSuffix(r.Header.Get("Location"), id) {
		t.Errorf("the callback's Location %q does not name the run %s", r.Header.Get("Location"), id)
	}
	held := in.waitStatus(s, id, "pending_approval")
	if got := str(held.Raw["limit"]); got != "web2" {
		t.Errorf("the callback run's limit = %q, want the matched host alone", got)
	}
	if got := str(held.Raw["source"]); got != "callback" {
		t.Errorf("the callback run's source = %q, want callback", got)
	}
	actor := str(held.Raw["actor"])
	if !strings.Contains(actor, "web2") || !strings.Contains(actor, "10.20.0.12") {
		t.Errorf("the callback run's actor = %q, want the host and the address it called from", actor)
	}
	if !strings.Contains(describe(held.Raw), "callbacks-wait") {
		t.Errorf("the held callback run does not name the policy that held it: %s", describe(held.Raw))
	}
	if again := callbackOn(replica, "10.20.0.12", hostConfigKey); again.Status != 409 {
		t.Errorf("a second callback while the first is held = %d, want 409: %s", again.Status,
			again.Body)
	}
	if got := in.marked("provision"); len(got) != 0 {
		t.Fatalf("the held callback run executed before anyone approved it: %v", got)
	}

	in.must(s, "approver", "POST", "/v1/runs/"+id+"/approve", nil, 200)
	if done := in.waitDone(s, id); done.Status != "succeeded" {
		t.Fatalf("the approved callback run = %s: %s", done.Status, describe(done.Raw))
	}
	if diff := cmp.Diff([]string{"web2"}, in.marked("provision")); diff != "" {
		t.Errorf("hosts the callback run reached (-want +got):\n%s", diff)
	}

	awxID, awxActor := in.callBackThroughAWX(s, tpl)

	ev := in.checkEvidence(s, id, awxID)
	rec := ev.Receipts[id]
	requireRecord(t, rec, recordWant{
		Launcher: actor, LauncherType: "host", Approver: "approver-laptop", Playbook: "mark.yml",
		Hosts: []string{"web2"}, Rules: []string{"callbacks-wait: decided by Rego package"},
	})
	if got := str(rec.outcome(t).Spec["limit"]); got != "web2" {
		t.Errorf("the receipt binds limit %q, want web2", got)
	}
	if launch := rec.launch(t); launch.Path != "/v1/templates/"+tpl+"/callback/fired" {
		t.Errorf("the native callback was recorded at %s, want the native address", launch.Path)
	}
	awxRec := ev.Receipts[awxID]
	requireRecord(t, awxRec, recordWant{
		Launcher: awxActor, LauncherType: "host", Approver: "approver-laptop", Playbook: "mark.yml",
		Hosts: []string{"web1"}, Rules: []string{"callbacks-wait: decided by Rego package"},
	})
	want := "/v1/templates/" + tpl + "/callback/fired/awx/" + awxProvisionID
	if launch := awxRec.launch(t); launch.Path != want {
		t.Errorf("the AWX callback was recorded at %s, want %s", launch.Path, want)
	}
}

// callBackThroughAWX is the half of scenario seven a host built for AWX drives: its boot script
// posts the key as a form to the template's old AWX address. The import report names that address,
// the template page shows it, an id nothing is bound to answers exactly as a wrong key does, and a
// GET is refused. The callback itself is held like any other, executes only on the calling host
// once approved, and the template then shows when the address was last called. It returns the run
// and the actor the callback recorded.
func (in *install) callBackThroughAWX(s *server, tpl string) (string, string) {
	t := in.t
	t.Helper()
	list, err := filepath.Abs(awxTemplateList)
	if err != nil {
		t.Fatalf("locate the AWX template list: %v", err)
	}
	address := "/api/v2/job_templates/" + awxProvisionID + "/callback/"
	preview := in.cli("import", "awx", filepath.Join(in.root, "awx-export.json"),
		"--awx-template-ids", list, "--db", filepath.Join(in.root, "preview.db"))
	if !strings.Contains(preview, address) {
		t.Errorf("the import report does not name the AWX-compatible address %s:\n%s", address,
			preview)
	}
	served := func() map[string]any {
		t.Helper()
		var page struct {
			// Templates are the served templates.
			Templates []map[string]any `json:"templates"`
		}
		in.must(s, "admin", "GET", "/v1/templates", nil, 200).decode(t, &page)
		for _, x := range page.Templates {
			if x["id"] == tpl {
				return x
			}
		}
		t.Fatalf("the template %s is not served", tpl)
		return nil
	}
	if got := served(); str(got["awx_job_template_id"]) != awxProvisionID ||
		got["awx_callback"] != true || got["awx_callback_called_at"] != nil {
		t.Errorf("the template shows %v, want AWX id %s, the address on, and never called",
			got, awxProvisionID)
	}
	post := func(path, addr, key string) response {
		return in.apiHeaders(s, "", "POST", path, "host_config_key="+key, map[string]string{
			"Content-Type": "application/x-www-form-urlencoded", "X-Forwarded-For": addr})
	}
	unknown := post("/api/v2/job_templates/8000/callback/", "10.20.0.21", hostConfigKey)
	wrong := post(address, "10.20.0.21", "hck-not-the-key-at-all-000000")
	if unknown.Status != 403 || wrong.Status != 403 || string(unknown.Body) != string(wrong.Body) {
		t.Errorf("an unknown AWX id = %d %s and a wrong key = %d %s, want the same 403",
			unknown.Status, unknown.Body, wrong.Status, wrong.Body)
	}
	if get := in.api(s, "admin", "GET", address, nil); get.Status != 405 {
		t.Errorf("a GET of the AWX-compatible address = %d, want 405", get.Status)
	}

	r := post(address, "10.20.0.11", hostConfigKey)
	if r.Status != 201 {
		t.Fatalf("the host's callback through the AWX address = %d, want 201: %s", r.Status, r.Body)
	}
	var created struct {
		// Run is the run the callback created.
		Run string `json:"run"`
	}
	if err := json.Unmarshal(r.Body, &created); err != nil || created.Run == "" {
		t.Fatalf("decode the callback answer %s: %v", r.Body, err)
	}
	held := in.waitStatus(s, created.Run, "pending_approval")
	if got := str(held.Raw["limit"]); got != "web1" {
		t.Errorf("the AWX callback run's limit = %q, want the matched host alone", got)
	}
	if again := post(address, "10.20.0.11", hostConfigKey); again.Status != 409 {
		t.Errorf("a second AWX callback while the first is held = %d, want 409: %s", again.Status,
			again.Body)
	}
	in.must(s, "approver", "POST", "/v1/runs/"+created.Run+"/approve", nil, 200)
	if done := in.waitDone(s, created.Run); done.Status != "succeeded" {
		t.Fatalf("the approved AWX callback run = %s: %s", done.Status, describe(done.Raw))
	}
	if diff := cmp.Diff([]string{"web1", "web2"}, in.marked("provision")); diff != "" {
		t.Errorf("hosts the two callback runs reached (-want +got):\n%s", diff)
	}
	if got := served(); got["awx_callback_called_at"] == nil {
		t.Errorf("the template does not show when its AWX address was last called: %v", got)
	}
	return created.Run, str(held.Raw["actor"])
}
