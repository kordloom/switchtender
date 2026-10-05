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

// TestAReceiptForASecretAnswerVerifiesAndDisclosesOnlyTheName holds the run's spec record to what a
// secret survey answer may put on the chain: the name of the variable it was supplied for, never
// its value and never its sealed form. The receipt disclosing that record is checked with the
// format's own verifier, since the field is a change to what that tool is handed.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAReceiptForASecretAnswerVerifiesAndDisclosesOnlyTheName(t *testing.T) {
	const sealed = "c2VhbGVkLWNpcGhlcnRleHQtZm9yLXRoZS1hbnN3ZXI="
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "ops-user", ActorType: "session",
		Method: "POST", Path: "/v1/templates/tpl_1/launch",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	ended := time.Now()
	exit := 0
	r := &run.Run{
		ID: "run_secret", Status: run.StatusSucceeded, CreatedAt: time.Now(), EndedAt: &ended,
		Tool: run.ToolBash, Command: "rotate", ExitCode: &exit,
		Actor: "ops-user", ActorType: "session", ExtraVars: map[string]any{"env": "prod"},
		AuditReceipt: fmt.Sprintf("%d:%s", creation.Seq, creation.Hash),
	}
	run.WithSealedVars(map[string]string{"db_password": sealed})(r)
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	spec, err := outcome.Spec(r)
	if err != nil {
		t.Fatalf("Spec() error = %v", err)
	}
	if !strings.Contains(string(spec), `"sealed_vars":["db_password"]`) ||
		strings.Contains(string(spec), sealed) {
		t.Fatalf("spec record = %s, want the answer's name and not its sealed form", spec)
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
	if strings.Contains(string(res.Signed), sealed) {
		t.Fatal("the signed receipt carries the sealed answer")
	}
	if !strings.Contains(string(res.Signed), "db_password") {
		t.Fatal("the signed receipt does not say a secret answer was supplied, so verifying it " +
			"would prove nothing about that record")
	}
	// The receipt binds which sealed answer ran by the digest of its ciphertext, so verifying it
	// proves that too, and the digest is all of the answer it carries.
	if len(r.SealedDigests) != 1 || !strings.Contains(string(res.Signed), r.SealedDigests[0].SHA256) {
		t.Fatalf("the signed receipt does not carry the sealed answer's digest %v, so verifying it "+
			"would prove nothing about which answer was approved", r.SealedDigests)
	}

	report := verifyWithLoomSeal(t, loomsealRepo(t), res.Signed)
	if !report.OK {
		t.Fatalf("loomseal refused a receipt for a run with a secret answer: %v", report.Problems)
	}
}
