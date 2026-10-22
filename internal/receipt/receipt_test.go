package receipt_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/kordloom/loomseal/jcs"

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
	specDigest, err := outcome.CommitDecision(ctx, audits, r, verdict,
		outcome.Decider{Name: "dana", Type: "session"}, time.Now)
	if err != nil {
		t.Fatalf("CommitDecision: %v", err)
	}
	r.ApprovedSpecDigest = specDigest

	// The run finishes and its outcome is committed, which is what makes it receiptable. It is
	// committed the way every path that finishes a run commits it, so the store records the outcome
	// as on the chain and no longer owed.
	ended := time.Now()
	code := 0
	r.Status = run.StatusSucceeded
	r.ExitCode = &code
	r.EndedAt = &ended
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save terminal run: %v", err)
	}
	if err := outcome.CommitOwed(ctx, audits, runs, r, "system:test", nil); err != nil {
		t.Fatalf("CommitOwed outcome: %v", err)
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

// preSealOutcome is the outcome body a release before seals committed for the workflow
// preSealWorkflow builds, byte for byte. It was captured from the outcome package as it stood
// before the seal fields existed and is never regenerated, since what it pins is what an older
// binary wrote to a chain that a newer one must still receipt.
const preSealOutcome = `{"run_id":"run_presealed","status":"succeeded","exit_code":0,"tool":"bash",` +
	`"started_at":"2026-10-01T12:00:00Z","ended_at":"2026-10-01T12:01:30Z",` +
	`"log_sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",` +
	`"spec_digest":"sha256:52eabe17c4cada51c92ad3999bc2ba8856e5c1891f9b825f8382ea8ec4c76c49",` +
	`"children":[{"run_id":"run_presealed_build","name":"build","index":0,"attempt":1,` +
	`"status":"succeeded","exit_code":0,` +
	`"log_sha256":"45960edc3c59bf0464cb0f656ff22742eb452746bab1b7b13ddcedc8c55c5eb8"},` +
	`{"run_id":"run_presealed_ship","name":"ship","index":1,"attempt":1,"status":"succeeded",` +
	`"exit_code":0,` +
	`"log_sha256":"74aef5540c514c97ecae2d2bff31ad8b4713ac0fc5e23ba441538e02dfcca86d"}]}`

// preSealWorkflow builds a workflow that finished before seals existed: a pipeline whose parent and
// both steps carry outputs, and whose rows carry no seal. Every input to its outcome is fixed, so
// the body rebuilt from it is comparable byte for byte. It returns the stores and the workflow.
func preSealWorkflow(t *testing.T) (run.Store, audit.Store, audit.Identity, *run.Run) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "casey", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("append creation: %v", err)
	}
	started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ended := started.Add(90 * time.Second)
	code := 0
	parent := &run.Run{
		ID: "run_presealed", Kind: run.KindPipeline, Status: run.StatusRunning, CreatedAt: started,
		Tool: run.ToolBash, Actor: "casey", ActorType: "session", AuditReceipt: chainReceipt(creation),
		Steps: []run.PipelineStep{
			{Name: "build", Tool: run.ToolBash, Command: "make"},
			{Name: "ship", Tool: run.ToolBash, Command: "ship it", DependsOn: []string{"build"}},
		},
		StartedAt: &started,
	}
	if err := runs.Save(ctx, parent); err != nil {
		t.Fatalf("save workflow: %v", err)
	}
	for i, s := range parent.Steps {
		idx := i
		child := &run.Run{
			ID: "run_presealed_" + s.Name, ParentID: &parent.ID, StepIndex: &idx, StepName: s.Name,
			Attempt: 1, Tool: s.Tool, Command: s.Command, Status: run.StatusRunning,
			CreatedAt: started, AuditReceipt: parent.AuditReceipt,
		}
		if err := runs.Save(ctx, child); err != nil {
			t.Fatalf("save step %s: %v", s.Name, err)
		}
		if err := runs.AppendLog(ctx, child.ID, []byte(s.Name+" output\n")); err != nil {
			t.Fatalf("append log %s: %v", s.Name, err)
		}
		child.Status, child.ExitCode = run.StatusSucceeded, &code
		child.Outputs = map[string]any{s.Name: "done"}
		if err := runs.Save(ctx, child); err != nil {
			t.Fatalf("save finished step %s: %v", s.Name, err)
		}
	}
	parent.Status, parent.ExitCode, parent.EndedAt = run.StatusSucceeded, &code, &ended
	parent.Outputs = map[string]any{"release": "1.2.3"}
	if err := runs.Save(ctx, parent); err != nil {
		t.Fatalf("save finished workflow: %v", err)
	}
	return runs, audits, id, parent
}

