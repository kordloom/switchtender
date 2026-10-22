package binding

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// errTestStore is the error a failing test store returns.
var errTestStore = errors.New("database is locked")

// testRuns is a run store a test writes as a database writer would, with no rule about which
// fields a save may touch. Only Get and Steps are implemented.
type testRuns struct {
	// Store is nil and present only to satisfy run.Store.
	run.Store
	// mu guards every field below.
	mu sync.Mutex
	// runs holds each row by id.
	runs map[string]*run.Run
	// failGets is how many of the next reads fail with errTestStore.
	failGets int
	// gets counts reads.
	gets int
}

// Get returns a copy of the row id.
func (s *testRuns) Get(_ context.Context, id string) (*run.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.failGets > 0 {
		s.failGets--
		return nil, errTestStore
	}
	r, ok := s.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r.Clone(), nil
}

// Steps returns a copy of every row whose parent is parentID.
func (s *testRuns) Steps(_ context.Context, parentID string) ([]*run.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*run.Run
	for _, r := range s.runs {
		if r.ParentID != nil && *r.ParentID == parentID {
			out = append(out, r.Clone())
		}
	}
	return out, nil
}

// testChain is a chain a test writes as a database writer would, read by id.
type testChain struct {
	// mu guards every field below.
	mu sync.Mutex
	// entries holds each entry by id.
	entries map[string]*audit.Entry
	// failReads is how many of the next reads fail with errTestStore.
	failReads int
	// reads counts reads.
	reads int
}

