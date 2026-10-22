package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// adoptActor is who the chain names for adoption in these tests, the way the command names the
// account running it.
func adoptActor() outcome.Decider {
	return outcome.Decider{Name: "cli:operator", Type: "cli"}
}

// adoptRunner is a publishRunner that also counts how many times each playbook executed.
type adoptRunner struct {
	// publishRunner publishes the outputs and records the vars each playbook last ran with.
	*publishRunner
	// mu guards ran.
	mu sync.Mutex
	// ran counts executions per playbook.
	ran map[string]int
}

// newAdoptRunner returns a runner on which build.yml and site.yml publish the version.
func newAdoptRunner() *adoptRunner {
	return &adoptRunner{publishRunner: newPublishRunner(false), ran: map[string]int{}}
}

// Run counts the playbook and runs it as publishRunner does.
func (a *adoptRunner) Run(ctx context.Context, spec roundhouse.Spec, w io.Writer) (roundhouse.Result,
	error) {
	a.mu.Lock()
	a.ran[spec.Playbook]++
	a.mu.Unlock()
	return a.publishRunner.Run(ctx, spec, w)
}

// counts returns how many times each playbook executed.
func (a *adoptRunner) counts() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.ran)
}

// adoptInstall is an install upgraded in place over one store and one chain: a dispatcher with no
// binding key stands for the binary before the upgrade, and keyed ones for the binary after it.
type adoptInstall struct {
	// store is the runs table.
	store run.Store
	// chain is the audit table, whose DECISION appends can be stopped.
	chain *claimChain
	// runner records what executed, on either binary.
	runner *adoptRunner
	// key is the binding key the upgrade brings.
	key *binding.Key
	// old is the binary before the upgrade, nil once it has stopped.
	old *Dispatcher
}

// newAdoptInstall starts the binary before the upgrade over store, serving the default queue.
func newAdoptInstall(t *testing.T, store run.Store) *adoptInstall {
	t.Helper()
	in := &adoptInstall{store: store, chain: &claimChain{Store: audit.NewMemStore()},
		runner: newAdoptRunner(), key: bndKey(t)}
	in.old = New(store, in.runner, nil, WithNoJanitor(), WithAudits(in.chain),
		WithClaimInterval(5*time.Millisecond))
	t.Cleanup(in.stopOld)
	return in
}

// stopOld stops the binary before the upgrade.
func (in *adoptInstall) stopOld() {
	if in.old != nil {
		in.old.Close()
		in.old = nil
	}
}

// deps returns what adopting the install reads and writes, under the upgrade's key.
func (in *adoptInstall) deps() AdoptionDeps {
	return AdoptionDeps{Runs: in.store, Audits: in.chain, Key: in.key, Actor: adoptActor()}
}

// keyed starts a binary after the upgrade, serving the default queue and the work queue.
func (in *adoptInstall) keyed(t *testing.T) *Dispatcher {
	t.Helper()
	d := New(in.store, in.runner, nil, WithNoJanitor(), WithAudits(in.chain),
		WithBindingKey(in.key), WithClaimInterval(5*time.Millisecond),
		WithQueues([]string{"", "work"}))
	t.Cleanup(d.Close)
	return d
}

// verifier returns the checks a keyed process holds rows to.
func (in *adoptInstall) verifier() *binding.Verifier {
	return NewBindingVerifier(in.store, in.chain, in.key)
}

// get reads the run id.
func (in *adoptInstall) get(t *testing.T, id string) *run.Run {
	t.Helper()
	r, err := in.store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return r
}

// held submits a run held for approval, with opts, on the binary before the upgrade.
func (in *adoptInstall) held(t *testing.T, opts ...run.SubmitOption) *run.Run {
	t.Helper()
	r, err := in.old.Submit(context.Background(), "site.yml", "inv",
		append([]run.SubmitOption{run.WithRequireApproval(true)}, opts...)...)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	return r
}

// down runs act with DECISION appends stopped, so a decision act makes is left claimed on its row
// and unrecorded, the way a process that died after claiming leaves it.
func (in *adoptInstall) down(act func()) {
	in.chain.down.Store(true)
	defer in.chain.down.Store(false)
	act()
}

// stuck makes a decision through decide with DECISION appends stopped and requires that it was
// left unrecorded.
func (in *adoptInstall) stuck(t *testing.T, decide func() error) {
	t.Helper()
	var err error
	in.down(func() { err = decide() })
	if !errors.Is(err, errDecisionUnrecorded) {
		t.Fatalf("decision error = %v, want it left unrecorded", err)
	}
}

// parked submits adoptGated on the binary before the upgrade and returns the workflow and its
// approval step once it is parked there, build having succeeded.
func (in *adoptInstall) parked(t *testing.T) (*run.Run, *run.Run) {
	t.Helper()
	parent, err := in.old.SubmitPipeline(context.Background(), "release", "inv", adoptGated())
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	return parent, waitParked(t, in.store, parent.ID)
}

