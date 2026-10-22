package outcome_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// releasedStep returns a workflow step as a launch builds it, before anything saves it.
func releasedStep() *run.Run {
	parent, idx := "run_wf", 1
	return &run.Run{
		ID: "run_wf_deploy", ParentID: &parent, StepIndex: &idx, StepName: "deploy", Attempt: 2,
		Tool: run.ToolBash, Command: "deploy", ExtraVars: map[string]any{"region": "east"},
		Status: run.StatusPending, CreatedAt: time.Now(),
	}
}

// flowRoot and gateRoot are two decision entries a release derives from, each named by its id and
// the content digest it committed.
var (
	flowRoot = outcome.ReleaseRoot{ID: "aud_flow", ContentDigest: "sha256s:flow"}
	gateRoot = outcome.ReleaseRoot{ID: "aud_gate", ContentDigest: "sha256s:gate"}
)

// idsOf returns the ids roots names, in their order.
func idsOf(roots []outcome.ReleaseRoot) []string {
	ids := make([]string, len(roots))
	for i, root := range roots {
		ids[i] = root.ID
	}
	return ids
}

// inputsDigestOf returns the digest a release names for vars, computed here from the redaction
// the audit package exports rather than through the outcome package.
func inputsDigestOf(t *testing.T, vars string) string {
	t.Helper()
	reduced, err := audit.CanonicalRedacted([]byte(vars))
	if err != nil {
		t.Fatalf("CanonicalRedacted() error = %v", err)
	}
	return audit.UnkeyedDigestOfReduced(reduced)
}