// TestReceiptForAWorkflowFinishedBeforeSealsVerifies covers the compatibility the seal fields
// promise. A receipt rebuilds a run's outcome from the store when it is asked for, so an outcome
// field that appeared on a row with no seal would rebuild every workflow an older binary finished
// to bytes its chain never committed, and every one of their receipts would read as forged. The
// outcome here is committed over the bytes the older binary committed, never over a body rebuilt
// today, so the receipt is held to the chain an upgrade inherits.
func TestReceiptForAWorkflowFinishedBeforeSealsVerifies(t *testing.T) {
	runs, audits, id, wf := preSealWorkflow(t)
	ctx := context.Background()

	body, err := outcome.Body(ctx, runs, wf)
	if err != nil {
		t.Fatalf("outcome.Body: %v", err)
	}
	if diff := cmp.Diff(preSealOutcome, string(body)); diff != "" {
		t.Errorf("an unsealed workflow rebuilds to other bytes than it was committed under "+
			"(-want +got):\n%s", diff)
	}
	committed, err := outcome.Committed([]byte(preSealOutcome))
	if err != nil {
		t.Fatalf("outcome.Committed: %v", err)
	}
	digest, nonce, err := audit.ExactDigestOf(committed)
	if err != nil {
		t.Fatalf("ExactDigestOf: %v", err)
	}
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "system:dispatcher", ActorType: "system",
		OnBehalfOf: wf.Actor, Method: audit.MethodRun,
		Path: audit.OutcomePath(wf.ID, string(wf.Status)), ContentDigest: digest, Nonce: nonce,
	}); err != nil {
		t.Fatalf("append outcome: %v", err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", wf.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %q, want none for a workflow nothing changed", res.Notes)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OK() || !rep.OutcomePresent || !rep.OutcomeDigestOK {
		t.Fatalf("a receipt for a workflow finished before seals does not verify: ok=%v "+
			"outcome present=%v digest ok=%v", rep.OK(), rep.OutcomePresent, rep.OutcomeDigestOK)
	}
}

// decide claims and settles a held run or approval step with the decision decisionID, the only
// writes that set the decision a row credits, and returns the row as the store then holds it.
func decide(t *testing.T, runs run.Store, id, decisionID string, settle run.DecisionSettle) *run.Run {
	t.Helper()
	ctx := context.Background()
	if ok, err := runs.ClaimDecision(ctx, id, decisionID, "claim"); err != nil || !ok {
		t.Fatalf("ClaimDecision(%s) = %v, %v", id, ok, err)
	}
	if ok, err := runs.SettleDecision(ctx, id, decisionID, settle); err != nil || !ok {
		t.Fatalf("SettleDecision(%s) = %v, %v", id, ok, err)
	}
	r, err := runs.Get(ctx, id)
	if err != nil {
		t.Fatalf("read decided run %s: %v", id, err)
	}
	return r
}