// stepOf returns the step of the workflow parentID named name, waiting for its record.
func (in *adoptInstall) stepOf(t *testing.T, parentID, name string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for time.Now().Before(deadline) {
		steps, err := in.store.Steps(context.Background(), parentID)
		if err != nil {
			t.Fatalf("Steps() error = %v", err)
		}
		for _, s := range steps {
			if s.StepName == name {
				return s
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("workflow %s stored no step %q", parentID, name)
	return nil
}

// released counts the RELEASE entries adoption appended.
func (in *adoptInstall) released(t *testing.T) int {
	t.Helper()
	chain, err := in.chain.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	n := 0
	for _, e := range chain {
		if e.Method == audit.MethodRelease && e.Actor == adoptActor().Name {
			n++
		}
	}
	return n
}

// claimHolds checks the decision claim on r the way a keyed finisher does before recording it,
// and requires an approval's claim to carry the seal the decision stamps.
func (in *adoptInstall) claimHolds(r *run.Run) error {
	c, err := claimOf(r)
	if err != nil {
		return err
	}
	raw, err := claimBytes(c)
	if err != nil {
		return err
	}
	switch {
	case !in.key.VerifyClaim(r.ID, raw, c.ClaimSeal):
		return errors.New("the claim seal does not hold")
	case c.Verdict == outcome.StepApproved && c.Seal == "":
		return errors.New("the approval carries no seal to stamp")
	}
	return nil
}

// adoptGated is build, which publishes the version, an approval step, deploy on approval, and
// notify on denial.
func adoptGated() []run.PipelineStep {
	return []run.PipelineStep{
		{Name: "build", Playbook: "build.yml"},
		{Name: "gate", Type: run.StepApproval, Description: "Ship it?", DependsOn: []string{"build"}},
		{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"gate"}},
		{Name: "notify", Playbook: "notify.yml", IfDenied: []string{"gate"}},
	}
}

// adoptCase is what the binary before the upgrade left for a case: the workflow or split, the
// approval step of a workflow, and the row under test.
type adoptCase struct {
	// parent is the workflow or split, nil for a run.
	parent *run.Run
	// node is the workflow's approval step, nil when there is none.
	node *run.Run
	// row is the row under test.
	row *run.Run
}

// reread returns c with every row read again from the store.
func (c adoptCase) reread(t *testing.T, in *adoptInstall) adoptCase {
	t.Helper()
	out := adoptCase{row: in.get(t, c.row.ID)}
	if c.parent != nil {
		out.parent = in.get(t, c.parent.ID)
	}
	if c.node != nil {
		out.node = in.get(t, c.node.ID)
	}
	return out
}

// adoptResult is what adoption sealed for a case and how the work ended on the keyed binary.
type adoptResult struct {
	// Seals are the kinds of seal adoption wrote, sorted.
	Seals []string
	// Released counts the RELEASE entries adoption appended.
	Released int
	// Holds reports whether the row under test passed the check a keyed process makes on it.
	Holds bool
	// Status is how the work ended.
	Status run.Status
	// Ran counts each playbook's executions across both binaries.
	Ran map[string]int
	// Received is the version deploy ran with, empty when it did not run.
	Received string
}

// TestAdoptSealsWhatAnOldBinaryLeft pins that adoption gives every row an earlier binary left the
// seal a keyed process holds it to, one case per rule of the adoption table: a held run, an
// approved run waiting for an executor, a run whose approval was claimed and never recorded, a
// shard of a held split, an approval step waiting for a person, an approval step whose decision
// was claimed and never recorded, a step no decision released and one a decision released waiting
// for an executor, a finished step a parked workflow feeds forward, and an approved approval step a
// parked workflow resumes past. Each case is left by a dispatcher with no key, adopted, held to the
// keyed check its row is next given, and then decided, executed, or resumed on a keyed dispatcher,
// and runs.
//
//nolint:funlen // Test function.
func TestAdoptSealsWhatAnOldBinaryLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	approve := StepDecision{Approve: true, By: bndDecider()}
	ran := func(playbooks ...string) map[string]int {
		out := map[string]int{}
		for _, p := range playbooks {
			out[p]++
		}
		return out
	}
	tests := []struct {
		// Name labels the rule of the adoption table the case stands for.
		Name string
		// Leave builds the case on the binary before the upgrade, and stops it unless a
		// coordinator of it must keep running.
		Leave func(t *testing.T, in *adoptInstall) adoptCase
		// Check holds the adopted row to the check a keyed process makes on it next.
		Check func(in *adoptInstall, c adoptCase) error
		// Drive decides, executes, or resumes the case on a keyed dispatcher and returns the run
		// whose end is compared.
		Drive func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run
		// WantResult is what adoption sealed and how the work ended.
		WantResult adoptResult
	}{{ // Test 0: A held run is sealed as submitted, and an approval then releases it.
		Name: "top-level, no decision",
		Leave: func(t *testing.T, in *adoptInstall) adoptCase {
			r := in.held(t)
			in.stopOld()
			return adoptCase{row: r}
		},
		Check: func(in *adoptInstall, c adoptCase) error { return in.verifier().Held(ctx, c.row) },
		Drive: func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run {
			if _, err := in.keyed(t).Approve(ctx, c.row.ID, bndDecider()); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			return waitTerminal(t, in.store, c.row.ID)
		},
		WantResult: adoptResult{Seals: []string{binding.KindSubmit}, Holds: true,
			Status: run.StatusSucceeded, Ran: ran("site.yml")},
	}, { // Test 1: An approved run waiting for an executor is sealed to its DECISION entry.
		Name: "top-level, decision set",
		Leave: func(t *testing.T, in *adoptInstall) adoptCase {
			r := in.held(t, run.WithQueue("work"))
			if _, err := in.old.Approve(ctx, r.ID, bndDecider()); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			in.stopOld()
			return adoptCase{row: r}
		},
		Check: func(in *adoptInstall, c adoptCase) error {
			return in.verifier().Executable(ctx, c.row)
		},
		Drive: func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run {
			in.keyed(t)
			return waitTerminal(t, in.store, c.row.ID)
		},
		WantResult: adoptResult{Seals: []string{binding.KindRun}, Holds: true,
			Status: run.StatusSucceeded, Ran: ran("site.yml")},
	}, { // Test 2: An approval claimed and never recorded has its claim completed.
		Name: "top-level, in flight",
		Leave: func(t *testing.T, in *adoptInstall) adoptCase {
			r := in.held(t)
			in.stuck(t, func() error {
				_, err := in.old.Approve(ctx, r.ID, bndDecider())
				return err
			})
			in.stopOld()
			return adoptCase{row: r}
		},
		Check: func(in *adoptInstall, c adoptCase) error { return in.claimHolds(c.row) },
		Drive: func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run {
			in.keyed(t).sweepApprovalSteps()
			return waitTerminal(t, in.store, c.row.ID)
		},
		WantResult: adoptResult{Seals: []string{binding.KindClaim}, Holds: true,
			Status: run.StatusSucceeded, Ran: ran("site.yml")},
	}, { // Test 3: The shards of a held split are sealed to it, and an approval then runs them.
		Name: "shard",
		Leave: func(t *testing.T, in *adoptInstall) adoptCase {
			parent, err := in.old.SubmitSplit(ctx, "site.yml", "inv", 2,
				run.WithRequireApproval(true))
			if err != nil {
				t.Fatalf("SubmitSplit() error = %v", err)
			}
			in.stopOld()
			shards, err := in.store.Shards(ctx, parent.ID)
			if err != nil || len(shards) != 2 {
				t.Fatalf("Shards() = %d shards, error %v, want two", len(shards), err)
			}
			return adoptCase{parent: parent, row: shards[0]}
		},
		Check: func(in *adoptInstall, c adoptCase) error {
			return in.verifier().Executable(ctx, c.row)
		},
		Drive: func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run {
			if _, err := in.keyed(t).Approve(ctx, c.parent.ID, bndDecider()); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			return waitTerminal(t, in.store, c.parent.ID)
		},
		WantResult: adoptResult{Seals: []string{binding.KindShard, binding.KindShard,
			binding.KindSubmit}, Holds: true, Status: run.StatusSucceeded,
			Ran: ran("site.yml", "site.yml")},
	}, { // Test 4: An approval step waiting for a person is sealed as created, and a denial then
		// runs the deny path.
		Name: "pending approval step",
		Leave: func(t *testing.T, in *adoptInstall) adoptCase {
			parent, node := in.parked(t)
			in.stopOld()
			return adoptCase{parent: parent, node: node, row: node}
		},
		Check: func(in *adoptInstall, c adoptCase) error {
			return in.verifier().Pending(ctx, c.parent, c.row)
		},
		Drive: func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run {
			deny := StepDecision{Approve: false, By: bndDecider()}
			if _, err := in.keyed(t).DecideStep(ctx, c.node.ID, deny); err != nil {
				t.Fatalf("DecideStep() error = %v", err)
			}
			return waitTerminal(t, in.store, c.parent.ID)
		},
		WantResult: adoptResult{Seals: []string{binding.KindResult, binding.KindStep,
			binding.KindSubmit}, Holds: true, Status: run.StatusSucceeded,
			Ran: ran("build.yml", "notify.yml")},
	}, { // Test 5: An approval step whose decision was claimed and never recorded has its claim
		// completed, and the workflow then resumes past it.
		Name: "approval step in flight",
		Leave: func(t *testing.T, in *adoptInstall) adoptCase {
			parent, node := in.parked(t)
			in.stuck(t, func() error {
				_, err := in.old.DecideStep(ctx, node.ID, approve)
				return err
			})
			in.stopOld()
			return adoptCase{parent: parent, node: node, row: node}
		},
		Check: func(in *adoptInstall, c adoptCase) error { return in.claimHolds(c.row) },
		Drive: func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run {
			in.keyed(t).sweepApprovalSteps()
			return waitTerminal(t, in.store, c.parent.ID)
		},
		WantResult: adoptResult{Seals: []string{binding.KindClaim, binding.KindResult,
			binding.KindSubmit}, Holds: true, Status: run.StatusSucceeded,
			Ran: ran("build.yml", "deploy.yml"), Received: published},
	}, { // Test 6: A step no decision released, waiting for an executor while its coordinator
		// still runs on the binary before the upgrade, is sealed to its workflow.
		Name: "step, no roots",
		Leave: func(t *testing.T, in *adoptInstall) adoptCase {
			parent, err := in.old.SubmitPipeline(ctx, "release", "inv",
				[]run.PipelineStep{{Name: "deploy", Playbook: "deploy.yml"}}, run.WithQueue("work"))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			return adoptCase{parent: parent, row: in.stepOf(t, parent.ID, "deploy")}
		},
		Check: func(in *adoptInstall, c adoptCase) error {
			return in.verifier().Executable(ctx, c.row)
		},
		Drive: func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run {
			in.keyed(t)
			return waitTerminal(t, in.store, c.parent.ID)
		},
		WantResult: adoptResult{Seals: []string{binding.KindStep, binding.KindSubmit}, Holds: true,
			Status: run.StatusSucceeded, Ran: ran("deploy.yml")},
	}, { // Test 7: A step its workflow's approval released, waiting for an executor while its
		// coordinator still runs on the binary before the upgrade, is released on the chain and
		// sealed to its workflow, the decision, and the release.
		Name: "step, with roots",
		Leave: func(t *testing.T, in *adoptInstall) adoptCase {
			parent, err := in.old.SubmitPipeline(ctx, "release", "inv",
				[]run.PipelineStep{{Name: "deploy", Playbook: "deploy.yml"}},
				run.WithRequireApproval(true), run.WithQueue("work"))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			if _, err := in.old.Approve(ctx, parent.ID, bndDecider()); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			return adoptCase{parent: parent, row: in.stepOf(t, parent.ID, "deploy")}
		},
		Check: func(in *adoptInstall, c adoptCase) error {
			return in.verifier().Executable(ctx, c.row)
		},
		Drive: func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run {
			in.keyed(t)
			return waitTerminal(t, in.store, c.parent.ID)
		},
		WantResult: adoptResult{Seals: []string{binding.KindRun, binding.KindStep}, Released: 1,
			Holds: true, Status: run.StatusSucceeded, Ran: ran("deploy.yml")},
	}, { // Test 8: A finished step a parked workflow feeds forward is result sealed, and its
		// outputs reach the step after the approval.
		Name: "finished step",
		Leave: func(t *testing.T, in *adoptInstall) adoptCase {
			parent, node := in.parked(t)
			in.stopOld()
			return adoptCase{parent: parent, node: node, row: in.stepOf(t, parent.ID, "build")}
		},
		Check: func(in *adoptInstall, c adoptCase) error {
			return in.verifier().Result(ctx, c.row)
		},
		Drive: func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run {
			if _, err := in.keyed(t).DecideStep(ctx, c.node.ID, approve); err != nil {
				t.Fatalf("DecideStep() error = %v", err)
			}
			return waitTerminal(t, in.store, c.parent.ID)
		},
		WantResult: adoptResult{Seals: []string{binding.KindResult, binding.KindStep,
			binding.KindSubmit}, Holds: true, Status: run.StatusSucceeded,
			Ran: ran("build.yml", "deploy.yml"), Received: published},
	}, { // Test 9: An approval step approved before the upgrade, whose workflow has not resumed,
		// is sealed over the state it was approved on, and the workflow then resumes past it.
		Name: "approved approval step",
		Leave: func(t *testing.T, in *adoptInstall) adoptCase {
			parent, node := in.parked(t)
			release := in.old.enterCoordinator(parent.ID)
			_, err := in.old.DecideStep(ctx, node.ID, approve)
			release()
			if err != nil {
				t.Fatalf("DecideStep() error = %v", err)
			}
			in.stopOld()
			return adoptCase{parent: parent, node: node, row: node}
		},
		Check: func(in *adoptInstall, c adoptCase) error {
			state, err := outcome.StepStateOf(ctx, in.store, c.parent, c.row)
			if err != nil {
				return err
			}
			return in.verifier().Gate(ctx, c.parent, c.row, state)
		},
		Drive: func(t *testing.T, in *adoptInstall, c adoptCase) *run.Run {
			in.keyed(t).sweepApprovalSteps()
			return waitTerminal(t, in.store, c.parent.ID)
		},
		WantResult: adoptResult{Seals: []string{binding.KindGate, binding.KindResult,
			binding.KindSubmit}, Holds: true, Status: run.StatusSucceeded,
			Ran: ran("build.yml", "deploy.yml"), Received: published},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			in := newAdoptInstall(t, run.NewMemStore())
			left := test.Leave(t, in)
			res, err := Adopt(ctx, in.deps())
			if err != nil {
				t.Fatalf("Adopt() error = %v", err)
			}
			if res.Earlier || len(res.Work.Left)+len(res.Left)+len(res.Moved) != 0 ||
				len(res.Sealed) != len(res.Work.Rows) {
				t.Fatalf("Adopt() = %+v, %+v, want every planned row sealed", res, res.Work)
			}
			adopted := left.reread(t, in)
			herr := test.Check(in, adopted)
			if herr != nil {
				t.Logf("check error: %v", herr)
			}
			ended := test.Drive(t, in, adopted)
			received, _ := in.runner.input("deploy.yml")
			got := adoptResult{Released: in.released(t), Holds: herr == nil,
				Status: ended.Status, Ran: in.runner.counts(), Received: received}
			for _, r := range res.Work.Rows {
				got.Seals = append(got.Seals, r.Seal)
			}
			slices.Sort(got.Seals)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("adoption mismatch (-want +got):\n%s\nerror: %s", diff, ended.Error)
			}
		})
	}
}

