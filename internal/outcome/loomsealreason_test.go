package outcome_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/outcometest"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAReceiptDisclosingAFailureReasonVerifiesInLoomSeal is the cross-verification the failure
// reason has to survive. The record is not this product's private structure: it is disclosed inside
// a signed bundle a relying party checks with the format's own tool, so a field added to it is a
// change to what that tool is handed.
//
// The receipt built here is for the run the field exists for, failed at exit code 0 with the reason
// naming what went wrong, and it is verified by shelling out to the loomseal command rather than by
// importing a verifier, for the same reason the cross-check in internal/audit does: a verifier
// vendored into this repository would drift toward whatever this repository emits.
//
// The disclosure is asserted before the verdict. A receipt whose outcome could not be rebuilt
// withholds the body and says so in a note, and it would still verify, so checking only the verdict
// would pass over the case where the reason never reached the bundle at all.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with,
// and t.Setenv and t.Parallel cannot both apply to one test.
func TestAReceiptDisclosingAFailureReasonVerifiesInLoomSeal(t *testing.T) {
	repo := outcometest.LoomsealRepo(t)
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}

	// The request that created the run, and the receipt tying the run to it. Without both, a receipt
	// cannot place the run's start on the chain and never gets as far as the outcome.
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "casey", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}

	ended := time.Now()
	exit := 0
	r := &run.Run{
		ID: "run_zero_host", Status: run.StatusFailed, CreatedAt: time.Now(), EndedAt: &ended,
		Tool: run.ToolAnsible, Playbook: "site.yml", Inventory: "inv",
		Actor: "casey", ActorType: "session", ExitCode: &exit, Error: zeroHostReason,
		AuditReceipt: fmt.Sprintf("%d:%s", creation.Seq, creation.Hash),
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := runs.AppendLog(ctx, r.ID, []byte("PLAY RECAP ****\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
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
	if !strings.Contains(string(res.Signed), "no host was touched") {
		t.Fatal("the signed receipt does not disclose the reason the run failed, so verifying it " +
			"would prove nothing about the reason")
	}

	report := outcometest.VerifyWithLoomSeal(t, repo, res.Signed)
	if !report.OK {
		t.Fatalf("loomseal refused a receipt disclosing a run's failure reason: %v", report.Problems)
	}
}
