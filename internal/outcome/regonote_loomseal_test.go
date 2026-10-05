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

// TestAReceiptCarryingAPolicyNoteVerifiesInLoomSeal carries a run a Rego policy noted, without
// holding it, through the chain and out to the format's own verifier. Nothing approved the run, so
// the note in its outcome is the only place the evidence says it was warned about, and the receipt
// has to carry it under a signature LoomSeal accepts.
//
// The note and the rule set naming the setting are asserted inside the signed receipt before the
// verdict is read, because a receipt that withheld the outcome would still verify and prove nothing
// about the warning.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAReceiptCarryingAPolicyNoteVerifiesInLoomSeal(t *testing.T) {
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
		File: "advice.rego",
		Source: "package switchtender\n\n" +
			"warn contains \"no change ticket on the run\" if not input.run.labels.ticket\n",
	}}, policy.WithRegoWarn(policy.RegoWarnNote))
	if err != nil {
		t.Fatalf("CompileRego() error = %v", err)
	}
	set := []*policy.Policy{{
		ID: "pol_note", Name: "staging-advice", MaxDestroy: policy.DisabledMaxDestroy, Rego: prog,
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
		ID: "run_noted", Status: run.StatusSucceeded, CreatedAt: started, StartedAt: &started,
		EndedAt: &ended, Tool: run.ToolBash, Command: "uptime", Actor: "operator",
		ActorType: "session", ExitCode: &exit,
		AuditReceipt: fmt.Sprintf("%d:%s", creation.Seq, creation.Hash),
	}
	if held := policy.Requiring(set, r); held != nil {
		t.Fatalf("a noted warning held the run: %s", held.Label())
	}
	r.PolicyNotes = policy.Noting(set, r)
	if len(r.PolicyNotes) != 1 {
		t.Fatalf("notes = %v, want the one warning", r.PolicyNotes)
	}
	inForce := policy.InForce(set)
	r.PolicySet = &run.PolicySet{Digest: inForce.Digest, Count: inForce.Count, Rules: inForce.Rules}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := runs.AppendLog(ctx, r.ID, []byte("load average: 0.01\n")); err != nil {
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
	for _, want := range []string{
		`policy_notes`,
		"staging-advice (no change ticket on the run, rego sha256:" + prog.Digest()[:12] + ")",
		"bundle sha256:" + prog.Digest() + ", warnings noted without holding",
	} {
		if !strings.Contains(signed, want) {
			t.Fatalf("the signed receipt does not carry %q, so verifying it proves nothing about "+
				"the warning the run went ahead past", want)
		}
	}

	report := verifyWithLoomSeal(t, repo, res.Signed)
	if !report.OK {
		t.Fatalf("loomseal refused a receipt carrying a policy note: %v", report.Problems)
	}
}
