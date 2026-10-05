package migration

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// holdAnsible is a policy file that holds every Ansible run for a person, so a scenario can change
// the world between a launch and its execution.
const holdAnsible = `policies:
  - name: ansible-waits-for-a-person
    tool: ansible
`

// TestImportedComposedInventoriesRunOnTheHostsResolvedAtLaunch is scenario three. AWX exports a
// smart inventory and a constructed inventory over two ordinary ones. Each launch has to record the
// exact hosts it resolved to, reach only those, and keep to that set after an input inventory gains
// a host that matches: the run executes what was approved, not what the inputs hold by then.
func TestImportedComposedInventoriesRunOnTheHostsResolvedAtLaunch(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite, Policy: holdAnsible})
	s := in.startServer("a")
	fleet := in.lookup(s, "inventories", "fleet")
	edge := in.lookup(s, "inventories", "edge")

	tests := []struct {
		// Template launches against the composed inventory.
		Template string
		// Marker is the marker its play writes.
		Marker string
		// WantKind is the composed kind the run records.
		WantKind string
		// WantHosts are the hosts it resolves to at launch.
		WantHosts []string
		// WantInputs are the inputs that contributed hosts.
		WantInputs []string
		// WantEngine is the engine the resolution records: native for a smart inventory over
		// static documents, ansible for a constructed one.
		WantEngine string
		// WantChecked says the run is cross-checked against Ansible before its play, which is
		// what an Ansible run against a natively resolved inventory gets.
		WantChecked bool
	}{{
		Template: "smart mark", Marker: "smart", WantKind: "smart",
		WantHosts: []string{"edge1", "web1", "web2"}, WantInputs: []string{fleet, edge},
		WantEngine: "native", WantChecked: true,
	}, {
		Template: "constructed mark", Marker: "constructed", WantKind: "constructed",
		WantHosts: []string{"db1", "edge-canary"}, WantInputs: []string{fleet, edge},
		WantEngine: "ansible",
	}}

	var ids []string
	resolutions := map[string]map[string]any{}
	for _, test := range tests {
		rec := in.launched(s, "operator", test.Template, nil)
		held := in.waitStatus(s, rec.ID, "pending_approval")
		res, _ := held.Raw["inventory_resolution"].(map[string]any)
		resolutions[rec.ID] = res
		if got := str(res["kind"]); got != test.WantKind {
			t.Errorf("%s: resolution kind = %q, want %q", test.Template, got, test.WantKind)
		}
		// The engine follows the definition, and the evidence says which one resolved it, with
		// the ansible-core version when Ansible did and the digests of what it read and produced.
		if got := str(res["engine"]); got != test.WantEngine {
			t.Errorf("%s: resolution engine = %q, want %q", test.Template, got, test.WantEngine)
		}
		if got := str(res["ansible_core"]); (got != "") != (test.WantEngine == "ansible") {
			t.Errorf("%s: resolution ansible_core = %q for engine %s", test.Template, got,
				test.WantEngine)
		}
		for _, key := range []string{"input_digest", "resolved_digest"} {
			if !strings.HasPrefix(str(res[key]), "sha256:") {
				t.Errorf("%s: resolution %s = %q, want a sha256 digest", test.Template, key,
					str(res[key]))
			}
		}
		if diff := cmp.Diff(test.WantHosts, stringsOf(res["hosts"])); diff != "" {
			t.Errorf("%s: hosts resolved at launch (-want +got):\n%s", test.Template, diff)
		}
		// Inputs are recorded in the order they were read, which for inventories imported in one
		// instant is the order of their generated ids, so they are compared as a set.
		if diff := cmp.Diff(test.WantInputs, stringsOf(res["inputs"]),
			cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
			t.Errorf("%s: inputs resolved at launch (-want +got):\n%s", test.Template, diff)
		}
		ids = append(ids, rec.ID)
	}

	// Both inputs gain a host each composed inventory would now match, while both runs wait.
	in.addHosts(s, fleet, "web3 ansible_host=10.20.0.13", "web",
		"db2 ansible_host=10.20.0.22 state=shutdown", "db")
	preview := in.must(s, "operator", "POST", "/v1/inventories/"+in.lookup(s, "inventories",
		"web everywhere")+"/preview", nil, 200)
	if !strings.Contains(string(preview.Body), "web3") {
		t.Fatalf("the smart inventory's preview does not reach the host just added, so the "+
			"scenario would prove nothing: %s", preview.Body)
	}

	for i, test := range tests {
		in.must(s, "approver", "POST", "/v1/runs/"+ids[i]+"/approve", nil, 200)
		done := in.waitDone(s, ids[i])
		if done.Status != "succeeded" {
			t.Fatalf("%s = %s, want succeeded: %s", test.Template, done.Status, describe(done.Raw))
		}
		if diff := cmp.Diff(test.WantHosts, in.marked(test.Marker)); diff != "" {
			t.Errorf("%s: hosts the play reached (-want +got):\n%s", test.Template, diff)
		}
		// The smart run was checked against the real ansible-inventory before its play. It executes
		// the snapshot taken at launch and reads no input again, so both digests match the launch's
		// although an input gained a host while it waited.
		check, _ := done.Raw["inventory_check"].(map[string]any)
		if (check != nil) != test.WantChecked {
			t.Fatalf("%s: inventory_check = %v, want checked %v", test.Template, check,
				test.WantChecked)
		}
		if check == nil {
			continue
		}
		res := resolutions[ids[i]]
		if str(check["ansible_core"]) == "" || check["differences"] != nil {
			t.Errorf("%s: the cross-check did not agree cleanly: %v", test.Template, check)
		}
		if str(check["resolved_digest"]) != str(res["resolved_digest"]) {
			t.Errorf("%s: the cross-check read %s, the launch resolved %s", test.Template,
				str(check["resolved_digest"]), str(res["resolved_digest"]))
		}
		if str(check["input_digest"]) != str(res["input_digest"]) {
			t.Errorf("%s: the cross-check read inputs %s, the launch snapshotted %s, so the run read "+
				"an input again", test.Template, str(check["input_digest"]), str(res["input_digest"]))
		}
	}

	ev := in.checkEvidence(s, ids...)
	for i, test := range tests {
		rec := ev.Receipts[ids[i]]
		requireRecord(t, rec, recordWant{
			Launcher: "operator-laptop", OnBehalfOf: "operator", Approver: "approver-laptop",
			Playbook: "mark.yml", Hosts: test.WantHosts, Rules: []string{"ansible-waits-for-a-person"},
		})
		out := rec.outcome(t)
		res, _ := out.Spec["inventory_resolution"].(map[string]any)
		if diff := cmp.Diff(test.WantHosts, stringsOf(res["hosts"])); diff != "" {
			t.Errorf("%s: hosts the receipt's spec binds (-want +got):\n%s", test.Template, diff)
		}
		// The receipt binds the engine in the approved spec and the cross-check in the outcome.
		if got := str(res["engine"]); got != test.WantEngine {
			t.Errorf("%s: the receipt's spec names engine %q, want %q", test.Template, got,
				test.WantEngine)
		}
		if _, checked := out.Outcome["inventory_check"]; checked != test.WantChecked {
			t.Errorf("%s: the receipt's outcome carries a cross-check = %v, want %v", test.Template,
				checked, test.WantChecked)
		}
	}

	// A new launch resolves again and reaches the new host, which shows the first run left it out
	// because it ran the set it was approved for, not because the host could not be reached.
	in.clearMarkers()
	again := in.launched(s, "operator", "smart mark", nil)
	in.waitStatus(s, again.ID, "pending_approval")
	in.must(s, "approver", "POST", "/v1/runs/"+again.ID+"/approve", nil, 200)
	in.waitStatus(s, again.ID, "succeeded")
	if diff := cmp.Diff([]string{"edge1", "web1", "web2", "web3"}, in.marked("smart")); diff != "" {
		t.Errorf("hosts a fresh launch reached (-want +got):\n%s", diff)
	}
	in.checkEvidence(s, again.ID)
}

// addHosts adds hosts to a stored inventory, each pair naming a host line and the group it joins.
func (in *install) addHosts(s *server, invID string, pairs ...string) {
	in.t.Helper()
	var list struct {
		// Inventories are the stored inventories.
		Inventories []struct {
			// ID is the inventory's id.
			ID string `json:"id"`
			// Name is its name.
			Name string `json:"name"`
			// Content is its INI text.
			Content string `json:"content"`
		} `json:"inventories"`
	}
	in.must(s, "admin", "GET", "/v1/inventories", nil, 200).decode(in.t, &list)
	for _, inv := range list.Inventories {
		if inv.ID != invID {
			continue
		}
		content := inv.Content
		for i := 0; i+1 < len(pairs); i += 2 {
			line, group := pairs[i], pairs[i+1]
			host := strings.Fields(line)[0]
			content = line + "\n" + content
			content = strings.Replace(content, "["+group+"]\n", "["+group+"]\n"+host+"\n", 1)
		}
		in.must(s, "admin", "PUT", "/v1/inventories/"+invID,
			map[string]any{"name": inv.Name, "content": content}, 200)
		return
	}
	in.t.Fatalf("no inventory %s", invID)
}