// entry reads one entry by id, nil when the chain holds none.
func (c *testChain) entry(_ context.Context, id string) (*audit.Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	if c.failReads > 0 {
		c.failReads--
		return nil, errTestStore
	}
	e, ok := c.entries[id]
	if !ok {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

// testFixture is one install's rows, chain, and seals: a submitted run, two approved runs, an
// approved split with one shard, an approved workflow with a finished step, a decided approval
// step, and a step that approval released, a run held for a decision, and an approved workflow
// waiting at an approval step nobody has decided.
type testFixture struct {
	// Key is the key every seal in the fixture was made under.
	Key *Key
	// Runs holds the rows.
	Runs *testRuns
	// Chain holds the entries.
	Chain *testChain
	// mu guards the three maps below.
	mu sync.Mutex
	// specSeals holds each row's spec seal by id.
	specSeals map[string]string
	// resultSeals holds each finished row's result seal by id.
	resultSeals map[string]string
	// roots holds each step's root entry ids by id.
	roots map[string][]string
}

// newTestFixture builds and seals the fixture.
func newTestFixture(t *testing.T) *testFixture {
	t.Helper()
	f := &testFixture{
		Key:         testKey(t, 7),
		Runs:        &testRuns{runs: map[string]*run.Run{}},
		Chain:       &testChain{entries: map[string]*audit.Entry{}},
		specSeals:   map[string]string{},
		resultSeals: map[string]string{},
		roots:       map[string][]string{},
	}
	policy := &run.PolicySet{Digest: "pd", Count: 1, Rules: []string{"prod changes"}}
	f.put(&run.Run{ID: "run_sub", Playbook: "site.yml", Limit: "web1", PolicySet: policy})
	f.put(&run.Run{ID: "run_app", Playbook: "site.yml", HeldByPolicy: "prod changes",
		RequireDistinctApprover: true, PolicySet: policy, DecisionID: "aud_app"})
	f.put(&run.Run{ID: "run_other", Playbook: "other.yml", HeldByPolicy: "prod changes",
		DecisionID: "aud_other"})
	f.put(&run.Run{ID: "run_split", Kind: run.KindSplit, Playbook: "site.yml",
		ShardCount: new(1), HeldByPolicy: "prod changes", DecisionID: "aud_split"})
	f.put(&run.Run{ID: "run_shard", Playbook: "site.yml", ParentID: new("run_split"),
		ShardIndex: new(0), ShardCount: new(1), Limit: "web1", HeldByPolicy: "prod changes"})
	f.put(&run.Run{ID: "run_wf", Kind: run.KindPipeline, HeldByPolicy: "prod changes",
		DecisionID: "aud_wf", Steps: []run.PipelineStep{
			{Name: "build", Playbook: "build.yml"},
			{Name: "gate", Type: run.StepApproval, DependsOn: []string{"build"}},
			{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"gate"}},
		}})
	f.put(&run.Run{ID: "run_build", Playbook: "build.yml", ParentID: new("run_wf"),
		StepIndex: new(0), StepName: "build", Status: run.StatusSucceeded,
		Outputs: map[string]any{"version": "1.2"}})
	f.put(&run.Run{ID: "run_gate", Kind: run.KindApproval, ParentID: new("run_wf"),
		StepIndex: new(1), StepName: "gate", Status: run.StatusSucceeded,
		HeldByPolicy: `approval step "gate"`, DecisionID: "aud_gate"})
	f.put(&run.Run{ID: "run_deploy", Playbook: "deploy.yml", ParentID: new("run_wf"),
		StepIndex: new(2), StepName: "deploy", Status: run.StatusPending,
		ExtraVars: map[string]any{"version": "1.2"}})
	f.put(&run.Run{ID: "run_held", Playbook: "site.yml", Status: run.StatusPendingApproval,
		Actor: "requester", HeldByPolicy: "prod changes", RequireDistinctApprover: true,
		PolicySet: policy})
	f.put(&run.Run{ID: "run_park", Kind: run.KindPipeline, Status: run.StatusPendingApproval,
		HeldByPolicy: "prod changes", RequireDistinctApprover: true, DecisionID: "aud_park",
		Steps: []run.PipelineStep{
			{Name: "hold", Type: run.StepApproval},
			{Name: "ship", Playbook: "ship.yml", DependsOn: []string{"hold"}},
		}})
	f.put(&run.Run{ID: "run_wait", Kind: run.KindApproval, ParentID: new("run_park"),
		StepIndex: new(0), StepName: "hold", Status: run.StatusPendingApproval,
		HeldByPolicy: `approval step "hold"`, RequireDistinctApprover: true})
	f.addEntry("aud_app", audit.MethodDecision, "/runs/run_app/decision/approved")
	f.addEntry("aud_other", audit.MethodDecision, "/runs/run_other/decision/approved")
	f.addEntry("aud_split", audit.MethodDecision, "/runs/run_split/decision/approved")
	f.addEntry("aud_wf", audit.MethodDecision, "/runs/run_wf/decision/approved")
	f.addEntry("aud_gate", audit.MethodDecision,
		outcome.StepDecisionPath("run_wf", "run_gate", outcome.StepApproved))
	f.addEntry("aud_rel", "RELEASE", "/runs/run_wf/steps/run_deploy/release")
	f.addEntry("aud_park", audit.MethodDecision, "/runs/run_park/decision/approved")
	f.roots["run_build"] = []string{"aud_wf"}
	f.roots["run_deploy"] = []string{"aud_wf", "aud_gate", "aud_rel"}
	for _, id := range []string{"run_sub", "run_app", "run_other", "run_split", "run_shard",
		"run_wf", "run_build", "run_gate", "run_deploy", "run_held", "run_park", "run_wait"} {
		f.mu.Lock()
		f.specSeals[id] = f.sealOf(t, f.Key, f.row(t, id))
		f.mu.Unlock()
	}
	rec, err := ResultRecord(f.row(t, "run_build"))
	if err != nil {
		t.Fatalf("result record: %v", err)
	}
	f.resultSeals["run_build"] = f.Key.Seal(rec)
	return f
}

// put stores r.
func (f *testFixture) put(r *run.Run) {
	f.Runs.mu.Lock()
	defer f.Runs.mu.Unlock()
	f.Runs.runs[r.ID] = r
}

// row returns a copy of the stored row id.
func (f *testFixture) row(t *testing.T, id string) *run.Run {
	t.Helper()
	r, err := f.Runs.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return r
}

// edit rewrites the stored row id as a database writer would.
func (f *testFixture) edit(t *testing.T, id string, change func(r *run.Run)) {
	t.Helper()
	r := f.row(t, id)
	change(r)
	f.put(r)
}

// addEntry appends an entry with a content digest derived from its id, recorded by an approver.
func (f *testFixture) addEntry(id, method, path string) {
	f.Chain.mu.Lock()
	defer f.Chain.mu.Unlock()
	f.Chain.entries[id] = &audit.Entry{ID: id, Method: method, Path: path,
		ContentDigest: "cd_" + id, Actor: "approver", ActorType: "session",
		OnBehalfOf: "approver"}
}

// editEntry rewrites the entry id as a database writer would.
func (f *testFixture) editEntry(id string, change func(e *audit.Entry)) {
	f.Chain.mu.Lock()
	defer f.Chain.mu.Unlock()
	change(f.Chain.entries[id])
}

// sealOf returns the spec seal the dispatcher would make for r under k, built from the stored
// chain the way the dispatcher builds it at the moment it seals. An approval step nobody decided
// carries the seal it was created with, a step's with no roots.
func (f *testFixture) sealOf(t *testing.T, k *Key, r *run.Run) string {
	t.Helper()
	ctx := context.Background()
	rootOf := func(id string) Root {
		e, err := f.Chain.entry(ctx, id)
		if err != nil || e == nil {
			t.Fatalf("entry %s: %v", id, err)
		}
		return RootOf(e)
	}
	var rec Record
	var err error
	switch {
	case r.ParentID == nil && r.DecisionID == "":
		rec, err = SubmitRecord(r)
	case r.ParentID == nil:
		rec, err = RunRecord(r, rootOf(r.DecisionID))
	case r.Kind == run.KindApproval && r.DecisionID != "":
		parent := f.row(t, *r.ParentID)
		state, serr := outcome.StepStateOf(ctx, f.Runs, parent, r)
		if serr != nil {
			t.Fatalf("state of %s: %v", r.ID, serr)
		}
		b, berr := state.Binding()
		if berr != nil {
			t.Fatalf("bind state of %s: %v", r.ID, berr)
		}
		rec, err = GateRecord(parent, r, b, rootOf(r.DecisionID))
	case r.StepIndex != nil:
		var roots []Root
		for _, id := range f.roots[r.ID] {
			roots = append(roots, rootOf(id))
		}
		rec, err = StepRecord(f.row(t, *r.ParentID), r, roots)
	default:
		rec, err = ShardRecord(f.row(t, *r.ParentID), r)
	}
	if err != nil {
		t.Fatalf("record of %s: %v", r.ID, err)
	}
	return k.Seal(rec)
}

// setSpecSeal stores seal as row id's spec seal.
func (f *testFixture) setSpecSeal(id, seal string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specSeals[id] = seal
}

// setRoots stores ids as the root ids the dispatcher names for step id.
func (f *testFixture) setRoots(id string, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roots[id] = ids
}

// verifier returns a Verifier over the fixture under k.
func (f *testFixture) verifier(k *Key) *Verifier {
	return &Verifier{
		Runs: f.Runs, Key: k,
		SpecSeal: func(r *run.Run) string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.specSeals[r.ID]
		},
		ResultSeal: func(r *run.Run) string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.resultSeals[r.ID]
		},
		Entry: f.Chain.entry,
		StepRoots: func(_ context.Context, _, child *run.Run) ([]string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]string(nil), f.roots[child.ID]...), nil
		},
	}
}

// gate runs Gate on the stored approval step, rebuilding the workflow state from the store.
func (f *testFixture) gate(t *testing.T, v *Verifier, parentID string) error {
	t.Helper()
	parent, node := f.row(t, parentID), f.row(t, "run_gate")
	state, err := outcome.StepStateOf(context.Background(), f.Runs, f.row(t, "run_wf"), node)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	return v.Gate(context.Background(), parent, node, state)
}