// claimFinish is how a claim adoption completed ended once a keyed process finished it.
type claimFinish struct {
	// Checked reports whether the claim passed the check a keyed finisher makes before recording.
	Checked bool
	// Recorded counts DECISION entries recording a verdict on the row.
	Recorded int
	// Status is the row's status once finished.
	Status run.Status
	// Sealed reports whether the row carries a seal that holds once the decision settled it.
	Sealed bool
}

// TestAnAdoptedClaimFinishes pins that a decision a process with no key claimed and never recorded
// finishes once adopted: the keyed janitor's claim check passes, the decision is recorded once, and
// an approval stamps the seal adoption put in its claim, which the run or the step is then held to.
// A rejection or a timeout releases nothing and stamps no seal.
//
//nolint:funlen // Test function.
func TestAnAdoptedClaimFinishes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runClaim := func(decide func(in *adoptInstall, id string) error) func(t *testing.T,
		in *adoptInstall) adoptCase {
		return func(t *testing.T, in *adoptInstall) adoptCase {
			r := in.held(t, run.WithQueue("parked"))
			in.stuck(t, func() error { return decide(in, r.ID) })
			return adoptCase{row: r}
		}
	}
	stepClaim := func(decide func(t *testing.T, in *adoptInstall, node *run.Run)) func(
		t *testing.T, in *adoptInstall) adoptCase {
		return func(t *testing.T, in *adoptInstall) adoptCase {
			parent, node := in.parked(t)
			in.down(func() { decide(t, in, node) })
			return adoptCase{parent: parent, node: node, row: node}
		}
	}
	tests := []struct {
		// Leave leaves a decision claimed and unrecorded on the binary before the upgrade.
		Leave func(t *testing.T, in *adoptInstall) adoptCase
		// WantResult is how the claim ended.
		WantResult claimFinish
	}{{ // Test 0: An approval of a run.
		Leave: runClaim(func(in *adoptInstall, id string) error {
			_, err := in.old.Approve(ctx, id, bndDecider())
			return err
		}),
		WantResult: claimFinish{Checked: true, Recorded: 1, Status: run.StatusPending, Sealed: true},
	}, { // Test 1: A rejection of a run.
		Leave: runClaim(func(in *adoptInstall, id string) error {
			_, err := in.old.Reject(ctx, id, "", bndDecider())
			return err
		}),
		WantResult: claimFinish{Checked: true, Recorded: 1, Status: run.StatusRejected},
	}, { // Test 2: An approval of a workflow approval step.
		Leave: stepClaim(func(t *testing.T, in *adoptInstall, node *run.Run) {
			_, err := in.old.DecideStep(ctx, node.ID, StepDecision{Approve: true, By: bndDecider()})
			if !errors.Is(err, errDecisionUnrecorded) {
				t.Errorf("DecideStep() error = %v, want it left unrecorded", err)
			}
		}),
		WantResult: claimFinish{Checked: true, Recorded: 1, Status: run.StatusSucceeded,
			Sealed: true},
	}, { // Test 3: A timeout of a workflow approval step.
		Leave: stepClaim(func(t *testing.T, in *adoptInstall, node *run.Run) {
			if in.old.timeOutStep(ctx, node) {
				t.Error("timeOutStep() recorded the timeout, want it left unrecorded")
			}
		}),
		WantResult: claimFinish{Checked: true, Recorded: 1, Status: run.StatusFailed},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			in := newAdoptInstall(t, run.NewMemStore())
			left := test.Leave(t, in)
			if !in.get(t, left.row.ID).InFlight() {
				t.Fatalf("run %s is not left claimed", left.row.ID)
			}
			in.stopOld()
			if _, err := Adopt(ctx, in.deps()); err != nil {
				t.Fatalf("Adopt() error = %v", err)
			}
			d := in.keyed(t)
			if left.parent != nil {
				// A coordinator registered for the workflow keeps the finished decision from
				// resuming it, so the step is read as the decision left it.
				t.Cleanup(d.enterCoordinator(left.parent.ID))
			}
			got := claimFinish{}
			if c, err := claimOf(in.get(t, left.row.ID)); err == nil {
				got.Checked = d.checkClaim(left.row.ID, c) == nil
			}
			d.sweepApprovalSteps()
			stored := in.get(t, left.row.ID)
			got.Status = stored.Status
			if stored.SpecSeal != "" {
				if left.parent == nil {
					got.Sealed = d.binder.Executable(ctx, stored) == nil
				} else {
					got.Sealed = d.checkStepBinding(ctx, in.get(t, left.parent.ID), stored) == nil
				}
			}
			paths := map[string]bool{}
			for _, verdict := range []string{outcome.StepApproved, outcome.StepRejected,
				outcome.StepTimedOut} {
				if left.parent == nil {
					paths[runPath(left.row.ID, verdict)] = true
					continue
				}
				paths[outcome.StepDecisionPath(left.parent.ID, left.row.ID, verdict)] = true
			}
			chain, err := in.chain.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			for _, e := range chain {
				got.Recorded += boolCount(e.Method == audit.MethodDecision && paths[e.Path])
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("finish mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// boolCount returns 1 for true and 0 for false.
func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

// movingRows is a run store on which another process acts on a row in the moment between
// adoption's read of it and adoption's write to it.
type movingRows struct {
	// Store is the run store the rows live in.
	run.Store
	// mu guards moves.
	mu sync.Mutex
	// moves maps a run id to what the other process does to it, done once.
	moves map[string]func()
}

// before has move done to the row id just before adoption writes to it.
func (s *movingRows) before(id string, move func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moves[id] = move
}

// SealAdopted lets the other process act on the row first, once, and then writes.
func (s *movingRows) SealAdopted(ctx context.Context, id string, a run.Adoption) (bool, error) {
	s.mu.Lock()
	move := s.moves[id]
	delete(s.moves, id)
	s.mu.Unlock()
	if move != nil {
		move()
	}
	return s.Store.SealAdopted(ctx, id, a)
}

// movedResult is what adoption did to a row another process may have acted on meanwhile.
type movedResult struct {
	// Moved reports whether adoption reported the row as moved.
	Moved bool
	// Sealed reports whether adoption reported the row as sealed.
	Sealed bool
	// Kept reports whether the row reads back exactly as the other process left it.
	Kept bool
}

// TestAdoptLeavesARowThatMoved pins that a row another process seals, decides, or settles between
// adoption's read and its write is left as that process left it and reported as moved, never
// overwritten with what adoption computed from the row it read. Every other row of the same
// adoption is still sealed.
//
//nolint:funlen // Test function.
func TestAdoptLeavesARowThatMoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Name labels what the other process does.
		Name string
		// Leave builds the row on the binary before the upgrade and returns it with what the other
		// process does to it, nil for nothing.
		Leave func(t *testing.T, in *adoptInstall, s *movingRows) (*run.Run, func())
		// WantResult is what adoption did to the row.
		WantResult movedResult
	}{{ // Test 0: A held run another process seals meanwhile.
		Name: "sealed",
		Leave: func(t *testing.T, in *adoptInstall, s *movingRows) (*run.Run, func()) {
			r := in.held(t)
			return r, func() {
				if err := s.StampApprovedSpec(ctx, r.ID, "", "", "bk1:another"); err != nil {
					t.Errorf("StampApprovedSpec() error = %v", err)
				}
			}
		},
		WantResult: movedResult{Moved: true, Kept: true},
	}, { // Test 1: A claimed approval the process that made it records and settles meanwhile.
		Name: "decided",
		Leave: func(t *testing.T, in *adoptInstall, _ *movingRows) (*run.Run, func()) {
			r := in.held(t, run.WithQueue("parked"))
			in.stuck(t, func() error {
				_, err := in.old.Approve(ctx, r.ID, bndDecider())
				return err
			})
			return r, func() { in.old.sweepApprovalSteps() }
		},
		WantResult: movedResult{Moved: true, Kept: true},
	}, { // Test 2: A finished step whose result another process seals meanwhile.
		Name: "settled",
		Leave: func(t *testing.T, in *adoptInstall, s *movingRows) (*run.Run, func()) {
			parent, _ := in.parked(t)
			build := in.stepOf(t, parent.ID, "build")
			return build, func() {
				r, err := s.Get(ctx, build.ID)
				if err != nil {
					t.Errorf("Get() error = %v", err)
					return
				}
				r.ResultSeal = "bk1:another"
				if err := s.Save(ctx, r); err != nil {
					t.Errorf("Save() error = %v", err)
				}
			}
		},
		WantResult: movedResult{Moved: true, Kept: true},
	}, { // Test 3: A held run nothing touches is sealed.
		Name: "untouched",
		Leave: func(t *testing.T, in *adoptInstall, _ *movingRows) (*run.Run, func()) {
			return in.held(t), nil
		},
		WantResult: movedResult{Sealed: true},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			s := &movingRows{Store: run.NewMemStore(), moves: map[string]func(){}}
			in := newAdoptInstall(t, s)
			row, move := test.Leave(t, in, s)
			var left *run.Run
			if move != nil {
				s.before(row.ID, func() {
					move()
					left = in.get(t, row.ID)
				})
			}
			res, err := Adopt(ctx, in.deps())
			if err != nil {
				t.Fatalf("Adopt() error = %v", err)
			}
			got := movedResult{Moved: slices.Contains(res.Moved, row.ID),
				Sealed: slices.Contains(res.Sealed, row.ID)}
			final := in.get(t, row.ID)
			if left != nil {
				got.Kept = cmp.Diff(left, final, cmpopts.EquateEmpty()) == ""
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("moved row mismatch (-want +got):\n%s", diff)
			}
			if len(res.Sealed)+len(res.Moved) != len(res.Work.Rows) || len(res.Left) != 0 {
				t.Errorf("Adopt() sealed %v and moved %v of %d rows, left %v", res.Sealed,
					res.Moved, len(res.Work.Rows), res.Left)
			}
		})
	}
}

// onceResult is what a sequence of adoptions left on an install.
type onceResult struct {
	// Appended counts the calls that appended the adoption record.
	Appended int
	// Earlier counts the calls that found the record already on the chain.
	Earlier int
	// Records counts adoption records on the chain.
	Records int
	// Unsealed counts the runs under test that carry no seal afterward.
	Unsealed int
	// Grew counts the chain entries the last call appended.
	Grew int
	// Commits reports whether the record is a CLI entry at the adoption path, naming the adopting
	// actor, whose content digest commits the body listing what was sealed.
	Commits bool
}

// adoptRecorded inspects what an install's adoptions left on its chain.
type adoptRecorded struct {
	// in is the install.
	in *adoptInstall
	// res is the result being built.
	res onceResult
}

// count tallies the results of adoption calls that returned no error.
func (a *adoptRecorded) count(results ...*Adopted) {
	for _, r := range results {
		if r == nil {
			continue
		}
		if r.Earlier {
			a.res.Earlier++
		} else {
			a.res.Appended++
		}
	}
}

// finish fills in what the chain and the runs hold, the record being expected to commit body.
func (a *adoptRecorded) finish(t *testing.T, body string, runs ...*run.Run) onceResult {
	t.Helper()
	chain, err := a.in.chain.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		if e.ID != a.in.key.AdoptionID() {
			continue
		}
		a.res.Records++
		a.res.Commits = e.Method == audit.MethodCLI && e.Path == audit.AdoptedPath &&
			e.Actor == adoptActor().Name && e.ActorType == adoptActor().Type &&
			audit.VerifyContentDigest(e.ContentDigest, e.Nonce, []byte(body))
	}
	for _, r := range runs {
		a.res.Unsealed += boolCount(a.in.get(t, r.ID).SpecSeal == "")
	}
	return a.res
}