// releasedWorkflow builds a held workflow an approver released, whose approval step an approver
// approved and whose deploy step that approval released: the RELEASE entry, naming each decision by
// the content digest its entry committed, is appended before the step's record is saved, the order
// the product writes them in. tamper, when set, runs after the release and before the step
// executes, the window a writer of the database has. It returns the stores, the workflow, the
// release entry, and the deploy step's id.
func releasedWorkflow(t *testing.T, tamper func(run.Store, string)) (run.Store, audit.Store,
	audit.Identity, *run.Run, *audit.Entry, string) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "casey", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("append creation: %v", err)
	}
	wf := &run.Run{
		ID: "run_wf", Kind: run.KindPipeline, Status: run.StatusPendingApproval, CreatedAt: time.Now(),
		Tool: run.ToolBash, Actor: "casey", ActorType: "session", AuditReceipt: chainReceipt(creation),
		HeldByPolicy: "deploys need a person",
		Steps: []run.PipelineStep{
			{Name: "gate", Type: run.StepApproval},
			{Name: "deploy", Tool: run.ToolBash, Command: "deploy", DependsOn: []string{"gate"}},
		},
	}
	approver := outcome.Decider{Name: "dana", Type: "session"}
	if err := runs.Save(ctx, wf); err != nil {
		t.Fatalf("save held workflow: %v", err)
	}
	wfDecision := audit.NewID()
	spec, err := outcome.CommitDecisionWith(ctx, audits, wf, outcome.StepApproved, approver,
		time.Now, outcome.DecisionExtras{ID: wfDecision})
	if err != nil {
		t.Fatalf("CommitDecisionWith: %v", err)
	}
	wf = decide(t, runs, wf.ID, wfDecision, run.DecisionSettle{Status: run.StatusRunning,
		Owner: "test"})
	wf.ApprovedSpecDigest = spec
	if err := runs.Save(ctx, wf); err != nil {
		t.Fatalf("save approved workflow: %v", err)
	}

	gateIndex, deployIndex := 0, 1
	gate := &run.Run{
		ID: "run_wf_gate", Kind: run.KindApproval, ParentID: &wf.ID, StepIndex: &gateIndex,
		StepName: "gate", Attempt: 1, Status: run.StatusPendingApproval, CreatedAt: time.Now(),
	}
	if err := runs.Save(ctx, gate); err != nil {
		t.Fatalf("save approval step: %v", err)
	}
	state, err := outcome.StepStateOf(ctx, runs, wf, gate)
	if err != nil {
		t.Fatalf("StepStateOf: %v", err)
	}
	gateDecision := audit.NewID()
	if _, err := outcome.CommitStepDecisionWith(ctx, audits, state, outcome.StepApproved,
		approver, time.Now, outcome.DecisionExtras{ID: gateDecision}); err != nil {
		t.Fatalf("CommitStepDecisionWith: %v", err)
	}
	decide(t, runs, gate.ID, gateDecision, run.DecisionSettle{Status: run.StatusSucceeded,
		EndedAt: time.Now()})

	deploy := &run.Run{
		ID: "run_wf_deploy", ParentID: &wf.ID, StepIndex: &deployIndex,
		StepName: "deploy", Attempt: 1, Tool: run.ToolBash, Command: "deploy",
		ExtraVars: map[string]any{"region": "east"}, Status: run.StatusPending,
		CreatedAt: time.Now(), AuditReceipt: wf.AuditReceipt, SpecSeal: "bk1:sealed",
	}
	steps, err := runs.Steps(ctx, wf.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	roots := outcome.RootsOf(outcome.ReleaseRoots(wf, deploy, steps), func(id string) string {
		return chainDigest(t, audits, id)
	})
	release, err := outcome.ReleaseEntry(deploy, roots,
		outcome.Decider{Name: "system:dispatcher", Type: "system", OnBehalfOf: wf.Actor}, time.Now)
	if err != nil {
		t.Fatalf("ReleaseEntry: %v", err)
	}
	if err := audits.Append(ctx, release); err != nil {
		t.Fatalf("append release: %v", err)
	}
	if err := runs.Save(ctx, deploy); err != nil {
		t.Fatalf("save released step: %v", err)
	}
	if tamper != nil {
		tamper(runs, deploy.ID)
	}

	code := 0
	if err := runs.AppendLog(ctx, deploy.ID, []byte("deployed\n")); err != nil {
		t.Fatalf("append log: %v", err)
	}
	deploy, err = runs.Get(ctx, deploy.ID)
	if err != nil {
		t.Fatalf("read released step: %v", err)
	}
	deploy.Status, deploy.ExitCode = run.StatusSucceeded, &code
	if err := runs.Save(ctx, deploy); err != nil {
		t.Fatalf("save finished step: %v", err)
	}
	ended := time.Now()
	wf.Status, wf.ExitCode, wf.EndedAt = run.StatusSucceeded, &code, &ended
	if err := runs.Save(ctx, wf); err != nil {
		t.Fatalf("save finished workflow: %v", err)
	}
	if err := outcome.CommitOwed(ctx, audits, runs, wf, "system:test", nil); err != nil {
		t.Fatalf("CommitOwed: %v", err)
	}
	return runs, audits, id, wf, release, deploy.ID
}