// TestReleaseBodyNamesTheStepItReleased pins the release record: the workflow, the step record, its
// position and attempt, the decisions that released it by id and content digest in id order, and
// digests of its spec and its inputs. A body for a run that is no workflow step is refused, since
// nothing releases it.
func TestReleaseBodyNamesTheStepItReleased(t *testing.T) {
	t.Parallel()
	base := releasedStep()
	specDigest, err := outcome.SpecDigest(base)
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	missing := outcome.ReleaseRoot{ID: "aud_gone"}
	tests := []struct {
		Edit          func(*run.Run)
		Roots         []outcome.ReleaseRoot
		WantRecord    outcome.ReleaseRecord
		WantRootsJSON string
		WantErr       bool
	}{{ // Test 0: The roots are recorded by id and digest, sorted by id, whatever order they came in.
		Roots: []outcome.ReleaseRoot{gateRoot, flowRoot},
		WantRecord: outcome.ReleaseRecord{
			RunID: "run_wf", StepRunID: "run_wf_deploy", Step: "deploy", Index: 1, Attempt: 2,
			Roots: []outcome.ReleaseRoot{flowRoot, gateRoot}, SpecDigest: specDigest,
			InputsDigest: inputsDigestOf(t, `{"region":"east"}`),
		},
		WantRootsJSON: `[{"id":"aud_flow","content_digest":"sha256s:flow"},` +
			`{"id":"aud_gate","content_digest":"sha256s:gate"}]`,
	}, { // Test 1: No roots encode as an empty list rather than null.
		WantRecord: outcome.ReleaseRecord{
			RunID: "run_wf", StepRunID: "run_wf_deploy", Step: "deploy", Index: 1, Attempt: 2,
			Roots: []outcome.ReleaseRoot{}, SpecDigest: specDigest,
			InputsDigest: inputsDigestOf(t, `{"region":"east"}`),
		},
		WantRootsJSON: `[]`,
	}, { // Test 2: A step handed no inputs names the digest of none.
		Edit: func(c *run.Run) { c.ExtraVars = nil }, Roots: []outcome.ReleaseRoot{flowRoot},
		WantRecord: outcome.ReleaseRecord{
			RunID: "run_wf", StepRunID: "run_wf_deploy", Step: "deploy", Index: 1, Attempt: 2,
			Roots: []outcome.ReleaseRoot{flowRoot}, InputsDigest: inputsDigestOf(t, `{}`),
		},
	}, { // Test 3: A root whose entry was not found is recorded with its empty digest, not dropped.
		Roots: []outcome.ReleaseRoot{missing, flowRoot},
		WantRecord: outcome.ReleaseRecord{
			RunID: "run_wf", StepRunID: "run_wf_deploy", Step: "deploy", Index: 1, Attempt: 2,
			Roots: []outcome.ReleaseRoot{flowRoot, missing}, SpecDigest: specDigest,
			InputsDigest: inputsDigestOf(t, `{"region":"east"}`),
		},
		WantRootsJSON: `[{"id":"aud_flow","content_digest":"sha256s:flow"},` +
			`{"id":"aud_gone","content_digest":""}]`,
	}, { // Test 4: Roots sharing an id are ordered by digest, so any order encodes to one body.
		Roots: []outcome.ReleaseRoot{{ID: "aud_flow", ContentDigest: "sha256s:b"},
			{ID: "aud_flow", ContentDigest: "sha256s:a"}},
		WantRecord: outcome.ReleaseRecord{
			RunID: "run_wf", StepRunID: "run_wf_deploy", Step: "deploy", Index: 1, Attempt: 2,
			Roots: []outcome.ReleaseRoot{{ID: "aud_flow", ContentDigest: "sha256s:a"},
				{ID: "aud_flow", ContentDigest: "sha256s:b"}},
			SpecDigest: specDigest, InputsDigest: inputsDigestOf(t, `{"region":"east"}`),
		},
	}, { // Test 5: A top-level run is no workflow step.
		Edit: func(c *run.Run) { c.ParentID = nil }, Roots: []outcome.ReleaseRoot{flowRoot},
		WantErr: true,
	}, { // Test 6: A child with no step index is a shard, which no decision releases one by one.
		Edit: func(c *run.Run) { c.StepIndex = nil }, Roots: []outcome.ReleaseRoot{flowRoot},
		WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			child := releasedStep()
			if test.Edit != nil {
				test.Edit(child)
			}
			roots := append([]outcome.ReleaseRoot(nil), test.Roots...)
			body, err := outcome.ReleaseBody(child, roots)
			if (err != nil) != test.WantErr {
				t.Fatalf("ReleaseBody() error = %v, want error = %v", err, test.WantErr)
			}
			if test.WantErr {
				return
			}
			if test.WantRecord.SpecDigest == "" {
				if test.WantRecord.SpecDigest, err = outcome.SpecDigest(child); err != nil {
					t.Fatalf("SpecDigest() error = %v", err)
				}
			}
			var got outcome.ReleaseRecord
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if diff := cmp.Diff(test.WantRecord, got); diff != "" {
				t.Errorf("release record mismatch (-want +got):\n%s", diff)
			}
			if test.WantRootsJSON != "" && !strings.Contains(string(body),
				`"roots":`+test.WantRootsJSON+`,`) {
				t.Errorf("body %s does not encode its roots as %s", body, test.WantRootsJSON)
			}
			if diff := cmp.Diff(test.Roots, roots, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ReleaseBody() reordered its input (-want +got):\n%s", diff)
			}
		})
	}
}

