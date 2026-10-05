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

// TestAReceiptDisclosingADryRunScanVerifiesInLoomSeal is the cross-verification for the gate's scan
// of a dry run. The scan is part of the outcome record, an array of objects carrying the scanner,
// its version, the files read, what was found, the classification, and the module download the gate
// ran first with its exit status, so a receipt for a plan the gate held for an external data source
// discloses it, and the format's own verifier has to accept that body and the digests over it.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAReceiptDisclosingADryRunScanVerifiesInLoomSeal(t *testing.T) {
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
	finding := "module.net.data.external.lookup runs a program during plan " +
		"(infra/.terraform/modules/net/main.tf line 3)"
	r := &run.Run{
		ID: "run_scanned", Status: run.StatusSucceeded, CreatedAt: time.Now(), EndedAt: &ended,
		Tool: run.ToolTerraform, Command: "infra", DryRun: true, ExitCode: &exit,
		Actor: "operator", ActorType: "session",
		AuditReceipt: fmt.Sprintf("%d:%s", creation.Seq, creation.Hash),
		DryRunScans: []run.DryRunScan{run.DryRunScan{
			Tool: run.ToolTerraform, Scanner: "terraform-external", Version: 1,
			Source:   "read at commit 0123456789ab, the commit this run is pinned to",
			Inputs:   []string{"infra/.terraform/modules/net/main.tf", "infra/main.tf"},
			Findings: []string{finding},
			Fetch:    &run.ModuleFetch{Command: "terraform get", ExitStatus: 0},
		}.Classified()},
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
	for _, want := range []string{"dry_run_scans", "terraform-external", "not_change_free",
		"module.net.data.external.lookup", "terraform get", "exit_status"} {
		if !strings.Contains(string(res.Signed), want) {
			t.Fatalf("the signed receipt does not disclose %s", want)
		}
	}
	report := verifyWithLoomSeal(t, repo, res.Signed)
	if !report.OK {
		t.Fatalf("loomseal refused a receipt disclosing a dry-run scan: %v", report.Problems)
	}
}
