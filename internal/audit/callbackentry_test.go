package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestCallbackEntryVerifiesInLoomSeal checks the entry a provisioning callback writes, a host
// classified by the actor type host and named with the address it called from, is one the format's
// own verifier accepts when a receipt discloses it. The actor type is new to the chain, and a
// verifier that refused an unfamiliar one would make every host-initiated run unprovable.
func TestCallbackEntryVerifiesInLoomSeal(t *testing.T) {
	id := treeIdentity(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	var chain []*audit.Entry
	var prev *audit.Entry
	for i, e := range []*audit.Entry{
		{Actor: "deploy-session", ActorType: "session", Method: "POST",
			Path: "/v1/templates/tpl_boot/callback-key"},
		{Actor: "host web01 from 10.0.0.11", ActorType: "host", Method: "POST",
			Path: "/v1/templates/tpl_boot/callback/fired"},
		{Actor: "control-node", ActorType: "system", OnBehalfOf: "host web01 from 10.0.0.11",
			Method: audit.MethodRun, Path: "/runs/run_boot/outcome/succeeded"},
	} {
		e.ID = audit.NewID()
		e.At = at.Add(time.Duration(i) * time.Minute)
		audit.Link(prev, e)
		chain = append(chain, e)
		prev = e
	}
	doc, err := audit.BuildTreeBundle(chain, map[int64]bool{2: true, 3: true}, id, "v-test",
		audit.BundleSubject{Type: "run", ID: "run_boot"}, time.Now())
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	if !strings.Contains(string(signed), "host web01 from 10.0.0.11") {
		t.Fatal("the receipt does not carry the host and the address the callback came from")
	}
	report := verifyWithLoomSeal(t, signed)
	if !report.OK {
		t.Fatalf("loomseal refused a receipt carrying a callback entry: %v", report.Problems)
	}
}
