package outcome

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/kordloom/loomseal/jcs"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// TestBodyTaskDurationsAreJCSSafe proves the outcome record of a real run, whose task durations are
// fractional seconds, reduces to bytes the LoomSeal JCS profile accepts. The profile refuses
// non-integer numbers, so a float anywhere in the record makes every range receipt unsignable.
func TestBodyTaskDurationsAreJCSSafe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Seconds float64
		WantMS  int64
	}{{ // Test 0: The fractional duration every real run produces.
		Seconds: 0.013500213, WantMS: 14,
	}, { // Test 1: A whole-second duration.
		Seconds: 3, WantMS: 3000,
	}, { // Test 2: A zero duration.
		Seconds: 0, WantMS: 0,
	}, { // Test 3: A half-millisecond rounds rather than truncates.
		Seconds: 0.0015, WantMS: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			// Mirror the real order: summaries land while the run is live, then it finalizes.
			r := &run.Run{ID: "run_jcs", Playbook: "site.yml", Status: run.StatusRunning}
			if err := store.Save(ctx, r); err != nil {
				t.Fatalf("Save(running) error = %v", err)
			}
			if err := store.SaveTaskSummary(ctx, r.ID, []run.TaskSummary{
				{Task: "deploy", Seconds: test.Seconds},
			}); err != nil {
				t.Fatalf("SaveTaskSummary() error = %v", err)
			}
			r.Status = run.StatusSucceeded
			if err := store.Save(ctx, r); err != nil {
				t.Fatalf("Save(succeeded) error = %v", err)
			}

			body, err := Body(ctx, store, r)
			if err != nil {
				t.Fatalf("Body() error = %v", err)
			}
			if _, err := jcs.Canonicalize(body); err != nil {
				t.Fatalf("Canonicalize() refused the outcome body: %v", err)
			}
			rec, err := Parse(body)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(rec.Tasks) != 1 || rec.Tasks[0].Milliseconds != test.WantMS {
				t.Errorf("Tasks = %+v, want one task at %d ms", rec.Tasks, test.WantMS)
			}
		})
	}
}