// chainDigest returns the content digest the entry id committed, read from the chain audits
// streams, or the empty string when the chain holds no entry under id.
func chainDigest(t *testing.T, audits audit.Store, id string) string {
	t.Helper()
	digest := ""
	err := audits.ChainScan(context.Background(), 0, func(e *audit.Entry) error {
		if e.ID == id {
			digest = e.ContentDigest
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChainScan: %v", err)
	}
	return digest
}

// releaseClaimOf returns the payload of the claim a signed receipt carries for the release entry
// at path, nil when the receipt carries none.
func releaseClaimOf(t *testing.T, signed []byte, path string) map[string]any {
	t.Helper()
	var doc audit.Bundle
	if err := json.Unmarshal(signed, &doc); err != nil {
		t.Fatalf("parse the receipt: %v", err)
	}
	for _, c := range doc.Claims {
		method, _ := c.Payload["method"].(string)
		gotPath, _ := c.Payload["path"].(string)
		if method == audit.MethodRelease && gotPath == path {
			return c.Payload
		}
	}
	return nil
}

// TestReceiptDisclosesWhatADecisionReleased covers the release disclosure. A gated step's RELEASE
// entry commits the step a decision released, and the receipt discloses that body beside the entry,
// rebuilt from the stored step, so a reader can prove the chain committed it and hold it to the
// step the workflow's outcome says ran. Each decision that released the step is named by the
// content digest its entry holds in the receipt's segment, and by an empty digest when the segment
// holds no such entry. A step rewritten after its release, a decision swapped on the approval step
// that released it, or that decision's entry rewritten on the chain rebuilds to a body the
// committed digest refuses, and the body is disclosed anyway rather than dropped.
//
//nolint:funlen // Test function.
func TestReceiptDisclosesWhatADecisionReleased(t *testing.T) {
	tests := []struct {
		Tamper       func(run.Store, string)
		Wrap         func(run.Store) run.Store
		WrapAudits   func(run.Store, audit.Store) audit.Store
		Name         string
		WantVerifies bool
	}{{ // Test 0: Nothing changed, so the disclosed release is the one the chain committed.
		Name: "untouched", WantVerifies: true,
	}, { // Test 1: The step's command is rewritten after its release and before it runs.
		Name: "step rewritten", Tamper: rewriteStep(func(c *run.Run) {
			c.Command = "deploy --everywhere"
		}),
	}, { // Test 2: The step's inputs are rewritten after its release and before it runs.
		Name: "inputs rewritten", Tamper: rewriteStep(func(c *run.Run) {
			c.ExtraVars = map[string]any{"region": "west"}
		}),
	}, { // Test 3: The approval step is made to name another decision than the one that released.
		Name: "gate decision swapped", Wrap: func(runs run.Store) run.Store {
			return &decisionSwapped{Store: runs, id: "run_wf_gate", decisionID: audit.NewID()}
		},
	}, { // Test 4: The approval step's decision entry is rewritten on the chain after the release.
		Name: "gate decision entry rewritten",
		WrapAudits: func(runs run.Store, audits audit.Store) audit.Store {
			gate, err := runs.Get(context.Background(), "run_wf_gate")
			if err != nil {
				panic(err)
			}
			return &digestRewritten{Store: audits, id: gate.DecisionID,
				digest: "sha256s:" + strings.Repeat("0", 64)}
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			runs, audits, id, wf, release, deployID := releasedWorkflow(t, test.Tamper)
			ctx := context.Background()
			if test.Wrap != nil {
				runs = test.Wrap(runs)
			}
			if test.WrapAudits != nil {
				audits = test.WrapAudits(runs, audits)
			}

			res, err := receipt.Build(ctx, runs, audits, id, "test", wf.ID, receipt.Options{})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
			if err != nil {
				t.Fatalf("VerifyBundle: %v", err)
			}
			if test.WantVerifies && !rep.OK() {
				t.Fatalf("a receipt carrying a release does not verify: %+v", rep)
			}
			claim := releaseClaimOf(t, res.Signed, release.Path)
			if claim == nil {
				t.Fatal("the receipt carries no claim for the release entry, which lies inside the " +
					"workflow's segment")
			}
			if nonce, _ := claim["release_nonce"].(string); nonce != release.Nonce {
				t.Errorf("release_nonce = %q, want the entry's nonce", nonce)
			}
			bodyObj, ok := claim["release_body"].(map[string]any)
			if !ok {
				t.Fatalf("the release claim discloses no release body: %v", claim)
			}
			raw, err := json.Marshal(bodyObj)
			if err != nil {
				t.Fatalf("marshal release_body: %v", err)
			}
			canonical, err := jcs.Canonicalize(raw)
			if err != nil {
				t.Fatalf("canonicalize release_body: %v", err)
			}
			verifies := audit.VerifyCanonicalDigest(release.ContentDigest, release.Nonce, canonical)
			if verifies != test.WantVerifies {
				t.Errorf("the disclosed release verifies = %v, want %v: %s", verifies,
					test.WantVerifies, canonical)
			}

			// The release names each decision the stored rows name by the digest the chain holds
			// for it, read here straight from the chain rather than through the receipt.
			var rel outcome.ReleaseRecord
			if err := json.Unmarshal(raw, &rel); err != nil {
				t.Fatalf("decode release_body: %v", err)
			}
			gate, err := runs.Get(ctx, "run_wf_gate")
			if err != nil {
				t.Fatalf("read approval step: %v", err)
			}
			var wantRoots []outcome.ReleaseRoot
			for _, did := range []string{wf.DecisionID, gate.DecisionID} {
				wantRoots = append(wantRoots, outcome.ReleaseRoot{ID: did,
					ContentDigest: chainDigest(t, audits, did)})
			}
			slices.SortFunc(wantRoots, func(a, b outcome.ReleaseRoot) int {
				return strings.Compare(a.ID, b.ID)
			})
			if diff := cmp.Diff(wantRoots, rel.Roots); diff != "" {
				t.Errorf("release roots mismatch (-want +got):\n%s", diff)
			}
			if !test.WantVerifies {
				return
			}

			// The release names the step the outcome says ran, by the spec digest both carry.
			text := disclosedOutcomeText(t, res.Signed, wf.ID)
			rec, err := outcome.Parse([]byte(text))
			if err != nil {
				t.Fatalf("parse disclosed outcome: %v", err)
			}
			at := slices.IndexFunc(rec.Children, func(c outcome.RecordChild) bool {
				return c.RunID == deployID
			})
			if at < 0 {
				t.Fatalf("the outcome does not name the released step: %s", text)
			}
			if got := rec.Children[at].SpecDigest; got == "" || got != rel.SpecDigest {
				t.Errorf("the outcome names the released step with spec digest %q, want the "+
					"release's %q", got, rel.SpecDigest)
			}
		})
	}
}

// rewriteStep returns a tamper that rewrites the released step's row through an ordinary save, the
// write a database writer has.
func rewriteStep(edit func(*run.Run)) func(run.Store, string) {
	return func(runs run.Store, id string) {
		ctx := context.Background()
		c, err := runs.Get(ctx, id)
		if err != nil {
			panic(err)
		}
		edit(c)
		if err := runs.Save(ctx, c); err != nil {
			panic(err)
		}
	}
}

// decisionSwapped is a run store whose copy of one row names another decision than the one that
// won it, the column a database writer can rewrite where no save of this product can.
type decisionSwapped struct {
	run.Store
	// id is the row whose decision is swapped.
	id string
	// decisionID is the decision the row is made to name.
	decisionID string
}

// Get returns the row with its decision swapped when it is the row id names.
func (s *decisionSwapped) Get(ctx context.Context, id string) (*run.Run, error) {
	r, err := s.Store.Get(ctx, id)
	if err == nil && r.ID == s.id {
		r.DecisionID = s.decisionID
	}
	return r, err
}

// Steps returns a workflow's step rows with the one id names carrying the swapped decision.
func (s *decisionSwapped) Steps(ctx context.Context, parentID string) ([]*run.Run, error) {
	steps, err := s.Store.Steps(ctx, parentID)
	for _, r := range steps {
		if r.ID == s.id {
			r.DecisionID = s.decisionID
		}
	}
	return steps, err
}

// digestRewritten is an audit store whose copy of one entry commits another content digest than the
// one appended, with every entry from it on linked again over the rewrite, the chain a database
// writer can leave behind. The links are plain hashes, so the rewritten chain verifies on its own,
// and only the bodies committed before the rewrite still name the digest it replaced.
type digestRewritten struct {
	audit.Store
	// id is the entry whose digest is rewritten.
	id string
	// digest is the content digest the entry is made to commit.
	digest string
}

// ChainScan streams the rewritten chain above afterSeq. It reads from the start, since each link
// is computed over the one before it.
func (s *digestRewritten) ChainScan(ctx context.Context, afterSeq int64,
	fn func(*audit.Entry) error) error {
	prev, relinking := "", false
	return s.Store.ChainScan(ctx, 0, func(e *audit.Entry) error {
		rewritten := *e
		if rewritten.ID == s.id {
			rewritten.ContentDigest = s.digest
			relinking = true
		}
		if relinking {
			rewritten.PrevHash = prev
			rewritten.Hash = audit.EntryHash(&rewritten)
		}
		prev = rewritten.Hash
		if rewritten.Seq <= afterSeq {
			return nil
		}
		return fn(&rewritten)
	})
}

// TestSparseReceiptCarriesTheReleaseWithoutItsBody covers the other shape. A sparse receipt
// discloses this run's own entries, and a release's path names the workflow, so the release travels
// proved by its audit path; nothing is added beside it, since a tree leaf commits its whole claim.
func TestSparseReceiptCarriesTheReleaseWithoutItsBody(t *testing.T) {
	runs, audits, id, wf, release, _ := releasedWorkflow(t, nil)
	ctx := context.Background()

	res, err := receipt.Build(ctx, runs, audits, id, "test", wf.ID, receipt.Options{Sparse: true})
	if err != nil {
		t.Fatalf("Build sparse: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a sparse receipt carrying a release does not verify: %+v", rep)
	}
	claim := releaseClaimOf(t, res.Signed, release.Path)
	if claim == nil {
		t.Fatal("the sparse receipt leaves out the release, whose path names the workflow")
	}
	for _, member := range []string{"release_body", "release_nonce"} {
		if _, ok := claim[member]; ok {
			t.Errorf("a sparse receipt carries %q, which makes its own leaf recompute differently",
				member)
		}
	}
}

// disclosedOutcomeText returns the outcome body a signed contiguous receipt discloses for a run.
func disclosedOutcomeText(t *testing.T, signed []byte, runID string) string {
	t.Helper()
	var doc audit.Bundle
	if err := json.Unmarshal(signed, &doc); err != nil {
		t.Fatalf("parse the receipt: %v", err)
	}
	for _, c := range doc.Claims {
		method, _ := c.Payload["method"].(string)
		path, _ := c.Payload["path"].(string)
		if method != audit.MethodRun || !strings.HasPrefix(path, "/runs/"+runID+"/outcome/") {
			continue
		}
		text, ok := c.Payload["outcome_body"].(string)
		if !ok {
			t.Fatalf("the receipt's outcome claim discloses no outcome text: %v", c.Payload)
		}
		return text
	}
	t.Fatalf("the receipt has no outcome claim for %s", runID)
	return ""
}