// chainLen returns how many entries the install's chain holds.
func (in *adoptInstall) chainLen(t *testing.T) int {
	t.Helper()
	chain, err := in.chain.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	return len(chain)
}

// TestAdoptionIsRecordedOnce pins that an install is adopted once. The record goes on the chain
// before any row is sealed, under the id the key fixes, so a second adoption finds it and writes
// nothing, adoptions racing each other append one record between them and only its appender
// writes, and an install with nothing to adopt still records that adoption ran. Rows that already
// carry a seal on a chain with no record are refused, with nothing written.
//
//nolint:funlen // Test function.
func TestAdoptionIsRecordedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const empty = `{"runs":[],"claims":[],"releases":[]}`
	tests := []struct {
		// Run adopts the install under test and returns what it left and the first error.
		Run func(t *testing.T, in *adoptInstall) (onceResult, error)
		// WantResult is what the adoptions left.
		WantResult onceResult
		// Want is the error an adoption returned.
		Want error
	}{{ // Test 0: A second adoption finds the record and writes nothing.
		Run: func(t *testing.T, in *adoptInstall) (onceResult, error) {
			a := &adoptRecorded{in: in}
			first := in.held(t)
			res, err := Adopt(ctx, in.deps())
			if err != nil {
				return onceResult{}, err
			}
			later := in.held(t)
			before := in.chainLen(t)
			again, err := Adopt(ctx, in.deps())
			if err != nil {
				return onceResult{}, err
			}
			a.count(res, again)
			a.res.Grew = in.chainLen(t) - before
			return a.finish(t, `{"runs":["`+first.ID+`"],"claims":[],"releases":[]}`, first,
				later), nil
		},
		WantResult: onceResult{Appended: 1, Earlier: 1, Records: 1, Unsealed: 1, Commits: true},
	}, { // Test 1: Sealed rows on a chain with no record are refused, with nothing written.
		Run: func(t *testing.T, in *adoptInstall) (onceResult, error) {
			a := &adoptRecorded{in: in}
			if _, err := in.keyed(t).Submit(ctx, "site.yml", "inv",
				run.WithRequireApproval(true)); err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			unsealed := in.held(t)
			before := in.chainLen(t)
			_, err := Adopt(ctx, in.deps())
			a.res.Grew = in.chainLen(t) - before
			return a.finish(t, "", unsealed), err
		},
		WantResult: onceResult{Unsealed: 1},
		Want:       binding.ErrDrift,
	}, { // Test 2: Adoptions racing each other append one record, and every row is sealed once.
		Run: func(t *testing.T, in *adoptInstall) (onceResult, error) {
			a := &adoptRecorded{in: in}
			runs := []*run.Run{in.held(t), in.held(t), in.held(t)}
			results := make([]*Adopted, 8)
			errs := make([]error, len(results))
			var wg sync.WaitGroup
			for i := range results {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i], errs[i] = Adopt(ctx, in.deps())
				}()
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				return onceResult{}, err
			}
			a.count(results...)
			ids := []string{runs[0].ID, runs[1].ID, runs[2].ID}
			slices.Sort(ids)
			return a.finish(t, `{"runs":["`+ids[0]+`","`+ids[1]+`","`+ids[2]+
				`"],"claims":[],"releases":[]}`, runs...), nil
		},
		WantResult: onceResult{Appended: 1, Earlier: 7, Records: 1, Commits: true},
	}, { // Test 3: An install with nothing to adopt records that adoption ran.
		Run: func(t *testing.T, in *adoptInstall) (onceResult, error) {
			a := &adoptRecorded{in: in}
			res, err := Adopt(ctx, in.deps())
			if err != nil {
				return onceResult{}, err
			}
			a.count(res)
			return a.finish(t, empty), nil
		},
		WantResult: onceResult{Appended: 1, Records: 1, Commits: true},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			in := newAdoptInstall(t, run.NewMemStore())
			got, err := test.Run(t, in)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Adopt() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("adoptions mismatch (-want +got):\n%s", diff)
			}
			recorded, err := Recorded(ctx, in.deps())
			if err != nil {
				t.Fatalf("Recorded() error = %v", err)
			}
			if recorded != (test.WantResult.Records == 1) {
				t.Errorf("Recorded() = %t, want %t", recorded, test.WantResult.Records == 1)
			}
		})
	}
}

