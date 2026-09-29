package receipt_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// held builds a run that was submitted, held for a decision, decided, and finished, with its chain
// entries in the order the product writes them. It returns the stores and the run.
//
// The fixture is the whole point: a receipt for a run nobody questioned proves little, and every
// interesting disclosure path, the decision body, the spec digest binding, the outcome, only exists on
// a run that was actually gated.
func held(t *testing.T, verdict string) (run.Store, audit.Store, audit.Identity, *run.Run) {
	t.Helper()
	return heldWith(t, verdict, nil)
}

// heldWith is held with a hook to shape the run before anything commits, so a case can vary what the
// spec digest is computed over. The hook runs before the decision and the outcome, which is the only
// point where a change to the spec is the spec the evidence binds to rather than a tamper.
func heldWith(t *testing.T, verdict string,
	shape func(*run.Run)) (run.Store, audit.Store, audit.Identity, *run.Run) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}

	// The request that created the run is recorded first, and its receipt is what ties the run to
	// that entry: without it a receipt cannot place the run's start on the chain at all.
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "casey", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("append creation: %v", err)
	}

	r := &run.Run{
		ID: "run_1", Status: run.StatusRunning, CreatedAt: time.Now(),
		Tool: run.ToolBash, Command: "deploy the thing", Actor: "casey", ActorType: "session",
		HeldByPolicy: "deploys need a person", AuditReceipt: chainReceipt(creation),
	}
	if shape != nil {
		shape(r)
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := runs.AppendLog(ctx, r.ID, []byte("deploying\ndone\n")); err != nil {
		t.Fatalf("append log: %v", err)
	}

	// The decision, committed to the chain the way an approval does it, and stamped on the run.
	specDigest, err := outcome.CommitDecision(ctx, audits, r, verdict, "dana", "session")
	if err != nil {
		t.Fatalf("CommitDecision: %v", err)
	}
	r.ApprovedSpecDigest = specDigest

	// The run finishes and its outcome is committed, which is what makes it receiptable.
	ended := time.Now()
	code := 0
	r.Status = run.StatusSucceeded
	r.ExitCode = &code
	r.EndedAt = &ended
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save terminal run: %v", err)
	}
	if err := outcome.Commit(ctx, audits, runs, r, "system:test", nil); err != nil {
		t.Fatalf("Commit outcome: %v", err)
	}
	return runs, audits, id, r
}

// chainReceipt renders an entry's seq:link, the form a run stores.
func chainReceipt(e *audit.Entry) string {
	return itoa(e.Seq) + ":" + e.Hash
}

// itoa renders an int64 without pulling strconv into the test's reading.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestReceiptProvesWhoApprovedWhat covers the disclosure a governed run's receipt exists for. The
// contiguous receipt carries the chain segment from the request that created the run through the entry
// recording what it did, and discloses the decision bodies beside the digests the chain committed, so a
// third party can read who approved this change and prove the chain committed that exact statement.
func TestReceiptProvesWhoApprovedWhat(t *testing.T) {
	runs, audits, id, r := held(t, "approved")
	ctx := context.Background()

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Claims < 3 {
		t.Errorf("claims = %d, want the creation, the decision, and the outcome at least", res.Claims)
	}

	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a freshly built receipt does not verify: %+v", rep)
	}
	if rep.DecisionsPresent != 1 || !rep.DecisionsOK {
		t.Errorf("decisions present=%d ok=%v, want one disclosed decision that matches the chain",
			rep.DecisionsPresent, rep.DecisionsOK)
	}
	if len(rep.Decisions) != 1 {
		t.Fatalf("decisions = %+v, want one", rep.Decisions)
	}
	d := rep.Decisions[0]
	if d.Verdict != "approved" || d.Actor != "dana" {
		t.Errorf("decision = %+v, want approved by dana", d)
	}
	if d.SpecDigest != r.ApprovedSpecDigest {
		t.Errorf("decision binds spec %q, want the digest stamped on the run %q",
			d.SpecDigest, r.ApprovedSpecDigest)
	}
	if !rep.SpecConsistent {
		t.Error("the approved, executed, and disclosed digests disagree on a run nothing changed")
	}
	if !rep.OutcomePresent || !rep.OutcomeDigestOK {
		t.Error("the outcome is not disclosed or does not match what the chain committed")
	}
}