// TestReleaseBodyMovesWithWhatWasReleased pins what a release binds. Every field that decides what
// the step does, or which decisions, as their entries committed them, and which attempt released
// it, moves the body, so a receipt rebuilding it from a row or a decision entry changed since the
// release shows a body the chain refuses. What the step went on to do after its release does not
// move it, so an honest step rebuilds to the same bytes.
func TestReleaseBodyMovesWithWhatWasReleased(t *testing.T) {
	t.Parallel()
	roots := []outcome.ReleaseRoot{flowRoot, gateRoot}
	swapped := outcome.ReleaseRoot{ID: "aud_else", ContentDigest: gateRoot.ContentDigest}
	rewritten := outcome.ReleaseRoot{ID: gateRoot.ID, ContentDigest: "sha256s:other"}
	base, err := outcome.ReleaseBody(releasedStep(), roots)
	if err != nil {
		t.Fatalf("ReleaseBody() error = %v", err)
	}
	tests := []struct {
		Edit      func(*run.Run)
		Roots     []outcome.ReleaseRoot
		WantMoved bool
	}{{ // Test 0: The command.
		Edit: func(c *run.Run) { c.Command = "deploy --everywhere" }, WantMoved: true,
	}, { // Test 1: The inputs.
		Edit: func(c *run.Run) { c.ExtraVars = map[string]any{"region": "west"} }, WantMoved: true,
	}, { // Test 2: The attempt.
		Edit: func(c *run.Run) { c.Attempt = 3 }, WantMoved: true,
	}, { // Test 3: The step's position.
		Edit: func(c *run.Run) { idx := 2; c.StepIndex = &idx }, WantMoved: true,
	}, { // Test 4: The step's name.
		Edit: func(c *run.Run) { c.StepName = "rollback" }, WantMoved: true,
	}, { // Test 5: The step record.
		Edit: func(c *run.Run) { c.ID = "run_wf_other" }, WantMoved: true,
	}, { // Test 6: The workflow.
		Edit: func(c *run.Run) { other := "run_other"; c.ParentID = &other }, WantMoved: true,
	}, { // Test 7: A decision swapped for another.
		Roots: []outcome.ReleaseRoot{flowRoot, swapped}, WantMoved: true,
	}, { // Test 8: A decision dropped.
		Roots: []outcome.ReleaseRoot{flowRoot}, WantMoved: true,
	}, { // Test 9: The same decisions in another order are the same release.
		Roots: []outcome.ReleaseRoot{gateRoot, flowRoot},
	}, { // Test 10: A decision entry rewritten on the chain commits another digest under its id.
		Roots: []outcome.ReleaseRoot{flowRoot, rewritten}, WantMoved: true,
	}, { // Test 11: A decision entry that could not be found names no digest.
		Roots: []outcome.ReleaseRoot{flowRoot, {ID: gateRoot.ID}}, WantMoved: true,
	}, { // Test 12: How the step ended is not what was released.
		Edit: func(c *run.Run) {
			code := 0
			c.Status, c.ExitCode = run.StatusSucceeded, &code
			c.Outputs = map[string]any{"url": "https://example.com"}
		},
	}, { // Test 13: Its seal and its creation time are not what was released.
		Edit: func(c *run.Run) { c.SpecSeal, c.CreatedAt = "bk1:sealed", time.Unix(0, 0) },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			child := releasedStep()
			if test.Edit != nil {
				test.Edit(child)
			}
			r := roots
			if test.Roots != nil {
				r = test.Roots
			}
			body, err := outcome.ReleaseBody(child, r)
			if err != nil {
				t.Fatalf("ReleaseBody() error = %v", err)
			}
			if moved := string(body) != string(base); moved != test.WantMoved {
				t.Errorf("body moved = %v, want %v:\n%s\n%s", moved, test.WantMoved, base, body)
			}
		})
	}
}

// TestReleaseBodyRebuildsFromTheStoredStep covers the reason the body is exported: a receipt
// rebuilds it from the step as the store holds it, which must give the bytes the launch committed
// before it saved the step.
func TestReleaseBodyRebuildsFromTheStoredStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	child := releasedStep()
	child.ExtraVars = map[string]any{"region": "east", "replicas": 3, "canary": true}
	roots := []outcome.ReleaseRoot{gateRoot, flowRoot}
	launched, err := outcome.ReleaseBody(child, roots)
	if err != nil {
		t.Fatalf("ReleaseBody(launched) error = %v", err)
	}
	if err := store.Save(ctx, child); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	stored, err := store.Get(ctx, child.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	rebuilt, err := outcome.ReleaseBody(stored, roots)
	if err != nil {
		t.Fatalf("ReleaseBody(stored) error = %v", err)
	}
	if diff := cmp.Diff(string(launched), string(rebuilt)); diff != "" {
		t.Errorf("the stored step rebuilds to another release (-launched +rebuilt):\n%s", diff)
	}
}