// TestRecordEmptyTrustsNoRow pins the empty adoption a keyed start records when it finds nothing to
// adopt. It appends a record listing no row on an install with nothing to adopt, leaves a record
// already there as it is, and writes nothing on an install with rows waiting for adopt or with
// sealed rows and no record, which only a removed record leaves.
func TestRecordEmptyTrustsNoRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Setup leaves the install as RecordEmpty finds it.
		Setup func(t *testing.T, store run.Store, deps AdoptionDeps)
		// Want is the error RecordEmpty must return, nil when it records.
		Want error
		// WantRecorded reports whether the chain must hold the record afterward.
		WantRecorded bool
	}{{ // Test 0: Nothing to adopt is recorded.
		Setup: func(*testing.T, run.Store, AdoptionDeps) {}, WantRecorded: true,
	}, { // Test 1: A record already there is left as it is.
		Setup: func(t *testing.T, _ run.Store, deps AdoptionDeps) {
			t.Helper()
			if err := RecordEmpty(ctx, deps); err != nil {
				t.Fatalf("RecordEmpty() error = %v", err)
			}
		},
		WantRecorded: true,
	}, { // Test 2: A row an earlier release left waits for adopt.
		Setup: func(t *testing.T, store run.Store, _ AdoptionDeps) {
			t.Helper()
			r := &run.Run{ID: run.NewID(), Tool: run.ToolBash, Command: "deploy",
				Status: run.StatusPending, CreatedAt: time.Now()}
			if err := store.Save(ctx, r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		},
		Want: ErrAdoptionPending,
	}, { // Test 3: A sealed row with no record means the record was removed.
		Setup: func(t *testing.T, store run.Store, _ AdoptionDeps) {
			t.Helper()
			r := &run.Run{ID: run.NewID(), Tool: run.ToolBash, Command: "deploy",
				Status: run.StatusPending, CreatedAt: time.Now(), SpecSeal: binding.SealPrefix + "00"}
			if err := store.Save(ctx, r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		},
		Want: binding.ErrDrift,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			deps := AdoptionDeps{Runs: store, Audits: audit.NewMemStore(), Key: bndKey(t)}
			test.Setup(t, store, deps)
			before, err := deps.Audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			err = RecordEmpty(ctx, deps)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Fatalf("RecordEmpty() error = %v, want %v", err, test.Want)
			}
			recorded, err := Recorded(ctx, deps)
			if err != nil || recorded != test.WantRecorded {
				t.Errorf("Recorded() = %t, %v, want %t", recorded, err, test.WantRecorded)
			}
			after, err := deps.Audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			if wantGrew := testNum == 0; (len(after) != len(before)) != wantGrew {
				t.Errorf("the chain went from %d to %d entries, want it to grow only when "+
					"nothing was recorded before", len(before), len(after))
			}
		})
	}
}