// TestBodyNamesSealedDigestsOnly covers the two digests a sealed child adds to its coordinator's
// outcome, and the rule that keeps every older receipt verifying: a receipt rebuilds the outcome from
// the store when it is asked for, so a member that appeared on a row with no seal would rebuild every
// outcome an older binary committed to bytes the chain never held. A child's outputs_digest is present
// only when the child carries a result seal, and its spec_digest only when it carries a spec seal. Only
// a child is sealed over its result, so the coordinator's own record never names its outputs.
func TestBodyNamesSealedDigestsOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// ChildOutputs are the values the child published.
		ChildOutputs map[string]any
		// ParentResultSeal is the result seal on the coordinator's own row.
		ParentResultSeal string
		// ChildResultSeal is the result seal on the child's row.
		ChildResultSeal string
		// ChildSpecSeal is the spec seal on the child's row.
		ChildSpecSeal string
		// WantChildOutputs reports whether the child's record names its outputs digest.
		WantChildOutputs bool
		// WantChildSpec reports whether the child's record names its spec digest.
		WantChildSpec bool
	}{{ // Test 0: No seal anywhere, so the record is the one it always was.
		ChildOutputs: map[string]any{"version": "1.2.3"},
	}, { // Test 1: A result seal on the child adds the digest of its outputs.
		ChildOutputs: map[string]any{"version": "1.2.3"}, ChildResultSeal: "bk1:result",
		WantChildOutputs: true,
	}, { // Test 2: A spec seal on the child adds that child's spec digest, and nothing else.
		ChildOutputs: map[string]any{"version": "1.2.3"}, ChildSpecSeal: "bk1:spec",
		WantChildSpec: true,
	}, { // Test 3: Both seals add both digests.
		ChildOutputs: map[string]any{"version": "1.2.3"}, ChildResultSeal: "bk1:result",
		ChildSpecSeal: "bk1:spec", WantChildOutputs: true, WantChildSpec: true,
	}, { // Test 4: A sealed child that published nothing names the digest of no outputs.
		ChildResultSeal: "bk1:result", WantChildOutputs: true,
	}, { // Test 5: A result seal on the coordinator adds nothing to the record.
		ChildOutputs: map[string]any{"version": "1.2.3"}, ParentResultSeal: "bk1:result",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			parent := &run.Run{
				ID: "run_sealed", Kind: run.KindPipeline, Status: run.StatusSucceeded,
				Tool: run.ToolBash, Steps: []run.PipelineStep{{Name: "build", Command: "make"}},
				Outputs: map[string]any{"release": "1.2.3"}, ResultSeal: test.ParentResultSeal,
				CreatedAt: time.Now(),
			}
			if err := store.Save(ctx, parent); err != nil {
				t.Fatalf("Save(parent) error = %v", err)
			}
			idx := 0
			child := &run.Run{
				ID: "run_sealed_build", ParentID: &parent.ID, StepIndex: &idx, StepName: "build",
				Tool: run.ToolBash, Command: "make", Status: run.StatusSucceeded,
				Outputs: test.ChildOutputs, SpecSeal: test.ChildSpecSeal,
				ResultSeal: test.ChildResultSeal, CreatedAt: time.Now(),
			}
			if err := store.Save(ctx, child); err != nil {
				t.Fatalf("Save(child) error = %v", err)
			}

			body, err := Body(ctx, store, parent)
			if err != nil {
				t.Fatalf("Body() error = %v", err)
			}
			var top map[string]json.RawMessage
			if err := json.Unmarshal(body, &top); err != nil {
				t.Fatalf("Unmarshal(body) error = %v", err)
			}
			var children []map[string]json.RawMessage
			if err := json.Unmarshal(top["children"], &children); err != nil || len(children) != 1 {
				t.Fatalf("children = %s, want one child: %v", top["children"], err)
			}
			_, gotOutputs := top["outputs_digest"]
			_, gotChildOutputs := children[0]["outputs_digest"]
			_, gotChildSpec := children[0]["spec_digest"]
			if diff := cmp.Diff([3]bool{false, test.WantChildOutputs, test.WantChildSpec},
				[3]bool{gotOutputs, gotChildOutputs, gotChildSpec}); diff != "" {
				t.Fatalf("members present mismatch, as [outputs_digest, child outputs_digest, "+
					"child spec_digest] (-want +got):\n%s\nbody: %s", diff, body)
			}

			rec, err := Parse(body)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if test.WantChildOutputs {
				want, err := varsDigest(test.ChildOutputs)
				if err != nil {
					t.Fatalf("varsDigest() error = %v", err)
				}
				if diff := cmp.Diff(want, rec.Children[0].OutputsDigest); diff != "" {
					t.Errorf("child outputs_digest mismatch (-want +got):\n%s", diff)
				}
			}
			if test.WantChildSpec {
				want, err := SpecDigest(child)
				if err != nil {
					t.Fatalf("SpecDigest() error = %v", err)
				}
				if diff := cmp.Diff(want, rec.Children[0].SpecDigest); diff != "" {
					t.Errorf("child spec_digest mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestVarsDigestIsTakenOverTheRedactedForm pins what the disclosed digest of a run's outputs or a
// step's inputs commits to. It is shown to anybody holding a receipt, so a value that looks like a
// secret must not be the input to it, and it must survive the round trip a store applies to the
// values it digests.
func TestVarsDigestIsTakenOverTheRedactedForm(t *testing.T) {
	t.Parallel()
	tests := []struct {
		A        map[string]any
		B        map[string]any
		WantSame bool
	}{{ // Test 0: Nil and empty are the same, since a store does not keep the difference.
		A: nil, B: map[string]any{}, WantSame: true,
	}, { // Test 1: An integer and the float a store reads it back as are the same value.
		A: map[string]any{"count": 3}, B: map[string]any{"count": float64(3)}, WantSame: true,
	}, { // Test 2: Another value is another digest.
		A: map[string]any{"region": "east"}, B: map[string]any{"region": "west"},
	}, { // Test 3: Another name is another digest.
		A: map[string]any{"region": "east"}, B: map[string]any{"zone": "east"},
	}, { // Test 4: A password is redacted before the digest, so no guess can be confirmed by it.
		A: map[string]any{"password": "hunter2"}, B: map[string]any{"password": "letmein"},
		WantSame: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			a, err := varsDigest(test.A)
			if err != nil {
				t.Fatalf("varsDigest(A) error = %v", err)
			}
			b, err := varsDigest(test.B)
			if err != nil {
				t.Fatalf("varsDigest(B) error = %v", err)
			}
			if (a == b) != test.WantSame {
				t.Errorf("varsDigest() = %q and %q, want same = %v", a, b, test.WantSame)
			}
			vars := test.A
			if vars == nil {
				vars = map[string]any{}
			}
			raw, err := json.Marshal(vars)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			reduced, err := audit.CanonicalRedacted(raw)
			if err != nil {
				t.Fatalf("CanonicalRedacted() error = %v", err)
			}
			if want := audit.UnkeyedDigestOfReduced(reduced); a != want {
				t.Errorf("varsDigest(A) = %q, want the unkeyed digest of the redacted form %q", a,
					want)
			}
		})
	}
}