// TestReleaseEntryCommitsTheBodyUnderTheFixedID pins the entry a launch appends: the release body
// committed under a content digest, the launcher named, the release path, and the id ReleaseID
// fixes from the roots' ids alone, so two launchers of one attempt build entries under one id even
// when they read different digests for its decisions.
func TestReleaseEntryCommitsTheBodyUnderTheFixedID(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
	launcher := outcome.Decider{Name: "system:dispatcher", Type: "system", OnBehalfOf: "casey"}
	tests := []struct {
		Edit    func(*run.Run)
		Now     func() time.Time
		Roots   []outcome.ReleaseRoot
		WantErr bool
	}{{ // Test 0: A step released by its workflow's decision and an approval step's.
		Roots: []outcome.ReleaseRoot{gateRoot, flowRoot}, Now: func() time.Time { return at },
	}, { // Test 1: A nil clock reads the time now.
		Roots: []outcome.ReleaseRoot{flowRoot},
	}, { // Test 2: A run that is no workflow step has no release entry.
		Edit: func(c *run.Run) { c.ParentID = nil }, Roots: []outcome.ReleaseRoot{flowRoot},
		WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			child := releasedStep()
			if test.Edit != nil {
				test.Edit(child)
			}
			before := time.Now()
			entry, err := outcome.ReleaseEntry(child, test.Roots, launcher, test.Now)
			if (err != nil) != test.WantErr {
				t.Fatalf("ReleaseEntry() error = %v, want error = %v", err, test.WantErr)
			}
			if test.WantErr {
				if entry != nil {
					t.Errorf("ReleaseEntry() = %+v beside an error, want nil", entry)
				}
				return
			}
			want := &audit.Entry{
				ID: audit.ReleaseID(idsOf(test.Roots), *child.ParentID, *child.StepIndex,
					child.Attempt),
				Actor: launcher.Name, ActorType: launcher.Type, OnBehalfOf: launcher.OnBehalfOf,
				Method: audit.MethodRelease, Path: audit.ReleasePath(*child.ParentID, child.ID),
			}
			if diff := cmp.Diff(want, entry, cmpopts.IgnoreFields(audit.Entry{}, "At",
				"ContentDigest", "Nonce")); diff != "" {
				t.Errorf("entry mismatch (-want +got):\n%s", diff)
			}
			switch {
			case test.Now != nil && !entry.At.Equal(at):
				t.Errorf("entry At = %v, want the clock's %v", entry.At, at)
			case test.Now == nil && entry.At.Before(before):
				t.Errorf("entry At = %v, want no earlier than %v", entry.At, before)
			}
			body, err := outcome.ReleaseBody(child, test.Roots)
			if err != nil {
				t.Fatalf("ReleaseBody() error = %v", err)
			}
			if !audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce, body) {
				t.Errorf("the entry's digest does not commit the release body %s", body)
			}
			again, err := outcome.ReleaseEntry(child, test.Roots, launcher, test.Now)
			if err != nil {
				t.Fatalf("second ReleaseEntry() error = %v", err)
			}
			if again.ID != entry.ID || again.Nonce == entry.Nonce {
				t.Errorf("a second launch built id %q nonce %q beside %q %q, want the same id "+
					"under a fresh nonce", again.ID, again.Nonce, entry.ID, entry.Nonce)
			}
			undigested := outcome.RootsOf(idsOf(test.Roots), func(string) string { return "" })
			other, err := outcome.ReleaseEntry(child, undigested, launcher, test.Now)
			if err != nil {
				t.Fatalf("ReleaseEntry(undigested) error = %v", err)
			}
			if other.ID != entry.ID || other.ContentDigest == entry.ContentDigest {
				t.Errorf("a launch that read other digests built id %q digest %q beside %q %q, "+
					"want the same id over another body", other.ID, other.ContentDigest, entry.ID,
					entry.ContentDigest)
			}
		})
	}
}

