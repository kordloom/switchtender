package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// claimForger is the approver a writer names in a claim it stores or rewrites.
const claimForger = "mallory"

// claimChain is an audit store whose DECISION appends can be stopped, so a decision a process
// claimed is left on its row unrecorded, as one is when its process dies after the claim. A stopped
// append fails the way a write that landed in part does, which the retries do not wait on.
type claimChain struct {
	audit.Store
	// down stops DECISION appends while it is set.
	down atomic.Bool
}

// Append appends e unless DECISION appends are stopped.
func (c *claimChain) Append(ctx context.Context, e *audit.Entry) error {
	if c.down.Load() && e.Method == audit.MethodDecision {
		return fmt.Errorf("audit table unreachable: %w", run.ErrPartlyDelivered)
	}
	return c.Store.Append(ctx, e)
}

// claimRows is a run store as a writer who can rewrite the decision claim stored on a row left it:
// every read of the row returns its claim as the writer changed it.
type claimRows struct {
	run.Store
	// mu guards edits.
	mu sync.Mutex
	// edits maps a run id to the writer's change to the claim stored on it.
	edits map[string]func(c *decisionClaim)
}

// rewrite records the writer's change to the claim stored on the row id.
func (s *claimRows) rewrite(id string, edit func(c *decisionClaim)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.edits[id] = edit
}

// edited applies the writer's change to the claim on a row read from the store.
func (s *claimRows) edited(r *run.Run) *run.Run {
	s.mu.Lock()
	edit := s.edits[r.ID]
	s.mu.Unlock()
	if edit == nil || r.DecisionClaim == "" {
		return r
	}
	var c decisionClaim
	if err := json.Unmarshal([]byte(r.DecisionClaim), &c); err != nil {
		return r
	}
	edit(&c)
	if raw, err := json.Marshal(&c); err == nil {
		r.DecisionClaim = string(raw)
	}
	return r
}

// Get reads a run with its claim as the writer left it.
func (s *claimRows) Get(ctx context.Context, id string) (*run.Run, error) {
	r, err := s.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.edited(r), nil
}

// NonTerminal lists the runs the janitor sweeps with their claims as the writer left them.
func (s *claimRows) NonTerminal(ctx context.Context) ([]*run.Run, error) {
	rows, err := s.Store.NonTerminal(ctx)
	for _, r := range rows {
		s.edited(r)
	}
	return rows, err
}

// claimInstall is one install whose decision claims a writer can store or rewrite.
type claimInstall struct {
	// store is the runs table.
	store *claimRows
	// chain is the audit table.
	chain *claimChain
	// d decides, and finishes claimed decisions when its sweep is called.
	d *Dispatcher
}

// newClaimInstall starts a dispatcher with no janitor serving the default queue, with a binding key
// when keyed is set.
func newClaimInstall(t *testing.T, keyed bool) *claimInstall {
	t.Helper()
	in := &claimInstall{
		store: &claimRows{Store: run.NewMemStore(), edits: map[string]func(c *decisionClaim){}},
		chain: &claimChain{Store: audit.NewMemStore()},
	}
	opts := []Option{WithNoJanitor(), WithAudits(in.chain), WithClaimInterval(5 * time.Millisecond)}
	if keyed {
		opts = append(opts, WithBindingKey(bndKey(t)))
	}
	in.d = New(in.store, okRunner(), nil, opts...)
	t.Cleanup(in.d.Close)
	return in
}

// held submits a run held for approval to a queue nobody serves.
func (in *claimInstall) held(t *testing.T) *run.Run {
	t.Helper()
	r, err := in.d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash),
		run.WithCommand("deploy"), run.WithQueue("parked"), run.WithActor("requester"),
		run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	return r
}

// waiting submits a workflow and returns it and its approval step once it waits there. A
// coordinator registered for the workflow keeps a finished decision from resuming it.
func (in *claimInstall) waiting(t *testing.T) (*run.Run, *run.Run) {
	t.Helper()
	parent, err := in.d.SubmitPipeline(context.Background(), "release", "",
		gatedWorkflow(0, false), run.WithActor("requester"))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, in.store.Store, parent.ID)
	t.Cleanup(in.d.enterCoordinator(parent.ID))
	return parent, node
}

// stuck makes a decision through decide with DECISION appends stopped, so the process's claim is
// left on the row id unrecorded.
func (in *claimInstall) stuck(t *testing.T, id string, decide func() error) {
	t.Helper()
	in.chain.down.Store(true)
	err := decide()
	in.chain.down.Store(false)
	if err != nil && !errors.Is(err, errDecisionUnrecorded) {
		t.Fatalf("decision error = %v, want it left unrecorded", err)
	}
	if r, gerr := in.store.Store.Get(context.Background(), id); gerr != nil || !r.InFlight() {
		t.Fatalf("run %s is not left claimed: %v", id, gerr)
	}
}

