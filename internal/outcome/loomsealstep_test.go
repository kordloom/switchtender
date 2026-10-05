package outcome_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// gatedReceipt runs a workflow with one approval step through a real dispatcher and a real chain,
// approves the step, and returns what a receipt needs once the workflow has finished.
func gatedReceipt(t *testing.T) (run.Store, audit.Store, audit.Identity, string) {
	t.Helper()
	return gatedReceiptWithStep(t, "gate")
}

// gatedReceiptWithStep is gatedReceipt with the approval step named gate, so a test can put text in
// the step name that the redaction rewrites.
func gatedReceiptWithStep(t *testing.T, gate string) (run.Store, audit.Store, audit.Identity, string) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "requester", ActorType: "session",
		Method: "POST", Path: "/v1/pipelines",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := dispatch.New(runs, runner, nil, dispatch.WithAudits(audits), dispatch.WithNoJanitor())
	defer d.Close()
	parent, err := d.SubmitPipeline(ctx, "release", "", []run.PipelineStep{
		{Name: "build", Tool: run.ToolBash, Command: "make"},
		{Name: gate, Type: run.StepApproval, Description: "Ship it?", DependsOn: []string{"build"}},
		{Name: "deploy", Tool: run.ToolBash, Command: "ship", DependsOn: []string{gate}},
	}, run.WithActor("requester"), run.WithAuditReceiptOf(fmt.Sprintf("%d:%s", creation.Seq,
		creation.Hash)))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	var node *run.Run
	deadline := time.Now().Add(5 * time.Second)
	for node == nil && time.Now().Before(deadline) {
		pending, perr := run.PendingApprovalSteps(ctx, runs, parent.ID)
		if perr != nil {
			t.Fatalf("PendingApprovalSteps() error = %v", perr)
		}
		if len(pending) == 1 {
			node = pending[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	if node == nil {
		t.Fatal("the workflow never reached its approval step")
	}
	if _, err := d.DecideStep(ctx, node.ID, dispatch.StepDecision{Approve: true,
		By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	for time.Now().Before(deadline.Add(5 * time.Second)) {
		got, gerr := runs.Get(ctx, parent.ID)
		if gerr == nil && got.Status.Terminal() {
			if got.Status != run.StatusSucceeded {
				t.Fatalf("workflow status = %q (%s), want succeeded", got.Status, got.Error)
			}
			return runs, audits, id, parent.ID
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the approved workflow never finished")
	return nil, nil, id, ""
}

// TestAWorkflowReceiptDisclosesItsApprovalStep pins the evidence an approval step leaves. The
// workflow's receipt discloses the step's request and its approval beside the digests the chain
// committed, each binding the workflow's own spec digest, and the approval reads as recorded before
// the workflow's outcome. A step record that changes after the fact rebuilds a body the chain
// refuses, so the receipt reports the tamper rather than vouching for it.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAWorkflowReceiptDisclosesItsApprovalStep(t *testing.T) {
	tests := []struct {
		// Tamper rewrites an upstream step's output after the approval.
		Tamper bool
		// WantDecisions reports whether the disclosed decisions verify.
		WantDecisions bool
	}{{ // Test 0: An untouched workflow's receipt verifies with both step entries disclosed.
		Tamper: false, WantDecisions: true,
	}, { // Test 1: A step output rewritten after the approval breaks the disclosed decision.
		Tamper: true, WantDecisions: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			ctx := context.Background()
			runs, audits, id, runID := gatedReceipt(t)
			if test.Tamper {
				steps, err := runs.Steps(ctx, runID)
				if err != nil {
					t.Fatalf("Steps() error = %v", err)
				}
				for _, s := range steps {
					if s.StepName == "build" {
						s.Outputs = map[string]any{"artifact": "a different build"}
						if err := runs.Save(ctx, s); err != nil {
							t.Fatalf("Save() error = %v", err)
						}
					}
				}
			}
			res, err := receipt.Build(ctx, runs, audits, id, "v-test", runID, receipt.Options{})
			if err != nil {
				t.Fatalf("receipt.Build() error = %v", err)
			}
			rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
			if err != nil {
				t.Fatalf("VerifyBundle() error = %v", err)
			}
			if rep.DecisionsOK != test.WantDecisions {
				t.Errorf("decisions ok = %v, want %v", rep.DecisionsOK, test.WantDecisions)
			}
			if test.Tamper {
				return
			}
			if !rep.OK() || !rep.SpecConsistent || !rep.ApprovalPrecedesRun {
				t.Fatalf("the workflow receipt does not verify: %+v", rep)
			}
			verdicts := map[string]bool{}
			for _, d := range rep.Decisions {
				verdicts[d.Verdict] = true
			}
			if rep.DecisionsPresent != 2 || !verdicts[outcome.StepRequested] ||
				!verdicts[outcome.StepApproved] {
				t.Errorf("disclosed decisions = %+v, want the step's request and its approval",
					rep.Decisions)
			}
		})
	}
}

// TestAWorkflowReceiptWithAnApprovalStepVerifiesInLoomSeal cross-verifies the same receipt with the
// format's own verifier, since the step entries are new claims inside a signed bundle a relying
// party checks with that tool.
func TestAWorkflowReceiptWithAnApprovalStepVerifiesInLoomSeal(t *testing.T) {
	repo := loomsealRepo(t)
	ctx := context.Background()
	runs, audits, id, runID := gatedReceipt(t)
	res, err := receipt.Build(ctx, runs, audits, id, "v-test", runID, receipt.Options{})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	if report := verifyWithLoomSeal(t, repo, res.Signed); !report.OK {
		t.Fatalf("loomseal refused a workflow receipt with an approval step: %v", report.Problems)
	}
	sparse, err := receipt.Build(ctx, runs, audits, id, "v-test", runID, receipt.Options{Sparse: true})
	if err != nil {
		t.Fatalf("receipt.Build(sparse) error = %v", err)
	}
	if report := verifyWithLoomSeal(t, repo, sparse.Signed); !report.OK {
		t.Fatalf("loomseal refused a sparse workflow receipt with an approval step: %v",
			report.Problems)
	}
}

// TestAStepNameHoldingAnAssignmentIsDisclosedAsCommitted pins that a decision body is disclosed as
// the exact bytes its digest was taken over. A step named with a password assignment is redacted
// before its body is committed, and the body used to be disclosed as assembled, so the receipt
// showed the password the redaction had removed, and a verifier that hashes what it is shown, which
// every LoomSeal verifier does, refused it. The receipt now discloses the redacted body: the
// password appears nowhere in it, and both this product's verifier and the open one accept it.
//
// The second name is one the redaction rewrites again when handed its own output, so the receipt
// verifies here only because this product's verifier checks the disclosed bytes as carried before
// it falls back to reducing them.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAStepNameHoldingAnAssignmentIsDisclosedAsCommitted(t *testing.T) {
	tests := []struct {
		// Name is the approval step's name.
		Name string
		// WantAbsent is text the receipt must not disclose anywhere, empty for none.
		WantAbsent string
	}{{ // Test 0: A password assignment in the name is disclosed redacted.
		Name: "release password=hunter2-step-secret", WantAbsent: "hunter2-step-secret",
	}, { // Test 1: A name the redaction does not reduce in one pass still verifies as disclosed.
		Name: "release password=''tail",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			ctx := context.Background()
			runs, audits, id, runID := gatedReceiptWithStep(t, test.Name)
			res, err := receipt.Build(ctx, runs, audits, id, "v-test", runID, receipt.Options{})
			if err != nil {
				t.Fatalf("receipt.Build() error = %v", err)
			}
			if test.WantAbsent != "" && strings.Contains(string(res.Signed), test.WantAbsent) {
				t.Errorf("the receipt discloses %q from the step name", test.WantAbsent)
			}
			rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
			if err != nil {
				t.Fatalf("VerifyBundle() error = %v", err)
			}
			if !rep.OK() || rep.DecisionsPresent == 0 {
				t.Fatalf("the receipt does not verify with its step decisions: %+v", rep)
			}
			if report := verifyWithLoomSeal(t, loomsealRepo(t), res.Signed); !report.OK {
				t.Fatalf("loomseal refused the receipt of a step named %q: %v", test.Name,
					report.Problems)
			}
		})
	}
}

// TestAReceiptDisclosingAnAssembledBodyStillVerifies pins the fallback for receipts issued before
// bodies were disclosed in their canonical redacted form. Such a receipt carried a step's decision
// body as it was assembled, password and all, while the entry committed the redacted form, so only
// this product's own redaction reproduces the digest. This product's verifier still accepts it.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAReceiptDisclosingAnAssembledBodyStillVerifies(t *testing.T) {
	const name = "release password=hunter2-step-secret"
	ctx := context.Background()
	runs, audits, id, runID := gatedReceiptWithStep(t, name)
	res, err := receipt.Build(ctx, runs, audits, id, "v-test", runID, receipt.Options{})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	var doc audit.Bundle
	if err := json.Unmarshal(res.Signed, &doc); err != nil {
		t.Fatalf("parse the receipt: %v", err)
	}
	assembled := 0
	for i := range doc.Claims {
		body, ok := doc.Claims[i].Payload["decision_body"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := body["step"]; ok {
			body["step"] = name
			assembled++
		}
	}
	if assembled == 0 {
		t.Fatal("the receipt disclosed no step decision to put back in its assembled form")
	}
	doc.Signatures = []any{}
	older, err := audit.SignBundleDoc(&doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	rep, err := audit.VerifyBundle(older, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.DecisionsOK || !rep.OK() {
		t.Errorf("a receipt carrying the assembled step body no longer verifies: %+v", rep)
	}
}
