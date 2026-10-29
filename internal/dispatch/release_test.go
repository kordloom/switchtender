package dispatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// errLostAnswer is what an append whose answer was lost reports, though the entry landed.
var errLostAnswer = errors.New("read tcp 10.0.0.9:5432: i/o timeout")

// lossyChain is an audit store whose appends can land with their answer lost: the entry is on the
// chain, and the caller is told the write failed.
type lossyChain struct {
	audit.Store
	// mu guards lose.
	mu sync.Mutex
	// lose is how many more appends land and then report a failure.
	lose int
}

// Append appends e and, while lose lasts, reports a failure after it landed.
func (c *lossyChain) Append(ctx context.Context, e *audit.Entry) error {
	if err := c.Store.Append(ctx, e); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lose > 0 {
		c.lose--
		return errLostAnswer
	}
	return nil
}

// releaseResult is what launching one attempt of a released step left behind.
type releaseResult struct {
	// Records counts the distinct records the launchers were handed.
	Records int
	// Steps counts the step records stored for the attempt.
	Steps int
	// Releases counts the RELEASE entries on the chain.
	Releases int
	// FixedID reports whether the release carries the id audit.ReleaseID fixes.
	FixedID bool
	// Verified reports whether the executor accepts the stored record.
	Verified bool
	// Named reports whether the record's seal names the workflow's decision and the release.
	Named bool
	// Bound reports whether the release commits a body naming the workflow's decision by the
	// content digest its entry committed.
	Bound bool
}

// TestAReleaseIsRecordedOnceUnderAFixedID pins that launching a step an approver released appends
// one RELEASE entry under the id its decisions, its workflow, its index, and its attempt fix,
// before the step's record is stored. A second launcher of the same attempt, whether it starts
// after the first or at the same moment, builds no second record and is handed the one the first
// released. A launcher whose append landed with its answer lost keeps its own record rather than
// adopting it as somebody else's. The stored record's seal names the release, so the executor
// holds it to the entry, and the release names the decision by the content digest its entry
// committed. The step is launched by hand behind an approval step nobody has decided yet, in a
// workflow an approver released, so nothing else launches it.
//
//nolint:funlen // Test function.
func TestAReleaseIsRecordedOnceUnderAFixedID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Launchers is how many launchers start the attempt.
		Launchers int
		// Together reports whether the launchers start at once rather than one after another.
		Together bool
		// Lose is how many appends land with their answer lost.
		Lose int
		// WantResult is what the launches left behind.
		WantResult releaseResult
	}{{ // Test 0: One launcher.
		Launchers: 1,
		WantResult: releaseResult{Records: 1, Steps: 1, Releases: 1, FixedID: true, Verified: true,
			Named: true, Bound: true},
	}, { // Test 1: A second launcher after the first stored its record.
		Launchers: 2,
		WantResult: releaseResult{Records: 1, Steps: 1, Releases: 1, FixedID: true, Verified: true,
			Named: true, Bound: true},
	}, { // Test 2: Two launchers at once.
		Launchers: 2, Together: true,
		WantResult: releaseResult{Records: 1, Steps: 1, Releases: 1, FixedID: true, Verified: true,
			Named: true, Bound: true},
	}, { // Test 3: One launcher whose append landed with its answer lost.
		Launchers: 1, Lose: 1,
		WantResult: releaseResult{Records: 1, Steps: 1, Releases: 1, FixedID: true, Verified: true,
			Named: true, Bound: true},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			key := bndKey(t)
			store, chain := run.NewMemStore(), &lossyChain{Store: audit.NewMemStore()}
			d := New(store, dispatchtest.OKRunner(), nil, WithNoJanitor(), WithAudits(chain), WithBindingKey(key),
				WithQueues([]string{"decisions"}))
			t.Cleanup(d.Close)
			steps := []run.PipelineStep{
				{Name: "gate", Type: run.StepApproval, Description: "Ship it?"},
				{Name: "deploy", Tool: run.ToolBash, Command: "deploy", DependsOn: []string{"gate"}},
			}
			held, err := d.SubmitPipeline(ctx, "release", "", steps, run.WithRequireApproval(true))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			if _, err := d.Approve(ctx, held.ID, bndDecider()); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			waitParked(t, store, held.ID)
			parent, err := store.Get(ctx, held.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			roots, err := d.workflowRoots(ctx, parent)
			if err != nil || len(roots) != 1 {
				t.Fatalf("workflowRoots() = %v, %v, want the workflow's decision", roots, err)
			}
			chain.mu.Lock()
			chain.lose = test.Lose
			chain.mu.Unlock()

			step := run.GraphSteps(parent.Steps)[1]
			ids := make([]string, test.Launchers)
			launch := func(n int) {
				child, ended, reason := d.startAttempt(ctx, parent, step, 1, 0, nil, roots)
				if child == nil {
					t.Errorf("launcher %d stored nothing: ended %s: %s", n, ended, reason)
					return
				}
				ids[n] = child.ID
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			for n := range test.Launchers {
				if !test.Together {
					launch(n)
					continue
				}
				wg.Go(func() {
					<-start
					launch(n)
				})
			}
			close(start)
			wg.Wait()

			got := releaseResult{Records: len(slices.Compact(slices.Sorted(slices.Values(ids))))}
			stored, err := store.Steps(ctx, parent.ID)
			if err != nil {
				t.Fatalf("Steps() error = %v", err)
			}
			var child *run.Run
			for _, s := range stored {
				if s.StepIndex != nil && *s.StepIndex == 1 && s.Attempt == 0 {
					got.Steps++
					child = s
				}
			}
			entries, err := chain.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			var release, decided *audit.Entry
			for _, e := range entries {
				switch {
				case e.Method == audit.MethodRelease:
					got.Releases++
					release = e
				case e.ID == parent.DecisionID:
					decided = e
				}
			}
			if child != nil && release != nil {
				got.FixedID = release.ID == audit.ReleaseID([]string{parent.DecisionID}, parent.ID, 1, 0) &&
					release.Path == audit.ReleasePath(parent.ID, child.ID)
				got.Verified = d.binder.Executable(ctx, child) == nil
				rec, err := binding.StepRecord(parent, child,
					append(slices.Clone(roots), binding.RootOf(release)))
				got.Named = err == nil && key.Verify(rec, child.SpecSeal)
			}
			if child != nil && release != nil && decided != nil {
				body, err := outcome.ReleaseBody(child, []outcome.ReleaseRoot{{ID: decided.ID,
					ContentDigest: decided.ContentDigest}})
				got.Bound = err == nil &&
					audit.VerifyContentDigest(release.ContentDigest, release.Nonce, body)
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("release mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
