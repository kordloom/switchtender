package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// published is the value the first step of every workflow in these tests publishes for the steps
// after it.
const published = "1.2.3"

// publishRunner is a runner and host lister that publishes fixed outputs per playbook, fails the
// playbooks it is told to, and records the extra vars each playbook last ran with.
type publishRunner struct {
	// publish maps a playbook to the outputs its stats event carries.
	publish map[string]map[string]any
	// fail names the playbooks that exit nonzero.
	fail map[string]bool
	// mu guards received.
	mu sync.Mutex
	// received maps a playbook that ran to the extra vars it last ran with.
	received map[string]map[string]any
}

// newPublishRunner returns a runner on which build.yml and site.yml publish the version, and
// build.yml and site.yml fail when fail is set.
func newPublishRunner(fail bool) *publishRunner {
	out := map[string]any{"version": published}
	return &publishRunner{
		publish:  map[string]map[string]any{"build.yml": out, "site.yml": out},
		fail:     map[string]bool{"build.yml": fail, "site.yml": fail},
		received: map[string]map[string]any{},
	}
}

// Run records the extra vars, writes a stats event carrying the playbook's outputs, and exits
// nonzero for a playbook it fails.
func (p *publishRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result,
	error) {
	vars := map[string]any{}
	maps.Copy(vars, spec.ExtraVars)
	p.mu.Lock()
	p.received[spec.Playbook] = vars
	p.mu.Unlock()
	if out := p.publish[spec.Playbook]; len(out) > 0 && spec.EventsPath != "" {
		data, err := json.Marshal(map[string]any{"type": "stats", "ts": 1719000000, "outputs": out})
		if err != nil {
			return roundhouse.Result{ExitCode: -1}, err
		}
		if err := os.WriteFile(spec.EventsPath, append(data, '\n'), 0o600); err != nil {
			return roundhouse.Result{ExitCode: -1}, err
		}
	}
	if p.fail[spec.Playbook] {
		return roundhouse.Result{ExitCode: 2}, nil
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts lists the two hosts every split in these tests fans out across.
func (p *publishRunner) Hosts(context.Context, string, string) ([]string, error) {
	return []string{"web1", "web2"}, nil
}

// input returns the version playbook last ran with, and whether it ran at all.
func (p *publishRunner) input(playbook string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	vars, ran := p.received[playbook]
	version, _ := vars["version"].(string)
	return version, ran
}

// sealedRow submits one shape of run on d and returns the row whose result seal is under test once
// the run has ended.
type sealedRow func(t *testing.T, d *Dispatcher, store run.Store) *run.Run

// sealedStep is a one-step workflow, with its step under test.
func sealedStep(t *testing.T, d *Dispatcher, store run.Store) *run.Run {
	t.Helper()
	ctx := context.Background()
	parent, err := d.SubmitPipeline(ctx, "release", "inv",
		[]run.PipelineStep{{Name: "build", Playbook: "site.yml"}})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	waitTerminal(t, store, parent.ID)
	steps, err := store.Steps(ctx, parent.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("Steps() = %d steps, error %v, want one step", len(steps), err)
	}
	return steps[0]
}

// sealedShard is a split across two hosts, with its first shard under test.
func sealedShard(t *testing.T, d *Dispatcher, store run.Store) *run.Run {
	t.Helper()
	ctx := context.Background()
	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	waitTerminal(t, store, parent.ID)
	shards, err := store.Shards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	for _, s := range shards {
		if isShard(s) {
			return s
		}
	}
	t.Fatalf("split %s stored no first shard", parent.ID)
	return nil
}

// sealedTop is a run with no parent, itself under test.
func sealedTop(t *testing.T, d *Dispatcher, store run.Store) *run.Run {
	t.Helper()
	r, err := d.Submit(context.Background(), "site.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	return waitTerminal(t, store, r.ID)
}

// TestOnlyASucceededChildIsResultSealed pins which terminal writes carry a result seal: a shard or
// a workflow step that succeeded on a keyed dispatcher, and nothing else. A sealed row's seal holds
// against the row as the store gives it back, which is what a walk reading the step later checks.
func TestOnlyASucceededChildIsResultSealed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the row.
		Name string
		// Keyed gives the dispatcher a binding key.
		Keyed bool
		// Fail makes the playbook fail.
		Fail bool
		// Row submits the run and returns the row under test.
		Row sealedRow
		// WantSealed reports whether the row carries a result seal.
		WantSealed bool
	}{{ // Test 0: A workflow step that succeeded is sealed.
		Name: "step", Keyed: true, Row: sealedStep, WantSealed: true,
	}, { // Test 1: A shard that succeeded is sealed.
		Name: "shard", Keyed: true, Row: sealedShard, WantSealed: true,
	}, { // Test 2: A workflow step that failed feeds nothing forward and is not sealed.
		Name: "failed step", Keyed: true, Fail: true, Row: sealedStep,
	}, { // Test 3: A run with no parent feeds no walk and is not sealed.
		Name: "top-level run", Keyed: true, Row: sealedTop,
	}, { // Test 4: A dispatcher with no binding key seals nothing.
		Name: "unkeyed step", Row: sealedStep,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			opts := []Option{WithNoJanitor(), WithAudits(audit.NewMemStore()),
				WithClaimInterval(5 * time.Millisecond)}
			if test.Keyed {
				opts = append(opts, WithBindingKey(bndKey(t)))
			}
			d := New(store, newPublishRunner(test.Fail), nil, opts...)
			defer d.Close()
			row := test.Row(t, d, store)
			if diff := cmp.Diff(test.WantSealed, row.ResultSeal != ""); diff != "" {
				t.Fatalf("sealed mismatch (-want +got):\n%s\nrow %s ended %s with seal %q", diff,
					row.ID, row.Status, row.ResultSeal)
			}
			if !test.WantSealed {
				return
			}
			if err := d.binder.Result(context.Background(), row); err != nil {
				t.Errorf("Result() error = %v, want the stored row to hold to its seal", err)
			}
		})
	}
}

