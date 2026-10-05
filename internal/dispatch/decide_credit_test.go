package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dossier"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// TestEveryReaderCreditsOnlyTheStoredWinner decides a held run, then leaves beside the winning
// approval what a losing decider can: a rejection entry on the chain, of the kind an earlier
// release appended before its compare-and-set refused it, and a stray decision record, of the kind
// a decider that lost its claim leaves when it dies before withdrawing it. The register, the
// receipt, and the decision list each credit the decision the run stores as its winner and no
// other, where the register used to credit the newest entry on the chain and the list every record.
func TestEveryReaderCreditsOnlyTheStoredWinner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	records := decision.NewMemStore()
	d := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithAudits(audits),
		WithDecisions(records))
	defer d.Close()
	start := time.Now().Add(-time.Minute)
	creation := &audit.Entry{ID: audit.NewID(), At: time.Now(), Actor: "requester",
		ActorType: "session", Method: "POST", Path: "/v1/runs"}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("append the creation request: %v", err)
	}
	launch := run.WithAuditReceipt(ctx, audit.Receipt(creation))
	held, err := d.Submit(launch, "play.yml", "inv", run.WithRequireApproval(true),
		run.WithActor("requester"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	winner := outcome.Decider{Name: "approver-a", Type: "session"}
	if _, err := d.DecideRun(ctx, held.ID, RunDecision{Approve: true, By: winner}); err != nil {
		t.Fatalf("DecideRun() error = %v", err)
	}
	loser := &decision.Record{ID: audit.NewID(), Kind: decision.KindDecision, RunID: held.ID,
		Verdict: "rejected", At: time.Now(), Actor: "approver-b", ActorType: "session"}
	loser.DecisionID = loser.ID
	if err := records.Save(ctx, loser); err != nil {
		t.Fatalf("Save() of the stray record error = %v", err)
	}
	if _, err := outcome.CommitDecisionWith(ctx, audits, held, "rejected",
		outcome.Decider{Name: "approver-b", Type: "session"}, time.Now,
		outcome.DecisionExtras{ID: loser.ID}); err != nil {
		t.Fatalf("append the losing entry: %v", err)
	}
	waitTerminal(t, store, held.ID)
	apprWaitOutcome(t, audits, held.ID)

	listed, err := d.Decisions(ctx, held.ID)
	if err != nil {
		t.Fatalf("Decisions() error = %v", err)
	}
	var actors []string
	for _, rec := range listed {
		actors = append(actors, rec.Actor)
	}
	if diff := cmp.Diff([]string{"approver-a"}, actors); diff != "" {
		t.Errorf("the decisions listed for the run (-want +got):\n%s", diff)
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
	got := reg.Decisions[held.ID]
	want := dossier.Decision{Verdict: "Approved", Actor: "approver-a"}
	if diff := cmp.Diff(want, dossier.Decision{Verdict: got.Verdict, Actor: got.Actor}); diff != "" {
		t.Errorf("the register's account of the run (-want +got):\n%s", diff)
	}

	res, err := receipt.Build(ctx, store, audits, id, "test", held.ID,
		receipt.Options{Decisions: records})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	var disclosed []apprVerdict
	for _, dec := range rep.Decisions {
		disclosed = append(disclosed, apprVerdict{Actor: dec.Actor, Verdict: dec.Verdict})
	}
	wantDisclosed := []apprVerdict{{Actor: "approver-a", Verdict: "approved"}}
	if diff := cmp.Diff(wantDisclosed, disclosed); diff != "" {
		t.Errorf("the decisions the verified receipt discloses (-want +got):\n%s", diff)
	}
}