// TestReceiptSurfacesASpecChangedAfterApproval covers the tamper the decision disclosure exists to
// catch. The chain commits a digest of the decision body, and that body names the spec digest as it
// stood when the approver decided. Editing the run afterward, which is what an operator would do to
// slip a different change past a recorded approval, makes the rebuilt body disagree with the committed
// digest, and the receipt says so rather than verifying.
func TestReceiptSurfacesASpecChangedAfterApproval(t *testing.T) {
	runs, audits, id, r := held(t, "approved")
	ctx := context.Background()

	// The command is rewritten after the decision was recorded. Everything else stands.
	r.Command = "deploy something else entirely"
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save edited run: %v", err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if rep.OK() {
		t.Fatal("a receipt for a run whose spec changed after approval verified clean, so an edited " +
			"change can be presented as an approved one")
	}
	if rep.DecisionsOK && rep.SpecConsistent {
		t.Error("neither the decision disclosure nor the spec check noticed the edit")
	}
}

// TestReceiptRefusesWhatItCannotAttest covers the refusals, each of which exists so a weaker artifact
// is never published in place of a real one.
func TestReceiptRefusesWhatItCannotAttest(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	// Test 0: A run the chain never recorded the creation of cannot be placed on the chain.
	noReceipt := *r
	noReceipt.ID = "run_noreceipt"
	noReceipt.AuditReceipt = ""
	if err := runs.Save(ctx, &noReceipt); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := receipt.Build(ctx, runs, audits, id, "test", noReceipt.ID, receipt.Options{}); err == nil {
		t.Error("a run with no creation receipt produced a receipt anyway")
	} else if !strings.Contains(err.Error(), "creation receipt") {
		t.Errorf("error = %v, want it to name the missing creation receipt", err)
	}

	// Test 1: A run that has not finished has nothing to attest yet, and says so in those terms: this
	// is the ordinary case for a run still going, not a fault.
	running := *r
	running.ID = "run_running"
	running.Status = run.StatusRunning
	running.AuditReceipt = r.AuditReceipt
	if err := runs.Save(ctx, &running); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := receipt.Build(ctx, runs, audits, id, "test", running.ID, receipt.Options{}); err == nil {
		t.Error("a run with no committed outcome produced a receipt")
	} else if !strings.Contains(err.Error(), "finished") {
		t.Errorf("error = %v, want it to say a receipt covers a finished run", err)
	}

	// Test 2: A run that does not exist at all.
	if _, err := receipt.Build(ctx, runs, audits, id, "test", "run_missing", receipt.Options{}); err == nil {
		t.Error("a receipt was built for a run that does not exist")
	}
}

// TestSparseReceiptDisclosesOnlyItsOwnRun covers the shape an install hands an outside auditor: the
// run's own entries, each proved to belong to the whole chain, and nothing about the runs around it.
// The privacy claim is the point, so this checks what is absent as well as what verifies.
func TestSparseReceiptDisclosesOnlyItsOwnRun(t *testing.T) {
	runs, audits, id, r := held(t, "approved")
	ctx := context.Background()

	// Another tenant's run, recorded between this run's own entries, is what must not travel.
	other := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "someone-else", ActorType: "session",
		Method: "POST", Path: "/v1/runs/run_secret/cancel",
	}
	if err := audits.Append(ctx, other); err != nil {
		t.Fatalf("append other: %v", err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{Sparse: true})
	if err != nil {
		t.Fatalf("Build sparse: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a sparse receipt does not verify: %+v", rep)
	}
	if strings.Contains(string(res.Signed), "run_secret") {
		t.Error("the sparse receipt carries another run's entry, which is the disclosure it exists " +
			"to avoid")
	}
	if strings.Contains(string(res.Signed), "someone-else") {
		t.Error("the sparse receipt names an actor from another run")
	}
	// With no tree anchor over the chain, the root it proves membership in rests on this install's
	// word, and the caller is told so rather than left to assume otherwise.
	if !res.UnanchoredSparse {
		t.Error("an unanchored sparse receipt did not report that nothing outside this install fixes " +
			"the root it proves membership in")
	}
}