// resultShape is the workflow a walk test drives, named by how the walk meets the finished build
// step: on a fresh wait, or on a resume after the workflow parked.
type resultShape int

const (
	// shapeGraph is build then deploy as a graph, walked live, so deploy reads build on a fresh
	// wait.
	shapeGraph resultShape = iota
	// shapeSequence is build then deploy as a sequence, run live, so deploy reads build on a
	// fresh wait.
	shapeSequence
	// shapeResumeBefore is build, an approval step, then deploy, with the writer acting while the
	// workflow is parked and before the step is approved, so the approval is given over what the
	// writer left and the walk reads build again on the resume.
	shapeResumeBefore
	// shapeResumeAfter is shapeResumeBefore with the writer acting after the step is approved.
	shapeResumeAfter
)

// steps returns the workflow s drives.
func (s resultShape) steps() []run.PipelineStep {
	switch s {
	case shapeGraph:
		return []run.PipelineStep{
			{Name: "build", Playbook: "build.yml"},
			{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"build"}},
		}
	case shapeSequence:
		return []run.PipelineStep{
			{Name: "build", Playbook: "build.yml"},
			{Name: "deploy", Playbook: "deploy.yml"},
		}
	}
	return []run.PipelineStep{
		{Name: "build", Playbook: "build.yml"},
		{Name: "gate", Type: run.StepApproval, Description: "Ship it?", DependsOn: []string{"build"}},
		{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"gate"}},
	}
}

// walkResult is how a workflow under test ended and what its deploy step ran with.
type walkResult struct {
	// Status is the workflow's final status.
	Status run.Status
	// Ran reports whether deploy executed.
	Ran bool
	// Received is the version deploy ran with, empty when it did not run.
	Received string
	// Refused reports whether the workflow's error starts with the refusal's prefix.
	Refused bool
}

// isFinishedBuild picks the build step once it has ended.
func isFinishedBuild(r *run.Run) bool { return r.StepName == "build" && r.Status.Terminal() }

