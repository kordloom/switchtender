package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dossier"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// apprGatedDecisions is a decision store that holds the first record one named decider saves until
// the test releases it. It stands in for a replica whose decision has passed every check and stalls
// on its record insert, a slow statement or a pool wait, before its chain entry is written.
type apprGatedDecisions struct {
	decision.Store
	// actor is the decider whose save is held.
	actor string
	// reached is closed when the held save arrives.
	reached chan struct{}
	// release lets the held save continue.
	release chan struct{}
	// once makes only the first matching save wait.
	once sync.Once
}

// apprNewGatedDecisions wraps inner so the first save by actor waits for release.
func apprNewGatedDecisions(inner decision.Store, actor string) *apprGatedDecisions {
	return &apprGatedDecisions{Store: inner, actor: actor, reached: make(chan struct{}),
		release: make(chan struct{})}
}

// Save holds the named decider's first record until released, then saves it.
func (g *apprGatedDecisions) Save(ctx context.Context, r *decision.Record) error {
	if r.Actor == g.actor {
		g.once.Do(func() {
			close(g.reached)
			<-g.release
		})
	}
	return g.Store.Save(ctx, r)
}

// apprGatedAudits is an audit store that holds the first append whose path ends with suffix until
// the test releases it, so one replica's chain entry can be placed after another's.
type apprGatedAudits struct {
	audit.Store
	// suffix selects the append that is held.
	suffix string
	// reached is closed when the held append arrives.
	reached chan struct{}
	// release lets the held append continue.
	release chan struct{}
	// once makes only the first matching append wait.
	once sync.Once
}

// apprNewGatedAudits wraps inner so the first append whose path ends with suffix waits.
func apprNewGatedAudits(inner audit.Store, suffix string) *apprGatedAudits {
	return &apprGatedAudits{Store: inner, suffix: suffix, reached: make(chan struct{}),
		release: make(chan struct{})}
}

// Append holds the first matching append until released, then appends it.
func (g *apprGatedAudits) Append(ctx context.Context, e *audit.Entry) error {
	if strings.HasSuffix(e.Path, g.suffix) {
		g.once.Do(func() {
			close(g.reached)
			<-g.release
		})
	}
	return g.Store.Append(ctx, e)
}

