package outcome_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAReceiptNamingTheRegoBundleThatDecidedVerifiesInLoomSeal carries a run decided by a Rego
// policy through the chain and out to the format's own verifier. The run was held by the policy,
// approved, and executed, so the chain holds its request, its decision, and its outcome, and the
// outcome record names the exact Rego bundle that was in force by its full digest.
//
// The digest is asserted inside the signed receipt before the verdict is read, because a receipt
// that withheld the outcome would still verify and prove nothing about which policy decided.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAReceiptNamingTheRegoBundleThatDecidedVerifiesInLoomSeal(t *testing.T) {
	repo := loomsealRepo(t)
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}

	prog, err := policy.CompileRego("", "", []policy.RegoModule{{
		File: "guardrails.rego",
		Source: "package switchtender\n\n" +
			"hold contains \"terraform needs a person\" if input.run.tool == \"terraform\"\n",
	}})
	if err != nil {
		t.Fatalf("CompileRego() error = %v", err)
	}
	set := []*policy.Policy{{
		ID: "pol_rego", Name: "guardrails", MaxDestroy: policy.DisabledMaxDestroy, Rego: prog,
	}}

	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "operator", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	started := time.Now().Add(-time.Minute)
	ended := time.Now()
	exit := 0
	r := &run.Run{
		ID: "run_rego", Status: run.StatusSucceeded, CreatedAt: started, StartedAt: &started,
		EndedAt: &ended, Tool: run.ToolTerraform, Command: "infra/prod", Actor: "operator",
		ActorType: "session", ExitCode: &exit,
		AuditReceipt: fmt.Sprintf("%d:%s", creation.Seq, creation.Hash),
	}
	held := policy.Requiring(set, r)
	if held == nil {
		t.Fatal("the Rego policy did not hold the run it was written for")
	}
	r.HeldByPolicy = held.Label()
	inForce := policy.InForce(set)
	r.PolicySet = &run.PolicySet{Digest: inForce.Digest, Count: inForce.Count, Rules: inForce.Rules}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := outcome.CommitDecision(ctx, audits, r, "approved",
		outcome.Decider{Name: "approver", Type: "session"}, time.Now); err != nil {
		t.Fatalf("CommitDecision() error = %v", err)
	}
	if err := runs.AppendLog(ctx, r.ID, []byte("Apply complete!\n")); err != nil {
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
		t.Fatalf("the receipt withheld part of the record: %v", res.Notes)
	}
	signed := string(res.Signed)
	if !strings.Contains(signed, "bundle sha256:"+prog.Digest()) {
		t.Fatal("the signed receipt does not name the Rego bundle that was in force, so verifying " +
			"it proves nothing about which policy decided")
	}
	if !strings.Contains(signed, "decision_body") {
		t.Fatal("the signed receipt does not disclose the approval decision")
	}

	report := verifyWithLoomSeal(t, repo, res.Signed)
	if !report.OK {
		t.Fatalf("loomseal refused a receipt naming a Rego bundle: %v", report.Problems)
	}
}
