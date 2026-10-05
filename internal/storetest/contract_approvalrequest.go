package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// testApprovalRequestedKept pins how every backend keeps a request's ask for approval: it round
// trips through every read, and a later whole-row save that does not carry it, such as one from a
// copy built without it, does not clear it. The executor reads it to hold the apply the request's
// plan proposes, so a save that dropped it would let that apply run without the approval it asked
// for.
func testApprovalRequestedKept(t *testing.T, store run.Store) {
	ctx := context.Background()
	r := &run.Run{ID: "run_asked", Tool: run.ToolTerraform, Command: "infra",
		Status: run.StatusPending, CreatedAt: time.Now(), ApprovalRequested: true}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.ApprovalRequested {
		t.Fatal("the request's ask for approval did not round trip")
	}
	listed, err := store.NonTerminal(ctx)
	if err != nil {
		t.Fatalf("NonTerminal() error = %v", err)
	}
	if len(listed) != 1 || !listed[0].ApprovalRequested {
		t.Errorf("a list read lost the ask for approval: %+v", listed)
	}
	without := got.Clone()
	without.ApprovalRequested = false
	without.Status = run.StatusRunning
	if err := store.Save(ctx, without); err != nil {
		t.Fatalf("Save(without) error = %v", err)
	}
	if kept, err := store.Get(ctx, r.ID); err != nil || !kept.ApprovalRequested {
		t.Errorf("a later save cleared the ask for approval: %v", err)
	}
	plain := &run.Run{ID: "run_plain", Tool: run.ToolTerraform, Command: "infra",
		Status: run.StatusPending, CreatedAt: time.Now()}
	if err := store.Save(ctx, plain); err != nil {
		t.Fatalf("Save(plain) error = %v", err)
	}
	if got, err := store.Get(ctx, plain.ID); err != nil || got.ApprovalRequested {
		t.Errorf("a run that asked for nothing reads as asking: %v", err)
	}
}
