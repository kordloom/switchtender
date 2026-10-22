package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// errBndUnreachable is what a failing chain read returns. It carries the word a database driver
// uses for a closed port, so a test can tell the store's own words never reach a run's error.
var errBndUnreachable = errors.New("dial tcp 10.0.0.9:5432: connect: connection refused")

// bndKey derives the binding key every keyed dispatcher in one test shares, the way every replica
// of an install derives the same key from the same producer identity.
func bndKey(t *testing.T) *binding.Key {
	t.Helper()
	key, err := binding.DeriveKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	return key
}

// bndDecider names who decides in these tests.
func bndDecider() outcome.Decider {
	return outcome.Decider{Name: "approver", Type: "session"}
}

// bndChain is an audit store as a principal who can write the audit table left it: a read by id
// returns the entry as the writer changed it, and reads can fail the way an unreachable database's
// do. Appends and every other read pass through.
type bndChain struct {
	audit.Store
	// mu guards edit and fails.
	mu sync.Mutex
	// edit, when set, returns what a read of e by id sees: e changed, or nil for an entry the
	// writer deleted.
	edit func(e *audit.Entry) *audit.Entry
	// fails is how many reads by id fail before reads succeed again. A negative count fails every
	// read.
	fails int
}

// set replaces what reads by id see.
func (c *bndChain) set(edit func(e *audit.Entry) *audit.Entry, fails int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edit, c.fails = edit, fails
}

// Entry reads an entry by id as the writer left it.
func (c *bndChain) Entry(ctx context.Context, id string) (*audit.Entry, error) {
	c.mu.Lock()
	edit, fail := c.edit, c.fails != 0
	if c.fails > 0 {
		c.fails--
	}
	c.mu.Unlock()
	if fail {
		return nil, errBndUnreachable
	}
	e, err := c.Store.Entry(ctx, id)
	if err != nil || edit == nil {
		return e, err
	}
	if e = edit(e); e == nil {
		return nil, fmt.Errorf("audit entry %s: %w", id, audit.ErrEntryNotFound)
	}
	return e, nil
}

// bndRows is a run store as a principal who can write the runs table left it: every read of a row
// returns the row as the writer changed it, which is what the executor and the walk are handed.
type bndRows struct {
	run.Store
	// mu guards edits.
	mu sync.Mutex
	// edits maps a run id to the change the writer made to its row.
	edits map[string]func(r *run.Run)
}

// newBndRows returns a memory store no writer has touched yet.
func newBndRows() *bndRows {
	return &bndRows{Store: run.NewMemStore(), edits: map[string]func(r *run.Run){}}
}

// rewrite records the writer's change to the row id. Each call adds to the changes already made.
func (s *bndRows) rewrite(id string, edit func(r *run.Run)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.edits[id]
	s.edits[id] = func(r *run.Run) {
		if prev != nil {
			prev(r)
		}
		edit(r)
	}
}

// edited applies the writer's change to a row read from the store, which hands out copies.
func (s *bndRows) edited(r *run.Run) *run.Run {
	if r == nil {
		return nil
	}
	s.mu.Lock()
	edit := s.edits[r.ID]
	s.mu.Unlock()
	if edit != nil {
		edit(r)
	}
	return r
}

// Get reads a run as the writer left it.
func (s *bndRows) Get(ctx context.Context, id string) (*run.Run, error) {
	r, err := s.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.edited(r), nil
}

// Claim leases a pending run and hands it over as the writer left it.
func (s *bndRows) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	r, err := s.Store.Claim(ctx, owner, queues)
	if err != nil {
		return nil, err
	}
	return s.edited(r), nil
}

// Steps reads a workflow's step records as the writer left them.
func (s *bndRows) Steps(ctx context.Context, parentID string) ([]*run.Run, error) {
	steps, err := s.Store.Steps(ctx, parentID)
	if err != nil {
		return nil, err
	}
	for _, r := range steps {
		s.edited(r)
	}
	return steps, nil
}

