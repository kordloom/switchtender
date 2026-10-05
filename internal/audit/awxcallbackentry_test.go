package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestAWXCallbackEntryVerifiesInLoomSeal checks the entry a provisioning callback writes when it
// arrives on the AWX-compatible address, whose path names the AWX job template id it called, is one
// the format's own verifier accepts when a receipt discloses it. The path is how the evidence says
// which address a host used, so a receipt that could not carry it would hide exactly that.
func TestAWXCallbackEntryVerifiesInLoomSeal(t *testing.T) {
	id := treeIdentity(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	var chain []*audit.Entry
	var prev *audit.Entry
	for i, e := range []*audit.Entry{
		{Actor: "host web01 from 10.0.0.11", ActorType: "host", Method: "POST",
			Path: "/v1/templates/tpl_boot/callback/fired/awx/42"},
		{Actor: "control-node", ActorType: "system", OnBehalfOf: "host web01 from 10.0.0.11",
			Method: audit.MethodRun, Path: "/runs/run_boot/outcome/succeeded"},
	} {
		e.ID = audit.NewID()
		e.At = at.Add(time.Duration(i) * time.Minute)
		audit.Link(prev, e)
		chain = append(chain, e)
		prev = e
	}
	doc, err := audit.BuildTreeBundle(chain, map[int64]bool{1: true, 2: true}, id, "v-test",
		audit.BundleSubject{Type: "run", ID: "run_boot"}, time.Now())
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	if !strings.Contains(string(signed), "/callback/fired/awx/42") {
		t.Fatal("the receipt does not carry the AWX-compatible address the callback arrived on")
	}
	report := verifyWithLoomSeal(t, signed)
	if !report.OK {
		t.Fatalf("loomseal refused a receipt carrying an AWX callback entry: %v", report.Problems)
	}
}
