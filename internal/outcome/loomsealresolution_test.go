package outcome_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAReceiptDisclosingResolvedHostsVerifiesInLoomSeal is the cross-verification for the host set a
// smart or constructed inventory resolved to. The set is part of the spec a receipt discloses, so a
// receipt for a run against a composed inventory carries the machines it was launched against, and
// the format's own verifier has to accept that body and the digests over it.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAReceiptDisclosingResolvedHostsVerifiesInLoomSeal(t *testing.T) {
	repo := loomsealRepo(t)
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "operator", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	ended := time.Now()
	exit := 0
	r := &run.Run{
		ID: "run_composed", Status: run.StatusSucceeded, CreatedAt: time.Now(), EndedAt: &ended,
		Tool: run.ToolAnsible, Playbook: "site.yml", InventoryID: "inv_smart", ExitCode: &exit,
		Actor: "operator", ActorType: "session",
		AuditReceipt: fmt.Sprintf("%d:%s", creation.Seq, creation.Hash),
		InventoryResolution: &run.InventoryResolution{
			Kind: "smart", Inputs: []string{"inv_web"}, Hosts: []string{"web1", "web2"},
			Engine: "native", InputDigest: "sha256:" + strings.Repeat("a", 64),
			ResolvedDigest: "sha256:" + strings.Repeat("b", 64),
		},
		InventoryCheck: &run.InventoryCheck{AnsibleCore: "2.18.1",
			InputDigest:    "sha256:" + strings.Repeat("a", 64),
			ResolvedDigest: "sha256:" + strings.Repeat("b", 64)},
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := outcome.Commit(ctx, audits, runs, r, "system:test", nil); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	res, err := receipt.Build(ctx, runs, audits, id, "v-test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	if len(res.Notes) > 0 {
		t.Fatalf("the receipt withheld its outcome: %v", res.Notes)
	}
	for _, host := range []string{"web1", "web2"} {
		if !strings.Contains(string(res.Signed), host) {
			t.Fatalf("the signed receipt does not disclose the resolved host %s", host)
		}
	}
	// The engine, the digests, and the cross-check are evidence too, so the receipt discloses them.
	for _, part := range []string{"input_digest", "resolved_digest", "inventory_check", "2.18.1"} {
		if !strings.Contains(string(res.Signed), part) {
			t.Fatalf("the signed receipt does not disclose %s", part)
		}
	}
	report := verifyWithLoomSeal(t, repo, res.Signed)
	if !report.OK {
		t.Fatalf("loomseal refused a receipt disclosing a composed inventory's hosts: %v",
			report.Problems)
	}
}