// requireDrift fails t unless err is a refusal, and only a refusal, whose text carries want.
func requireDrift(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, ErrDrift) || errors.Is(err, ErrUnverified) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if !strings.HasPrefix(err.Error(), "refused: ") || !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to start with \"refused: \" and carry %q", err, want)
	}
}

// requireUnverified fails t unless err is a could-not-verify carrying the store error, with no
// wording that claims the row was changed.
func requireUnverified(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrUnverified) || errors.Is(err, ErrDrift) {
		t.Fatalf("error = %v, want could not verify", err)
	}
	for _, tamper := range []string{"refused", "changed after"} {
		if strings.Contains(err.Error(), tamper) {
			t.Errorf("error %q claims tampering with %q", err, tamper)
		}
	}
}

// TestExecutableAcceptsHonestRows pins that every row the dispatcher sealed verifies, under the key
// and with no key.
func TestExecutableAcceptsHonestRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ID    string
		Keyed bool
	}{
		{ID: "run_sub", Keyed: true},     // Test 0: A submitted run.
		{ID: "run_app", Keyed: true},     // Test 1: An approved run.
		{ID: "run_shard", Keyed: true},   // Test 2: A shard of an approved split.
		{ID: "run_build", Keyed: true},   // Test 3: A step of an approved workflow.
		{ID: "run_deploy", Keyed: true},  // Test 4: A step an approval released.
		{ID: "run_sub", Keyed: false},    // Test 5: A submitted run, no key.
		{ID: "run_app", Keyed: false},    // Test 6: An approved run, no key.
		{ID: "run_shard", Keyed: false},  // Test 7: A shard, no key.
		{ID: "run_deploy", Keyed: false}, // Test 8: A released step, no key.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			k := f.Key
			if !test.Keyed {
				k = nil
			}
			err := f.verifier(k).Executable(context.Background(), f.row(t, test.ID))
			if err != nil {
				t.Errorf("Executable(%s) = %v, want nil", test.ID, err)
			}
		})
	}
}

