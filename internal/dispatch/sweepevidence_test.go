package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// TestASweptRunLeavesEvidence covers the runs nobody is left to finalize. When a worker dies mid-run,
// its lease expires and the janitor settles the run: the row becomes interrupted and the operator sees
// it in the interface. Nothing committed that outcome to the chain, because the sweep is a bulk update
// in the store rather than a pass through the dispatcher's finalize, so the run ended with no evidence
// at all. It could not be receipted, and its dossier showed a run that started and then simply stopped
// having a story.
//
// That is the worst case to have no record of: a change was executing on real hosts when the process
// running it died, which is exactly the incident an auditor asks about afterward.
func TestASweptRunLeavesEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()

	// A run a dead worker was holding: running, leased, and the lease long past renewal.
	stale := time.Now().Add(-10 * time.Minute)
	orphan := &run.Run{
		ID: "run_orphan", Status: run.StatusRunning, CreatedAt: stale,
		Tool: run.ToolBash, Command: "deploy", Actor: "casey", ActorType: "session",
		ClaimedBy: "worker-that-died", ClaimedAt: &stale, StartedAt: &stale,
	}
	if err := store.Save(ctx, orphan); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A dispatcher with an audit store sweeps once as it starts, which is the restart case too.
	d := New(store, okRunner(), zap.NewNop(), WithAudits(audits))
	defer d.Close()

	deadline := time.Now().Add(15 * time.Second)
	var got *run.Run
	for time.Now().Before(deadline) {
		got, _ = store.Get(ctx, orphan.ID)
		if got != nil && got.Status.Terminal() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got == nil || !got.Status.Terminal() {
		t.Fatalf("the swept run never reached a terminal state: %+v", got)
	}
	if got.Status != run.StatusInterrupted {
		t.Errorf("swept run status = %q, want interrupted", got.Status)
	}

	// The evidence: the chain has to hold this run's outcome, the same as any other terminal run.
	deadline = time.Now().Add(10 * time.Second)
	var outcomes int
	for time.Now().Before(deadline) {
		chain, err := audits.Chain(ctx)
		if err != nil {
			t.Fatalf("Chain: %v", err)
		}
		outcomes = 0
		for _, e := range chain {
			if strings.Contains(e.Path, orphan.ID) && strings.Contains(e.Path, "/outcome/") {
				outcomes++
			}
		}
		if outcomes > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if outcomes != 1 {
		t.Fatalf("the chain holds %d outcome entries for a run the janitor settled, want exactly 1: "+
			"a run interrupted when its worker died is the incident an auditor asks about, and it "+
			"left no record", outcomes)
	}
}
