package dispatch

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// failUnsealedState is what FailUnsealed did to an install, each row named by its role.
type failUnsealedState struct {
	// Failed are the roles FailUnsealed ended, sorted.
	Failed []string
	// Left are the roles it left unsealed, sorted.
	Left []string
	// Sealed are the roles it left for a keyed server's check, sorted.
	Sealed []string
	// Status holds each role's status afterward.
	Status map[string]run.Status
}

// TestFailUnsealedTrustsNoRow pins FailUnsealed across every state an earlier binary leaves a row
// in. A held run, a queued run, and a workflow parked at an approval step with that step are
// failed as not adopted, each top-level outcome committed. A run under a lease and a decision in
// flight are left, and a run that carries a seal is left for the keyed check, which refuses one a
// writer planted. Nothing is sealed, a keyed server afterward runs none of it, and a second call
// finds the record and writes nothing.
//
//nolint:funlen // Test function.
func TestFailUnsealedTrustsNoRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := newAdoptInstall(t, run.NewMemStore())
	parent, node := in.parked(t)
	held := in.held(t)
	inFlight := in.held(t)
	in.stuck(t, func() error {
		_, err := in.old.Approve(ctx, inFlight.ID, bndDecider())
		return err
	})
	in.stopOld()
	ranBefore := in.runner.counts()
	now := time.Now()
	queued := &run.Run{ID: run.NewID(), Playbook: "site.yml", Inventory: "inv",
		Status: run.StatusPending, CreatedAt: now}
	running := &run.Run{ID: run.NewID(), Playbook: "site.yml", Inventory: "inv",
		Status: run.StatusRunning, CreatedAt: now, ClaimedBy: "old-worker", ClaimedAt: &now,
		StartedAt: &now}
	sealed := &run.Run{ID: run.NewID(), Playbook: "site.yml", Inventory: "inv",
		Status: run.StatusPending, CreatedAt: now, SpecSeal: "bk1:00"}
	for _, r := range []*run.Run{queued, running, sealed} {
		if err := in.store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	roles := map[string]string{parent.ID: "parked workflow", node.ID: "approval step",
		held.ID: "held", inFlight.ID: "in flight", queued.ID: "queued", running.ID: "running",
		sealed.ID: "sealed"}
	named := func(ids []string) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, roles[id])
		}
		slices.Sort(out)
		return out
	}

	res, err := FailUnsealed(ctx, in.deps())
	if err != nil {
		t.Fatalf("FailUnsealed() error = %v", err)
	}
	if res.Earlier || res.Record == nil {
		t.Fatalf("FailUnsealed() = earlier %t, record %v, want this call's record", res.Earlier,
			res.Record)
	}
	got := failUnsealedState{Failed: named(res.Failed), Sealed: named(res.Work.Sealed),
		Status: map[string]run.Status{}}
	for _, l := range res.Work.Left {
		got.Left = append(got.Left, roles[l.ID])
	}
	slices.Sort(got.Left)
	for id, role := range roles {
		r := in.get(t, id)
		got.Status[role] = r.Status
		if r.Status == run.StatusFailed && r.Error != NotAdoptedText {
			t.Errorf("%s failed with %q, want %q", role, r.Error, NotAdoptedText)
		}
		if role != "sealed" && CarriesSeal(r) {
			t.Errorf("%s carries a seal after FailUnsealed", role)
		}
	}
	want := failUnsealedState{
		Failed: []string{"approval step", "held", "parked workflow", "queued"},
		Left:   []string{"in flight", "running"},
		Sealed: []string{"sealed"},
		Status: map[string]run.Status{"parked workflow": run.StatusFailed,
			"approval step": run.StatusFailed, "held": run.StatusFailed,
			"queued": run.StatusFailed, "in flight": run.StatusPendingApproval,
			"running": run.StatusRunning, "sealed": run.StatusPending},
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("FailUnsealed mismatch (-want +got):\n%s", diff)
	}
	for _, id := range []string{parent.ID, held.ID, queued.ID} {
		if owed, err := in.store.OutcomeOwed(ctx, id); err != nil || owed {
			t.Errorf("OutcomeOwed(%s) = %t, %v, want the outcome committed", roles[id], owed, err)
		}
	}

	again, err := FailUnsealed(ctx, in.deps())
	if err != nil || !again.Earlier {
		t.Errorf("second FailUnsealed() = %+v, %v, want the earlier record", again, err)
	}
	in.keyed(t)
	if r := waitTerminal(t, in.store, sealed.ID); r.Status != run.StatusFailed {
		t.Errorf("the planted seal ended %s (%s), want refused", r.Status, r.Error)
	}
	if diff := cmp.Diff(ranBefore, in.runner.counts()); diff != "" {
		t.Errorf("executions after FailUnsealed mismatch (-before +after):\n%s", diff)
	}
}

// shownDecision is how adoption shows one row's decision, the decision entry named by role.
type shownDecision struct {
	// DecidedBy is who the row is shown as decided by.
	DecidedBy string
	// DecisionEntry is the row's decision entry: "decision" for the one its row names, empty for
	// none.
	DecisionEntry string
	// Unrecorded reports a claim whose decision the chain does not hold.
	Unrecorded bool
}

// TestAdoptionShowsWhoDecided pins the decision columns of the adoption table. A held run nobody
// decided shows none, an approved run shows its approver and the DECISION entry its seal names,
// and a claim whose decision never reached the chain shows the decider its claim records and
// counts as unrecorded.
func TestAdoptionShowsWhoDecided(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := newAdoptInstall(t, run.NewMemStore())
	held := in.held(t)
	approved := in.held(t, run.WithQueue("work"))
	if _, err := in.old.Approve(ctx, approved.ID, bndDecider()); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	claimed := in.held(t)
	in.stuck(t, func() error {
		_, err := in.old.Approve(ctx, claimed.ID, bndDecider())
		return err
	})
	in.stopOld()
	decisionOf := map[string]string{in.get(t, approved.ID).DecisionID: "decision",
		in.get(t, claimed.ID).DecisionID: "decision"}
	work, err := AdoptionPlan(ctx, in.deps())
	if err != nil {
		t.Fatalf("AdoptionPlan() error = %v", err)
	}
	got := map[string]shownDecision{}
	roles := map[string]string{held.ID: "held", approved.ID: "approved", claimed.ID: "claimed"}
	for _, r := range work.Rows {
		got[roles[r.ID]] = shownDecision{DecidedBy: r.DecidedBy,
			DecisionEntry: decisionOf[r.DecisionEntry], Unrecorded: r.Unrecorded}
	}
	want := map[string]shownDecision{
		"held":     {},
		"approved": {DecidedBy: bndDecider().Name, DecisionEntry: "decision"},
		"claimed":  {DecidedBy: bndDecider().Name, DecisionEntry: "decision", Unrecorded: true},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("shown decisions mismatch (-want +got):\n%s", diff)
	}
}