// TestExecutableRefusesDrift performs a database writer's move against each kind of executable
// row and requires a refusal naming what moved.
func TestExecutableRefusesDrift(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	tests := []struct {
		Attack   func(t *testing.T, f *testFixture)
		ID       string
		WantText string
	}{{ // Test 0: A submitted run's spec rewritten.
		ID: "run_sub", WantText: "changed after it was submitted",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_sub", func(r *run.Run) { r.Limit = "all" })
		},
	}, { // Test 1: A submitted run's approval request flipped.
		ID: "run_sub", WantText: "changed after it was submitted",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_sub", func(r *run.Run) { r.ApprovalRequested = true })
		},
	}, { // Test 2: A submitted run's rule set rewritten.
		ID: "run_sub", WantText: "changed after it was submitted",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_sub", func(r *run.Run) { r.PolicySet.Rules = nil })
		},
	}, { // Test 3: A decision forged onto an unplanned run by naming another run's decision.
		ID: "run_sub", WantText: "aud_app is not the decision this row names",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_sub", func(r *run.Run) { r.DecisionID = "aud_app" })
		},
	}, { // Test 4: A decision forged onto an unplanned run with a forged entry at its path.
		ID: "run_sub", WantText: "changed after it was approved",
		Attack: func(t *testing.T, f *testFixture) {
			f.addEntry("aud_forged", audit.MethodDecision, "/runs/run_sub/decision/approved")
			f.edit(t, "run_sub", func(r *run.Run) { r.DecisionID = "aud_forged" })
		},
	}, { // Test 5: An approved run's spec rewritten with both stamps recomputed.
		ID: "run_app", WantText: "changed after it was approved",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_app", func(r *run.Run) {
				r.Tags = []string{"destroy"}
				r.ApprovedSpecBinding, _ = outcome.SpecBinding(r)
				r.ApprovedSpecDigest, _ = outcome.SpecDigest(r)
			})
		},
	}, { // Test 6: An approved run's separation-of-duties flag cleared.
		ID: "run_app", WantText: "changed after it was approved",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_app", func(r *run.Run) { r.RequireDistinctApprover = false })
		},
	}, { // Test 7: An approved run's decision swapped for another run's.
		ID: "run_app", WantText: "aud_other is not the decision this row names",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_app", func(r *run.Run) { r.DecisionID = "aud_other" })
		},
	}, { // Test 8: Another run's seal copied onto an approved run.
		ID: "run_app", WantText: "changed after it was approved",
		Attack: func(t *testing.T, f *testFixture) {
			f.mu.Lock()
			other := f.specSeals["run_other"]
			f.mu.Unlock()
			f.setSpecSeal("run_app", other)
		},
	}, { // Test 9: An approved run's decision entry deleted.
		ID: "run_app", WantText: "aud_app it derives from is not on the chain",
		Attack: func(_ *testing.T, f *testFixture) {
			f.Chain.mu.Lock()
			delete(f.Chain.entries, "aud_app")
			f.Chain.mu.Unlock()
		},
	}, { // Test 10: An approved run's decision entry rewritten.
		ID: "run_app", WantText: "changed after it was approved",
		Attack: func(_ *testing.T, f *testFixture) {
			f.editEntry("aud_app", func(e *audit.Entry) { e.ContentDigest = "cd_rewritten" })
		},
	}, { // Test 11: An approved run's decision entry turned into a rejection.
		ID: "run_app", WantText: "aud_app is not the decision this row names",
		Attack: func(_ *testing.T, f *testFixture) {
			f.editEntry("aud_app", func(e *audit.Entry) {
				e.Path = "/runs/run_app/decision/rejected"
			})
		},
	}, { // Test 12: An approved run's seal cleared.
		ID: "run_app", WantText: "carries no seal",
		Attack: func(_ *testing.T, f *testFixture) { f.setSpecSeal("run_app", "") },
	}, { // Test 13: An approved run's seal under a wrong prefix.
		ID: "run_app", WantText: "changed after it was approved",
		Attack: func(_ *testing.T, f *testFixture) {
			f.mu.Lock()
			own := f.specSeals["run_app"]
			f.mu.Unlock()
			f.setSpecSeal("run_app", "bk2:"+strings.TrimPrefix(own, SealPrefix))
		},
	}, { // Test 14: An approved run sealed under another key.
		ID: "run_app", WantText: "changed after it was approved",
		Attack: func(t *testing.T, f *testFixture) {
			f.setSpecSeal("run_app", f.sealOf(t, testKey(t, 8), f.row(t, "run_app")))
		},
	}, { // Test 15: A shard's spec rewritten.
		ID: "run_shard", WantText: "changed after the split was built",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_shard", func(r *run.Run) { r.Limit = "all" })
		},
	}, { // Test 16: A shard's index moved.
		ID: "run_shard", WantText: "changed after the split was built",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_shard", func(r *run.Run) { r.ShardIndex = new(1) })
		},
	}, { // Test 17: A shard's split rewritten.
		ID: "run_shard", WantText: "parent run run_split: its spec",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_split", func(r *run.Run) { r.Playbook = "destroy.yml" })
		},
	}, { // Test 18: A shard pointed at a split that is not in the store.
		ID: "run_shard", WantText: "its parent run_gone is not in the store",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_shard", func(r *run.Run) { r.ParentID = new("run_gone") })
		},
	}, { // Test 19: A shard pointed at another approved parent.
		ID: "run_shard", WantText: "changed after the split was built",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_shard", func(r *run.Run) { r.ParentID = new("run_app") })
		},
	}, { // Test 20: A shard pointed at a parent that is itself a child.
		ID: "run_shard", WantText: "its parent run_build is itself a child",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_shard", func(r *run.Run) { r.ParentID = new("run_build") })
		},
	}, { // Test 21: A step's inputs rewritten.
		ID: "run_deploy", WantText: "changed after it was built",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_deploy", func(r *run.Run) { r.ExtraVars["version"] = "6.6" })
		},
	}, { // Test 22: A step's attempt moved.
		ID: "run_deploy", WantText: "changed after it was built",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_deploy", func(r *run.Run) { r.Attempt = 1 })
		},
	}, { // Test 23: A root digest that differs: the release entry rewritten.
		ID: "run_deploy", WantText: "changed after it was built",
		Attack: func(_ *testing.T, f *testFixture) {
			f.editEntry("aud_rel", func(e *audit.Entry) { e.ContentDigest = "cd_rewritten" })
		},
	}, { // Test 24: A step's release entry deleted.
		ID: "run_deploy", WantText: "aud_rel it derives from is not on the chain",
		Attack: func(_ *testing.T, f *testFixture) {
			f.Chain.mu.Lock()
			delete(f.Chain.entries, "aud_rel")
			f.Chain.mu.Unlock()
		},
	}, { // Test 25: A step named with one root fewer than it was sealed with.
		ID: "run_deploy", WantText: "changed after it was built",
		Attack: func(_ *testing.T, f *testFixture) {
			f.setRoots("run_deploy", "aud_wf", "aud_rel")
		},
	}, { // Test 26: A step's workflow decision deleted.
		ID: "run_deploy", WantText: "parent run run_wf: chain entry aud_wf",
		Attack: func(_ *testing.T, f *testFixture) {
			f.Chain.mu.Lock()
			delete(f.Chain.entries, "aud_wf")
			f.Chain.mu.Unlock()
		},
	}, { // Test 27: An approval step presented for execution.
		ID: "run_gate", WantText: "never executes",
		Attack: func(*testing.T, *testFixture) {},
	}, { // Test 28: An approval step flipped into an executable step.
		ID: "run_gate", WantText: "changed after it was built",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_gate", func(r *run.Run) { r.Kind = ""; r.Playbook = "destroy.yml" })
		},
	}, { // Test 29: An approved run's decision actor rewritten, its digest left alone.
		ID: "run_app", WantText: "changed after it was approved",
		Attack: func(_ *testing.T, f *testFixture) {
			f.editEntry("aud_app", func(e *audit.Entry) { e.Actor, e.ActorType = "mallory", "human" })
		},
	}, { // Test 30: A step's release entry actor rewritten.
		ID: "run_deploy", WantText: "changed after it was built",
		Attack: func(_ *testing.T, f *testFixture) {
			f.editEntry("aud_rel", func(e *audit.Entry) { e.Actor = "mallory" })
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			test.Attack(t, f)
			err := f.verifier(f.Key).Executable(context.Background(), f.row(t, test.ID))
			requireDrift(t, err, test.WantText)
		})
	}
}

// TestGateAcceptsAnHonestApproval pins that a decided approval step verifies on resume.
func TestGateAcceptsAnHonestApproval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Keyed bool
	}{
		{Keyed: true},  // Test 0: Keyed.
		{Keyed: false}, // Test 1: No key.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			k := f.Key
			if !test.Keyed {
				k = nil
			}
			if err := f.gate(t, f.verifier(k), "run_wf"); err != nil {
				t.Errorf("Gate = %v, want nil", err)
			}
		})
	}
}

