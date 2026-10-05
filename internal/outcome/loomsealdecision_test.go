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
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// reasonedReceipt runs an agent's change through a held approval that carries a reason, lets it
// finish, and returns the signed receipt with everything needed to tamper with it.
type reasonedReceipt struct {
	// signed is the receipt.
	signed []byte
	// id signs.
	id audit.Identity
	// d decided.
	d *dispatch.Dispatcher
	// runID is the run.
	runID string
	// runs, audits, and decisions are the stores the receipt was built from.
	runs      run.Store
	audits    audit.Store
	decisions decision.Store
}

// buildReasonedReceipt builds the receipt.
func buildReasonedReceipt(t *testing.T, reason string) *reasonedReceipt {
	t.Helper()
	ctx := context.Background()
	rr := &reasonedReceipt{runs: run.NewMemStore(), audits: audit.NewMemStore(),
		decisions: decision.NewMemStore()}
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	rr.id = id
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	rr.d = dispatch.New(rr.runs, runner, nil, dispatch.WithAudits(rr.audits),
		dispatch.WithDecisions(rr.decisions), dispatch.WithNoJanitor())
	t.Cleanup(rr.d.Close)
	creation := &audit.Entry{ID: audit.NewID(), At: time.Now(), Actor: "deploy-bot",
		ActorType: "agent", OnBehalfOf: "dev-lead", Method: "POST", Path: "/v1/runs"}
	if err := rr.audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	submitCtx := run.WithAuditReceipt(ctx, fmt.Sprintf("%d:%s", creation.Seq, creation.Hash))
	held, err := rr.d.Submit(submitCtx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
		run.WithActor("deploy-bot"), run.WithActorType("agent"), run.WithActorAccount("user_dev"),
		run.WithRequireApproval(true), run.WithInitiator(&run.Initiator{InitiatedBy: "deploy-bot",
			CredentialID: "tok_bot", BoundTo: "dev-lead", ProvisionedBy: "org-admin",
			ProvisionedByType: "session"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	rr.runID = held.ID
	if _, err := rr.d.DecideRun(ctx, held.ID, dispatch.RunDecision{Approve: true, Reason: reason,
		By: outcome.Decider{Name: "ops-admin", Type: "session", OnBehalfOf: "ops-admin",
			AccountID: "user_ops"}}); err != nil {
		t.Fatalf("DecideRun() error = %v", err)
	}
	waitFinished(t, rr.runs, rr.audits, held.ID)
	rr.rebuild(t)
	return rr
}

// rebuild signs a fresh receipt from the stores as they are now.
func (rr *reasonedReceipt) rebuild(t *testing.T) {
	t.Helper()
	res, err := receipt.Build(context.Background(), rr.runs, rr.audits, rr.id, "v-test", rr.runID,
		receipt.Options{Decisions: rr.decisions})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	rr.signed = res.Signed
}

// waitFinished waits for a run to reach a terminal state and for its outcome entry to be on the
// chain. The status turns terminal before the outcome is appended, so a receipt built on the status
// alone can find no outcome to end at.
func waitFinished(t *testing.T, runs run.Store, audits audit.Store, id string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := runs.Get(ctx, id)
		if err == nil && r.Status.Terminal() && hasOutcome(ctx, t, audits, id) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never finished with its outcome on the chain", id)
}

// hasOutcome reports whether the chain holds the outcome entry of run id.
func hasOutcome(ctx context.Context, t *testing.T, audits audit.Store, id string) bool {
	t.Helper()
	found := false
	err := audits.ChainScan(ctx, 0, func(e *audit.Entry) error {
		if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, "/runs/"+id+"/outcome/") {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChainScan() error = %v", err)
	}
	return found
}

// TestAReceiptCarriesTheReasonItsCommitmentOpens proves the offline check of an approver's reason:
// the receipt discloses the reason's text and random value beside the decision whose body commits
// the commitment, and the verifier opens it with no secret. It also carries the agent identity of
// the run and the separation-of-duties evaluation of the decision.
//
// The open LoomSeal verifier is run over the same receipt, since the reason travels inside the
// signed bundle a relying party checks with the format's own tool, and it must still verify.
func TestAReceiptCarriesTheReasonItsCommitmentOpens(t *testing.T) {
	rr := buildReasonedReceipt(t, "approved, change window confirmed with the database team")
	rep, err := audit.VerifyBundle(rr.signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OK() {
		t.Fatalf("an honest receipt carrying a reason did not verify: %+v", rep)
	}
	if len(rep.Decisions) != 1 {
		t.Fatalf("decisions disclosed = %d, want 1", len(rep.Decisions))
	}
	got := rep.Decisions[0]
	if got.ReasonState != audit.ReasonVerified ||
		got.Reason != "approved, change window confirmed with the database team" {
		t.Errorf("reason = %q (%s), want the text, verified", got.Reason, got.ReasonState)
	}
	if got.SeparationOfDuties == nil || got.SeparationOfDuties.Requester != "dev-lead" ||
		!got.SeparationOfDuties.Independent || got.SeparationOfDuties.Result != decision.SoDNotRequired {
		t.Errorf("separation of duties = %+v, want dev-lead as requester and an independent approver",
			got.SeparationOfDuties)
	}
	rec, err := outcome.Parse(rep.OutcomeBody)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if rec.Initiator == nil || rec.Initiator.InitiatedBy != "deploy-bot" ||
		rec.Initiator.BoundTo != "dev-lead" || rec.Initiator.ProvisionedBy != "org-admin" {
		t.Errorf("committed outcome initiator = %+v, want the agent, its account, and its issuer",
			rec.Initiator)
	}
	repo := loomsealRepo(t)
	if lrep := verifyWithLoomSeal(t, repo, rr.signed); !lrep.OK {
		t.Fatalf("loomseal refused a receipt carrying an approver's reason: %v", lrep.Problems)
	}
}

// TestAReasonThatDoesNotOpenItsCommitmentFailsTheReceipt proves the check is a check: a receipt
// whose disclosed reason was changed, even one the producer re-signed so its signature holds, no
// longer opens the commitment the chain committed, and the receipt does not verify.
func TestAReasonThatDoesNotOpenItsCommitmentFailsTheReceipt(t *testing.T) {
	rr := buildReasonedReceipt(t, "approved after the change review")
	var b audit.Bundle
	if err := json.Unmarshal(rr.signed, &b); err != nil {
		t.Fatalf("parse receipt: %v", err)
	}
	changed := false
	for i := range b.Claims {
		if _, ok := b.Claims[i].Payload["reason_text"]; ok {
			b.Claims[i].Payload["reason_text"] = "approved by the change board"
			changed = true
		}
	}
	if !changed {
		t.Fatal("the receipt disclosed no reason to change, so this proves nothing")
	}
	b.Signatures = []any{}
	resigned, err := audit.SignBundleDoc(&b, rr.id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	rep, err := audit.VerifyBundle(resigned, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.SignatureOK {
		t.Fatal("the re-signed receipt's signature does not hold, so the reason check went untested")
	}
	if rep.OK() || rep.DecisionsOK {
		t.Error("a receipt disclosing a reason its commitment does not open verified")
	}
}

// TestARedactedReasonLeavesAReceiptThatStillVerifies proves redaction keeps the record whole: after
// the reason's text and random value are removed, a new receipt discloses that it was redacted and
// why, the decision body still carries the commitment the chain committed, and the receipt
// verifies, in this product's verifier and in the open one.
func TestARedactedReasonLeavesAReceiptThatStillVerifies(t *testing.T) {
	rr := buildReasonedReceipt(t, "approved, the requester is on call this week")
	records, err := rr.decisions.ForRun(context.Background(), rr.runID)
	if err != nil || len(records) != 1 {
		t.Fatalf("ForRun() = %d records, %v, want 1", len(records), err)
	}
	if _, err := rr.d.RedactReason(context.Background(), rr.runID, records[0].ID,
		decision.CategoryPersonalData, outcome.Decider{Name: "privacy-admin", Type: "session",
			OnBehalfOf: "privacy-admin"}); err != nil {
		t.Fatalf("RedactReason() error = %v", err)
	}
	rr.rebuild(t)
	if strings.Contains(string(rr.signed), "on call this week") {
		t.Fatal("a receipt built after the redaction still discloses the reason")
	}
	rep, err := audit.VerifyBundle(rr.signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a receipt with a redacted reason did not verify: %+v", rep)
	}
	if got := rep.Decisions[0]; got.ReasonState != audit.ReasonRedacted ||
		got.RedactedCategory != decision.CategoryPersonalData {
		t.Errorf("reason state = %q (%q), want redacted for personal data", got.ReasonState,
			got.RedactedCategory)
	}
	repo := loomsealRepo(t)
	if lrep := verifyWithLoomSeal(t, repo, rr.signed); !lrep.OK {
		t.Fatalf("loomseal refused a receipt carrying a redacted reason: %v", lrep.Problems)
	}
}