// bndHeld is an install with two held runs approved on a keyed dispatcher and not yet claimed:
// the run under test, in the queue a later executor serves, and another, in a queue nobody serves.
type bndHeld struct {
	// store is the runs table.
	store *bndRows
	// chain is the audit table.
	chain *bndChain
	// key is the install's binding key.
	key *binding.Key
	// runner records what executed.
	runner *commandRecorder
	// run is the approved run under test as the approval left it.
	run *run.Run
	// other is the second approved run as the approval left it.
	other *run.Run
}

// newBndHeld holds two runs, approves both on a keyed dispatcher that serves neither queue, and
// returns the install before anything claims them. Both are submitted through the dispatcher, so
// each waits for its decision under the seal its submission made, the only way a keyed dispatcher
// accepts a decision on it.
func newBndHeld(t *testing.T) *bndHeld {
	t.Helper()
	ctx := context.Background()
	h := &bndHeld{store: newBndRows(), chain: &bndChain{Store: audit.NewMemStore()}, key: bndKey(t),
		runner: &commandRecorder{}}
	d := New(h.store, okRunner(), nil, WithNoJanitor(), WithAudits(h.chain), WithBindingKey(h.key),
		WithQueues([]string{"decisions"}))
	defer d.Close()
	for _, spec := range []struct{ command, queue string }{{"deploy", "work"}, {"other", "parked"}} {
		held, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand(spec.command),
			run.WithQueue(spec.queue), run.WithActor("requester"),
			run.WithHeldByPolicy("change window"), run.WithRequireApproval(true))
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		if held.Status != run.StatusPendingApproval || held.SpecSeal == "" {
			t.Fatalf("submitted run is %s carrying seal %q, want it held and sealed", held.Status,
				held.SpecSeal)
		}
		released, err := d.Approve(ctx, held.ID, bndDecider())
		if err != nil {
			t.Fatalf("Approve() error = %v", err)
		}
		if released.SpecSeal == "" || released.DecisionID == "" {
			t.Fatalf("approved run carries seal %q and decision %q, want both", released.SpecSeal,
				released.DecisionID)
		}
		if h.run == nil {
			h.run = released
		} else {
			h.other = released
		}
	}
	return h
}

// execute starts a keyed executor serving the run under test's queue and returns the run's record
// once it ends.
func (h *bndHeld) execute(t *testing.T) *run.Run {
	t.Helper()
	d := New(h.store, h.runner, nil, WithNoJanitor(), WithAudits(h.chain), WithBindingKey(h.key),
		WithQueues([]string{"work"}), WithClaimInterval(5*time.Millisecond))
	defer d.Close()
	return waitTerminal(t, h.store, h.run.ID)
}

// bndResult is how a run or a workflow under test ended and what executed.
type bndResult struct {
	// Status is the final status.
	Status run.Status
	// Ran counts the commands that executed on the path under test.
	Ran int
	// Refused reports whether the error starts with the refusal's prefix.
	Refused bool
}