// TestGateRefusesDrift performs a database writer's move against a decided approval step and
// requires a refusal naming what moved.
func TestGateRefusesDrift(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Attack   func(t *testing.T, f *testFixture)
		ParentID string
		WantText string
	}{{ // Test 0: An upstream step's outputs rewritten.
		ParentID: "run_wf", WantText: "workflow state or the decision changed",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_build", func(r *run.Run) { r.Outputs["version"] = "6.6" })
		},
	}, { // Test 1: The step's decision entry turned into a rejection.
		ParentID: "run_wf", WantText: "aud_gate is not the decision this row names",
		Attack: func(_ *testing.T, f *testFixture) {
			f.editEntry("aud_gate", func(e *audit.Entry) {
				e.Path = outcome.StepDecisionPath("run_wf", "run_gate", outcome.StepRejected)
			})
		},
	}, { // Test 2: The step's decision cleared.
		ParentID: "run_wf", WantText: "carries no decision",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_gate", func(r *run.Run) { r.DecisionID = "" })
		},
	}, { // Test 3: The step's decision entry rewritten.
		ParentID: "run_wf", WantText: "workflow state or the decision changed",
		Attack: func(_ *testing.T, f *testFixture) {
			f.editEntry("aud_gate", func(e *audit.Entry) { e.ContentDigest = "cd_rewritten" })
		},
	}, { // Test 4: The workflow's graph rewritten.
		ParentID: "run_wf", WantText: "workflow run run_wf: its spec",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_wf", func(r *run.Run) { r.Steps[2].Playbook = "destroy.yml" })
		},
	}, { // Test 5: The step's seal cleared.
		ParentID: "run_wf", WantText: "carries no seal",
		Attack: func(_ *testing.T, f *testFixture) { f.setSpecSeal("run_gate", "") },
	}, { // Test 6: The step presented under another workflow.
		ParentID: "run_app", WantText: "it is not a step of workflow run_app",
		Attack: func(*testing.T, *testFixture) {},
	}, { // Test 7: The step's decision swapped for another approved step's.
		ParentID: "run_wf", WantText: "aud_wf is not the decision this row names",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_gate", func(r *run.Run) { r.DecisionID = "aud_wf" })
		},
	}, { // Test 8: The step decision's delegated account rewritten.
		ParentID: "run_wf", WantText: "workflow state or the decision changed",
		Attack: func(_ *testing.T, f *testFixture) {
			f.editEntry("aud_gate", func(e *audit.Entry) { e.OnBehalfOf = "mallory" })
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			test.Attack(t, f)
			requireDrift(t, f.gate(t, f.verifier(f.Key), test.ParentID), test.WantText)
		})
	}
}

// undecided checks the row id before a decision on it: as a held run when parentID is empty, and as
// an approval step of the stored workflow parentID otherwise.
func (f *testFixture) undecided(t *testing.T, v *Verifier, parentID, id string) error {
	t.Helper()
	if parentID == "" {
		return v.Held(context.Background(), f.row(t, id))
	}
	return v.Pending(context.Background(), f.row(t, parentID), f.row(t, id))
}

// TestHeldAndPendingAcceptHonestRows pins that a held run and an approval step waiting for a
// decision verify as the dispatcher created them, under the key and with no key, and that with no
// key neither is held to a seal.
func TestHeldAndPendingAcceptHonestRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Attack   func(t *testing.T, f *testFixture)
		ParentID string
		ID       string
		Keyed    bool
	}{{ // Test 0: A held run.
		ID: "run_held", Keyed: true, Attack: func(*testing.T, *testFixture) {},
	}, { // Test 1: An approval step waiting in an approved workflow.
		ParentID: "run_park", ID: "run_wait", Keyed: true,
		Attack: func(*testing.T, *testFixture) {},
	}, { // Test 2: A held run, no key.
		ID: "run_held", Attack: func(*testing.T, *testFixture) {},
	}, { // Test 3: A waiting approval step, no key.
		ParentID: "run_park", ID: "run_wait", Attack: func(*testing.T, *testFixture) {},
	}, { // Test 4: A held run whose flag was cleared, no key, is left to the dispatcher's checks.
		ID: "run_held",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_held", func(r *run.Run) { r.RequireDistinctApprover = false })
		},
	}, { // Test 5: A waiting approval step with no seal, no key.
		ParentID: "run_park", ID: "run_wait",
		Attack: func(_ *testing.T, f *testFixture) { f.setSpecSeal("run_wait", "") },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			test.Attack(t, f)
			k := f.Key
			if !test.Keyed {
				k = nil
			}
			if err := f.undecided(t, f.verifier(k), test.ParentID, test.ID); err != nil {
				t.Errorf("check of %s = %v, want nil", test.ID, err)
			}
		})
	}
}

