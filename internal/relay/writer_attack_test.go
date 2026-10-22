package relay

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// relayAttack is how a database writer's move ended on a keyed control node.
type relayAttack struct {
	// Error is the reason the node recorded on the row the writer touched.
	Error string
	// Handed reports whether a worker was handed the writer's row, or allowed to run it again.
	Handed bool
}

// claimRewritten has the node's worker claim after the writer's edit and reports whether it was
// handed the run id, then reads how id ended.
func claimRewritten(t *testing.T, n *bndNode, id string) relayAttack {
	t.Helper()
	status, _, body := n.claim(t)
	got, err := n.Store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return relayAttack{Error: got.Error,
		Handed: status == http.StatusOK && strings.Contains(string(body), id)}
}

// TestADatabaseWriterIsRefusedByAKeyedControlNode is unit 10's table for the relay control node:
// each row is a move from the A1 to A22 list made by a principal who can write the runs table and
// not the binding key, performed through the store against a keyed control node. Each is refused
// before a worker runs anything, with the reason named on the row.
//
//nolint:funlen // Test function.
func TestADatabaseWriterIsRefusedByAKeyedControlNode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the row.
		Name string
		// Move makes the writer's move on n and drives the node until it lands.
		Move func(t *testing.T, n *bndNode) relayAttack
		// WantError is what the refusal must say.
		WantError string
	}{{ // Test 0: A submitted run's playbook rewritten before a worker claims it.
		Name: "submitted run rewritten",
		Move: func(t *testing.T, n *bndNode) relayAttack {
			n.submitted(t, "run_top")
			n.rewrite(t, "run_top", func(r *run.Run) { r.Playbook = "wipe.yml" })
			return claimRewritten(t, n, "run_top")
		},
		WantError: "changed after it was submitted",
	}, { // Test 1: An approved run's inventory rewritten after the approval.
		Name: "approved run rewritten",
		Move: func(t *testing.T, n *bndNode) relayAttack {
			n.approved(t, "run_ok", nil)
			n.rewrite(t, "run_ok", func(r *run.Run) { r.Inventory = "everywhere\n" })
			return claimRewritten(t, n, "run_ok")
		},
		WantError: "changed after it was approved",
	}, { // Test 2: An approved run named by another run's decision.
		Name: "forged decision id",
		Move: func(t *testing.T, n *bndNode) relayAttack {
			n.approved(t, "run_ok", nil)
			other := n.approved(t, "run_other", nil)
			n.rewrite(t, "run_other", func(r *run.Run) { r.Queue = "parked" })
			// A whole-row save keeps the decision a row holds, so the writer names the other
			// decision the way the decision columns are written, as a held run decided by it.
			n.rewrite(t, "run_ok", func(r *run.Run) { r.Status = run.StatusPendingApproval })
			n.decide(t, "run_ok", other.DecisionID, run.StatusPending, "")
			return claimRewritten(t, n, "run_ok")
		},
		WantError: "is not the decision this row names",
	}, { // Test 3: A released step's host limit rewritten.
		Name: "released step rewritten",
		Move: func(t *testing.T, n *bndNode) relayAttack {
			n.step(t, n.parentRun(t, "run_flow", run.KindPipeline, true), "run_step")
			n.rewrite(t, "run_step", func(r *run.Run) { r.Limit = "everywhere" })
			return claimRewritten(t, n, "run_step")
		},
		WantError: "changed after it was built",
	}, { // Test 4: A shard added under a split with no seal of its own.
		Name: "bare child of a split",
		Move: func(t *testing.T, n *bndNode) relayAttack {
			parent := n.parentRun(t, "run_split", run.KindSplit, true)
			index := 3
			n.save(t, &run.Run{ID: "run_bare", ParentID: &parent.ID, ShardIndex: &index,
				Playbook: "site.yml", Limit: "everywhere", Status: run.StatusPending,
				CreatedAt: time.Now()})
			return claimRewritten(t, n, "run_bare")
		},
		WantError: "carries no seal",
	}, { // Test 5: A finished run reset to pending and claimed again.
		Name: "finished run reset",
		Move: func(t *testing.T, n *bndNode) relayAttack {
			id := n.submitted(t, "run_once").ID
			runOnce(t, startWorker(n), bndWorker, id)
			n.rewrite(t, id, func(r *run.Run) {
				r.Status, r.ClaimedBy, r.ClaimSecret, r.Error = run.StatusPending, "", "", ""
				r.ClaimedAt, r.StartedAt, r.EndedAt, r.ExitCode = nil, nil, nil, nil
			})
			worker := startWorker(n)
			ctx := context.Background()
			if _, err := worker.Claim(ctx, "worker-2", []string{""}); err != nil {
				t.Fatalf("Claim() error = %v", err)
			}
			moved, err := worker.StartClaimed(ctx, id, "worker-2", "", time.Now())
			got, gerr := n.Store.Get(ctx, id)
			if gerr != nil {
				t.Fatalf("Get() error = %v", gerr)
			}
			return relayAttack{Error: got.Error, Handed: err == nil && moved}
		},
		WantError: "already started once",
	}, { // Test 6: A finished step's outputs rewritten after the node sealed its result.
		Name: "step result rewritten",
		Move: func(t *testing.T, n *bndNode) relayAttack {
			ctx := context.Background()
			n.step(t, n.parentRun(t, "run_flow", run.KindPipeline, true), "run_step")
			worker := startWorker(n)
			leased, err := worker.Claim(ctx, bndWorker, []string{""})
			if err != nil {
				t.Fatalf("Claim() error = %v", err)
			}
			code := 0
			leased.Status, leased.ExitCode = run.StatusSucceeded, &code
			leased.Outputs = map[string]any{"version": "1.4.2"}
			if err := worker.Save(ctx, leased); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			n.rewrite(t, "run_step", func(r *run.Run) { r.Outputs["version"] = "6.6.6" })
			forged, err := n.Store.Get(ctx, "run_step")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			rerr := n.Binder.Result(ctx, forged)
			if rerr == nil {
				return relayAttack{Handed: true}
			}
			return relayAttack{Error: rerr.Error()}
		},
		WantError: "its status or its outputs changed after it finished",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			n := newBndNode(t, bndConfig{})
			got := test.Move(t, n)
			if got.Handed {
				t.Errorf("a worker was handed the writer's row")
			}
			if !strings.HasPrefix(got.Error, "refused") || !strings.Contains(got.Error, test.WantError) {
				t.Errorf("the move ended with %q, want a refusal saying %q", got.Error, test.WantError)
			}
		})
	}
}