// forge stores c on the row id as a writer would, straight into the runs table.
func (in *claimInstall) forge(t *testing.T, id string, c *decisionClaim) {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	ok, err := in.store.ClaimDecision(context.Background(), id, c.ID, string(raw))
	if err != nil || !ok {
		t.Fatalf("ClaimDecision() = (%v, %v), want (true, nil)", ok, err)
	}
}

// forgedClaim is a claim a writer builds for the row id: verdict, settling the row to status, a
// step's decision when step is set, recorded by an entry at path naming claimForger.
func forgedClaim(id, verdict string, status run.Status, step bool, path string) *decisionClaim {
	entryID := audit.NewID()
	return &decisionClaim{ID: entryID, Verdict: verdict, Step: step, Status: status,
		Entry: &claimEntry{ID: entryID, At: time.Now(), Actor: claimForger, ActorType: "session",
			Method: audit.MethodDecision, Path: path, ContentDigest: "sha256s:" + id}}
}

// runPath returns the chain path of a decision on a run.
func runPath(id, verdict string) string {
	return "/runs/" + id + "/decision/" + verdict
}

// claimTarget is the row a claim under test is stored on, and the workflow when it is a step.
type claimTarget struct {
	// parent is the workflow of an approval step, nil for a run.
	parent *run.Run
	// row is the run or approval step.
	row *run.Run
}

// claimResult is what finishing the claims stored on an install did to the target.
type claimResult struct {
	// Recorded counts DECISION entries recording a verdict on the target.
	Recorded int
	// Forged reports whether any chain entry names the writer's approver.
	Forged bool
	// InFlight reports whether the target is still claimed and unsettled.
	InFlight bool
	// Status is the target's status.
	Status run.Status
}