// TestHeldAndPendingRefuseDrift performs a database writer's move against a row waiting for a
// decision and requires a refusal naming what moved, so a decision is never made on, or sealed
// over, a row whose approval fields the process did not write.
func TestHeldAndPendingRefuseDrift(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	tests := []struct {
		Attack   func(t *testing.T, f *testFixture)
		ParentID string
		ID       string
		WantText string
		Keyed    bool
	}{{ // Test 0: A held run's separation-of-duties flag cleared.
		ID: "run_held", Keyed: true, WantText: "changed after it was submitted",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_held", func(r *run.Run) { r.RequireDistinctApprover = false })
		},
	}, { // Test 1: The rule that held a run cleared.
		ID: "run_held", Keyed: true, WantText: "changed after it was submitted",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_held", func(r *run.Run) { r.HeldByPolicy = "" })
		},
	}, { // Test 2: A held run's spec rewritten.
		ID: "run_held", Keyed: true, WantText: "changed after it was submitted",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_held", func(r *run.Run) { r.Limit = "all" })
		},
	}, { // Test 3: A held run's seal cleared.
		ID: "run_held", Keyed: true, WantText: "carries no seal",
		Attack: func(_ *testing.T, f *testFixture) { f.setSpecSeal("run_held", "") },
	}, { // Test 4: A held run naming another run's decision.
		ID: "run_held", Keyed: true, WantText: "aud_app is not the decision this row names",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_held", func(r *run.Run) { r.DecisionID = "aud_app" })
		},
	}, { // Test 5: A held run presented with a parent.
		ID: "run_held", Keyed: true, WantText: "a child is decided through its parent",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_held", func(r *run.Run) { r.ParentID = new("run_wf") })
		},
	}, { // Test 6: An approval step presented as a held run.
		ID: "run_wait", Keyed: true, WantText: "decided as a step of its workflow",
		Attack: func(*testing.T, *testFixture) {},
	}, { // Test 7: A waiting step's separation-of-duties flag cleared.
		ParentID: "run_park", ID: "run_wait", Keyed: true, WantText: "changed after it was built",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_wait", func(r *run.Run) { r.RequireDistinctApprover = false })
		},
	}, { // Test 8: The rule a waiting step names cleared.
		ParentID: "run_park", ID: "run_wait", Keyed: true, WantText: "changed after it was built",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_wait", func(r *run.Run) { r.HeldByPolicy = "" })
		},
	}, { // Test 9: A waiting step's seal cleared.
		ParentID: "run_park", ID: "run_wait", Keyed: true, WantText: "carries no seal",
		Attack: func(_ *testing.T, f *testFixture) { f.setSpecSeal("run_wait", "") },
	}, { // Test 10: A decided step's seal copied onto a waiting step.
		ParentID: "run_park", ID: "run_wait", Keyed: true, WantText: "changed after it was built",
		Attack: func(_ *testing.T, f *testFixture) {
			f.mu.Lock()
			gate := f.specSeals["run_gate"]
			f.mu.Unlock()
			f.setSpecSeal("run_wait", gate)
		},
	}, { // Test 11: A waiting step's workflow rewritten.
		ParentID: "run_park", ID: "run_wait", Keyed: true,
		WantText: "workflow run run_park: its spec",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_park", func(r *run.Run) { r.Steps[1].Playbook = "destroy.yml" })
		},
	}, { // Test 12: A waiting step's workflow cleared of the flag it passed to the step.
		ParentID: "run_park", ID: "run_wait", Keyed: true,
		WantText: "workflow run run_park: its spec",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_park", func(r *run.Run) { r.RequireDistinctApprover = false })
		},
	}, { // Test 13: A waiting step's workflow decision deleted.
		ParentID: "run_park", ID: "run_wait", Keyed: true,
		WantText: "workflow run run_park: chain entry aud_park",
		Attack: func(_ *testing.T, f *testFixture) {
			f.Chain.mu.Lock()
			delete(f.Chain.entries, "aud_park")
			f.Chain.mu.Unlock()
		},
	}, { // Test 14: A waiting step presented under another workflow.
		ParentID: "run_wf", ID: "run_wait", Keyed: true,
		WantText: "it is not a step of workflow run_wf",
		Attack:   func(*testing.T, *testFixture) {},
	}, { // Test 15: A waiting step flipped into an executable step.
		ParentID: "run_park", ID: "run_wait", Keyed: true, WantText: "it is not an approval step",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_wait", func(r *run.Run) { r.Kind = "" })
		},
	}, { // Test 16: A waiting step whose workflow was made a child of another run.
		ParentID: "run_park", ID: "run_wait", Keyed: true,
		WantText: "its workflow run_park is itself a child",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_park", func(r *run.Run) { r.ParentID = new("run_wf") })
		},
	}, { // Test 17: A waiting step's workflow decision deleted, no key.
		ParentID: "run_park", ID: "run_wait", WantText: "aud_park it derives from is not on the chain",
		Attack: func(_ *testing.T, f *testFixture) {
			f.Chain.mu.Lock()
			delete(f.Chain.entries, "aud_park")
			f.Chain.mu.Unlock()
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			test.Attack(t, f)
			k := f.Key
			if !test.Keyed {
				k = nil
			}
			requireDrift(t, f.undecided(t, f.verifier(k), test.ParentID, test.ID), test.WantText)
		})
	}
}

// TestPendingReadErrorIsNotTampering pins that a waiting step whose workflow's decision cannot be
// read is reported as could not verify carrying the store error, never as a refusal.
func TestPendingReadErrorIsNotTampering(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	parent, node := f.row(t, "run_park"), f.row(t, "run_wait")
	f.Chain.failReads = 100
	err := f.verifier(f.Key).Pending(context.Background(), parent, node)
	requireUnverified(t, err)
	if !errors.Is(err, errTestStore) {
		t.Errorf("error = %v, want it to carry the store error", err)
	}
}

// TestResultHoldsASucceededStepToItsSeal pins that a succeeded step's status and outputs feed a
// walk only as the process wrote them, and that a step that did not succeed needs no seal.
func TestResultHoldsASucceededStepToItsSeal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Attack   func(t *testing.T, f *testFixture)
		ID       string
		WantText string
	}{{ // Test 0: An honest succeeded step.
		ID: "run_build", Attack: func(*testing.T, *testFixture) {},
	}, { // Test 1: A step that has not finished needs no seal.
		ID: "run_deploy", Attack: func(*testing.T, *testFixture) {},
	}, { // Test 2: A failed step needs no seal.
		ID: "run_build",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_build", func(r *run.Run) { r.Status = run.StatusFailed })
		},
	}, { // Test 3: Outputs rewritten.
		ID: "run_build", WantText: "its status or its outputs changed after it finished",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_build", func(r *run.Run) { r.Outputs["version"] = "6.6" })
		},
	}, { // Test 4: Outputs added.
		ID: "run_build", WantText: "its status or its outputs changed after it finished",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_build", func(r *run.Run) { r.Outputs["target"] = "all" })
		},
	}, { // Test 5: A step that never finished flipped to succeeded with outputs.
		ID: "run_deploy", WantText: "carries no seal",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_deploy", func(r *run.Run) {
				r.Status = run.StatusSucceeded
				r.Outputs = map[string]any{"version": "6.6"}
			})
		},
	}, { // Test 6: The attempt moved.
		ID: "run_build", WantText: "its status or its outputs changed after it finished",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_build", func(r *run.Run) { r.Attempt = 2 })
		},
	}, { // Test 7: The spec seal offered as the result seal.
		ID: "run_build", WantText: "its status or its outputs changed after it finished",
		Attack: func(_ *testing.T, f *testFixture) {
			f.mu.Lock()
			f.resultSeals["run_build"] = f.specSeals["run_build"]
			f.mu.Unlock()
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			test.Attack(t, f)
			err := f.verifier(f.Key).Result(context.Background(), f.row(t, test.ID))
			if test.WantText == "" {
				if err != nil {
					t.Errorf("Result = %v, want nil", err)
				}
				return
			}
			requireDrift(t, err, test.WantText)
		})
	}
}