// TestSparseReceiptProvesTheLogOnlyAppended covers the consistency proof a reader asks for when they
// already saw an earlier size: it shows the log they saw is a prefix of the log there is now, which is
// the check that catches a history rewritten between two readings.
func TestSparseReceiptProvesTheLogOnlyAppended(t *testing.T) {
	runs, audits, id, r := held(t, "approved")
	ctx := context.Background()

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	from := int64(len(chain)) - 1
	if from < 1 {
		t.Fatalf("chain too short to prove consistency from: %d", len(chain))
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID,
		receipt.Options{Sparse: true, From: from})
	if err != nil {
		t.Fatalf("Build with a consistency proof: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a receipt carrying a consistency proof does not verify: %+v", rep)
	}
	if !strings.Contains(string(res.Signed), "consistency") {
		t.Error("the receipt carries no consistency member, so nothing proves the log only appended")
	}
}

// TestRejectedRunReceiptSaysItWasRejected covers the other verdict. A rejection is evidence too: the
// record has to show that a change was asked for and refused, with the same strength as one that ran.
func TestRejectedRunReceiptSaysItWasRejected(t *testing.T) {
	runs, audits, id, r := held(t, "rejected")
	ctx := context.Background()

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a rejected run's receipt does not verify: %+v", rep)
	}
	if rep.DecisionsPresent != 1 || rep.Decisions[0].Verdict != "rejected" {
		t.Errorf("decisions = %+v, want one rejection", rep.Decisions)
	}
}

// TestReceiptRefusesAChainItsAnchorsDisown covers the strongest refusal in the builder. An anchor fixes
// a chain position somewhere the operator cannot rewrite alone, so a chain that no longer reaches its
// own anchor has visibly lost history. Publishing a receipt drawn from that chain would hand a third
// party an artifact that verifies internally while contradicting the very evidence meant to bound it,
// which is worse than refusing: it launders a truncated history into a signed document.
func TestReceiptRefusesAChainItsAnchorsDisown(t *testing.T) {
	runs, audits, id, r := held(t, "approved")
	ctx := context.Background()

	// Verify the receipt builds while the anchors are satisfied, so the refusal below is the anchor's
	// doing and not the fixture's.
	if _, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{}); err != nil {
		t.Fatalf("Build before anchoring: %v", err)
	}

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	anchors, ok := audits.(audit.AnchorStore)
	if !ok {
		t.Fatal("the audit store keeps no anchors, so this guarantee cannot be exercised")
	}
	// An anchor over a position past the chain's end is exactly what a lost tail looks like from the
	// outside: the world saw a longer log than this install can now produce.
	beyond := chain[len(chain)-1]
	if err := anchors.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_1", Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeLinear,
		Seq: beyond.Seq + 5, Link: beyond.Hash, At: time.Now(),
		Ref: "https://example.com/head", InstallID: id.InstallID,
	}); err != nil {
		t.Fatalf("SaveAnchor: %v", err)
	}

	_, err = receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err == nil {
		t.Fatal("a receipt was published from a chain that no longer satisfies an anchor recorded " +
			"over it, so a truncated history was signed as an intact one")
	}
	if !strings.Contains(err.Error(), "anchor") {
		t.Errorf("error = %v, want it to name the anchor the chain cannot reach", err)
	}
}