// TestConcurrentLaunchesReleaseAnAttemptOnce races launchers of one attempt against one chain, the
// case two replicas or a resume race produce. Exactly one release is recorded, and every other
// launcher learns the chain already holds it.
func TestConcurrentLaunchesReleaseAnAttemptOnce(t *testing.T) {
	t.Parallel()
	const launchers = 8
	ctx := context.Background()
	audits := audit.NewMemStore()
	roots := []outcome.ReleaseRoot{gateRoot, flowRoot}
	launcher := outcome.Decider{Name: "system:dispatcher", Type: "system"}
	errs := make([]error, launchers)
	var wg sync.WaitGroup
	for i := range launchers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entry, err := outcome.ReleaseEntry(releasedStep(), roots, launcher, nil)
			if err == nil {
				err = audits.Append(ctx, entry)
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	recorded := 0
	for _, err := range errs {
		switch {
		case err == nil:
			recorded++
		case !errors.Is(err, audit.ErrDuplicateID):
			t.Errorf("a launch failed with %v, want nil or %v", err, audit.ErrDuplicateID)
		}
	}
	if recorded != 1 {
		t.Fatalf("%d launches recorded a release, want exactly one", recorded)
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 1 || chain[0].Method != audit.MethodRelease {
		t.Errorf("chain = %d entries, want the one release", len(chain))
	}
}

// TestReleaseRootsReadTheDecisionsThatReleasedAStep pins the one rule both a launch and a receipt
// read to name a step's roots: the workflow's own decision, then the decision on each approval step
// the step waits behind over either kind of edge, in declaration order, from that approval step's
// latest record.
func TestReleaseRootsReadTheDecisionsThatReleasedAStep(t *testing.T) {
	t.Parallel()
	graph := []run.PipelineStep{
		{Name: "gate1", Type: run.StepApproval},
		{Name: "build", Command: "make", DependsOn: []string{"gate1"}},
		{Name: "gate2", Type: run.StepApproval, DependsOn: []string{"build"}},
		{Name: "deploy", Command: "deploy", DependsOn: []string{"gate2"}},
		{Name: "rollback", Command: "rollback", IfDenied: []string{"gate2"}},
		{Name: "side", Command: "lint"},
	}
	old := time.Unix(100, 0)
	young := time.Unix(200, 0)
	record := func(parent string, idx, attempt int, kind, decisionID string,
		created time.Time) *run.Run {
		i := idx
		return &run.Run{ID: fmt.Sprintf("%s_%d_%d_%s", parent, idx, attempt, decisionID),
			ParentID: &parent, StepIndex: &i, Attempt: attempt, Kind: kind,
			DecisionID: decisionID, CreatedAt: created}
	}
	decided := []*run.Run{
		record("run_wf", 0, 1, run.KindApproval, "aud_g1", young),
		record("run_wf", 2, 1, run.KindApproval, "aud_g2", young),
	}
	tests := []struct {
		Steps          []run.PipelineStep
		Records        []*run.Run
		ParentDecision string
		WantRoots      []string
		ChildIndex     int
	}{{ // Test 0: A step behind both approval steps, of a workflow an approver released.
		Records: decided, ParentDecision: "aud_wf", ChildIndex: 3,
		WantRoots: []string{"aud_wf", "aud_g1", "aud_g2"},
	}, { // Test 1: The same step of a workflow nobody held.
		Records: decided, ChildIndex: 3, WantRoots: []string{"aud_g1", "aud_g2"},
	}, { // Test 2: A step behind the first approval step only.
		Records: decided, ParentDecision: "aud_wf", ChildIndex: 1,
		WantRoots: []string{"aud_wf", "aud_g1"},
	}, { // Test 3: A deny path names the approval step it handles.
		Records: decided, ParentDecision: "aud_wf", ChildIndex: 4,
		WantRoots: []string{"aud_wf", "aud_g1", "aud_g2"},
	}, { // Test 4: A step behind no approval step derives from the workflow's decision alone.
		Records: decided, ParentDecision: "aud_wf", ChildIndex: 5, WantRoots: []string{"aud_wf"},
	}, { // Test 5: And from nothing in a workflow nobody held.
		Records: decided, ChildIndex: 5,
	}, { // Test 6: An approval step nobody decided names no decision.
		Records:        []*run.Run{decided[0], record("run_wf", 2, 1, run.KindApproval, "", young)},
		ParentDecision: "aud_wf", ChildIndex: 3, WantRoots: []string{"aud_wf", "aud_g1"},
	}, { // Test 7: The latest attempt of an approval step is the one that decided.
		Records: []*run.Run{record("run_wf", 0, 1, run.KindApproval, "aud_g1old", young),
			record("run_wf", 0, 2, run.KindApproval, "aud_g1", old), decided[1]},
		ChildIndex: 3, WantRoots: []string{"aud_g1", "aud_g2"},
	}, { // Test 8: Within an attempt, the record created last is the one that decided.
		Records: []*run.Run{record("run_wf", 0, 1, run.KindApproval, "aud_g1old", old),
			record("run_wf", 0, 1, run.KindApproval, "aud_g1", young), decided[1]},
		ChildIndex: 3, WantRoots: []string{"aud_g1", "aud_g2"},
	}, { // Test 9: A record of another workflow at that position is not this workflow's decision.
		Records:    []*run.Run{record("run_other", 0, 1, run.KindApproval, "aud_x", young), decided[1]},
		ChildIndex: 3, WantRoots: []string{"aud_g2"},
	}, { // Test 10: A record at an approval step's position that is not an approval record.
		Records:    []*run.Run{record("run_wf", 0, 1, "", "aud_x", young), decided[1]},
		ChildIndex: 3, WantRoots: []string{"aud_g2"},
	}, { // Test 11: A child outside the graph derives from the workflow's decision alone.
		Records: decided, ParentDecision: "aud_wf", ChildIndex: 9, WantRoots: []string{"aud_wf"},
	}, { // Test 12: A plain sequence with no approval step derives from the workflow's decision.
		Steps:          []run.PipelineStep{{Name: "a", Command: "a"}, {Name: "b", Command: "b"}},
		ParentDecision: "aud_wf", ChildIndex: 1, WantRoots: []string{"aud_wf"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			steps := graph
			if test.Steps != nil {
				steps = test.Steps
			}
			parent := &run.Run{ID: "run_wf", Kind: run.KindPipeline, Steps: steps,
				DecisionID: test.ParentDecision}
			idx := test.ChildIndex
			child := &run.Run{ID: "run_wf_child", ParentID: &parent.ID, StepIndex: &idx}
			got := outcome.ReleaseRoots(parent, child, test.Records)
			if diff := cmp.Diff(test.WantRoots, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("roots mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRootsOfNamesEachDecisionByItsDigest pins how a receipt names the decisions a step derives
// from: each id with the digest its entry committed, in the order the ids came in, and an empty
// digest for an entry the lookup cannot find, so the rebuilt body fails rather than being withheld.
func TestRootsOfNamesEachDecisionByItsDigest(t *testing.T) {
	t.Parallel()
	held := map[string]string{"aud_flow": "sha256s:flow", "aud_gate": "sha256s:gate"}
	tests := []struct {
		IDs       []string
		WantRoots []outcome.ReleaseRoot
	}{{ // Test 0: Each id with its entry's digest, in the order the ids came in.
		IDs:       []string{"aud_gate", "aud_flow"},
		WantRoots: []outcome.ReleaseRoot{gateRoot, flowRoot},
	}, { // Test 1: An entry the lookup cannot find is named with an empty digest, not dropped.
		IDs:       []string{"aud_flow", "aud_gone"},
		WantRoots: []outcome.ReleaseRoot{flowRoot, {ID: "aud_gone"}},
	}, { // Test 2: No ids name no roots.
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ids := append([]string(nil), test.IDs...)
			got := outcome.RootsOf(ids, func(id string) string { return held[id] })
			if diff := cmp.Diff(test.WantRoots, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("roots mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.IDs, ids, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("RootsOf() changed its input (-want +got):\n%s", diff)
			}
		})
	}
}
