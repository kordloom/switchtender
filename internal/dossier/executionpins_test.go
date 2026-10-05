package dossier

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestRunMetaStatesWhatTheApprovalBound pins that the evidence states what an approval bound about
// where a run executed and what it reached: whether its image was pinned to a digest, the digest the
// runtime pulled, the inventory as submitted or a dynamic source with the hosts it resolved to at
// execution, and the saved plan a gated apply carried out.
func TestRunMetaStatesWhatTheApprovalBound(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		// Run is the run described.
		Run *run.Run
		// Key is the row checked, WantRow what it must begin with.
		Key, WantRow string
	}{{ // Test 0: A tag the registry did not resolve says so.
		Run: &run.Run{Image: "reg.example.com/runner:2"}, Key: "Image",
		WantRow: "reg.example.com/runner:2 (tag not pinned to a digest)",
	}, { // Test 1: A pinned image says so.
		Run: &run.Run{Image: "reg.example.com/runner:2@" + digest}, Key: "Image",
		WantRow: "reg.example.com/runner:2@" + digest + " (pinned to a digest)",
	}, { // Test 2: The digest the runtime pulled.
		Run: &run.Run{Image: "reg.example.com/runner:2", ImageDigest: digest},
		Key: "Pulled image digest", WantRow: digest,
	}, { // Test 3: A static snapshot names its hosts.
		Run: &run.Run{InventorySnapshot: &run.InventorySnapshot{Hosts: []string{"web1", "web2"}}},
		Key: "Inventory snapshot", WantRow: "2 host(s) as submitted: web1, web2",
	}, { // Test 4: A dynamic source says its hosts resolve at execution.
		Run: &run.Run{InventorySnapshot: &run.InventorySnapshot{Dynamic: true}},
		Key: "Inventory snapshot", WantRow: "a dynamic source: its hosts resolve at execution",
	}, { // Test 5: And then what it resolved to.
		Run: &run.Run{ResolvedHosts: []string{"ec2-a", "ec2-b"}},
		Key: "Resolved at execution", WantRow: "ec2-a, ec2-b",
	}, { // Test 6: A gated apply names the saved plan it carried out.
		Run: &run.Run{PlanSHA256: strings.Repeat("b", 64)}, Key: "Plan file",
		WantRow: "applies the saved plan sha256:bbbbbbbbbbbb",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Key), func(t *testing.T) {
			t.Parallel()
			r := test.Run
			r.ID, r.Playbook, r.CreatedAt = "run_pins", "site.yml", evidenceTime
			got := ""
			for _, row := range runMeta(r) {
				if row.K == test.Key {
					got = row.V
				}
			}
			if !strings.HasPrefix(got, test.WantRow) {
				t.Errorf("%s = %q, want it to begin %q", test.Key, got, test.WantRow)
			}
		})
	}
}