// apprWait waits for ch to close, failing the test when it does not within the dispatch budget.
func apprWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitBudget):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// apprWaitOutcome waits until the chain holds the outcome entry of run id.
func apprWaitOutcome(t *testing.T, audits audit.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for time.Now().Before(deadline) {
		chain, err := audits.Chain(context.Background())
		if err != nil {
			t.Fatalf("Chain() error = %v", err)
		}
		for _, e := range chain {
			if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, "/runs/"+id+"/outcome/") {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never committed its outcome", id)
}

// apprDecisionVerdicts lists the verdicts of every DECISION entry naming run id, a whole run's and
// its approval steps' alike, in chain order.
func apprDecisionVerdicts(t *testing.T, audits audit.Store, id string) []string {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var out []string
	for _, e := range chain {
		if e.Method != audit.MethodDecision {
			continue
		}
		if runID, _, verdict, ok := outcome.ParseStepDecisionPath(e.Path); ok {
			if runID == id && verdict != outcome.StepRequested {
				out = append(out, verdict)
			}
			continue
		}
		if verdict, ok := strings.CutPrefix(e.Path, "/runs/"+id+"/decision/"); ok {
			out = append(out, verdict)
		}
	}
	return out
}

// apprFleetBundle builds, signs, and verifies the whole chain the way GET /v1/audit/bundle and
// switchtender verify do, and returns the verifier's report.
func apprFleetBundle(t *testing.T, audits audit.Store) *audit.BundleReport {
	t.Helper()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	doc, err := audit.BuildBundle(chain, id, "test", time.Now())
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	return rep
}

// TestApprovalThatLosesItsRaceLeavesTheChainVerifiable races an approval on one replica against
// the other way the same held work can end on a second replica sharing the store: another admin
// rejecting a held run, the requester canceling it, another approver denying a workflow's approval
// step, the step's timeout swept by the second replica's janitor, and the workflow being canceled.
//
// The approval has passed every check when it stalls on its record insert. The second replica ends
// the run and commits its outcome, and then the stalled approval continues. Its compare-and-set
// refuses it, and the approver is told the run is not awaiting approval, but its DECISION entry was
// appended before that compare-and-set. The chain therefore records an approval that never took
// effect, stamped after the outcome of a run that never executed under it.
//
// The bundle verifier reads an approval dated after the run it names as the gate being bypassed,
// so the whole-chain bundle of an honest install reports NOT VERIFIED from then on. The comment on
// approveRun names this exact symptom, after an approver double-clicks or rejects a moment too
// late, as the product's sharpest claim broken by its own record of a refused request. The status
// pre-check it added closes the sequential case only. The reliability page promises that approve
// and reject are compare-and-set transitions safe across replicas.
//
//nolint:funlen // Test function.
func TestApprovalThatLosesItsRaceLeavesTheChainVerifiable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Step decides a workflow approval step rather than a whole held run.
		Step bool
		// Race is how the second replica ends the work: reject, deny, timeout, or cancel.
		Race string
		// WantVerdicts are the decision verdicts the chain should hold for the run: only the one
		// that took effect.
		WantVerdicts []string
	}{{ // Test 0: A held run another admin rejects while the approval is in flight.
		Name: "held run rejected", Race: "reject", WantVerdicts: []string{"rejected"},
	}, { // Test 1: A held run the requester cancels while the approval is in flight.
		Name: "held run canceled", Race: "cancel", WantVerdicts: nil,
	}, { // Test 2: An approval step another approver denies while the approval is in flight.
		Name: "step denied", Step: true, Race: "deny",
		WantVerdicts: []string{outcome.StepRejected},
	}, { // Test 3: An approval step the other replica's janitor times out.
		Name: "step timed out", Step: true, Race: "timeout",
		WantVerdicts: []string{outcome.StepTimedOut},
	}, { // Test 4: A parked workflow canceled while the approval is in flight.
		Name: "workflow canceled", Step: true, Race: "cancel", WantVerdicts: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			audits := audit.NewMemStore()
			records := decision.NewMemStore()
			gated := apprNewGatedDecisions(records, "approver-a")
			runner := &commandRecorder{}
			replicaA := New(store, runner, nil, WithNoJanitor(), WithAudits(audits),
				WithDecisions(gated), WithOwner("replica-a"))
			defer replicaA.Close()
			replicaB := New(store, runner, nil, WithNoJanitor(), WithAudits(audits),
				WithDecisions(records), WithOwner("replica-b"))
			defer replicaB.Close()

			var target, workID string
			var created time.Time
			if test.Step {
				timeout := 0
				if test.Race == "timeout" {
					timeout = 1
				}
				parent, err := replicaB.SubmitPipeline(ctx, "release", "", gatedWorkflow(timeout, false),
					run.WithActor("requester"))
				if err != nil {
					t.Fatalf("SubmitPipeline() error = %v", err)
				}
				node := waitParked(t, store, parent.ID)
				target, workID, created = node.ID, parent.ID, node.CreatedAt
			} else {
				held, err := replicaB.Submit(ctx, "play.yml", "inv", run.WithRequireApproval(true),
					run.WithActor("requester"))
				if err != nil {
					t.Fatalf("Submit() error = %v", err)
				}
				target, workID = held.ID, held.ID
			}

			approverA := outcome.Decider{Name: "approver-a", Type: "session"}
			approved := make(chan error, 1)
			go func() {
				var err error
				if test.Step {
					_, err = replicaA.DecideStep(ctx, target, StepDecision{Approve: true, By: approverA})
				} else {
					_, err = replicaA.DecideRun(ctx, target, RunDecision{Approve: true, By: approverA})
				}
				approved <- err
			}()
			apprWait(t, gated.reached, "the approval to pass its checks")

			approverB := outcome.Decider{Name: "approver-b", Type: "session"}
			switch test.Race {
			case "reject":
				if _, err := replicaB.DecideRun(ctx, target, RunDecision{By: approverB}); err != nil {
					t.Fatalf("reject on the second replica error = %v", err)
				}
			case "deny":
				if _, err := replicaB.DecideStep(ctx, target, StepDecision{By: approverB}); err != nil {
					t.Fatalf("deny on the second replica error = %v", err)
				}
			case "timeout":
				time.Sleep(time.Until(created.Add(1100 * time.Millisecond)))
				replicaB.sweepApprovalSteps()
			case "cancel":
				if ok, err := replicaB.CancelWaiting(ctx, workID); err != nil || !ok {
					t.Fatalf("CancelWaiting() = (%v, %v), want (true, nil)", ok, err)
				}
			}
			final := waitTerminal(t, store, workID)
			apprWaitOutcome(t, audits, workID)

			close(gated.release)
			var err error
			select {
			case err = <-approved:
			case <-time.After(waitBudget):
				t.Fatal("the held approval never returned")
			}
			if !errors.Is(err, ErrNotPendingApproval) {
				t.Fatalf("the late approval returned %v, want ErrNotPendingApproval: the run ended "+
					"%s before it could take effect", err, final.Status)
			}

			if diff := cmp.Diff(test.WantVerdicts, apprDecisionVerdicts(t, audits, workID)); diff != "" {
				t.Errorf("the chain holds a decision the approver was told did not take effect, for "+
					"a run that ended %s (-want +got):\n%s", final.Status, diff)
			}
			if rep := apprFleetBundle(t, audits); !rep.OK() {
				t.Errorf("the whole-chain bundle of an install where no gate was bypassed reports NOT "+
					"VERIFIED (approval precedes run %v): %q", rep.ApprovalPrecedesRun,
					rep.TimeProblems)
			}
		})
	}
}

