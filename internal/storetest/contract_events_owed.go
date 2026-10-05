package storetest

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// owedKeys renders owed events as run/event, sorted.
func owedKeys(list []run.OwedEvent) []string {
	out := []string{}
	for _, ev := range list {
		out = append(out, ev.RunID+"/"+ev.Event)
	}
	sort.Strings(out)
	return out
}

// testEventLedger verifies every path that moves a top-level run into running leaves its start
// owed, and every path that stores a run held or moves it into a held state from one that was not
// leaves its hold owed, a parked workflow included, whichever write makes the move. A decision that
// claims a hold owes nothing new, nor does an approval that queues the run, a run stored already
// running, or a child of a split or pipeline, and a settled event stays settled when the run is
// saved again where it stands.
func testEventLedger(t *testing.T, store run.Store) {
	ledger, ok := store.(run.EventLedger)
	if !ok {
		t.Fatalf("%T keeps no ledger of owed starts and holds, so a run whose process stopped "+
			"before announcing either is never announced", store)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	parent := "ev_parent"
	for _, r := range []*run.Run{
		{ID: "ev_claimed", Status: run.StatusPending, ClaimedBy: "w1", ClaimedAt: &now,
			ClaimSecret: "secret_1"},
		{ID: "ev_fenced", Status: run.StatusPending, Kind: run.KindPipeline},
		{ID: "ev_decided", Status: run.StatusPendingApproval},
		{ID: "ev_parked", Status: run.StatusRunning, Kind: run.KindPipeline, ClaimedBy: "w1",
			ClaimedAt: &now, StartedAt: &now},
		{ID: "ev_saved", Status: run.StatusPending},
		{ID: "ev_saved_held", Status: run.StatusPending},
		{ID: "ev_approved", Status: run.StatusPendingApproval},
		{ID: "ev_running", Status: run.StatusRunning, ClaimedBy: "w1", ClaimedAt: &now,
			StartedAt: &now},
		{ID: parent, Status: run.StatusRunning, Kind: run.KindPipeline, ClaimedBy: "w1",
			ClaimedAt: &now, StartedAt: &now},
		{ID: "ev_child_held", Status: run.StatusPendingApproval, Kind: run.KindApproval,
			ParentID: &parent},
		{ID: "ev_child_run", Status: run.StatusPending, ParentID: &parent},
	} {
		r.Playbook, r.CreatedAt = "site.yml", now
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	// The run stored running and the parent are settled before the moves, so the parent's own
	// start is not mistaken for one of the moves below.
	_ = ledger.SettleEvent(ctx, parent, run.OwedStart)
	resave := func(id string, status run.Status) func() (bool, error) {
		return func() (bool, error) {
			r, err := store.Get(ctx, id)
			if err != nil {
				return false, err
			}
			r.Status = status
			return true, store.Save(ctx, r)
		}
	}
	moves := []struct {
		Name string
		Move func() (bool, error)
	}{{
		Name: "start a claimed run",
		Move: func() (bool, error) {
			return store.StartClaimed(ctx, "ev_claimed", "w1", "secret_1", now)
		},
	}, {
		Name: "fence a coordinator's start",
		Move: func() (bool, error) {
			return store.TransitionStatusAndClaim(ctx, "ev_fenced", run.StatusPending,
				run.StatusRunning, "w1", now)
		},
	}, {
		Name: "claim a decision",
		Move: func() (bool, error) { return store.ClaimDecision(ctx, "ev_decided", "dec_1", "c1") },
	}, {
		Name: "settle a decision that starts the run",
		Move: func() (bool, error) {
			return store.SettleDecision(ctx, "ev_decided", "dec_1", run.DecisionSettle{
				Status: run.StatusRunning, Owner: "w1"})
		},
	}, {
		Name: "park a workflow at an approval step",
		Move: func() (bool, error) { return store.ParkForApproval(ctx, "ev_parked", "w1") },
	}, {
		Name: "resume the parked workflow",
		Move: func() (bool, error) {
			return store.TransitionStatusAndClaim(ctx, "ev_parked", run.StatusPendingApproval,
				run.StatusRunning, "w2", time.Time{})
		},
	}, {
		Name: "whole-row save into running",
		Move: resave("ev_saved", run.StatusRunning),
	}, {
		Name: "whole-row save into held",
		Move: resave("ev_saved_held", run.StatusPendingApproval),
	}, {
		Name: "approve a hold into the queue",
		Move: func() (bool, error) {
			return store.TransitionStatus(ctx, "ev_approved", run.StatusPendingApproval,
				run.StatusPending)
		},
	}, {
		Name: "start a child",
		Move: func() (bool, error) {
			return store.TransitionStatus(ctx, "ev_child_run", run.StatusPending, run.StatusRunning)
		},
	}}
	for _, m := range moves {
		if moved, err := m.Move(); err != nil || !moved {
			t.Fatalf("%s: moved = %v, error = %v, want the run moved", m.Name, moved, err)
		}
	}

	owed, err := ledger.OwedEvents(ctx, 0, 0)
	if err != nil {
		t.Fatalf("OwedEvents() error = %v", err)
	}
	want := []string{
		"ev_approved/approval", "ev_claimed/started", "ev_decided/approval", "ev_decided/started",
		"ev_fenced/started", "ev_parked/approval", "ev_parked/started", "ev_saved/started",
		"ev_saved_held/approval",
	}
	if diff := cmp.Diff(want, owedKeys(owed)); diff != "" {
		t.Errorf("owed starts and holds mismatch (-want +got):\n%s", diff)
	}
	if limited, err := ledger.OwedEvents(ctx, 0, 2); err != nil || len(limited) != 2 {
		t.Errorf("OwedEvents() with a limit of 2 = %v, %v", limited, err)
	}
	if early, err := ledger.OwedEvents(ctx, time.Hour, 0); err != nil || len(early) != 0 {
		t.Errorf("OwedEvents() an hour of grace after the moves = %v, %v, want none yet", early, err)
	}

	for _, ev := range []run.OwedEvent{
		{RunID: "ev_saved_held", Event: run.OwedHold}, {RunID: "ev_saved_held", Event: run.OwedHold},
		{RunID: "ev_claimed", Event: run.OwedStart}, {RunID: "ev_ghost", Event: run.OwedStart},
	} {
		if err := ledger.SettleEvent(ctx, ev.RunID, ev.Event); err != nil {
			t.Fatalf("SettleEvent(%s %s) error = %v", ev.RunID, ev.Event, err)
		}
	}
	// A settled run saved again where it stands owes nothing new.
	for _, id := range []string{"ev_saved_held", "ev_claimed"} {
		again, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		again.Warning = "noted after the event"
		if err := store.Save(ctx, again); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}
	owed, err = ledger.OwedEvents(ctx, 0, 0)
	if err != nil {
		t.Fatalf("OwedEvents() after settling error = %v", err)
	}
	wantAfter := []string{
		"ev_approved/approval", "ev_decided/approval", "ev_decided/started", "ev_fenced/started",
		"ev_parked/approval", "ev_parked/started", "ev_saved/started",
	}
	if diff := cmp.Diff(wantAfter, owedKeys(owed)); diff != "" {
		t.Errorf("owed starts and holds after settling two and saving them again mismatch "+
			"(-want +got):\n%s", diff)
	}
}