// driveResultWalk runs shape on a keyed install, applying rewrite, when set, to the finished build
// step at the moment shape names, and returns how the workflow ended.
func driveResultWalk(t *testing.T, shape resultShape, fail bool, rewrite func(r *run.Run)) walkResult {
	t.Helper()
	ctx := context.Background()
	key := bndKey(t)
	store, chain := newPickedRows(), &bndChain{Store: audit.NewMemStore()}
	runner := newPublishRunner(fail)
	keyed := func() *Dispatcher {
		return New(store, runner, nil, WithNoJanitor(), WithAudits(chain), WithBindingKey(key),
			WithClaimInterval(5*time.Millisecond))
	}
	act := func() {
		if rewrite != nil {
			store.rewrite(isFinishedBuild, rewrite)
		}
	}
	first := keyed()
	var parent *run.Run
	var err error
	if shape == shapeGraph || shape == shapeSequence {
		defer first.Close()
		act()
		if parent, err = first.SubmitPipeline(ctx, "release", "inv", shape.steps()); err != nil {
			t.Fatalf("SubmitPipeline() error = %v", err)
		}
	} else {
		if parent, err = first.SubmitPipeline(ctx, "release", "inv", shape.steps()); err != nil {
			t.Fatalf("SubmitPipeline() error = %v", err)
		}
		node := waitParked(t, store.Store, parent.ID)
		first.Close()
		second := keyed()
		defer second.Close()
		// A coordinator registered for the workflow keeps the decision from resuming it, so the
		// writer can act between the decision and the resume.
		release := second.enterCoordinator(parent.ID)
		if shape == shapeResumeBefore {
			act()
		}
		decision := StepDecision{Approve: true, By: bndDecider()}
		if _, err := second.DecideStep(ctx, node.ID, decision); err != nil {
			release()
			t.Fatalf("DecideStep() error = %v", err)
		}
		if shape == shapeResumeAfter {
			act()
		}
		release()
		second.sweepApprovalSteps()
	}
	got := waitTerminal(t, store.Store, parent.ID)
	received, ran := runner.input("deploy.yml")
	return walkResult{Status: got.Status, Ran: ran, Received: received,
		Refused: strings.HasPrefix(got.Error, "refused: ")}
}