// TestAChainReadErrorIsNotTampering pins that a store read that fails is retried three times with
// no wait, that one that recovers within them verifies, and that one that does not is reported as
// could not verify carrying the store error, never as a refusal.
func TestAChainReadErrorIsNotTampering(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Setup        func(f *testFixture, v *Verifier)
		Reads        func(f *testFixture) int
		ID           string
		WantReads    int
		WantVerify   bool
		WantStoreErr bool
	}{{ // Test 0: One failed chain read, then an answer.
		ID: "run_app", WantReads: 2, WantVerify: true,
		Setup: func(f *testFixture, _ *Verifier) { f.Chain.failReads = 1 },
		Reads: func(f *testFixture) int { return f.Chain.reads },
	}, { // Test 1: Two failed chain reads, then an answer.
		ID: "run_app", WantReads: 3, WantVerify: true,
		Setup: func(f *testFixture, _ *Verifier) { f.Chain.failReads = 2 },
		Reads: func(f *testFixture) int { return f.Chain.reads },
	}, { // Test 2: Every chain read fails.
		ID: "run_app", WantReads: 3, WantStoreErr: true,
		Setup: func(f *testFixture, _ *Verifier) { f.Chain.failReads = 100 },
		Reads: func(f *testFixture) int { return f.Chain.reads },
	}, { // Test 3: Every read of a shard's parent fails.
		ID: "run_shard", WantReads: 3, WantStoreErr: true,
		Setup: func(f *testFixture, _ *Verifier) { f.Runs.failGets = 100 },
		Reads: func(f *testFixture) int { return f.Runs.gets },
	}, { // Test 4: Naming a step's roots fails.
		ID: "run_deploy", WantReads: 0, WantStoreErr: true,
		Setup: func(_ *testFixture, v *Verifier) {
			v.StepRoots = func(context.Context, *run.Run, *run.Run) ([]string, error) {
				return nil, errTestStore
			}
		},
		Reads: func(*testFixture) int { return 0 },
	}, { // Test 5: No chain reader.
		ID: "run_app", WantReads: 0,
		Setup: func(_ *testFixture, v *Verifier) { v.Entry = nil },
		Reads: func(f *testFixture) int { return f.Chain.reads },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			target := f.row(t, test.ID)
			f.Chain.reads, f.Runs.gets = 0, 0
			v := f.verifier(f.Key)
			test.Setup(f, v)
			err := v.Executable(context.Background(), target)
			if got := test.Reads(f); got != test.WantReads {
				t.Errorf("reads = %d, want %d", got, test.WantReads)
			}
			if test.WantVerify {
				if err != nil {
					t.Errorf("Executable = %v, want nil", err)
				}
				return
			}
			requireUnverified(t, err)
			if test.WantStoreErr && !errors.Is(err, errTestStore) {
				t.Errorf("error = %v, want it to carry the store error", err)
			}
		})
	}
}

// TestAnEndedContextStopsRetrying pins that a read under a context that has ended is tried once.
func TestAnEndedContextStopsRetrying(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	target := f.row(t, "run_app")
	f.Chain.reads = 0
	f.Chain.failReads = 100
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requireUnverified(t, f.verifier(f.Key).Executable(ctx, target))
	if f.Chain.reads != 1 {
		t.Errorf("reads = %d, want 1", f.Chain.reads)
	}
}

// TestUnkeyedVerifierChecksRootsOnly pins what an install with no producer identity gets: no seal
// is required or compared, and every chain entry a row names must still exist and record what the
// row says it does.
func TestUnkeyedVerifierChecksRootsOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Attack   func(t *testing.T, f *testFixture)
		ID       string
		WantText string
	}{{ // Test 0: A row with no seal.
		ID:     "run_app",
		Attack: func(_ *testing.T, f *testFixture) { f.setSpecSeal("run_app", "") },
	}, { // Test 1: A rewritten spec is left to the row checks the dispatcher keeps.
		ID: "run_shard",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_shard", func(r *run.Run) { r.Limit = "all" })
		},
	}, { // Test 2: A deleted decision entry.
		ID: "run_app", WantText: "is not on the chain",
		Attack: func(_ *testing.T, f *testFixture) {
			f.Chain.mu.Lock()
			delete(f.Chain.entries, "aud_app")
			f.Chain.mu.Unlock()
		},
	}, { // Test 3: Another run's decision.
		ID: "run_app", WantText: "is not the decision this row names",
		Attack: func(t *testing.T, f *testFixture) {
			f.edit(t, "run_app", func(r *run.Run) { r.DecisionID = "aud_other" })
		},
	}, { // Test 4: A deleted release entry.
		ID: "run_deploy", WantText: "is not on the chain",
		Attack: func(_ *testing.T, f *testFixture) {
			f.Chain.mu.Lock()
			delete(f.Chain.entries, "aud_rel")
			f.Chain.mu.Unlock()
		},
	}, { // Test 5: An approval step presented for execution.
		ID: "run_gate", WantText: "never executes",
		Attack: func(*testing.T, *testFixture) {},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			test.Attack(t, f)
			err := f.verifier(nil).Executable(context.Background(), f.row(t, test.ID))
			if test.WantText == "" {
				if err != nil {
					t.Errorf("Executable = %v, want nil", err)
				}
				return
			}
			requireDrift(t, err, test.WantText)
		})
	}
}

