package dispatch

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// rewritten is the value every writer's change in these tests puts into a row, so a test can say
// that nothing a writer wrote ever executed.
const rewritten = "everywhere"

// rowEdit is one change a writer makes to every row match picks.
type rowEdit struct {
	// match picks the rows the change applies to.
	match func(r *run.Run) bool
	// edit changes a picked row.
	edit func(r *run.Run)
}

// pickedRows is a run store as a writer who picks rows by what they are left it: every read of a
// picked row, including the claim that hands it to an executor, returns it changed. The writer
// picks before the rows exist, so a shard or a step built later is changed too.
type pickedRows struct {
	run.Store
	// mu guards edits.
	mu sync.Mutex
	// edits are the writer's changes in the order they were made.
	edits []rowEdit
}

// newPickedRows returns a memory store no writer has touched yet.
func newPickedRows() *pickedRows {
	return &pickedRows{Store: run.NewMemStore()}
}

// rewrite records the writer's change to every row match picks.
func (s *pickedRows) rewrite(match func(r *run.Run) bool, edit func(r *run.Run)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.edits = append(s.edits, rowEdit{match: match, edit: edit})
}

// edited applies the writer's changes to a row read from the store, which hands out copies.
func (s *pickedRows) edited(r *run.Run) *run.Run {
	if r == nil {
		return nil
	}
	s.mu.Lock()
	edits := append([]rowEdit(nil), s.edits...)
	s.mu.Unlock()
	for _, e := range edits {
		if e.match(r) {
			e.edit(r)
		}
	}
	return r
}

// Get reads a run as the writer left it.
func (s *pickedRows) Get(ctx context.Context, id string) (*run.Run, error) {
	r, err := s.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.edited(r), nil
}

// Claim leases a pending run and hands it over as the writer left it.
func (s *pickedRows) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	r, err := s.Store.Claim(ctx, owner, queues)
	if err != nil {
		return nil, err
	}
	return s.edited(r), nil
}

// Steps reads a workflow's step records as the writer left them.
func (s *pickedRows) Steps(ctx context.Context, parentID string) ([]*run.Run, error) {
	steps, err := s.Store.Steps(ctx, parentID)
	for _, r := range steps {
		s.edited(r)
	}
	return steps, err
}

// Shards reads a split's shards as the writer left them.
func (s *pickedRows) Shards(ctx context.Context, parentID string) ([]*run.Run, error) {
	shards, err := s.Store.Shards(ctx, parentID)
	for _, r := range shards {
		s.edited(r)
	}
	return shards, err
}

// childRunner is a runner and host lister that succeeds and records what each execution was handed,
// by command, or by host limit for a run with no command.
type childRunner struct {
	// mu guards seen.
	mu sync.Mutex
	// seen counts executions per command or limit.
	seen map[string]int
}

// Run records what it was handed and succeeds.
func (c *childRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result,
	error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]int{}
	}
	key := spec.Command
	if key == "" {
		key = "limit:" + spec.Limit
	}
	c.seen[key]++
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts lists the two hosts every split in these tests fans out across.
func (c *childRunner) Hosts(context.Context, string, string) ([]string, error) {
	return []string{"web1", "web2"}, nil
}

// count returns how many executions were handed key.
func (c *childRunner) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[key]
}

// childInstall is a keyed install whose rows and chain a writer can change while it works.
type childInstall struct {
	// store is the runs table.
	store *pickedRows
	// chain is the audit table.
	chain *bndChain
	// runner records what executed.
	runner *childRunner
	// d is the dispatcher, which both decides and executes.
	d *Dispatcher
}

// newChildInstall starts a keyed dispatcher that serves the default queue.
func newChildInstall(t *testing.T) *childInstall {
	t.Helper()
	ci := &childInstall{store: newPickedRows(), chain: &bndChain{Store: audit.NewMemStore()},
		runner: &childRunner{}}
	ci.d = New(ci.store, ci.runner, nil, WithNoJanitor(), WithAudits(ci.chain),
		WithBindingKey(bndKey(t)), WithClaimInterval(5*time.Millisecond))
	t.Cleanup(ci.d.Close)
	return ci
}

// childShape submits a parent of one shape through ci, drives it to its end, and returns the parent
// and its child under test as the store holds them.
type childShape func(t *testing.T, ci *childInstall) (parent, child *run.Run)

