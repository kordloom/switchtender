package dispatch

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// attackOutcome is how a database writer's move ended on a keyed install.
type attackOutcome struct {
	// Error is the reason recorded where the move was stopped: the run's or the workflow's error,
	// or the error a decision returned.
	Error string
	// Ran counts the executions the move would have bought: what the writer wrote, or a second
	// execution of what already ran.
	Ran int
}

// keyedAt returns a keyed dispatcher over store and chain serving queues, which claims quickly.
func keyedAt(t *testing.T, store run.Store, chain audit.Store, runner roundhouse.Runner,
	queues ...string) *Dispatcher {
	t.Helper()
	opts := []Option{WithNoJanitor(), WithAudits(chain), WithBindingKey(bndKey(t)),
		WithClaimInterval(5 * time.Millisecond)}
	if len(queues) > 0 {
		opts = append(opts, WithQueues(queues))
	}
	return New(store, runner, nil, opts...)
}

// planFirstMove submits a terraform apply that must plan first, as a person who asked for approval
// or as an agent whose run was approved, applies the writer's edit to its row, and lets a keyed
// executor claim it. Every apply and every plan of a refused row is counted as bought.
func planFirstMove(t *testing.T, agent bool, edit func(r, decided *run.Run) func(*run.Run)) attackOutcome {
	t.Helper()
	ctx := context.Background()
	store, chain := newBndRows(), audit.NewMemStore()
	submitter := keyedAt(t, store, chain, &phaseRunner{}, "decisions")
	opts := []run.SubmitOption{run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"),
		run.WithQueue("work")}
	if agent {
		opts = append(opts, run.WithActor("agent-1"), run.WithActorType("agent"))
	} else {
		opts = append(opts, run.WithRequireApproval(true))
	}
	r, err := submitter.Submit(ctx, "", "", opts...)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if agent {
		if r, err = submitter.Approve(ctx, r.ID, bndDecider()); err != nil {
			t.Fatalf("Approve() error = %v", err)
		}
	}
	other, err := submitter.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("other"),
		run.WithQueue("parked"), run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	decided, err := submitter.Approve(ctx, other.ID, bndDecider())
	if err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	submitter.Close()
	store.rewrite(r.ID, edit(r, decided))
	runner := &phaseRunner{}
	executor := keyedAt(t, store, chain, runner, "work")
	got := waitTerminal(t, store.Store, r.ID)
	executor.Close()
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return attackOutcome{Error: got.Error, Ran: runner.plans + runner.applies}
}

// finishedApproved runs an approved deploy to its end on a keyed install and returns the store,
// the chain, the runner, and the finished row, with the dispatcher still serving queue work.
func finishedApproved(t *testing.T) (run.Store, audit.Store, *commandRecorder, *run.Run) {
	t.Helper()
	ctx := context.Background()
	store, chain, runner := run.NewMemStore(), audit.NewMemStore(), &commandRecorder{}
	d := keyedAt(t, store, chain, runner, "work")
	t.Cleanup(d.Close)
	held, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
		run.WithQueue("work"), run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if _, err := d.Approve(ctx, held.ID, bndDecider()); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	return store, chain, runner, waitTerminal(t, store, held.ID)
}