// TestExecutorRefusesARewrittenApprovedRow pins that a held run whose row, or whose DECISION entry,
// was rewritten after the approval is refused before it executes, whatever was rewritten. One keyed
// dispatcher approves, a writer who holds no key changes the database, and a second dispatcher with
// the same key claims the run. Rewriting the spec together with both stamps passes every row to
// row check, which is the case only the seal catches.
//
//nolint:funlen // Test function.
func TestExecutorRefusesARewrittenApprovedRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the rewrite.
		Name string
		// Rewrite is what the writer changes after the approval.
		Rewrite func(t *testing.T, h *bndHeld)
		// WantResult is how the run ends.
		WantResult bndResult
		// WantText, when set, is what the run's error must say.
		WantText string
	}{{ // Test 0: An untouched approved run executes.
		Name:       "untouched",
		WantResult: bndResult{Status: run.StatusSucceeded, Ran: 1},
	}, { // Test 1: The spec alone is rewritten.
		Name: "spec",
		Rewrite: func(_ *testing.T, h *bndHeld) {
			h.store.rewrite(h.run.ID, func(r *run.Run) { r.Command = "deploy --everywhere" })
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 2: The spec is rewritten and both stamps are recomputed to match it.
		Name: "spec and both stamps",
		Rewrite: func(t *testing.T, h *bndHeld) {
			tampered := h.run.Clone()
			tampered.Command = "deploy --everywhere"
			digest, err := outcome.SpecDigest(tampered)
			if err != nil {
				t.Fatalf("SpecDigest() error = %v", err)
			}
			bound, err := outcome.SpecBinding(tampered)
			if err != nil {
				t.Fatalf("SpecBinding() error = %v", err)
			}
			h.store.rewrite(h.run.ID, func(r *run.Run) {
				r.Command, r.ApprovedSpecDigest, r.ApprovedSpecBinding = tampered.Command, digest, bound
			})
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 3: The run's decision is swapped for another run's.
		Name: "decision swapped",
		Rewrite: func(_ *testing.T, h *bndHeld) {
			h.store.rewrite(h.run.ID, func(r *run.Run) { r.DecisionID = h.other.DecisionID })
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 4: Another run's seal is copied onto the row.
		Name: "seal copied",
		Rewrite: func(_ *testing.T, h *bndHeld) {
			h.store.rewrite(h.run.ID, func(r *run.Run) { r.SpecSeal = h.other.SpecSeal })
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 5: The rule the row records as having held it is rewritten.
		Name: "governance",
		Rewrite: func(_ *testing.T, h *bndHeld) {
			h.store.rewrite(h.run.ID, func(r *run.Run) { r.HeldByPolicy = "" })
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 6: The DECISION entry is deleted from the chain.
		Name: "entry deleted",
		Rewrite: func(_ *testing.T, h *bndHeld) {
			h.chain.set(func(e *audit.Entry) *audit.Entry {
				if e.ID == h.run.DecisionID {
					return nil
				}
				return e
			}, 0)
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 7: The DECISION entry is rewritten on the chain.
		Name: "entry rewritten",
		Rewrite: func(_ *testing.T, h *bndHeld) {
			h.chain.set(func(e *audit.Entry) *audit.Entry {
				if e.ID == h.run.DecisionID {
					e.ContentDigest = "sha256:" + strings.Repeat("0", 64)
				}
				return e
			}, 0)
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 8: The decision, both stamps, and the seal are all stripped, so the row names nothing
		// a decision or this process fixed, and it is refused as carrying no seal.
		Name: "decision, stamps, and seal all stripped",
		Rewrite: func(_ *testing.T, h *bndHeld) {
			h.store.rewrite(h.run.ID, func(r *run.Run) {
				r.DecisionID, r.ApprovedSpecDigest, r.ApprovedSpecBinding, r.SpecSeal = "", "", "", ""
			})
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
		WantText:   "it carries no seal",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			h := newBndHeld(t)
			if test.Rewrite != nil {
				test.Rewrite(t, h)
			}
			got := h.execute(t)
			ran := h.runner.count("deploy") + h.runner.count("deploy --everywhere")
			result := bndResult{Status: got.Status, Ran: ran,
				Refused: strings.HasPrefix(got.Error, "refused: ")}
			if diff := cmp.Diff(test.WantResult, result); diff != "" {
				t.Errorf("run mismatch (-want +got):\n%s\nerror: %s", diff, got.Error)
			}
			if !strings.Contains(got.Error, test.WantText) {
				t.Errorf("error = %q, want it to say %q", got.Error, test.WantText)
			}
			if h.runner.count("other") != 0 {
				t.Errorf("the run in a queue nobody serves executed %d times", h.runner.count("other"))
			}
		})
	}
}

// TestAChainReadErrorIsNotTampering pins that a chain the executor cannot read is reported as a
// check that could not be made, never as a refusal of a changed row. One failed read is retried
// and the run executes. Reads that keep failing fail the run as unverified, and the store's own
// error, which here says "connection refused", stays out of the run's error.
func TestAChainReadErrorIsNotTampering(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Fails is how many chain reads by id fail, negative for every one.
		Fails int
		// WantStatus is how the run ends.
		WantStatus run.Status
		// WantRan is how many times the run's command executed.
		WantRan int
	}{{ // Test 0: One failed read is retried and the run executes.
		Fails: 1, WantStatus: run.StatusSucceeded, WantRan: 1,
	}, { // Test 1: Reads that keep failing leave the run unverified.
		Fails: -1, WantStatus: run.StatusFailed, WantRan: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h := newBndHeld(t)
			h.chain.set(nil, test.Fails)
			got := h.execute(t)
			if got.Status != test.WantStatus || h.runner.count("deploy") != test.WantRan {
				t.Fatalf("run = %s (%s) and ran %d times, want %s and %d", got.Status, got.Error,
					h.runner.count("deploy"), test.WantStatus, test.WantRan)
			}
			if test.WantStatus == run.StatusSucceeded {
				return
			}
			if !strings.HasPrefix(got.Error, "could not verify: run "+h.run.ID+": ") {
				t.Errorf("error = %q, want it to say the run %s could not be verified", got.Error,
					h.run.ID)
			}
			for _, word := range []string{"refused", "changed after", "connection"} {
				if strings.Contains(got.Error, word) {
					t.Errorf("error = %q, want no %q in it", got.Error, word)
				}
			}
		})
	}
}

// bndGated is a keyed install whose held workflow was approved, ran to its approval step, and had
// the step decided, with the resume held back so a writer acts before the walk continues.
type bndGated struct {
	// store is the runs table.
	store *bndRows
	// chain is the audit table.
	chain *bndChain
	// runner records what executed.
	runner *commandRecorder
	// d is the dispatcher that decided the step and resumes the workflow.
	d *Dispatcher
	// parent is the workflow.
	parent *run.Run
	// node is the approval step as the decision left it.
	node *run.Run
	// release lets the held-back resume go.
	release func()
}

// newBndGated builds the install, approving the step when approve is set and denying it otherwise,
// and holds the resume back until resume is called.
func newBndGated(t *testing.T, approve bool) *bndGated {
	t.Helper()
	ctx := context.Background()
	key := bndKey(t)
	g := &bndGated{store: newBndRows(), chain: &bndChain{Store: audit.NewMemStore()},
		runner: &commandRecorder{}}
	first := New(g.store, g.runner, nil, WithNoJanitor(), WithAudits(g.chain), WithBindingKey(key))
	parent, err := first.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true),
		run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if _, err := first.Approve(ctx, parent.ID, bndDecider()); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	node := waitParked(t, g.store, parent.ID)
	first.Close()

	g.d = New(g.store, g.runner, nil, WithNoJanitor(), WithAudits(g.chain), WithBindingKey(key))
	t.Cleanup(g.d.Close)
	// A coordinator registered for the workflow keeps the decision from resuming it.
	g.release = g.d.enterCoordinator(parent.ID)
	decision := StepDecision{Approve: approve, By: bndDecider()}
	if _, err := g.d.DecideStep(ctx, node.ID, decision); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	if g.node, err = g.store.Get(ctx, node.ID); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if approve && (g.node.SpecSeal == "" || g.node.Status != run.StatusSucceeded) {
		t.Fatalf("approved step = %s with seal %q, want succeeded and sealed", g.node.Status,
			g.node.SpecSeal)
	}
	if !approve && g.node.Status != run.StatusRejected {
		t.Fatalf("denied step = %s, want rejected", g.node.Status)
	}
	if g.parent, err = g.store.Get(ctx, parent.ID); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	return g
}

// resume lets the walk continue and returns the workflow's record once it ends.
func (g *bndGated) resume(t *testing.T) *run.Run {
	t.Helper()
	g.release()
	g.d.sweepApprovalSteps()
	return waitTerminal(t, g.store, g.parent.ID)
}

// rewriteBuild makes the finished build step read as having published another artifact, and with
// stamp set recomputes the approval step's stamps to the state that now reduces to.
func (g *bndGated) rewriteBuild(t *testing.T, stamp bool) {
	t.Helper()
	ctx := context.Background()
	steps, err := g.store.Steps(ctx, g.parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	for _, s := range steps {
		if s.StepName == "build" {
			g.store.rewrite(s.ID, func(r *run.Run) {
				r.Outputs = map[string]any{"artifact": "somebody-elses-build"}
			})
		}
	}
	if !stamp {
		return
	}
	state, err := outcome.StepStateOf(ctx, g.store, g.parent, g.node)
	if err != nil {
		t.Fatalf("StepStateOf() error = %v", err)
	}
	digest, err := state.Digest()
	if err != nil {
		t.Fatalf("Digest() error = %v", err)
	}
	bound, err := state.Binding()
	if err != nil {
		t.Fatalf("Binding() error = %v", err)
	}
	if bound == g.node.ApprovedSpecBinding {
		t.Fatal("the rewritten build left the state binding unchanged")
	}
	g.store.rewrite(g.node.ID, func(r *run.Run) {
		r.ApprovedSpecDigest, r.ApprovedSpecBinding = digest, bound
	})
}

// requestID returns the id of the approval step's request entry.
func (g *bndGated) requestID(t *testing.T) string {
	t.Helper()
	chain, err := g.chain.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	want := outcome.StepDecisionPath(g.parent.ID, g.node.ID, outcome.StepRequested)
	for _, e := range chain {
		if e.Path == want {
			return e.ID
		}
	}
	t.Fatal("the approval step's request is not on the chain")
	return ""
}

// TestResumeRefusesARewrittenGate pins that an approved workflow step is held, on resume, to the
// seal minted when it was approved. A writer who rewrites what the step released, the step's
// stamps along with it, the step's seal, or the DECISION entry it names gets neither of the
// step's paths: the workflow fails with the reason. Rewriting an upstream output together with the
// step's stamp passes the row to row check, which is the case only the seal catches.
//
//nolint:funlen // Test function.
func TestResumeRefusesARewrittenGate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the rewrite.
		Name string
		// Rewrite is what the writer changes after the approval and before the resume.
		Rewrite func(t *testing.T, g *bndGated)
		// WantResult is how the workflow ends, counting runs of either path.
		WantResult bndResult
	}{{ // Test 0: An untouched workflow resumes and runs the approved path.
		Name:       "untouched",
		WantResult: bndResult{Status: run.StatusSucceeded, Ran: 1},
	}, { // Test 1: An upstream step's outputs are rewritten.
		Name:       "outputs",
		Rewrite:    func(t *testing.T, g *bndGated) { g.rewriteBuild(t, false) },
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 2: An upstream step's outputs and the step's stamps are rewritten together.
		Name:       "outputs and stamps",
		Rewrite:    func(t *testing.T, g *bndGated) { g.rewriteBuild(t, true) },
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 3: The step's seal is removed.
		Name: "seal removed",
		Rewrite: func(_ *testing.T, g *bndGated) {
			g.store.rewrite(g.node.ID, func(r *run.Run) { r.SpecSeal = "" })
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 4: The step names its own request entry as its decision.
		Name: "decision swapped",
		Rewrite: func(t *testing.T, g *bndGated) {
			id := g.requestID(t)
			g.store.rewrite(g.node.ID, func(r *run.Run) { r.DecisionID = id })
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 5: The step's DECISION entry is deleted from the chain.
		Name: "entry deleted",
		Rewrite: func(_ *testing.T, g *bndGated) {
			g.chain.set(func(e *audit.Entry) *audit.Entry {
				if e.ID == g.node.DecisionID {
					return nil
				}
				return e
			}, 0)
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 6: The step's DECISION entry is rewritten on the chain.
		Name: "entry rewritten",
		Rewrite: func(_ *testing.T, g *bndGated) {
			g.chain.set(func(e *audit.Entry) *audit.Entry {
				if e.ID == g.node.DecisionID {
					e.ContentDigest = "sha256:" + strings.Repeat("0", 64)
				}
				return e
			}, 0)
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 7: The approved workflow itself is rewritten while it waits at the step.
		Name: "workflow rewritten",
		Rewrite: func(_ *testing.T, g *bndGated) {
			g.store.rewrite(g.parent.ID, func(r *run.Run) {
				r.ExtraVars = map[string]any{"target": "everywhere"}
			})
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			g := newBndGated(t, true)
			if test.Rewrite != nil {
				test.Rewrite(t, g)
			}
			got := g.resume(t)
			result := bndResult{Status: got.Status,
				Ran:     g.runner.count("deploy") + g.runner.count("notify"),
				Refused: strings.HasPrefix(got.Error, "refused: ")}
			if diff := cmp.Diff(test.WantResult, result); diff != "" {
				t.Errorf("workflow mismatch (-want +got):\n%s\nerror: %s", diff, got.Error)
			}
			if g.runner.count("build") != 1 {
				t.Errorf("build ran %d times, want once: resuming must not repeat it",
					g.runner.count("build"))
			}
		})
	}
}

// TestParentMayStartRefusesARewrittenParent pins that the fence a split or workflow parent starts
// through holds the parent to its seal before claiming it, since such a parent never reaches the
// executor. An approved run stands in for the parent: the fence does not read the kind, and an
// approved split or workflow starts on the process that approves it, which leaves no moment for a
// writer to act between the two.
func TestParentMayStartRefusesARewrittenParent(t *testing.T) {
	t.Parallel()
	// started is what the fence answered and where it left the parent.
	type started struct {
		// Started is the fence's answer.
		Started bool
		// Status is the parent's stored status afterward.
		Status run.Status
		// Refused reports whether the parent's error starts with the refusal's prefix.
		Refused bool
	}
	tests := []struct {
		// Rewrite rewrites the parent's spec after its decision.
		Rewrite bool
		// WantResult is what the fence does.
		WantResult started
	}{{ // Test 0: An untouched approved parent is claimed and starts.
		Rewrite: false, WantResult: started{Started: true, Status: run.StatusRunning},
	}, { // Test 1: A parent rewritten after its decision fails and does not start.
		Rewrite: true, WantResult: started{Status: run.StatusFailed, Refused: true},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newBndHeld(t)
			if test.Rewrite {
				h.store.rewrite(h.run.ID, func(r *run.Run) { r.Command = "deploy --everywhere" })
			}
			d := New(h.store, h.runner, nil, WithNoJanitor(), WithAudits(h.chain),
				WithBindingKey(h.key), WithQueues([]string{"decisions"}))
			defer d.Close()
			parent, err := h.store.Get(ctx, h.run.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			ok := d.parentMayStart(parent, nil)
			stored, err := h.store.Get(ctx, h.run.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			got := started{Started: ok, Status: stored.Status,
				Refused: strings.HasPrefix(stored.Error, "refused: ")}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("fence mismatch (-want +got):\n%s\nerror: %s", diff, stored.Error)
			}
		})
	}
}

// TestResumeRefusesARewrittenWorkflow pins that a resumed workflow is held to its own seal before
// anything is restored from its step records. A denied step is never held to a gate seal, so
// without this check a workflow rewritten while it waited would run its deny path.
func TestResumeRefusesARewrittenWorkflow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Rewrite rewrites the workflow's spec while it waits at its step.
		Rewrite bool
		// WantResult is how the workflow ends, counting runs of either path.
		WantResult bndResult
	}{{ // Test 0: An untouched workflow runs its deny path on the recorded denial.
		Rewrite: false, WantResult: bndResult{Status: run.StatusSucceeded, Ran: 1},
	}, { // Test 1: A workflow rewritten while it waited runs neither path.
		Rewrite: true, WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			g := newBndGated(t, false)
			if test.Rewrite {
				g.store.rewrite(g.parent.ID, func(r *run.Run) {
					r.ExtraVars = map[string]any{"target": "everywhere"}
				})
			}
			got := g.resume(t)
			result := bndResult{Status: got.Status,
				Ran:     g.runner.count("deploy") + g.runner.count("notify"),
				Refused: strings.HasPrefix(got.Error, "refused: run "+g.parent.ID+": ")}
			if diff := cmp.Diff(test.WantResult, result); diff != "" {
				t.Errorf("workflow mismatch (-want +got):\n%s\nerror: %s", diff, got.Error)
			}
		})
	}
}