// TestApprovalStepRegisterCreditsTheDecisionThatTookEffect races an approval and a denial of one
// workflow approval step from two replicas. The approval wins the compare-and-set and the workflow
// ships, but the denial had passed its checks first and appends its DECISION entry after the
// workflow finished. The change register, the document built for an auditor, takes the newest
// decision on the chain as the verdict, so it reports the shipped workflow as rejected by the
// approver whose denial was refused. Its own comment says a DECISION entry is written only when the
// decision committed, which is false for a step decision.
func TestApprovalStepRegisterCreditsTheDecisionThatTookEffect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	records := decision.NewMemStore()
	gated := apprNewGatedDecisions(records, "approver-b")
	runner := &commandRecorder{}
	replicaA := New(store, runner, nil, WithNoJanitor(), WithAudits(audits),
		WithDecisions(records), WithOwner("replica-a"))
	defer replicaA.Close()
	replicaB := New(store, runner, nil, WithNoJanitor(), WithAudits(audits),
		WithDecisions(gated), WithOwner("replica-b"))
	defer replicaB.Close()

	start := time.Now().Add(-time.Minute)
	parent, err := replicaA.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true),
		run.WithActor("requester"))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)

	denied := make(chan error, 1)
	go func() {
		_, derr := replicaB.DecideStep(ctx, node.ID, StepDecision{Reason: "canary is red",
			By: outcome.Decider{Name: "approver-b", Type: "session"}})
		denied <- derr
	}()
	apprWait(t, gated.reached, "the denial to pass its checks")
	if _, err := replicaA.DecideStep(ctx, node.ID, StepDecision{Approve: true,
		By: outcome.Decider{Name: "approver-a", Type: "session"}}); err != nil {
		t.Fatalf("approve on the first replica error = %v", err)
	}
	final := waitTerminal(t, store, parent.ID)
	apprWaitOutcome(t, audits, parent.ID)
	if final.Status != run.StatusSucceeded || runner.count("deploy") != 1 {
		t.Fatalf("workflow = %s with deploy run %d times, want it shipped once", final.Status,
			runner.count("deploy"))
	}
	close(gated.release)
	if derr := <-denied; !errors.Is(derr, ErrNotPendingApproval) {
		t.Fatalf("the late denial returned %v, want ErrNotPendingApproval", derr)
	}

	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	reg, err := dossier.CollectRegister(ctx, store, audits, id, start, time.Now().Add(time.Minute),
		time.Now(), 0)
	if err != nil {
		t.Fatalf("CollectRegister() error = %v", err)
	}
	got := reg.Decisions[parent.ID]
	want := dossier.Decision{Verdict: "Approved", Actor: "approver-a"}
	if diff := cmp.Diff(want, dossier.Decision{Verdict: got.Verdict, Actor: got.Actor}); diff != "" {
		t.Errorf("the register's account of a workflow that shipped on an approval "+
			"(-want +got):\n%s", diff)
	}
}

// TestApprovalStepTimesOutOnceAcrossReplicas runs the approval step timeout sweep on two replicas
// at the moment a step expires, as every replica's janitor does after a restart that brings the
// cluster back past a deadline. One replica's sweep re-reads the step as still waiting and stalls
// on its chain append while the other times the step out and the workflow ends. The stalled sweep
// then appends a second timeout. Its settle is refused, but the entry stands, which is the double
// timeout timeOutStep's own comment says the re-read prevents.
func TestApprovalStepTimesOutOnceAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	gated := apprNewGatedAudits(audits, "/decision/"+outcome.StepTimedOut)
	runner := &commandRecorder{}
	replicaA := New(store, runner, nil, WithNoJanitor(), WithAudits(gated), WithOwner("replica-a"))
	defer replicaA.Close()
	replicaB := New(store, runner, nil, WithNoJanitor(), WithAudits(audits), WithOwner("replica-b"))
	defer replicaB.Close()

	parent, err := replicaB.SubmitPipeline(ctx, "release", "", gatedWorkflow(1, true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)
	time.Sleep(time.Until(node.CreatedAt.Add(1100 * time.Millisecond)))

	sweptA := make(chan struct{})
	go func() {
		defer close(sweptA)
		replicaA.sweepApprovalSteps()
	}()
	apprWait(t, gated.reached, "the first sweep to reach its timeout entry")
	replicaB.sweepApprovalSteps()
	final := waitTerminal(t, store, parent.ID)
	apprWaitOutcome(t, audits, parent.ID)
	close(gated.release)
	apprWait(t, sweptA, "the first sweep to finish")

	want := []string{outcome.StepTimedOut}
	if diff := cmp.Diff(want, apprDecisionVerdicts(t, audits, parent.ID)); diff != "" {
		t.Errorf("one approval step's timeouts on the chain, for a workflow that ended %s "+
			"(-want +got):\n%s", final.Status, diff)
	}
}