// shapeSplit is a split held for approval and then approved, with its first shard under test.
func shapeSplit(t *testing.T, ci *childInstall) (*run.Run, *run.Run) {
	t.Helper()
	ctx := context.Background()
	held, err := ci.d.SubmitSplit(ctx, "site.yml", "inv", 2, run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if _, err := ci.d.Approve(ctx, held.ID, bndDecider()); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	parent := waitTerminal(t, ci.store.Store, held.ID)
	shards, err := ci.store.Store.Shards(ctx, held.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	for _, s := range shards {
		if s.ShardIndex != nil && *s.ShardIndex == 0 {
			return parent, s
		}
	}
	t.Fatalf("split %s stored no first shard", held.ID)
	return nil, nil
}

// shapeSplitRefused returns the shape of a split held for approval whose approval is refused
// because a writer changed the held row, with its first shard. The refusal names the held row and
// says why, and nothing about a decision is recorded: no decision on the row and no DECISION entry
// for it.
func shapeSplitRefused(why string) childShape {
	return func(t *testing.T, ci *childInstall) (*run.Run, *run.Run) {
		t.Helper()
		return splitRefused(t, ci, why)
	}
}

// splitRefused submits the split shapeSplitRefused describes, requires its approval to be refused
// for why, and returns the split and its first shard as the store holds them.
func splitRefused(t *testing.T, ci *childInstall, why string) (*run.Run, *run.Run) {
	t.Helper()
	ctx := context.Background()
	held, err := ci.d.SubmitSplit(ctx, "site.yml", "inv", 2, run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	want := "refused: run " + held.ID + ": " + why
	if _, err := ci.d.Approve(ctx, held.ID, bndDecider()); err == nil || err.Error() != want {
		t.Fatalf("Approve() error = %v, want %q", err, want)
	}
	parent, err := ci.store.Store.Get(ctx, held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if parent.DecisionID != "" {
		t.Errorf("refused split stores decision %q, want none", parent.DecisionID)
	}
	chain, err := ci.chain.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		if strings.HasPrefix(e.Path, "/runs/"+held.ID+"/decision/") {
			t.Errorf("refused split has chain entry %s %s", e.Method, e.Path)
		}
	}
	shards, err := ci.store.Store.Shards(ctx, held.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	for _, s := range shards {
		if isShard(s) {
			return parent, s
		}
	}
	t.Fatalf("split %s stored no first shard", held.ID)
	return nil, nil
}

// linearSteps is a one-step sequence, deploy.
func linearSteps() []run.PipelineStep {
	return []run.PipelineStep{{Name: "deploy", Tool: run.ToolBash, Command: "deploy"}}
}

// shapeLinear is a sequence nobody was asked to approve, with its step under test.
func shapeLinear(t *testing.T, ci *childInstall) (*run.Run, *run.Run) {
	t.Helper()
	parent, err := ci.d.SubmitPipeline(context.Background(), "release", "", linearSteps())
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	return ci.finished(t, parent.ID)
}

// shapeApprovedLinear is a sequence held for approval and then approved, so its step derives from
// the workflow's decision and is released on the chain.
func shapeApprovedLinear(t *testing.T, ci *childInstall) (*run.Run, *run.Run) {
	t.Helper()
	ctx := context.Background()
	held, err := ci.d.SubmitPipeline(ctx, "release", "", linearSteps(),
		run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if _, err := ci.d.Approve(ctx, held.ID, bndDecider()); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	return ci.finished(t, held.ID)
}

// shapeGated is build, an approval step, and deploy, in a workflow nobody was asked to approve as a
// whole. The step is approved, so deploy derives from the step's decision and is released on the
// chain.
func shapeGated(t *testing.T, ci *childInstall) (*run.Run, *run.Run) {
	t.Helper()
	ctx := context.Background()
	parent, err := ci.d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, false))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, ci.store.Store, parent.ID)
	decision := StepDecision{Approve: true, By: bndDecider()}
	if _, err := ci.d.DecideStep(ctx, node.ID, decision); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	return ci.finished(t, parent.ID)
}

// finished waits for the workflow id to end and returns it with its deploy step, nil when no deploy
// step was stored.
func (ci *childInstall) finished(t *testing.T, id string) (*run.Run, *run.Run) {
	t.Helper()
	parent := waitTerminal(t, ci.store.Store, id)
	steps, err := ci.store.Store.Steps(context.Background(), id)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	for _, s := range steps {
		if s.StepName == "deploy" {
			return parent, s
		}
	}
	return parent, nil
}

// childResult is how a parent and its child under test ended.
type childResult struct {
	// Parent is the parent's final status.
	Parent run.Status
	// Child is the child's final status, empty when no child was stored.
	Child run.Status
	// Refused reports whether the child's error starts with the refusal's prefix.
	Refused bool
}

// isShard picks the first shard of a split.
func isShard(r *run.Run) bool { return r.ShardIndex != nil && *r.ShardIndex == 0 }

// isDeploy picks the deploy step of a workflow.
func isDeploy(r *run.Run) bool { return r.StepIndex != nil && r.StepName == "deploy" }

// isTop picks a top-level run.
func isTop(r *run.Run) bool { return r.ParentID == nil }

// isRelease picks a RELEASE entry.
func isRelease(e *audit.Entry) bool { return e.Method == audit.MethodRelease }

// TestExecutorRefusesARewrittenChild pins that a shard or a workflow step whose row, whose parent,
// or whose RELEASE entry a writer changed after it was built is refused before it executes. A held
// split a writer changed is refused at its approval, and leaves nothing to execute. So is a held
// split a writer changed and stripped of its seal, as carrying no seal, since a keyed dispatcher
// checks every held row: clearing the seal does not reopen the approval. The untouched rows pin
// that every shape still executes on a keyed install, a never-held workflow's approved step among
// them.
//
//nolint:funlen // Test function.
func TestExecutorRefusesARewrittenChild(t *testing.T) {
	t.Parallel()
	succeeded := childResult{Parent: run.StatusSucceeded, Child: run.StatusSucceeded}
	refused := childResult{Parent: run.StatusFailed, Child: run.StatusFailed, Refused: true}
	held := childResult{Parent: run.StatusPendingApproval, Child: run.StatusPendingApproval}
	tests := []struct {
		// Name labels the row.
		Name string
		// Shape is the parent submitted and driven to its end.
		Shape childShape
		// Rewrite is what the writer changes, before the parent is submitted.
		Rewrite func(ci *childInstall)
		// WantResult is how the parent and its child under test end.
		WantResult childResult
	}{{ // Test 0: An untouched shard executes.
		Name: "shard untouched", Shape: shapeSplit, WantResult: succeeded,
	}, { // Test 1: A shard's host limit is rewritten.
		Name: "shard rewritten", Shape: shapeSplit,
		Rewrite: func(ci *childInstall) {
			ci.store.rewrite(isShard, func(r *run.Run) { r.Limit = rewritten })
		},
		WantResult: refused,
	}, { // Test 2: A held split is rewritten, and its approval is refused.
		Name:  "split rewritten while held",
		Shape: shapeSplitRefused("its spec or its approval fields changed after it was submitted"),
		Rewrite: func(ci *childInstall) {
			ci.store.rewrite(isTop, func(r *run.Run) { r.ExtraVars = map[string]any{"to": rewritten} })
		},
		WantResult: held,
	}, { // Test 3: A held split rewritten and stripped of its seal is refused at its approval.
		Name: "split rewritten and unsealed while held",
		Shape: shapeSplitRefused("it carries no seal, so this process did not build or release " +
			"it"),
		Rewrite: func(ci *childInstall) {
			ci.store.rewrite(isTop, func(r *run.Run) {
				r.ExtraVars = map[string]any{"to": rewritten}
				// The seal is stripped only until an approval stamps its own.
				if r.DecisionID == "" {
					r.SpecSeal = ""
				}
			})
		},
		WantResult: held,
	}, { // Test 4: An untouched step of a sequence nobody approved executes.
		Name: "linear untouched", Shape: shapeLinear, WantResult: succeeded,
	}, { // Test 5: A step of a sequence nobody approved is rewritten.
		Name: "linear step rewritten", Shape: shapeLinear,
		Rewrite: func(ci *childInstall) {
			ci.store.rewrite(isDeploy, func(r *run.Run) { r.Command = rewritten })
		},
		WantResult: refused,
	}, { // Test 6: A sequence nobody approved is rewritten while it runs.
		Name: "linear workflow rewritten", Shape: shapeLinear,
		Rewrite: func(ci *childInstall) {
			ci.store.rewrite(isTop, func(r *run.Run) { r.ExtraVars = map[string]any{"to": rewritten} })
		},
		WantResult: refused,
	}, { // Test 7: An untouched step of an approved sequence executes.
		Name: "approved untouched", Shape: shapeApprovedLinear, WantResult: succeeded,
	}, { // Test 8: A step of an approved sequence is rewritten.
		Name: "approved step rewritten", Shape: shapeApprovedLinear,
		Rewrite: func(ci *childInstall) {
			ci.store.rewrite(isDeploy, func(r *run.Run) { r.Command = rewritten })
		},
		WantResult: refused,
	}, { // Test 9: The RELEASE entry of a step of an approved sequence is deleted.
		Name: "approved release deleted", Shape: shapeApprovedLinear,
		Rewrite: func(ci *childInstall) {
			ci.chain.set(func(e *audit.Entry) *audit.Entry {
				if isRelease(e) {
					return nil
				}
				return e
			}, 0)
		},
		WantResult: refused,
	}, { // Test 10: The RELEASE entry of a step of an approved sequence is rewritten.
		Name: "approved release rewritten", Shape: shapeApprovedLinear,
		Rewrite: func(ci *childInstall) {
			ci.chain.set(func(e *audit.Entry) *audit.Entry {
				if isRelease(e) {
					e.ContentDigest = "sha256:" + strings.Repeat("0", 64)
				}
				return e
			}, 0)
		},
		WantResult: refused,
	}, { // Test 11: An untouched step behind an approved step of a never-held workflow executes.
		Name: "gated untouched", Shape: shapeGated, WantResult: succeeded,
	}, { // Test 12: A step behind an approved step is rewritten.
		Name: "gated step rewritten", Shape: shapeGated,
		Rewrite: func(ci *childInstall) {
			ci.store.rewrite(isDeploy, func(r *run.Run) { r.Command = rewritten })
		},
		WantResult: refused,
	}, { // Test 13: The RELEASE entry of a step behind an approved step is deleted.
		Name: "gated release deleted", Shape: shapeGated,
		Rewrite: func(ci *childInstall) {
			ci.chain.set(func(e *audit.Entry) *audit.Entry {
				if isRelease(e) {
					return nil
				}
				return e
			}, 0)
		},
		WantResult: refused,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ci := newChildInstall(t)
			if test.Rewrite != nil {
				test.Rewrite(ci)
			}
			parent, child := test.Shape(t, ci)
			got := childResult{Parent: parent.Status}
			if child != nil {
				got.Child = child.Status
				got.Refused = strings.HasPrefix(child.Error, "refused: ")
			}
			errText := ""
			if child != nil {
				errText = child.Error
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("result mismatch (-want +got):\n%s\nparent error: %s\nchild error: %s", diff,
					parent.Error, errText)
			}
			if n := ci.runner.count(rewritten) + ci.runner.count("limit:"+rewritten); n != 0 {
				t.Errorf("what the writer wrote executed %d times", n)
			}
		})
	}
}

// TestExecutorRefusesARewrittenSubmittedRow pins that a run nobody was asked to approve is held to
// the seal its submission made, so a writer who changes its spec, or a field the approval gate
// reads, before it is claimed is refused. One keyed dispatcher submits into a queue it does not
// serve, the writer changes the row, and a second dispatcher with the same key claims it.
//
//nolint:funlen // Test function.
func TestExecutorRefusesARewrittenSubmittedRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the rewrite.
		Name string
		// Rewrite is what the writer changes; other is a second submitted run, decided is an approved
		// run, both in a queue nobody serves.
		Rewrite func(t *testing.T, r, other, decided *run.Run) func(*run.Run)
		// WantResult is how the run ends.
		WantResult bndResult
	}{{ // Test 0: An untouched run executes.
		Name: "untouched", WantResult: bndResult{Status: run.StatusSucceeded, Ran: 1},
	}, { // Test 1: The spec is rewritten.
		Name: "spec",
		Rewrite: func(*testing.T, *run.Run, *run.Run, *run.Run) func(*run.Run) {
			return func(r *run.Run) { r.Command = rewritten }
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 2: The spec is rewritten with both approval stamps recomputed to match it.
		Name: "spec and stamps",
		Rewrite: func(t *testing.T, r, _, _ *run.Run) func(*run.Run) {
			tampered := r.Clone()
			tampered.Command = rewritten
			digest, err := outcome.SpecDigest(tampered)
			if err != nil {
				t.Fatalf("SpecDigest() error = %v", err)
			}
			bound, err := outcome.SpecBinding(tampered)
			if err != nil {
				t.Fatalf("SpecBinding() error = %v", err)
			}
			return func(r *run.Run) {
				r.Command, r.ApprovedSpecDigest, r.ApprovedSpecBinding = rewritten, digest, bound
			}
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 3: The run is made to read as one whose submission asked for approval.
		Name: "approval requested",
		Rewrite: func(*testing.T, *run.Run, *run.Run, *run.Run) func(*run.Run) {
			return func(r *run.Run) { r.ApprovalRequested = true }
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 4: The run is made to name a rule that held it.
		Name: "held by policy",
		Rewrite: func(*testing.T, *run.Run, *run.Run, *run.Run) func(*run.Run) {
			return func(r *run.Run) { r.HeldByPolicy = "change window" }
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 5: The separation-of-duties flag is flipped.
		Name: "distinct approver",
		Rewrite: func(*testing.T, *run.Run, *run.Run, *run.Run) func(*run.Run) {
			return func(r *run.Run) { r.RequireDistinctApprover = true }
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 6: The rule set recorded as in force at submit is rewritten.
		Name: "policy set",
		Rewrite: func(*testing.T, *run.Run, *run.Run, *run.Run) func(*run.Run) {
			return func(r *run.Run) { r.PolicySet = &run.PolicySet{Digest: "sha256:none", Count: 0} }
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 7: The run is made to name another run's approval.
		Name: "decision forged",
		Rewrite: func(_ *testing.T, _, _, decided *run.Run) func(*run.Run) {
			return func(r *run.Run) { r.DecisionID = decided.DecisionID }
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}, { // Test 8: Another submitted run's seal is copied onto the row.
		Name: "seal copied",
		Rewrite: func(_ *testing.T, _, other, _ *run.Run) func(*run.Run) {
			return func(r *run.Run) { r.SpecSeal = other.SpecSeal }
		},
		WantResult: bndResult{Status: run.StatusFailed, Refused: true},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			key := bndKey(t)
			store, chain := newPickedRows(), &bndChain{Store: audit.NewMemStore()}
			submitter := New(store, okRunner(), nil, WithNoJanitor(), WithAudits(chain),
				WithBindingKey(key), WithQueues([]string{"decisions"}))
			submit := func(command string, opts ...run.SubmitOption) *run.Run {
				r, err := submitter.Submit(ctx, "", "", append([]run.SubmitOption{
					run.WithTool(run.ToolBash), run.WithCommand(command)}, opts...)...)
				if err != nil {
					t.Fatalf("Submit() error = %v", err)
				}
				if r.SpecSeal == "" {
					t.Fatal("a submitted run carries no seal")
				}
				return r
			}
			target := submit("deploy", run.WithQueue("work"))
			other := submit("other", run.WithQueue("parked"))
			held := submit("held", run.WithQueue("parked"), run.WithRequireApproval(true))
			decided, err := submitter.Approve(ctx, held.ID, bndDecider())
			if err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			submitter.Close()
			if test.Rewrite != nil {
				edit := test.Rewrite(t, target, other, decided)
				store.rewrite(func(r *run.Run) bool { return r.ID == target.ID }, edit)
			}
			runner := &commandRecorder{}
			executor := New(store, runner, nil, WithNoJanitor(), WithAudits(chain),
				WithBindingKey(key), WithQueues([]string{"work"}),
				WithClaimInterval(5*time.Millisecond))
			defer executor.Close()
			got := waitTerminal(t, store.Store, target.ID)
			result := bndResult{Status: got.Status,
				Ran:     runner.count("deploy") + runner.count(rewritten),
				Refused: strings.HasPrefix(got.Error, "refused: ")}
			if diff := cmp.Diff(test.WantResult, result); diff != "" {
				t.Errorf("run mismatch (-want +got):\n%s\nerror: %s", diff, got.Error)
			}
		})
	}
}

// TestANeverHeldWorkflowsApprovalStepPassesGate pins that the approval step of a workflow nobody
// was asked to approve as a whole is held to the seal its workflow was submitted with, and passes:
// the step's approval is checked against a workflow this process sealed at submit, not refused for
// carrying no seal.
func TestANeverHeldWorkflowsApprovalStepPassesGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ci := newChildInstall(t)
	parent, err := ci.d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, false))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, ci.store.Store, parent.ID)
	release := ci.d.enterCoordinator(parent.ID)
	decision := StepDecision{Approve: true, By: bndDecider()}
	if _, err := ci.d.DecideStep(ctx, node.ID, decision); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	stored, err := ci.store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.DecisionID != "" || stored.SpecSeal == "" {
		t.Fatalf("workflow carries decision %q and seal %q, want no decision and a seal",
			stored.DecisionID, stored.SpecSeal)
	}
	decided, err := ci.store.Get(ctx, node.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if err := ci.d.checkStepBinding(ctx, stored, decided); err != nil {
		t.Errorf("checkStepBinding() error = %v, want the approved step to pass", err)
	}
	release()
	ci.d.sweepApprovalSteps()
	if got := waitTerminal(t, ci.store.Store, parent.ID); got.Status != run.StatusSucceeded {
		t.Errorf("workflow = %s (%s), want succeeded", got.Status, got.Error)
	}
	if n := ci.runner.count("deploy"); n != 1 {
		t.Errorf("deploy ran %d times, want once", n)
	}
}