// TestWalkRefusesRewrittenOutputs pins that a finished step's status and outputs enter a walk only
// under the result seal its terminal write carried, on a fresh wait and on a resume. A writer who
// changes the outputs a step published, removes its seal, or makes a failed step read as succeeded
// gets neither its values nor anything waiting behind it: the workflow fails with the reason. A
// writer acting while a workflow waits for an approval, before the approver decides, leaves a state
// the approval is then given over, so the row to row checks and the approval's own seal all pass and
// only the result seal stands between the writer's values and the step after the approval.
//
//nolint:funlen // Test function.
func TestWalkRefusesRewrittenOutputs(t *testing.T) {
	t.Parallel()
	outputs := func(r *run.Run) { r.Outputs = map[string]any{"version": rewritten} }
	unsealed := func(r *run.Run) { r.ResultSeal = "" }
	succeeded := func(r *run.Run) {
		r.Status, r.Outputs = run.StatusSucceeded, map[string]any{"version": rewritten}
	}
	ran := walkResult{Status: run.StatusSucceeded, Ran: true, Received: published}
	refused := walkResult{Status: run.StatusFailed, Refused: true}
	tests := []struct {
		// Name labels the row.
		Name string
		// Shape is the workflow and the moment the writer acts.
		Shape resultShape
		// Fail makes build fail.
		Fail bool
		// Rewrite is the writer's change to the finished build step, nil for none.
		Rewrite func(r *run.Run)
		// WantResult is how the workflow ends and what deploy ran with.
		WantResult walkResult
	}{{ // Test 0: An untouched graph hands build's outputs to deploy.
		Name: "graph untouched", Shape: shapeGraph, WantResult: ran,
	}, { // Test 1: Build's outputs are rewritten before the graph reads them.
		Name: "graph outputs", Shape: shapeGraph, Rewrite: outputs, WantResult: refused,
	}, { // Test 2: Build's result seal is removed.
		Name: "graph seal removed", Shape: shapeGraph, Rewrite: unsealed, WantResult: refused,
	}, { // Test 3: A build that failed is left as it is, so deploy is skipped and nothing refused.
		Name: "graph failed", Shape: shapeGraph, Fail: true,
		WantResult: walkResult{Status: run.StatusFailed},
	}, { // Test 4: A build that failed is rewritten to read succeeded with other outputs.
		Name: "graph failed rewritten", Shape: shapeGraph, Fail: true, Rewrite: succeeded,
		WantResult: refused,
	}, { // Test 5: An untouched sequence hands build's outputs to deploy.
		Name: "sequence untouched", Shape: shapeSequence, WantResult: ran,
	}, { // Test 6: Build's outputs are rewritten before the sequence reads them.
		Name: "sequence outputs", Shape: shapeSequence, Rewrite: outputs, WantResult: refused,
	}, { // Test 7: An untouched resumed workflow hands build's outputs to deploy.
		Name: "resume untouched", Shape: shapeResumeBefore, WantResult: ran,
	}, { // Test 8: Build's outputs are rewritten before the approval is given over them.
		Name: "resume outputs before", Shape: shapeResumeBefore, Rewrite: outputs,
		WantResult: refused,
	}, { // Test 9: Build's outputs are rewritten after the approval.
		Name: "resume outputs after", Shape: shapeResumeAfter, Rewrite: outputs,
		WantResult: refused,
	}, { // Test 10: Build's result seal is removed while the workflow waits.
		Name: "resume seal removed", Shape: shapeResumeBefore, Rewrite: unsealed,
		WantResult: refused,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := driveResultWalk(t, test.Shape, test.Fail, test.Rewrite)
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("workflow mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAStepBehindARefusalDoesNotRun pins the rule a restored walk needs and a live walk never
// reaches: a step waiting behind a refused step, directly or through steps that already settled,
// is skipped. Only a step none of whose dependencies was refused may start.
func TestAStepBehindARefusalDoesNotRun(t *testing.T) {
	t.Parallel()
	steps := []run.PipelineStep{
		{Name: "build", Playbook: "build.yml"},
		{Name: "gate", Type: run.StepApproval, DependsOn: []string{"build"}},
		{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"gate"}},
		{Name: "lint", Playbook: "lint.yml"},
	}
	tests := []struct {
		// Refused names the steps refused before the walk advances.
		Refused []string
		// WantBlocked names the waiting steps that can never start.
		WantBlocked []string
	}{{ // Test 0: Nothing refused, so nothing is blocked.
		Refused: nil,
	}, { // Test 1: A refused build blocks deploy through the gate that already settled.
		Refused: []string{"build"}, WantBlocked: []string{"deploy"},
	}, { // Test 2: A refused lint blocks nothing, since nothing waits behind it.
		Refused: []string{"lint"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d := &Dispatcher{}
			w := d.newDAGWalk(&run.Run{ID: "run_parent"}, steps)
			for _, name := range []string{"build", "gate"} {
				w.settle(w.byName[name], run.StatusSucceeded, nil)
			}
			for _, name := range test.Refused {
				w.refuse(w.byName[name], "refused: test")
			}
			var blocked []string
			for i, s := range steps {
				if w.states[i] != stepWaiting {
					continue
				}
				if _, never := w.readiness(i); never {
					blocked = append(blocked, s.Name)
				}
			}
			if diff := cmp.Diff(test.WantBlocked, blocked, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("blocked mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// stepRecord is what a walk finds when it reads a finished step's record back.
type stepRecord int

const (
	// recordSealed is the record this process finished succeeded and sealed.
	recordSealed stepRecord = iota
	// recordFailed is a record that reads failed although the walk saw the step succeed.
	recordFailed
	// recordUnreadable is a record whose reads keep failing.
	recordUnreadable
)

// TestStepOutputsHoldsTheRecordReadBack pins what a keyed walk does with the record of a step it
// saw succeed. A sealed record hands its outputs over. A record that now reads otherwise was changed
// after the step finished, and is refused. A record that cannot be read leaves the step's success
// unverified, which fails the walk without feeding anything forward and says it could not verify
// rather than that the step was changed, with no word of the store's own error. Without a key the
// read stays best effort.
func TestStepOutputsHoldsTheRecordReadBack(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Keyed gives the dispatcher a binding key.
		Keyed bool
		// Record is what the walk reads back.
		Record stepRecord
		// WantOutputs are the outputs handed to the walk.
		WantOutputs map[string]any
		// WantReason is the reason the walk is given.
		WantReason string
	}{{ // Test 0: A sealed record hands its outputs over.
		Keyed: true, Record: recordSealed, WantOutputs: map[string]any{"version": published},
	}, { // Test 1: A record that reads failed after the step succeeded is refused.
		Keyed: true, Record: recordFailed,
		WantReason: "refused: run run_step: its status changed after it finished, from succeeded " +
			"to failed",
	}, { // Test 2: A keyed walk that cannot read the record fails with the could-not-verify sentence.
		Keyed: true, Record: recordUnreadable,
		WantReason: "could not verify: run run_step: read its record",
	}, { // Test 3: A walk with no key hands over what the record holds.
		Record: recordFailed, WantOutputs: map[string]any{"version": published},
	}, { // Test 4: A walk with no key that cannot read the record goes on with no outputs.
		Record: recordUnreadable,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			if test.Record == recordUnreadable {
				store = &failingGets{Store: store, err: errBndUnreachable}
			}
			opts := []Option{WithNoJanitor(), WithAudits(audit.NewMemStore()),
				WithQueues([]string{"nobody"})}
			if test.Keyed {
				opts = append(opts, WithBindingKey(bndKey(t)))
			}
			d := New(store, okRunner(), nil, opts...)
			defer d.Close()
			parentID, idx := "run_parent", 0
			step := &run.Run{ID: "run_step", ParentID: &parentID, StepIndex: &idx, StepName: "build",
				Playbook: "build.yml", Status: run.StatusRunning, CreatedAt: time.Now(),
				ClaimedBy: "worker-1"}
			switch test.Record {
			case recordSealed:
				if err := store.Save(ctx, step); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
				step.Outputs = map[string]any{"version": published}
				code := 0
				d.finalize(step, run.StatusSucceeded, &code, "")
			case recordFailed:
				step.Status, step.Outputs = run.StatusFailed, map[string]any{"version": published}
				if err := store.Save(ctx, step); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
			}
			outputs, reason := d.stepOutputs(&run.Run{ID: step.ID})
			if diff := cmp.Diff(test.WantReason, reason); diff != "" {
				t.Errorf("reason mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantOutputs, outputs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("outputs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// failingGets is a run store whose point reads fail with err.
type failingGets struct {
	run.Store
	// err is what every Get returns.
	err error
}

// Get fails.
func (s *failingGets) Get(context.Context, string) (*run.Run, error) {
	return nil, s.err
}

// savingFinalizes is a run store whose conditional terminal write never moves a run, so every
// terminal record lands through the whole-run save recordTerminal falls back to.
type savingFinalizes struct {
	run.Store
}

// FinalizeRunning moves nothing.
func (s *savingFinalizes) FinalizeRunning(context.Context, string, run.Finalization) (bool, error) {
	return false, nil
}

// TestResultSealCoversTheStoredOutputs pins that the seal a terminal write carries is made over the
// outputs as the store keeps them, through the conditional terminal write and through the
// whole-run save that stands in for it. A store cleans text it cannot hold, so a seal made over the
// values as the playbook published them would refuse the honest row once it was read back.
func TestResultSealCoversTheStoredOutputs(t *testing.T) {
	t.Parallel()
	dirty := map[string]any{"version": "1.2\x00.3", "note": "caf\xe9"}
	tests := []struct {
		// Outputs are the values the step published.
		Outputs map[string]any
		// Saved makes the terminal record land through the whole-run save.
		Saved bool
	}{{ // Test 0: Plain values.
		Outputs: map[string]any{"version": published},
	}, { // Test 1: A value holding a NUL byte and a broken UTF-8 sequence, which a store cleans.
		Outputs: dirty,
	}, { // Test 2: No values at all.
		Outputs: nil,
	}, { // Test 3: Plain values through the whole-run save.
		Outputs: map[string]any{"version": published}, Saved: true,
	}, { // Test 4: Values a store cleans, through the whole-run save.
		Outputs: dirty, Saved: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			if test.Saved {
				store = &savingFinalizes{Store: store}
			}
			d := New(store, okRunner(), nil, WithNoJanitor(), WithAudits(audit.NewMemStore()),
				WithBindingKey(bndKey(t)), WithQueues([]string{"nobody"}))
			defer d.Close()
			parentID, idx := "run_parent", 0
			step := &run.Run{ID: "run_step", ParentID: &parentID, StepIndex: &idx, StepName: "build",
				Playbook: "build.yml", Status: run.StatusRunning, CreatedAt: time.Now(),
				ClaimedBy: "worker-1"}
			if err := store.Save(ctx, step); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			step.Outputs = maps.Clone(test.Outputs)
			code := 0
			d.finalize(step, run.StatusSucceeded, &code, "")
			stored, err := store.Get(ctx, step.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if stored.Status != run.StatusSucceeded || stored.ResultSeal == "" {
				t.Fatalf("stored step = %s with seal %q, want succeeded and sealed", stored.Status,
					stored.ResultSeal)
			}
			if err := d.binder.Result(ctx, stored); err != nil {
				t.Errorf("Result() error = %v, want the stored row to hold to its seal", err)
			}
			rec, err := binding.ResultRecord(stored)
			if err != nil {
				t.Fatalf("ResultRecord() error = %v", err)
			}
			if !d.binder.Key.Verify(rec, step.ResultSeal) {
				t.Error("the seal on the finished run in memory is not the stored row's seal")
			}
		})
	}
}