// TestFixtureRowsAreIndependent guards the fixture: a copy a test changes is never the stored row.
func TestFixtureRowsAreIndependent(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	r := f.row(t, "run_build")
	r.Outputs["version"] = "changed"
	want := map[string]any{"version": "1.2"}
	if got := f.row(t, "run_build").Outputs; !maps.Equal(got, want) {
		t.Errorf("stored outputs = %v, want %v", got, want)
	}
}

// TestAChildSealedOverAnotherParentIsRefused pins that a child's seal binds the parent it was
// built under. The parent stays honest and keeps its own seal, and each child carries a seal over a
// record built from a copy of its parent with a different spec, so the only field that differs from
// what Executable, Pending, and Gate rebuild is the parent's binding. Every dispatch-level flow
// checks a child's parent first, so a parent that passes there is the one the child was sealed
// under, and only this test holds the field to its purpose: a child's seal is self-contained when
// a caller checks it without its parent.
//
//nolint:funlen // Test function.
func TestAChildSealedOverAnotherParentIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Name labels the child.
		Name string
		// ParentID is the honest parent.
		ParentID string
		// ID is the child sealed over another parent.
		ID string
		// Kind is the kind of record the child is sealed with.
		Kind string
		// Record builds the child's record over parent the way its kind is built.
		Record func(t *testing.T, f *testFixture, parent, child *run.Run) (Record, error)
		// Check runs the check the child faces.
		Check func(t *testing.T, f *testFixture, v *Verifier) error
	}{{ // Test 0: A shard of an approved split, checked before it executes.
		Name: "shard", ParentID: "run_split", ID: "run_shard", Kind: KindShard,
		Record: func(_ *testing.T, _ *testFixture, parent, child *run.Run) (Record, error) {
			return ShardRecord(parent, child)
		},
		Check: func(t *testing.T, f *testFixture, v *Verifier) error {
			return v.Executable(ctx, f.row(t, "run_shard"))
		},
	}, { // Test 1: A step an approval released, checked before it executes.
		Name: "step", ParentID: "run_wf", ID: "run_deploy", Kind: KindStep,
		Record: func(t *testing.T, f *testFixture, parent, child *run.Run) (Record, error) {
			var roots []Root
			for _, id := range []string{"aud_wf", "aud_gate", "aud_rel"} {
				e, err := f.Chain.entry(ctx, id)
				if err != nil || e == nil {
					t.Fatalf("entry %s: %v", id, err)
				}
				roots = append(roots, RootOf(e))
			}
			return StepRecord(parent, child, roots)
		},
		Check: func(t *testing.T, f *testFixture, v *Verifier) error {
			return v.Executable(ctx, f.row(t, "run_deploy"))
		},
	}, { // Test 2: An approval step waiting for a decision, checked before it is decided.
		Name: "pending approval step", ParentID: "run_park", ID: "run_wait", Kind: KindStep,
		Record: func(_ *testing.T, _ *testFixture, parent, node *run.Run) (Record, error) {
			return StepRecord(parent, node, nil)
		},
		Check: func(t *testing.T, f *testFixture, v *Verifier) error {
			return f.undecided(t, v, "run_park", "run_wait")
		},
	}, { // Test 3: A decided approval step, checked before a resumed walk reads it as approved.
		Name: "decided approval step", ParentID: "run_wf", ID: "run_gate", Kind: KindGate,
		Record: func(t *testing.T, f *testFixture, parent, node *run.Run) (Record, error) {
			state, err := outcome.StepStateOf(ctx, f.Runs, f.row(t, "run_wf"), node)
			if err != nil {
				t.Fatalf("state: %v", err)
			}
			stateBinding, err := state.Binding()
			if err != nil {
				t.Fatalf("bind state: %v", err)
			}
			e, err := f.Chain.entry(ctx, "aud_gate")
			if err != nil || e == nil {
				t.Fatalf("entry aud_gate: %v", err)
			}
			return GateRecord(parent, node, stateBinding, RootOf(e))
		},
		Check: func(t *testing.T, f *testFixture, v *Verifier) error {
			return f.gate(t, v, "run_wf")
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			f := newTestFixture(t)
			parent, child := f.row(t, test.ParentID), f.row(t, test.ID)
			other := parent.Clone()
			other.ExtraVars = map[string]any{"to": "elsewhere"}
			honestBinding, err := storedBinding(parent)
			if err != nil {
				t.Fatalf("bind the parent: %v", err)
			}
			otherBinding, err := storedBinding(other)
			if err != nil {
				t.Fatalf("bind the other parent: %v", err)
			}
			if honestBinding == otherBinding {
				t.Fatal("the other parent binds the same as the honest one, so this proves nothing")
			}
			honest, err := test.Record(t, f, parent, child)
			if err != nil {
				t.Fatalf("honest record: %v", err)
			}
			shifted, err := test.Record(t, f, other, child)
			if err != nil {
				t.Fatalf("record over the other parent: %v", err)
			}
			// The two records differ in nothing but the parent's binding, so the refusal below is
			// that field's alone.
			honest.ParentBinding, shifted.ParentBinding = "", ""
			if diff := cmp.Diff(honest, shifted, cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("records differ beyond the parent binding (-honest +moved):\n%s", diff)
			}
			rec, err := test.Record(t, f, other, child)
			if err != nil {
				t.Fatalf("record over the other parent: %v", err)
			}
			f.setSpecSeal(test.ID, f.Key.Seal(rec))
			requireDrift(t, test.Check(t, f, f.verifier(f.Key)), moved(test.Kind))
		})
	}
}