// TestAForgedClaimIsNeverRecorded pins that a decision claim reaches the chain only when the
// process that claimed the run made it. Each row leaves a claim on a row, made by the process and
// left unrecorded as if the process died, or stored or rewritten by a writer who holds no key, and
// then the janitor's sweep finishes what it finds. On a keyed dispatcher a claim with no claim
// seal, one copied from another run, one whose approver or verdict was rewritten, and a forged
// rejection or timeout are refused: nothing appended and the row left claimed. The process's own
// claims of every kind still finish, and an unkeyed dispatcher finishes a writer's claim as it
// always has.
//
//nolint:funlen // Test function.
func TestAForgedClaimIsNeverRecorded(t *testing.T) {
	t.Parallel()
	approver := outcome.Decider{Name: "approver", Type: "session"}
	refused := claimResult{InFlight: true, Status: run.StatusPendingApproval}
	approveStuck := func(t *testing.T, in *claimInstall) claimTarget {
		t.Helper()
		r := in.held(t)
		in.stuck(t, r.ID, func() error {
			_, err := in.d.Approve(context.Background(), r.ID, approver)
			return err
		})
		return claimTarget{row: r}
	}
	rejectStuck := func(t *testing.T, in *claimInstall) claimTarget {
		t.Helper()
		r := in.held(t)
		in.stuck(t, r.ID, func() error {
			_, err := in.d.Reject(context.Background(), r.ID, "", approver)
			return err
		})
		return claimTarget{row: r}
	}
	tests := []struct {
		// Keyed starts the dispatcher with a binding key.
		Keyed bool
		// Setup leaves a claim on a row and returns the row.
		Setup func(t *testing.T, in *claimInstall) claimTarget
		// WantResult is what the sweep did to the row.
		WantResult claimResult
	}{{ // Test 0: The process's approval finishes.
		Keyed: true, Setup: approveStuck,
		WantResult: claimResult{Recorded: 1, Status: run.StatusPending},
	}, { // Test 1: The process's rejection finishes.
		Keyed: true, Setup: rejectStuck,
		WantResult: claimResult{Recorded: 1, Status: run.StatusRejected},
	}, { // Test 2: The process's approval of a step finishes.
		Keyed: true,
		Setup: func(t *testing.T, in *claimInstall) claimTarget {
			t.Helper()
			parent, node := in.waiting(t)
			in.stuck(t, node.ID, func() error {
				_, err := in.d.DecideStep(context.Background(), node.ID,
					StepDecision{Approve: true, By: approver})
				return err
			})
			return claimTarget{parent: parent, row: node}
		},
		WantResult: claimResult{Recorded: 1, Status: run.StatusSucceeded},
	}, { // Test 3: The process's timeout of a step finishes.
		Keyed: true,
		Setup: func(t *testing.T, in *claimInstall) claimTarget {
			t.Helper()
			parent, node := in.waiting(t)
			in.stuck(t, node.ID, func() error {
				in.d.timeOutStep(context.Background(), node)
				return nil
			})
			return claimTarget{parent: parent, row: node}
		},
		WantResult: claimResult{Recorded: 1, Status: run.StatusFailed},
	}, { // Test 4: A writer's approval with no claim seal is refused.
		Keyed: true,
		Setup: func(t *testing.T, in *claimInstall) claimTarget {
			t.Helper()
			r := in.held(t)
			in.forge(t, r.ID, forgedClaim(r.ID, outcome.StepApproved, run.StatusPending, false,
				runPath(r.ID, outcome.StepApproved)))
			return claimTarget{row: r}
		},
		WantResult: refused,
	}, { // Test 5: Another run's sealed claim copied onto this run is refused.
		Keyed: true,
		Setup: func(t *testing.T, in *claimInstall) claimTarget {
			t.Helper()
			other := approveStuck(t, in).row
			stored, err := in.store.Store.Get(context.Background(), other.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			r := in.held(t)
			ok, err := in.store.ClaimDecision(context.Background(), r.ID, stored.DecisionID,
				stored.DecisionClaim)
			if err != nil || !ok {
				t.Fatalf("ClaimDecision() = (%v, %v), want (true, nil)", ok, err)
			}
			return claimTarget{row: r}
		},
		WantResult: refused,
	}, { // Test 6: The process's approval with its approver rewritten is refused.
		Keyed: true,
		Setup: func(t *testing.T, in *claimInstall) claimTarget {
			t.Helper()
			target := approveStuck(t, in)
			in.store.rewrite(target.row.ID, func(c *decisionClaim) { c.Entry.Actor = claimForger })
			return target
		},
		WantResult: refused,
	}, { // Test 7: The process's rejection rewritten into an approval is refused.
		Keyed: true,
		Setup: func(t *testing.T, in *claimInstall) claimTarget {
			t.Helper()
			target := rejectStuck(t, in)
			id := target.row.ID
			in.store.rewrite(id, func(c *decisionClaim) {
				c.Verdict, c.Status, c.Error = outcome.StepApproved, run.StatusPending, ""
				c.Finalize, c.Entry.Path = false, runPath(id, outcome.StepApproved)
			})
			return target
		},
		WantResult: refused,
	}, { // Test 8: A writer's rejection is refused.
		Keyed: true,
		Setup: func(t *testing.T, in *claimInstall) claimTarget {
			t.Helper()
			r := in.held(t)
			c := forgedClaim(r.ID, outcome.StepRejected, run.StatusRejected, false,
				runPath(r.ID, outcome.StepRejected))
			c.Error, c.Finalize = "rejected by an approver", true
			in.forge(t, r.ID, c)
			return claimTarget{row: r}
		},
		WantResult: refused,
	}, { // Test 9: A writer's timeout of a step is refused.
		Keyed: true,
		Setup: func(t *testing.T, in *claimInstall) claimTarget {
			t.Helper()
			parent, node := in.waiting(t)
			c := forgedClaim(node.ID, outcome.StepTimedOut, run.StatusFailed, true,
				outcome.StepDecisionPath(parent.ID, node.ID, outcome.StepTimedOut))
			c.Error = "timed out"
			in.forge(t, node.ID, c)
			return claimTarget{parent: parent, row: node}
		},
		WantResult: refused,
	}, { // Test 10: With no key, a writer's approval is recorded as before.
		Setup: func(t *testing.T, in *claimInstall) claimTarget {
			t.Helper()
			r := in.held(t)
			in.forge(t, r.ID, forgedClaim(r.ID, outcome.StepApproved, run.StatusPending, false,
				runPath(r.ID, outcome.StepApproved)))
			return claimTarget{row: r}
		},
		WantResult: claimResult{Recorded: 1, Forged: true, Status: run.StatusPending},
	}, { // Test 11: With no key, the process's approval finishes as before.
		Setup:      approveStuck,
		WantResult: claimResult{Recorded: 1, Status: run.StatusPending},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			in := newClaimInstall(t, test.Keyed)
			target := test.Setup(t, in)
			in.d.sweepApprovalSteps()
			paths := map[string]bool{}
			for _, verdict := range []string{outcome.StepApproved, outcome.StepRejected,
				outcome.StepTimedOut} {
				if target.parent == nil {
					paths[runPath(target.row.ID, verdict)] = true
					continue
				}
				paths[outcome.StepDecisionPath(target.parent.ID, target.row.ID, verdict)] = true
			}
			chain, err := in.chain.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			stored, err := in.store.Store.Get(ctx, target.row.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			got := claimResult{InFlight: stored.InFlight(), Status: stored.Status}
			for _, e := range chain {
				if e.Method == audit.MethodDecision && paths[e.Path] {
					got.Recorded++
				}
				got.Forged = got.Forged || e.Actor == claimForger
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("finish mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