// approvedSplitMove approves a two-host split on a keyed install, lets the writer add a row under
// it, and returns how that row ended and how often the writer's host limit ran.
func approvedSplitMove(t *testing.T, add func(split, first *run.Run) *run.Run) attackOutcome {
	t.Helper()
	ctx := context.Background()
	store, chain, runner := run.NewMemStore(), audit.NewMemStore(), &childRunner{}
	d := keyedAt(t, store, chain, runner, "decisions")
	defer d.Close()
	split, err := d.SubmitSplit(ctx, "site.yml", "inv", 2, run.WithRequireApproval(true),
		run.WithQueue("work"))
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if _, err := d.Approve(ctx, split.ID, bndDecider()); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	var first *run.Run
	deadline := time.Now().Add(waitBudget)
	for first == nil && time.Now().Before(deadline) {
		shards, err := store.Shards(ctx, split.ID)
		if err != nil {
			t.Fatalf("Shards() error = %v", err)
		}
		for _, s := range shards {
			if isShard(s) {
				first = s
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if first == nil {
		t.Fatalf("split %s stored no first shard", split.ID)
	}
	row := add(split, first)
	if err := store.Save(ctx, row); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	e := keyedAt(t, store, chain, runner, "work")
	defer e.Close()
	got := waitTerminal(t, store, row.ID)
	return attackOutcome{Error: got.Error, Ran: runner.count("limit:" + rewritten)}
}

// parkedMove parks a build, approval, deploy, notify workflow on a keyed install, applies the
// writer's edit to the store while it waits, then lets act drive it on, and returns how the
// workflow ended, the error act returned when it has one, and how often the writer's command, the
// deny path, or a second build ran.
func parkedMove(t *testing.T, timeout int, edit func(t *testing.T, store run.Store, parent, node *run.Run),
	act func(d *Dispatcher, node *run.Run) error) attackOutcome {
	t.Helper()
	ctx := context.Background()
	store, chain, runner := run.NewMemStore(), audit.NewMemStore(), &commandRecorder{}
	d := keyedAt(t, store, chain, runner)
	defer d.Close()
	parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(timeout, true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)
	edit(t, store, parent, node)
	if err := act(d, node); err != nil {
		return attackOutcome{Error: err.Error(), Ran: runner.count(rewritten) + runner.count("notify")}
	}
	got := waitTerminal(t, store, parent.ID)
	return attackOutcome{Error: got.Error,
		Ran: runner.count(rewritten) + runner.count("notify") + runner.count("build") - 1}
}

// saveEdited reads id, applies edit, and saves it back, the whole-row write a writer makes.
func saveEdited(t *testing.T, store run.Store, id string, edit func(r *run.Run)) {
	t.Helper()
	r, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	edit(r)
	if err := store.Save(context.Background(), r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

// approveStep approves the waiting step node.
func approveStep(d *Dispatcher, node *run.Run) error {
	_, err := d.DecideStep(context.Background(), node.ID, StepDecision{Approve: true, By: bndDecider()})
	return err
}

// sweepSteps runs the approval timeout sweep once.
func sweepSteps(d *Dispatcher, _ *run.Run) error {
	d.sweepApprovalSteps()
	return nil
}

// TestADatabaseWriterIsRefusedByAKeyedDispatcher is unit 10's table for the dispatcher: each row
// is a move from the A1 to A22 list made by a principal who can write the runs table and not the
// binding key, performed through the store against a keyed dispatcher. Each is refused where it
// lands, with the reason named, and buys no execution: nothing the writer wrote runs, nothing runs
// twice, and no path a decision did not release is taken.
//
//nolint:funlen,maintidx // Test function.
func TestADatabaseWriterIsRefusedByAKeyedDispatcher(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the row.
		Name string
		// Move makes the writer's move and drives the install until it lands.
		Move func(t *testing.T) attackOutcome
		// WantError is what the refusal must say.
		WantError string
	}{{ // Test 0: The approved spec digest stamped on an apply that asked for approval.
		Name: "spec digest stamped",
		Move: func(t *testing.T) attackOutcome {
			return planFirstMove(t, false, func(r, _ *run.Run) func(*run.Run) {
				digest, err := outcome.SpecDigest(r)
				if err != nil {
					t.Fatalf("SpecDigest() error = %v", err)
				}
				return func(row *run.Run) { row.ApprovedSpecDigest = digest }
			})
		},
		WantError: "changed after it was submitted",
	}, { // Test 1: Another run's decision named on an apply that asked for approval.
		Name: "forged decision id",
		Move: func(t *testing.T) attackOutcome {
			return planFirstMove(t, false, func(_, decided *run.Run) func(*run.Run) {
				return func(row *run.Run) { row.DecisionID = decided.DecisionID }
			})
		},
		WantError: "is not the decision this row names",
	}, { // Test 2: An agent's approved apply turned into a person's.
		Name: "actor type flipped",
		Move: func(t *testing.T) attackOutcome {
			return planFirstMove(t, true, func(_, _ *run.Run) func(*run.Run) {
				return func(row *run.Run) { row.ActorType, row.Initiator = "session", nil }
			})
		},
		WantError: "changed after it was approved",
	}, { // Test 3: A finished approved run reset to pending.
		Name: "finished run reset",
		Move: func(t *testing.T) attackOutcome {
			store, _, runner, done := finishedApproved(t)
			resetToPending(t, store, done.ID)
			got := waitTerminal(t, store, done.ID)
			return attackOutcome{Error: got.Error, Ran: runner.count("deploy") - 1}
		},
		WantError: "already started once",
	}, { // Test 4: A finished approved run's row copied onto a pending run.
		Name: "finished row replayed onto another",
		Move: func(t *testing.T) attackOutcome {
			ctx := context.Background()
			store, _, runner, done := finishedApproved(t)
			copied := done.Clone()
			copied.ID, copied.Status, copied.ClaimedBy, copied.ClaimSecret = run.NewID(),
				run.StatusPendingApproval, "", ""
			copied.ClaimedAt, copied.StartedAt, copied.EndedAt, copied.ExitCode = nil, nil, nil, nil
			if err := store.Save(ctx, copied); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			// A whole-row save keeps the decision a row holds, so the writer names the finished
			// run's decision the way the decision columns are written.
			if ok, err := store.ClaimDecision(ctx, copied.ID, done.DecisionID, "approve"); err != nil || !ok {
				t.Fatalf("ClaimDecision() = %t, %v, want the claim", ok, err)
			}
			settle := run.DecisionSettle{Status: run.StatusPending}
			if ok, err := store.SettleDecision(ctx, copied.ID, done.DecisionID, settle); err != nil || !ok {
				t.Fatalf("SettleDecision() = %t, %v, want it settled", ok, err)
			}
			got := waitTerminal(t, store, copied.ID)
			return attackOutcome{Error: got.Error, Ran: runner.count("deploy") - 1}
		},
		WantError: "is not the decision this row names",
	}, { // Test 5: A held workflow turned into a plain run before its approval.
		Name: "kind flipped",
		Move: func(t *testing.T) attackOutcome {
			ctx := context.Background()
			store, runner := newBndRows(), &commandRecorder{}
			d := keyedAt(t, store, audit.NewMemStore(), runner)
			defer d.Close()
			p, err := d.SubmitPipeline(ctx, "release", "", linearSteps(), run.WithRequireApproval(true))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			store.rewrite(p.ID, func(r *run.Run) {
				r.Kind, r.Tool, r.Command, r.Steps = "", run.ToolBash, rewritten, nil
			})
			_, aerr := d.Approve(ctx, p.ID, bndDecider())
			if aerr == nil {
				got := waitTerminal(t, store.Store, p.ID)
				return attackOutcome{Error: got.Error, Ran: runner.count(rewritten)}
			}
			return attackOutcome{Error: aerr.Error(), Ran: runner.count(rewritten)}
		},
		WantError: "changed after it was submitted",
	}, { // Test 6: A parked workflow's graph rewritten before its step is approved.
		Name: "graph rewritten while parked",
		Move: func(t *testing.T) attackOutcome {
			return parkedMove(t, 0, func(t *testing.T, store run.Store, parent, _ *run.Run) {
				saveEdited(t, store, parent.ID, func(r *run.Run) {
					r.Steps[2].Command = rewritten
				})
			}, approveStep)
		},
		WantError: "changed after it was submitted",
	}, { // Test 7: A waiting approval step backdated past its timeout.
		Name: "timeout forged",
		Move: func(t *testing.T) attackOutcome {
			return parkedMove(t, 3600, func(t *testing.T, store run.Store, _, node *run.Run) {
				saveEdited(t, store, node.ID, func(r *run.Run) {
					r.CreatedAt = r.CreatedAt.Add(-2 * time.Hour)
				})
			}, sweepSteps)
		},
		WantError: "changed after it was built",
	}, { // Test 8: A deploy step written in before the approval step in front of it is decided.
		Name: "unasked step inserted",
		Move: func(t *testing.T) attackOutcome {
			var rows run.Store
			var stepID string
			got := parkedMove(t, 0, func(t *testing.T, store run.Store, parent, node *run.Run) {
				index := 2
				step := &run.Run{ID: run.NewID(), ParentID: &parent.ID, StepIndex: &index,
					StepName: "deploy", Tool: run.ToolBash, Command: rewritten,
					Status: run.StatusPending, CreatedAt: time.Now(), SpecSeal: node.SpecSeal}
				if err := store.Save(context.Background(), step); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
				rows, stepID = store, step.ID
			}, approveStep)
			step := waitTerminal(t, rows, stepID)
			return attackOutcome{Error: step.Error, Ran: got.Ran}
		},
		WantError: "it derives from is not on the chain",
	}, { // Test 9: A shard added to an approved split.
		Name: "shard added",
		Move: func(t *testing.T) attackOutcome {
			return approvedSplitMove(t, func(_, first *run.Run) *run.Run {
				added := first.Clone()
				index := 2
				added.ID, added.ShardIndex, added.Limit = run.NewID(), &index, rewritten
				added.Status, added.ClaimedBy, added.ClaimSecret = run.StatusPending, "", ""
				return added
			})
		},
		WantError: "changed after the split was built",
	}, { // Test 10: A bare child row naming an approved split as its parent.
		Name: "bare child of an approved parent",
		Move: func(t *testing.T) attackOutcome {
			return approvedSplitMove(t, func(split, first *run.Run) *run.Run {
				index := 2
				return &run.Run{ID: run.NewID(), ParentID: &split.ID, ShardIndex: &index,
					Playbook: "site.yml", Limit: rewritten, Status: run.StatusPending,
					Queue: first.Queue, CreatedAt: time.Now()}
			})
		},
		WantError: "carries no seal",
	}, { // Test 11: A finished step's published outputs rewritten before the step after it reads them.
		Name: "step outputs forged",
		Move: func(t *testing.T) attackOutcome {
			ctx := context.Background()
			store, runner := newPickedRows(), newPublishRunner(false)
			store.rewrite(isFinishedBuild, func(r *run.Run) {
				r.Outputs = map[string]any{"version": rewritten}
			})
			d := keyedAt(t, store, audit.NewMemStore(), runner)
			defer d.Close()
			parent, err := d.SubmitPipeline(ctx, "release", "inv", shapeSequence.steps())
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			got := waitTerminal(t, store.Store, parent.ID)
			_, ran := runner.input("deploy.yml")
			bought := 0
			if ran {
				bought = 1
			}
			return attackOutcome{Error: got.Error, Ran: bought}
		},
		WantError: "its status or its outputs changed after it finished",
	}, { // Test 12: Another run's inventory snapshot swapped onto a submitted run.
		Name: "snapshot swapped",
		Move: func(t *testing.T) attackOutcome {
			ctx := context.Background()
			store, chain, runner := newBndRows(), audit.NewMemStore(), &commandRecorder{}
			submitter := keyedAt(t, store, chain, runner, "decisions")
			r, err := submitter.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
				run.WithQueue("work"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			submitter.Close()
			store.rewrite(r.ID, func(row *run.Run) {
				row.InventorySnapshot = &run.InventorySnapshot{SealedSHA256: strings.Repeat("ab", 32),
					ContentSHA256: strings.Repeat("cd", 32)}
			})
			executor := keyedAt(t, store, chain, runner, "work")
			defer executor.Close()
			got := waitTerminal(t, store.Store, r.ID)
			return attackOutcome{Error: got.Error, Ran: runner.count("deploy")}
		},
		WantError: "changed after it was submitted",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := test.Move(t)
			if got.Ran != 0 {
				t.Errorf("the move bought %d executions, want 0", got.Ran)
			}
			if !strings.Contains(got.Error, "refused") || !strings.Contains(got.Error, test.WantError) {
				t.Errorf("the move ended with %q, want a refusal saying %q", got.Error, test.WantError)
			}
		})
	}
}