// apprHeldRunner succeeds every command and holds the named one until released, so a workflow can
// be kept running down one path while a decision lands on another replica.
type apprHeldRunner struct {
	// hold is the command that waits.
	hold string
	// started is closed when the held command begins.
	started chan struct{}
	// release lets the held command finish.
	release chan struct{}
	// once closes started a single time.
	once sync.Once
}

// Run holds the named command until released and succeeds.
func (h *apprHeldRunner) Run(ctx context.Context, spec roundhouse.Spec,
	_ io.Writer) (roundhouse.Result, error) {
	if spec.Command == h.hold {
		h.once.Do(func() { close(h.started) })
		select {
		case <-h.release:
		case <-ctx.Done():
			return roundhouse.Result{ExitCode: 1}, ctx.Err()
		}
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// apprVerdict is one decision a verified receipt discloses, reduced to who and what.
type apprVerdict struct {
	// Actor is who decided.
	Actor string
	// Verdict is what they decided.
	Verdict string
}

// TestApprovalStepReceiptDisclosesOnlyTheDecisionThatTookEffect races an approval on one replica
// against a denial on another, with the workflow's deny path still running when the refused
// approval continues. The approval's DECISION entry lands inside the workflow's receipt segment,
// before its outcome, so the receipt verifies and the verifier lists, as a digest-verified
// decision on the step, an approval the approver was told did not take effect, beside the denial
// that did. A relying party reading the verified receipt is shown the step approved by a person
// whose approval was refused, on a workflow that took its deny path.
func TestApprovalStepReceiptDisclosesOnlyTheDecisionThatTookEffect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	records := decision.NewMemStore()
	gated := apprNewGatedDecisions(records, "approver-a")
	runner := &apprHeldRunner{hold: "notify", started: make(chan struct{}),
		release: make(chan struct{})}
	replicaA := New(store, runner, nil, WithNoJanitor(), WithAudits(audits),
		WithDecisions(gated), WithOwner("replica-a"))
	defer replicaA.Close()
	replicaB := New(store, runner, nil, WithNoJanitor(), WithAudits(audits),
		WithDecisions(records), WithOwner("replica-b"))
	defer replicaB.Close()

	creation := &audit.Entry{ID: audit.NewID(), At: time.Now(), Actor: "requester",
		ActorType: "session", Method: "POST", Path: "/v1/workflows"}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("append the creation request: %v", err)
	}
	launch := run.WithAuditReceipt(ctx, audit.Receipt(creation))
	parent, err := replicaB.SubmitPipeline(launch, "release", "", gatedWorkflow(0, true),
		run.WithActor("requester"))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)

	approved := make(chan error, 1)
	go func() {
		_, aerr := replicaA.DecideStep(ctx, node.ID, StepDecision{Approve: true,
			By: outcome.Decider{Name: "approver-a", Type: "session"}})
		approved <- aerr
	}()
	apprWait(t, gated.reached, "the approval to pass its checks")
	if _, err := replicaB.DecideStep(ctx, node.ID, StepDecision{
		By: outcome.Decider{Name: "approver-b", Type: "session"}}); err != nil {
		t.Fatalf("deny on the second replica error = %v", err)
	}
	apprWait(t, runner.started, "the deny path to start")
	close(gated.release)
	if aerr := <-approved; !errors.Is(aerr, ErrNotPendingApproval) {
		t.Fatalf("the late approval returned %v, want ErrNotPendingApproval", aerr)
	}
	close(runner.release)
	final := waitTerminal(t, store, parent.ID)
	apprWaitOutcome(t, audits, parent.ID)

	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	res, err := receipt.Build(ctx, store, audits, id, "test", parent.ID,
		receipt.Options{Decisions: records})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	var got []apprVerdict
	for _, dec := range rep.Decisions {
		if dec.Verdict != outcome.StepRequested {
			got = append(got, apprVerdict{Actor: dec.Actor, Verdict: dec.Verdict})
		}
	}
	want := []apprVerdict{{Actor: "approver-b", Verdict: outcome.StepRejected}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the verified receipt (ok %v) of a workflow that ended %s on its deny path "+
			"discloses these decisions on its step (-want +got):\n%s", rep.OK(), final.Status, diff)
	}
}
